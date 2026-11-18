package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestOrderNotificationText(t *testing.T) {
	origin := "https://shop.example.invalid"
	id := uuid.New()
	first := "Снимок"
	contact := account{FirstName: "Профиль", SecondName: "Покупателя", Phone: "+375290000001",
		Email: new("account@example.invalid"), ContactEmail: new("contact@example.invalid")}
	entry := order{
		orderSummary:  orderSummary{ID: id, OrderID: new("TEST-123"), TotalPrice: 165.26},
		PaymentMethod: new("online"), DeliveryMethod: new("post"),
		IsCourierDelivery: new(true), DeliveryPostAddress: new("Тестовое отделение"),
		DeliveryAddress: new("Тестовый адрес"), DiscountAmount: new(float32(15)),
		Products: []orderProduct{{Product: product{FullTitle: "Тестовый крем"},
			Quantity: 2, HistoricalPrice: 90.13}},
		Comment: new("Тестовый комментарий"),
	}
	fallback := orderNotificationText(origin, entry, contact)
	if !strings.Contains(fallback, "Email: contact@example.invalid") {
		t.Fatal("legacy order did not fall back to contact email")
	}
	entry.ContactSnapshot = json.RawMessage(`{"firstName":"` + first + `","phone":"+375290000011","email":null}`)
	expected := "Получен новый заказ:\nНомер заказа: TEST-123\nЗаказ: " + origin + "/admin/order/" + id.String() + "\n" +
		"ФИО: Снимок Покупателя\nТелефон: +375290000011\nСпособ оплаты: Онлайн-оплата\n" +
		"Способ доставки: Доставка Европочтой\nКурьер до двери: Да\nОтделение Европочты: Тестовое отделение\n" +
		"Адрес доставки: Тестовый адрес\nТовары:\n---------------------------------\n" +
		"Название товара: Тестовый крем\nКоличество: 2\nЦена: 90.13 руб.\nОбщая стоимость: 180.26 руб.\n" +
		"---------------------------------\nИтого: 165.26 руб.\nСкидка: 15.00 руб.\nКомментарий: Тестовый комментарий\n"
	if actual := orderNotificationText(origin, entry, contact); actual != expected {
		t.Fatalf("notification mismatch\nwant:\n%s\ngot:\n%s", expected, actual)
	}
}

func TestOrderNotificationDelivery(t *testing.T) {
	pool := testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/guest_history_rows.sql", "testdata/checkout_confirmation_schema.sql")
	id := uuid.MustParse(guestPayOrder)
	offline, sending, closeReply := atomic.Bool{}, make(chan struct{}, 1), make(chan struct{}, 1)
	defer close(closeReply)
	offline.Store(true)
	var calls atomic.Int32
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sendMessage" || r.ParseForm() != nil ||
			r.Form.Get("chat_id") != "fixture-chat" || !strings.Contains(r.Form.Get("text"), "/admin/order/"+id.String()) ||
			!strings.Contains(r.Form.Get("text"), "Цена: 90.13 руб.") {
			t.Error("Telegram request does not match the order")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls.Add(1)
		if offline.Load() {
			_, _ = io.WriteString(w, `{"ok":false,"description":"fixture failure"}`)
			return
		}
		sending <- struct{}{}
		<-closeReply
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(mock.Close)

	notifications, err := newOrderNotifications(pool, "https://shop.example.invalid/", "fixture-bot", "fixture-chat")
	if err != nil {
		t.Fatal(err)
	}
	notifications.endpoint = mock.URL + "/sendMessage"
	notifications.client.Transport = mock.Client().Transport
	sent := func() bool {
		t.Helper()
		var marked bool
		err := pool.QueryRow(t.Context(), `SELECT notification_sent_at IS NOT NULL FROM "order" WHERE id = $1`, id).Scan(&marked)
		if err != nil {
			t.Fatal(err)
		}
		return marked
	}

	for _, reservation := range []struct {
		storedAt  string
		wantCalls int32
	}{
		{"now()", 0},
		{"now() - interval '1 minute'", 1},
	} {
		err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), `UPDATE "order" SET payment_started_at = `+reservation.storedAt+" WHERE id = $1", id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		err = notifications.deliver(t.Context(), id)
		if calls.Load() != reservation.wantCalls || sent() || (err == nil) != (reservation.wantCalls == 0) {
			t.Fatal("notification interfered with a pending bank registration or skipped an expired reservation")
		}
	}
	if calls.Load() != 1 || sent() {
		t.Fatal("failed Telegram notification was lost")
	}
	offline.Store(false)
	result := make(chan error, 1)
	go func() { result <- notifications.deliver(t.Context(), id) }()
	select {
	case <-sending:
	case err := <-result:
		t.Fatalf("did not send notification: %v", err)
	case <-time.After(4 * time.Second):
		t.Fatal("did not reach Telegram mock")
	}
	// The same claim used by Rust and a second Go worker must skip the in-flight send.
	lock := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		var other uuid.UUID
		return tx.QueryRow(t.Context(), `SELECT id FROM "order"
			WHERE id=$1 AND checkout_key IS NOT NULL AND notification_sent_at IS NULL FOR UPDATE SKIP LOCKED`, id).Scan(&other)
	})
	peer, err := newOrderNotifications(pool, notifications.clientURL, "fixture-bot", "fixture-chat")
	if err != nil {
		t.Fatal(err)
	}
	peer.endpoint, peer.client = notifications.endpoint, notifications.client
	otherOrder := uuid.MustParse(accountPayOrder)
	if lock != pgx.ErrNoRows || peer.deliver(t.Context(), id) != nil ||
		notifications.deliver(t.Context(), otherOrder) != nil || calls.Load() != 2 || sent() {
		t.Fatal("a second worker was able to deliver an in-flight order")
	}
	closeReply <- struct{}{}
	if err := <-result; err != nil || !sent() {
		t.Fatalf("successful notification was not recorded: %v", err)
	}
	if err := notifications.deliver(t.Context(), id); err != nil || calls.Load() != 2 {
		t.Fatal("a sent order was notified again")
	}
}

