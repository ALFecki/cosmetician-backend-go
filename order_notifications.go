package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pendingOrderNotification = `checkout_key IS NOT NULL AND notification_sent_at IS NULL
	AND (payment_started_at IS NULL OR payment_id IS NOT NULL OR payment_started_at <= now() - interval '30 seconds')`

var errTelegramDelivery = errors.New("Telegram did not confirm order notification")

type orderNotifications struct {
	db        *pgxpool.Pool
	client    *http.Client
	clientURL string
	endpoint  string
	chatID    string
	sendMu    sync.Mutex
}

func newOrderNotifications(db *pgxpool.Pool, clientURL, token, chatID string) (*orderNotifications, error) {
	storefront, err := url.Parse(clientURL)
	if err != nil || storefront.Hostname() == "" ||
		(storefront.Scheme != "https" && storefront.Scheme != "http") ||
		storefront.User != nil || storefront.RawQuery != "" || storefront.Fragment != "" {
		return nil, errors.New("ORDER_NOTIFICATIONS_ENABLED requires a valid CLIENT_URL")
	}
	if strings.TrimSpace(token) == "" || strings.TrimSpace(chatID) == "" {
		return nil, errors.New("ORDER_NOTIFICATIONS_ENABLED requires TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID")
	}
	return &orderNotifications{
		db:        db,
		client:    &http.Client{Timeout: 10 * time.Second, CheckRedirect: refuseRedirect},
		clientURL: strings.TrimRight(clientURL, "/"),
		endpoint:  "https://api.telegram.org/bot" + url.PathEscape(token) + "/sendMessage",
		chatID:    chatID,
	}, nil
}

type notificationPosition struct {
	ID        uuid.UUID
	CreatedAt time.Time
}

func (notifications *orderNotifications) run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	before := time.Now().Add(-15 * time.Second)
	position := notifications.retryPending(ctx, notificationPosition{}, before)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if position == (notificationPosition{}) {
				before = time.Now().Add(-15 * time.Second)
			}
			position = notifications.retryPending(ctx, position, before)
		}
	}
}

func (notifications *orderNotifications) retryPending(ctx context.Context, position notificationPosition, before time.Time) notificationPosition {
	const batchSize = 10
	queryCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	pending, err := queryStructs[notificationPosition](queryCtx, notifications.db, `
		SELECT id, created_at FROM "order" WHERE `+pendingOrderNotification+`
			AND (created_at, id) > ($1, $2) AND created_at <= $3
		ORDER BY created_at, id LIMIT $4`, position.CreatedAt, position.ID, before, batchSize)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("could not load pending order notifications")
		}
		return position
	}
	for _, ref := range pending {
		if ctx.Err() != nil {
			break
		}
		notifications.deliverLogged(ctx, ref.ID)
	}
	if len(pending) < batchSize {
		return notificationPosition{}
	}
	return pending[len(pending)-1]
}

func (notifications *orderNotifications) deliverLogged(ctx context.Context, id uuid.UUID) {
	if err := notifications.deliver(ctx, id); err != nil && ctx.Err() == nil {
		slog.Warn("order notification pending", "order", id)
	}
}

