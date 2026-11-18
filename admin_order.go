package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type orderEditInput struct {
	PersonID            *uuid.UUID `json:"person_id"`
	LegacyPersonID      *uuid.UUID `json:"personId"`
	OrderNumber         *string    `json:"orderId"`
	Status              *string    `json:"status"`
	PaymentStatus       *string    `json:"paymentStatus"`
	PaymentID           *string    `json:"paymentId"`
	PaymentFormURL      *string    `json:"paymentFormUrl"`
	TotalPrice          *float32   `json:"totalPrice"`
	DeliveryCost        *int32     `json:"deliveryCost"`
	PromoCode           *string    `json:"promoCode"`
	DiscountAmount      *float32   `json:"discountAmount"`
	DeliveryMethod      *string    `json:"deliveryMethod"`
	DeliveryDate        *time.Time `json:"deliveryDate"`
	DeliveryAddress     *string    `json:"deliveryAddress"`
	DeliveryPostAddress *string    `json:"deliveryPostAddress"`
	IsCourierDelivery   *bool      `json:"isCourierDelivery"`
	PaymentMethod       *string    `json:"paymentMethod"`
	Comment             *string    `json:"comment"`
}

func (auth *accountAuth) registerLegacyCommerce(mux *http.ServeMux, db cartDatabase, mailer accountMailer) {
	mux.HandleFunc("POST /api/order", auth.requireCommerceAdmin(auth.createAdminOrder(db)))
	mux.HandleFunc("PATCH /api/order/{id}", auth.editOrder(db))
	mux.HandleFunc("DELETE /api/order/{id}", auth.requireCommerceAdmin(auth.archiveAdminOrder(db)))
	mux.HandleFunc("GET /api/email/send_status_update", auth.requireCommerceAdmin(auth.sendOrderStatusEmail(mailer)))
	auth.registerLegacyCart(mux, db)
	auth.registerPromoCodes(mux, db)
	auth.registerStatistics(mux)
}

func (auth *accountAuth) requireCommerceAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}
		if !*claims.IsAdmin {
			writeText(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}

func (auth *accountAuth) editOrder(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		var input orderEditInput
		if !decodeCommerceJSON(w, r, &input) {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var entry order
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			var owner uuid.UUID
			var status string
			if err := tx.QueryRow(ctx, `SELECT person_id, status::text FROM "order" WHERE id = $1 AND is_deleted IS NOT TRUE`,
				id).Scan(&owner, &status); err != nil {
				return err
			}

			if !*claims.IsAdmin {
				if owner != claims.ID.Bytes {
					return cartRequestError{http.StatusForbidden, "Нельзя редактировать чужой заказ"}
				}
				if status != "NotChecked" {
					return cartRequestError{http.StatusConflict, "Оформленный заказ нельзя изменить"}
				}
				if input.adminFieldsPresent() {
					return quoteInputError("Поля заказа управляются сервером")
				}
				cart, err := lockCart(ctx, tx, owner, false)
				if err != nil {
					return err
				}
				if cart.ID != id.Bytes {
					return errCartChanged
				}
				if err := updateDraftOrder(ctx, tx, cart, input); err != nil {
					return err
				}
			} else if err := updateAdminOrder(ctx, tx, id.Bytes, input); err != nil {
				return err
			}

			record, err := readCommerceOrder(ctx, tx, id.Bytes)
			entry = record
			return err
		})
		if err != nil {
			writeCommerceOrderError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, singleResponse{Status: "success", Data: entry})
	}
}

func (input orderEditInput) adminFieldsPresent() bool {
	return input.PersonID != nil || input.LegacyPersonID != nil || input.OrderNumber != nil ||
		input.Status != nil || input.PaymentStatus != nil || input.PaymentID != nil ||
		input.PaymentFormURL != nil || input.TotalPrice != nil || input.PromoCode != nil || input.DiscountAmount != nil
}

