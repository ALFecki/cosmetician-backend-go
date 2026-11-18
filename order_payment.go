package main

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	bankCurrency       = "933"
	bankTimeout        = 15 * time.Second
	paymentHTTPTimeout = 25 * time.Second
)

var (
	errBankPaymentUnavailable = cartRequestError{http.StatusServiceUnavailable, "Заказ сохранён. Не удалось получить ответ банка; магазин проверит статус оплаты. Не создавайте повторный заказ"}
	errPaymentUnconfirmed     = cartRequestError{http.StatusConflict, "Заказ сохранён; статус запроса к банку уточняется. Свяжитесь с магазином, не создавая повторный заказ"}
	errPaymentNotAllowed      = cartRequestError{http.StatusBadRequest, "Этот заказ нельзя оплатить онлайн"}
)

type orderPayments struct {
	db          *pgxpool.Pool
	client      *http.Client
	clientURL   string
	registerURL string
	statusURL   string
	username    string
	password    string
}

type paymentResponse struct {
	OrderID string `json:"orderId"`
	FormURL string `json:"formUrl"`
}

func newOrderPayments(db *pgxpool.Pool, clientURL, registerURL, statusURL, username, password string) (*orderPayments, error) {
	storefront, err := url.Parse(clientURL)
	if err != nil || storefront.Hostname() == "" ||
		(storefront.Scheme != "https" && storefront.Scheme != "http") ||
		storefront.User != nil || storefront.RawQuery != "" || storefront.Fragment != "" {
		return nil, errors.New("ORDER_PAYMENTS_ENABLED requires a valid CLIENT_URL")
	}
	if !securePaymentURL(registerURL) || !securePaymentURL(statusURL) {
		return nil, errors.New("ORDER_PAYMENTS_ENABLED requires HTTPS ACQUIRING_URL and ACQUIRING_ORDER_STATUS_URL")
	}
	if username == "" || password == "" {
		return nil, errors.New("ORDER_PAYMENTS_ENABLED requires ACQUIRING_USERNAME and ACQUIRING_PASSWORD")
	}
	return &orderPayments{
		db: db, clientURL: strings.TrimRight(clientURL, "/"), registerURL: registerURL,
		statusURL: statusURL, username: username, password: password,
		client: &http.Client{Timeout: bankTimeout, CheckRedirect: refuseRedirect},
	}, nil
}

func securePaymentURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil
}

func refuseRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func (auth *accountAuth) registerPaymentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/payments/process", auth.processPayment(false))
	mux.HandleFunc("POST /api/checkout/guest/payments/process", auth.processPayment(true))
}

func (auth *accountAuth) processPayment(guest bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var person uuid.UUID
		if guest {
			claims, ok := auth.guestSession(w, r, true)
			if !ok {
				return
			}
			person = claims.ID
		} else {
			claims, ok := auth.protectedSession(w, r)
			if !ok {
				return
			}
			person = claims.ID.Bytes
		}

		if !isJSONRequest(r) {
			writeText(w, http.StatusBadRequest, "Content type error")
			return
		}
		var body struct {
			ID *uuid.UUID `json:"id"`
		}
		err := jsonv2.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBodySize), &body)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeText(w, http.StatusRequestEntityTooLarge, "Payment request is too large")
			return
		}
		if err != nil || body.ID == nil {
			writeText(w, http.StatusBadRequest, "Invalid payment JSON")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		payment, err := auth.payments.create(r.Context(), *body.ID, person, guest)
		switch {
		case err == nil:
			writeJSON(w, payment)
		case errors.Is(err, pgx.ErrNoRows):
			w.WriteHeader(http.StatusNotFound)
		default:
			var problem cartRequestError
			if errors.As(err, &problem) {
				writeCheckoutError(w, problem.status, problem.message)
				return
			}
			var invalid quoteInputError
			if errors.As(err, &invalid) {
				writeCheckoutError(w, http.StatusBadRequest, invalid.Error())
				return
			}
			slog.Error("payment transaction failed", "order", body.ID)
			writeCheckoutError(w, http.StatusInternalServerError, "Не удалось сохранить оплату заказа")
		}
	}
}