func TestOrderNotificationWorkerRecoversBacklog(t *testing.T) {
	pool := testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/guest_history_rows.sql", "testdata/checkout_confirmation_schema.sql")
	first := uuid.MustParse(guestPayOrder)
	second := uuid.MustParse(accountPayOrder)
	fresh := uuid.MustParse("00000000-0000-4000-8000-000000000925")
	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `UPDATE "order" SET checkout_key = NULL WHERE id <> ALL ($1)`, []uuid.UUID{first, second, fresh}); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `UPDATE "order" SET created_at = now() WHERE id = $1`, fresh)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(mock.Close)

	notifications, err := newOrderNotifications(pool, "https://shop.example.invalid", "fixture-bot", "fixture-chat")
	if err != nil {
		t.Fatal(err)
	}
	notifications.endpoint = mock.URL
	notifications.client.Transport = mock.Client().Transport
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		notifications.run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for range 250 {
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM "order"
			WHERE notification_sent_at IS NOT NULL`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			if calls.Load() != 2 {
				t.Fatal("worker sent an order too early or without a checkout key")
			}
			return
		}
		<-ticker.C
	}
	t.Fatal("worker did not recover previously confirmed orders")
}

func TestCheckoutSurvivesNotificationFailure(t *testing.T) {
	pool := testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/guest_history_rows.sql", "testdata/checkout_confirmation_schema.sql")
	var calls atomic.Int32
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(mock.Close)

	notifications, err := newOrderNotifications(pool, guestOrigin, "fixture-bot", "fixture-chat")
	if err != nil {
		t.Fatal(err)
	}
	notifications.endpoint = mock.URL
	notifications.client.Transport = mock.Client().Transport
	key := []byte("test-only")
	auth := &accountAuth{db: pool, cartDB: pool, key: key, guestOrigin: guestOrigin,
		confirmation: &checkoutConfirmation{guestEnabled: true, guestLimiter: &testGuestOrderLimiter{}, notifications: notifications}}
	handler := newHandler(pool, nil, auth, true)
	guest := guestHistoryCookie(t, key, firstGuestID, guestAudience, time.Now().Add(time.Hour).Unix())
	items := []cartItem{{VariantID: uuid.MustParse("00000000-0000-4000-8000-000000000601"), Quantity: 1}}
	quote, err := quoteCart(t.Context(), pool, quoteRequest{Items: items, IsCourierDelivery: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	req := checkoutRequest{
		CartID: uuid.New(), CheckoutKey: uuid.New(), ExpectedRevision: new(int64),
		Items: items, ExpectedTotalCents: &quote.TotalCents,
		Details: &checkoutDetails{Phone: new("+375290000011"), FirstName: "Тест", SecondName: "Покупатель",
			DeliveryMethod: "pickup", PaymentMethod: "online", OfferConsent: true},
	}
	var first, retry order
	readOrderHistory(t, guestCheckoutRequest(t, handler, req, guest, guestCSRF), &first)
	var sent bool
	err = pool.QueryRow(t.Context(), `SELECT notification_sent_at IS NOT NULL FROM "order" WHERE id = $1`, first.ID).Scan(&sent)
	if err != nil || sent || first.Status != "Checked" || calls.Load() != 1 {
		t.Fatalf("checkout did not preserve order and pending notification: %v", err)
	}

	readOrderHistory(t, guestCheckoutRequest(t, handler, req, guest, guestCSRF), &retry)
	err = pool.QueryRow(t.Context(), `SELECT notification_sent_at IS NOT NULL FROM "order" WHERE id = $1`, first.ID).Scan(&sent)
	if err != nil || !sent || retry.ID != first.ID || calls.Load() != 2 {
		t.Fatalf("replay did not deliver the original order: %v", err)
	}
	readOrderHistory(t, guestCheckoutRequest(t, handler, req, guest, guestCSRF), &retry)
	if retry.ID != first.ID || calls.Load() != 2 {
		t.Fatal("replay notified an already sent order")
	}
}