func updateDraftOrder(ctx context.Context, tx pgx.Tx, cart savedCart, input orderEditInput) error {
	method := "pickup"
	courier := false
	err := tx.QueryRow(ctx, `SELECT COALESCE(delivery_method, 'pickup'), COALESCE(is_courier_delivery, false)
		FROM "order" WHERE id = $1`, cart.ID).Scan(&method, &courier)
	if err != nil {
		return err
	}
	if input.DeliveryMethod != nil {
		method = *input.DeliveryMethod
	}
	if input.IsCourierDelivery != nil {
		courier = *input.IsCourierDelivery
	}
	items, err := savedCartItems(ctx, tx, cart.ID)
	if err != nil {
		return err
	}
	quote, err := quoteCart(ctx, tx, quoteRequest{Items: items, PromoCode: cart.PromoCode, DeliveryMethod: &method, IsCourierDelivery: &courier})
	if err != nil {
		return err
	}
	if err := validateOrderEdit(input); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE "order" SET delivery_method = $1, is_courier_delivery = $2,
		delivery_cost = $3, payment_method = COALESCE($4, payment_method), comment = COALESCE($5, comment),
		delivery_address = CASE WHEN $1 = 'standart' OR ($1 = 'post' AND $2) THEN COALESCE($6, delivery_address) ELSE NULL END,
		delivery_post_address = CASE WHEN $1 = 'post' THEN COALESCE($7, delivery_post_address) ELSE NULL END,
		delivery_date = COALESCE($8, delivery_date), updated_at = now(), cart_revision = cart_revision + 1 WHERE id = $9`,
		method, courier, quote.Order.DeliveryCost, input.PaymentMethod, input.Comment,
		input.DeliveryAddress, input.DeliveryPostAddress, input.DeliveryDate, cart.ID)
	return err
}

func updateAdminOrder(ctx context.Context, tx pgx.Tx, id uuid.UUID, input orderEditInput) error {
	if err := validateOrderEdit(input); err != nil {
		return err
	}
	if input.PersonID != nil || input.LegacyPersonID != nil || input.OrderNumber != nil ||
		input.PaymentStatus != nil || input.PaymentID != nil || input.PaymentFormURL != nil ||
		input.TotalPrice != nil || input.DiscountAmount != nil || input.PromoCode != nil {
		return quoteInputError("Реквизиты и суммы заказа управляются сервером")
	}
	if input.Status != nil && *input.Status == "NotChecked" {
		return quoteInputError("Оформленный заказ нельзя вернуть в корзину")
	}

	result, err := tx.Exec(ctx, `UPDATE "order" SET status = COALESCE($1, status),
		payment_method = COALESCE($2, payment_method), delivery_method = COALESCE($3, delivery_method),
		delivery_date = COALESCE($4, delivery_date), delivery_address = COALESCE($5, delivery_address),
		delivery_post_address = COALESCE($6, delivery_post_address), is_courier_delivery = COALESCE($7, is_courier_delivery),
		comment = COALESCE($8, comment), updated_at = now()
		WHERE id = $9 AND is_deleted IS NOT TRUE`,
		input.Status, input.PaymentMethod, input.DeliveryMethod, input.DeliveryDate, input.DeliveryAddress,
		input.DeliveryPostAddress, input.IsCourierDelivery, input.Comment, id)
	if err == nil && result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

func validateOrderEdit(input orderEditInput) error {
	if input.Status != nil && !slices.Contains([]string{"NotChecked", "Checked", "Processing", "InDelivery", "Delivered", "Completed", "Cancelled"}, *input.Status) {
		return quoteInputError("Недопустимый статус заказа")
	}
	if input.PaymentStatus != nil && !slices.Contains([]string{"Pending", "Processing", "Completed", "Failed"}, *input.PaymentStatus) {
		return quoteInputError("Недопустимый статус оплаты")
	}
	if input.PaymentMethod != nil && !slices.Contains([]string{"cash", "card", "online"}, *input.PaymentMethod) {
		return quoteInputError("Выберите способ оплаты")
	}
	if input.DeliveryMethod != nil {
		if _, err := quoteDeliveryCost(*input.DeliveryMethod, false, 0); err != nil {
			return err
		}
	}
	for _, value := range []*string{input.DeliveryAddress, input.DeliveryPostAddress, input.Comment} {
		if value != nil && !validCheckoutText(*value, 2000) {
			return quoteInputError("Проверьте данные доставки")
		}
	}
	return nil
}

func (auth *accountAuth) createAdminOrder(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input orderEditInput
		if !decodeCommerceJSON(w, r, &input) {
			return
		}
		if input.PersonID == nil {
			input.PersonID = input.LegacyPersonID
		}
		if input.PersonID == nil || input.Status == nil || input.TotalPrice == nil {
			writeCommerceOrderError(w, quoteInputError("Укажите покупателя, статус и сумму заказа"))
			return
		}
		if err := validateOrderEdit(input); err != nil {
			writeCommerceOrderError(w, err)
			return
		}
		total, err := checkoutCents(*input.TotalPrice)
		if err != nil {
			writeCommerceOrderError(w, err)
			return
		}
		paymentStatus := "Pending"
		if input.PaymentStatus != nil {
			paymentStatus = *input.PaymentStatus
		}
		ctx := r.Context()
		var entry order
		err = pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			var owner uuid.UUID
			if err := tx.QueryRow(ctx, "SELECT id FROM person WHERE id = $1 AND is_deleted = false FOR UPDATE", input.PersonID).Scan(&owner); err != nil {
				return err
			}
			if *input.Status == "NotChecked" {
				var existing bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM "order" WHERE person_id = $1
					AND status = 'NotChecked' AND is_deleted = false)`, owner).Scan(&existing); err != nil {
					return err
				}
				if existing {
					return cartRequestError{http.StatusConflict, "У покупателя уже есть корзина"}
				}
			}
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO "order" (person_id, status, payment_status, payment_id,
				payment_form_url, total_price, delivery_cost, discount_amount, delivery_method, delivery_date,
				delivery_address, delivery_post_address, is_courier_delivery, payment_method, comment, cart_revision)
				VALUES ($1,$2,$3,$4,$5,$6,COALESCE($7,0),$8,$9,$10,$11,$12,$13,$14,$15,1)
				RETURNING id`, owner, input.Status, paymentStatus, input.PaymentID, input.PaymentFormURL,
				float32(total)/100, input.DeliveryCost, input.DiscountAmount, input.DeliveryMethod, input.DeliveryDate,
				input.DeliveryAddress, input.DeliveryPostAddress, input.IsCourierDelivery, input.PaymentMethod, input.Comment).Scan(&id); err != nil {
				return err
			}
			record, err := readCommerceOrder(ctx, tx, id)
			entry = record
			return err
		})
		if err != nil {
			writeCommerceOrderError(w, err)
			return
		}
		writeJSON(w, singleResponse{Status: "success", Data: entry})
	}
}

func (auth *accountAuth) archiveAdminOrder(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		var entry order
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			result, err := tx.Exec(ctx, `UPDATE "order" SET is_deleted = true, updated_at = now() WHERE id = $1`, id)
			if err != nil {
				return err
			}
			if result.RowsAffected() == 0 {
				return pgx.ErrNoRows
			}
			record, err := readCommerceOrder(ctx, tx, id.Bytes)
			entry = record
			return err
		})
		if err != nil {
			writeCommerceOrderError(w, err)
			return
		}
		writeJSON(w, singleResponse{Status: "success", Data: entry})
	}
}

func readCommerceOrder(ctx context.Context, db database, id uuid.UUID) (order, error) {
	records, err := queryStructs[order](ctx, db, orderSelect+" WHERE o.id = $1", id)
	if err != nil {
		return order{}, err
	}
	if len(records) == 0 {
		return order{}, pgx.ErrNoRows
	}
	if err := loadOrderProducts(ctx, db, records); err != nil {
		return order{}, err
	}
	return records[0], nil
}

func writeCommerceOrderError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeCheckoutError(w, http.StatusNotFound, "Заказ или товар не найден")
		return
	}
	writeCartError(w, err)
}

func (auth *accountAuth) sendOrderStatusEmail(mailer accountMailer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		params := r.URL.Query()
		to, err := uuid.Parse(params.Get("to"))
		orderID := params.Get("orderId")
		status := params.Get("status")
		allowed := []string{"Подтвержден", "В обработке", "В доставке", "Доставлен", "Завершен", "Отменен"}
		if err != nil || orderID == "" || !validCheckoutText(orderID, 100) || !slices.Contains(allowed, status) {
			writeCommerceOrderError(w, quoteInputError("Проверьте получателя и статус заказа"))
			return
		}
		var email *string
		var number string
		err = auth.db.QueryRow(r.Context(), `SELECT CASE WHEN o.contact_snapshot IS NULL
			THEN COALESCE(p.contact_email, p.email) ELSE o.contact_snapshot->>'email' END, COALESCE(o.order_id, o.id::text)
			FROM "order" o JOIN person p ON p.id = o.person_id WHERE o.person_id = $1 AND o.is_deleted = false
			AND (o.id::text = $2 OR o.order_id = $2) ORDER BY (o.id::text = $2) DESC LIMIT 1`, to, orderID).Scan(&email, &number)
		if err != nil {
			writeCommerceOrderError(w, err)
			return
		}
		if email != nil && *email != "" {
			if mailer == nil {
				writeCheckoutError(w, http.StatusServiceUnavailable, "Отправка почты временно недоступна")
				return
			}
			subject := "Статус заказа обновлен"
			body := fmt.Sprintf("SkViDerm - Обновление статуса заказа\n\nЗдравствуйте!\n\nВаш заказ был обновлен.\n\nНомер заказа: #%s\nТекущий статус: %s\n\nС уважением,\nКоманда SkViDerm", number, status)
			if status == "Завершен" {
				subject = "Ваш заказ успешно завершен"
				body = fmt.Sprintf("SkViDerm - Заказ завершен\n\nЗдравствуйте!\n\nВаш заказ был успешно завершен!\n\nНомер заказа: #%s\n\nСпасибо за покупку! Будем рады видеть вас снова.\n\nС уважением,\nКоманда SkViDerm", number)
			}
			if err := mailer.send(r.Context(), *email, subject, body); err != nil {
				writeCheckoutError(w, http.StatusBadGateway, "Не удалось отправить письмо")
				return
			}
		}
		writeJSON(w, nil)
	}
}

func optionalBoolFlag(value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	flag, err := strconv.ParseBool(value)
	if err != nil {
		return false, quoteInputError("Некорректный параметр оплаты")
	}
	return flag, nil
}