func (payments *orderPayments) create(ctx context.Context, id, person uuid.UUID, guest bool) (paymentResponse, error) {
	if err := ctx.Err(); err != nil {
		return paymentResponse{}, err
	}
	// Keep the reservation and bank result if the browser disconnects mid-payment.
	paymentCtx := context.WithoutCancel(ctx)
	prepareCtx, cancel := context.WithTimeout(paymentCtx, 4*time.Second)
	cached, amount, err := payments.reserve(prepareCtx, id, person, guest)
	cancel()
	if err != nil || cached.OrderID != "" {
		return cached, err
	}

	finishCtx, finish := context.WithTimeout(paymentCtx, bankTimeout+4*time.Second)
	defer finish()
	result, err := payments.register(finishCtx, id, amount)
	if err != nil {
		slog.Warn("bank registration outcome unknown", "order", id)
		return paymentResponse{}, err
	}
	err = pgx.BeginTxFunc(finishCtx, payments.db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		updated, err := tx.Exec(finishCtx, `UPDATE "order" SET payment_id = $2, payment_form_url = $3, updated_at = now()
			WHERE id = $1 AND payment_id IS NULL`, id, result.OrderID, result.FormURL)
		if err == nil && updated.RowsAffected() != 1 {
			return errPaymentUnconfirmed
		}
		return err
	})
	if err != nil {
		slog.Error("bank registration result was not saved", "order", id)
		return paymentResponse{}, errPaymentUnconfirmed
	}
	return result, nil
}

func (payments *orderPayments) reserve(ctx context.Context, id, person uuid.UUID, guest bool) (paymentResponse, int64, error) {
	var result paymentResponse
	var amount int64
	err := pgx.BeginTxFunc(ctx, payments.db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		if guest {
			if _, err := lockCheckoutPerson(ctx, tx, person, true); err != nil {
				return err
			}
		}
		var status, paymentStatus string
		var method, paymentID, formURL *string
		var started bool
		var total float32
		err := tx.QueryRow(ctx, `SELECT status::text, payment_status::text, payment_method, payment_id,
			payment_form_url, payment_started_at IS NOT NULL, total_price FROM "order"
			WHERE id = $1 AND person_id = $2 AND is_deleted = false FOR UPDATE`, id, person).
			Scan(&status, &paymentStatus, &method, &paymentID, &formURL, &started, &total)
		if errors.Is(err, pgx.ErrNoRows) && !guest {
			var otherOwner bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM "order" WHERE id = $1 AND person_id <> $2 AND is_deleted = false)`, id, person).Scan(&otherOwner); err != nil {
				return err
			}
			if otherOwner {
				return cartRequestError{http.StatusForbidden, ""}
			}
		}
		if err != nil {
			return err
		}
		if status == "NotChecked" || status == "Cancelled" {
			if !guest {
				return cartRequestError{http.StatusBadRequest, "Order cannot be paid"}
			}
			return errPaymentNotAllowed
		}
		if paymentStatus == "Completed" || method == nil || *method != "online" {
			return errPaymentNotAllowed
		}
		if paymentStatus == "Failed" {
			return cartRequestError{http.StatusConflict, "Предыдущая оплата не завершилась. Свяжитесь с магазином для проверки платежа"}
		}
		if paymentID != nil && *paymentID != "" && formURL != nil && securePaymentURL(*formURL) {
			result = paymentResponse{OrderID: *paymentID, FormURL: *formURL}
			return nil
		}
		if started || paymentID != nil {
			return errPaymentUnconfirmed
		}
		amount, err = checkoutCents(total)
		if err != nil {
			return err
		}
		if amount == 0 {
			return cartRequestError{http.StatusBadRequest, "Сумма оплаты должна быть больше нуля"}
		}
		_, err = tx.Exec(ctx, `UPDATE "order" SET payment_started_at = now(), payment_status = 'Processing',
			updated_at = now() WHERE id = $1`, id)
		return err
	})
	return result, amount, err
}

func (payments *orderPayments) register(ctx context.Context, id uuid.UUID, amount int64) (paymentResponse, error) {
	params := url.Values{
		"amount":      {strconv.FormatInt(amount, 10)},
		"currency":    {bankCurrency},
		"description": {"Платеж за заказ №" + id.String()},
		"orderNumber": {id.String()},
		"returnUrl":   {payments.clientURL + "/order/" + id.String()},
	}
	var result paymentResponse
	if err := payments.bankPost(ctx, payments.registerURL, params, &result); err != nil ||
		strings.TrimSpace(result.OrderID) == "" || !securePaymentURL(result.FormURL) {
		return paymentResponse{}, errBankPaymentUnavailable
	}
	return result, nil
}

func (payments *orderPayments) bankPost(ctx context.Context, endpoint string, form url.Values, reply any) error {
	form.Set("userName", payments.username)
	form.Set("password", payments.password)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errBankPaymentUnavailable
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := payments.client.Do(request)
	if err != nil {
		return errBankPaymentUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errBankPaymentUnavailable
	}
	const maxBankResponse = 256 * 1024
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBankResponse+1))
	if err != nil || len(body) > maxBankResponse || jsonv2.Unmarshal(body, reply) != nil {
		return errBankPaymentUnavailable
	}
	return nil
}
