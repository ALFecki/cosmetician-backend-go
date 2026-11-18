package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	accountPaymentPath = "/api/payments/process"
	guestPaymentPath   = "/api/checkout/guest/payments/process"
	accountPayOrder    = "00000000-0000-4000-8000-000000000926"
	guestPayOrder      = "00000000-0000-4000-8000-000000000921"
)

func TestPaymentRegistration(t *testing.T) {
	pool := paymentFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{}, 1)
	var calls atomic.Int32
	var cancelled atomic.Bool
	bank := fakeAcquirer(t, pool, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/register" || r.ParseForm() != nil ||
			r.Form.Get("userName") != "fixture-merchant" || r.Form.Get("password") != "fixture-password" ||
			r.Form.Get("currency") != "933" {
			t.Error("invalid bank registration request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls.Add(1)
		id := r.Form.Get("orderNumber")
		if r.Form.Get("returnUrl") != guestOrigin+"/order/"+id || r.Form.Get("description") != "Платеж за заказ №"+id {
			t.Error("bank return URL or description changed")
		}
		switch id {
		case accountPayOrder:
			if r.Form.Get("amount") != "3000" {
				t.Error("account total was not sent in kopeks")
			}
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				cancelled.Store(true)
				return
			}
			_, _ = io.WriteString(w, `{"orderId":"account-bank-id","formUrl":"https://bank.example.invalid/pay"}`)
		case guestPayOrder:
			if r.Form.Get("amount") != "9013" {
				t.Error("guest total was not sent in kopeks")
			}
			w.WriteHeader(http.StatusBadGateway)
		default:
			t.Error("unexpected order sent to bank")
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	key := []byte("test-only")
	handler := newHandler(pool, nil, &accountAuth{db: pool, key: key, guestOrigin: guestOrigin, payments: bank}, true)
	account := profileCookie(t, key, testAccountID, false, false, false)
	admin := profileCookie(t, key, testAdminID, true, true, false)
	guest := guestHistoryCookie(t, key, firstGuestID, guestAudience, time.Now().Add(time.Hour).Unix())
	assertCartStatus(t, paymentRequest(t.Context(), handler, accountPaymentPath, accountPayOrder, nil, ""), http.StatusUnauthorized)
	assertCartStatus(t, paymentRequest(t.Context(), handler, accountPaymentPath, accountPayOrder, admin, "fixture-csrf"), http.StatusForbidden)
	assertCartStatus(t, paymentRequest(t.Context(), handler, accountPaymentPath, accountPayOrder, account, "wrong"), http.StatusUnauthorized)
	assertCartStatus(t, paymentRequest(t.Context(), handler, guestPaymentPath, guestPayOrder, guest, "wrong"), http.StatusUnauthorized)
	assertCartStatus(t, paymentRequest(t.Context(), handler, guestPaymentPath, accountPayOrder, guest, guestCSRF), http.StatusNotFound)
	assertCartStatus(t, paymentRequest(t.Context(), handler, guestPaymentPath, "00000000-0000-4000-8000-000000000924", guest, guestCSRF), http.StatusBadRequest)
	assertCartStatus(t, paymentRequest(t.Context(), handler, guestPaymentPath, "00000000-0000-4000-8000-000000000922", guest, guestCSRF), http.StatusBadRequest)
	assertCartStatus(t, paymentRequest(t.Context(), handler, guestPaymentPath, "00000000-0000-4000-8000-000000000923", guest, guestCSRF), http.StatusNotFound)
	if calls.Load() != 0 {
		t.Fatal("a rejected payment reached the bank")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	original := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		original <- paymentRequest(ctx, handler, accountPaymentPath, accountPayOrder, account, "fixture-csrf")
	}()
	defer close(release)
	select {
	case <-entered:
	case response := <-original:
		t.Fatalf("did not reach the bank: %d %s", response.Code, response.Body)
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for bank registration")
	}
	response := paymentRequest(t.Context(), handler, accountPaymentPath, accountPayOrder, account, "fixture-csrf")
	assertCartStatus(t, response, http.StatusConflict)
	cancel()
	// The customer's connection may close while the bank is still registering the payment.
	release <- struct{}{}
	first := <-original
	assertCartStatus(t, first, http.StatusOK)
	replayed := paymentRequest(t.Context(), handler, accountPaymentPath, accountPayOrder, account, "fixture-csrf")
	assertCartStatus(t, replayed, http.StatusOK)
	var result paymentResponse
	if err := json.Unmarshal(replayed.Body.Bytes(), &result); err != nil ||
		result.OrderID != "account-bank-id" || result.FormURL != "https://bank.example.invalid/pay" ||
		first.Body.String() != replayed.Body.String() || replayed.Header().Get("Cache-Control") != "no-store" ||
		calls.Load() != 1 || cancelled.Load() {
		t.Fatal("bank registration did not survive cancellation or replay")
	}

	assertCartStatus(t, paymentRequest(t.Context(), handler, guestPaymentPath, guestPayOrder, guest, guestCSRF), http.StatusServiceUnavailable)
	assertCartStatus(t, paymentRequest(t.Context(), handler, guestPaymentPath, guestPayOrder, guest, guestCSRF), http.StatusConflict)
	var started bool
	var bankID *string
	err := pool.QueryRow(t.Context(), `SELECT payment_started_at IS NOT NULL, payment_id FROM "order" WHERE id = $1`, guestPayOrder).Scan(&started, &bankID)
	if calls.Load() != 2 || err != nil || !started || bankID != nil {
		t.Fatal("an uncertain bank attempt was retried or marked successful")
	}
}

func TestPaymentStatusSync(t *testing.T) {
	pool := paymentFixture(t)
	for _, tc := range []struct {
		order, reference, method, state, amount, currency, code, want string
	}{
		{accountPayOrder, "success", "online", "2", "3000", "933", "0", "Completed"},
		{guestPayOrder, "wrong total", "online", "2", "9014", "933", "0", "Processing"},
		{"00000000-0000-4000-8000-000000000922", "wrong currency", "online", "2", "9925", "978", "0", "Processing"},
		{"00000000-0000-4000-8000-000000000924", "reversed", "online", "3", "1300", "933", "0", "Failed"},
		{"00000000-0000-4000-8000-000000000925", "declined", "online", "6", "8000", "933", "0", "Failed"},
		{"00000000-0000-4000-8000-000000000923", "bank error", "online", "2", "1000", "933", "5", "Processing"},
		{guestPayOrder, "missing status", "online", "null", "9013", "933", "0", "Processing"},
		{guestPayOrder, "awaiting buyer", "online", "0", "9013", "933", "0", "Processing"},
	} {
		t.Run(tc.reference, func(t *testing.T) {
			err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
				_, err := tx.Exec(t.Context(), `UPDATE "order" SET is_deleted = false, status = 'Checked',
					payment_method = $2, payment_status = 'Processing', payment_id = $3 WHERE id = $1`, tc.order, tc.method, tc.reference)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			bank := fakeAcquirer(t, pool, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/status" || r.ParseForm() != nil ||
					r.Form.Get("userName") != "fixture-merchant" || r.Form.Get("password") != "fixture-password" {
					t.Error("invalid bank status request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if r.Form.Get("orderId") == tc.reference {
					_, _ = fmt.Fprintf(w, `{"orderStatus":%s,"amount":%s,"currency":%q,"errorCode":%q,"errorMessage":""}`, tc.state, tc.amount, tc.currency, tc.code)
					return
				}
				_, _ = io.WriteString(w, `{"orderStatus":0,"amount":3000,"currency":"933","errorCode":"0","errorMessage":""}`)
			})
			if _, err := bank.syncBatch(t.Context(), uuid.Nil); err != nil {
				t.Fatal(err)
			}
			var status string
			err = pool.QueryRow(t.Context(), `SELECT payment_status FROM "order" WHERE id = $1`, tc.order).Scan(&status)
			if err != nil || status != tc.want {
				t.Fatalf("status = %q, want %q: %v", status, tc.want, err)
			}
		})
	}
}

func TestPaymentBankRedirectAndInvalidResponse(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
	}{
		{"redirect", ""},
		{"missing id", `{"formUrl":"https://bank.example.invalid/pay"}`},
		{"unsafe form", `{"orderId":"bank-id","formUrl":"http://bank.example.invalid/pay"}`},
		{"malformed json", `{"orderId":"bank-id","formUrl":"https://bank.example.invalid/pay"}{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bank := fakeAcquirer(t, nil, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					t.Error("followed a redirect with bank credentials")
				}
				if tc.reply == "" {
					http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
				} else {
					_, _ = io.WriteString(w, tc.reply)
				}
			})
			if _, err := bank.register(t.Context(), uuid.MustParse(accountPayOrder), 3000); err == nil {
				t.Fatal("accepted an invalid bank response")
			}
		})
	}
}

func paymentFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/guest_history_rows.sql", "testdata/order_payment_rows.sql")
}

func fakeAcquirer(t *testing.T, pool *pgxpool.Pool, handle http.HandlerFunc) *orderPayments {
	t.Helper()
	server := httptest.NewTLSServer(handle)
	t.Cleanup(server.Close)
	bank, err := newOrderPayments(pool, guestOrigin, server.URL+"/register", server.URL+"/status", "fixture-merchant", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	bank.client.Transport = server.Client().Transport
	return bank
}

func paymentRequest(ctx context.Context, handler http.Handler, path, id string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewBufferString(`{"id":"`+id+`"}`))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if strings.HasPrefix(path, guestReadPath) {
		req.Header.Set("Origin", guestOrigin)
		req.Header.Set("X-Guest-CSRF", csrf)
	} else {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}