func (notifications *orderNotifications) deliver(ctx context.Context, id uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep one slow Telegram request from exhausting the shared database pool.
	if !notifications.sendMu.TryLock() {
		return nil
	}
	defer notifications.sendMu.Unlock()

	// Let an in-flight send record Telegram's reply before graceful shutdown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return pgx.BeginTxFunc(ctx, notifications.db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		// Rust uses the same row lock and delivery marker during rollout.
		entries, err := queryStructs[order](ctx, tx, orderSelect+" WHERE o.id = $1 AND "+pendingOrderNotification+
			" FOR UPDATE SKIP LOCKED", id)
		if err != nil || len(entries) == 0 {
			return err
		}
		entry := entries[0]
		var person account
		err = tx.QueryRow(ctx, `SELECT first_name, second_name, phone, contact_email, email FROM person WHERE id = $1`,
			entry.PersonID).Scan(&person.FirstName, &person.SecondName, &person.Phone, &person.ContactEmail, &person.Email)
		if err != nil {
			return err
		}
		if err := loadOrderProducts(ctx, tx, entries); err != nil {
			return err
		}
		if err := notifications.send(ctx, orderNotificationText(notifications.clientURL, entries[0], person)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE "order" SET notification_sent_at = now() WHERE id = $1`, id)
		return err
	})
}

func (notifications *orderNotifications) send(ctx context.Context, text string) error {
	count := 0
	for end := range text {
		if count == 3800 {
			text = text[:end] + "\nПолный состав и контакты — в заказе по ссылке выше."
			break
		}
		count++
	}
	form := url.Values{"chat_id": {notifications.chatID}, "text": {text}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, notifications.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errTelegramDelivery
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := notifications.client.Do(request)
	if err != nil {
		return errTelegramDelivery
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errTelegramDelivery
	}
	const maxReply = 256 * 1024
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReply+1))
	var result struct {
		OK bool `json:"ok"`
	}
	if err != nil || len(body) > maxReply || json.Unmarshal(body, &result) != nil || !result.OK {
		return errTelegramDelivery
	}
	return nil
}

func orderNotificationText(clientURL string, entry order, person account) string {
	number := "template"
	if entry.OrderID != nil {
		number = *entry.OrderID
	}
	var message strings.Builder
	fmt.Fprintf(&message, "Получен новый заказ:\nНомер заказа: %s\nЗаказ: %s/admin/order/%s\n",
		number, clientURL, entry.ID)

	first, second, phone := person.FirstName, person.SecondName, person.Phone
	email := person.ContactEmail
	if email == nil {
		email = person.Email
	}
	if len(entry.ContactSnapshot) != 0 {
		var snapshot map[string]any
		_ = json.Unmarshal(entry.ContactSnapshot, &snapshot)
		if value, ok := snapshot["firstName"].(string); ok {
			first = value
		}
		if value, ok := snapshot["secondName"].(string); ok {
			second = value
		}
		if value, ok := snapshot["phone"].(string); ok {
			phone = value
		}
		email = nil
		if value, ok := snapshot["email"].(string); ok {
			email = &value
		}
	}
	fmt.Fprintf(&message, "ФИО: %s %s\nТелефон: %s\n", first, second, phone)
	writeNotificationField(&message, "Email", email)

	payment := notificationLabel(entry.PaymentMethod, map[string]string{
		"cash": "Наличные", "card": "Карта", "online": "Онлайн-оплата",
	})
	delivery := notificationLabel(entry.DeliveryMethod, map[string]string{
		"standart": "Обычная доставка", "post": "Доставка Европочтой", "pickup": "Самовывоз",
	})
	courier := "Нет"
	if entry.IsCourierDelivery != nil && *entry.IsCourierDelivery {
		courier = "Да"
	}
	fmt.Fprintf(&message, "Способ оплаты: %s\nСпособ доставки: %s\nКурьер до двери: %s\n", payment, delivery, courier)
	writeNotificationField(&message, "Отделение Европочты", entry.DeliveryPostAddress)
	writeNotificationField(&message, "Адрес доставки", entry.DeliveryAddress)

	const separator = "---------------------------------\n"
	message.WriteString("Товары:\n")
	for _, line := range entry.Products {
		message.WriteString(separator)
		price := strconv.FormatFloat(float64(line.HistoricalPrice), 'f', -1, 32)
		fmt.Fprintf(&message, "Название товара: %s\nКоличество: %d\nЦена: %s руб.\nОбщая стоимость: %.2f руб.\n",
			line.Product.FullTitle, line.Quantity, price, line.HistoricalPrice*float32(line.Quantity))
	}
	message.WriteString(separator)
	fmt.Fprintf(&message, "Итого: %.2f руб.\n", entry.TotalPrice)
	if entry.DiscountAmount != nil && *entry.DiscountAmount > 0 {
		fmt.Fprintf(&message, "Скидка: %.2f руб.\n", *entry.DiscountAmount)
	}
	writeNotificationField(&message, "Комментарий", entry.Comment)
	return message.String()
}

func writeNotificationField(message *strings.Builder, label string, value *string) {
	if value != nil {
		fmt.Fprintf(message, "%s: %s\n", label, *value)
	}
}

func notificationLabel(value *string, labels map[string]string) string {
	if value != nil && labels[*value] != "" {
		return labels[*value]
	}
	return "Неизвестно"
}
