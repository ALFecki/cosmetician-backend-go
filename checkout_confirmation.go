package main

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type guestCheckoutLimiter interface {
	allow(context.Context, uuid.UUID, *http.Request) error
}

type checkoutConfirmation struct {
	guestEnabled  bool
	guestLimiter  guestCheckoutLimiter
	notifications *orderNotifications
}

var errCheckoutKeyConflict = cartRequestError{http.StatusConflict, "Заказ с этим ключом уже оформлен с другими данными"}

func (auth *accountAuth) registerCheckoutConfirmation(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/checkout", auth.confirmCheckout(false))
	if auth.guestOrigin != "" {
		mux.HandleFunc("POST /api/checkout/guest", auth.confirmCheckout(true))
	}
}

func (auth *accountAuth) confirmCheckout(guest bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var person uuid.UUID
		if guest {
			if !auth.confirmation.guestEnabled {
				writeCheckoutError(w, http.StatusServiceUnavailable, "Для нового заказа подтвердите номер телефона")
				return
			}
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
		var request checkoutRequest
		body := http.MaxBytesReader(w, r.Body, maxJSONBodySize)
		err := jsonv2.UnmarshalRead(body, &request)
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeText(w, http.StatusRequestEntityTooLarge, "Checkout request is too large")
			return
		case err != nil || !request.valid():
			writeText(w, http.StatusBadRequest, "Invalid checkout JSON")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		id, err := auth.placeOrder(ctx, r, person, guest, request)
		if err != nil {
			writeCartError(w, err)
			return
		}

		if notifications := auth.confirmation.notifications; notifications != nil {
			cancel()
			notifications.deliverLogged(r.Context(), id)
			replyCtx, finish := context.WithTimeout(r.Context(), 5*time.Second)
			defer finish()
			ctx = replyCtx
		}
		auth.readCheckoutOrder(w, r.WithContext(ctx), " WHERE o.person_id = $1 AND o.id = $2", person, id, false)
	}
}

func (auth *accountAuth) placeOrder(ctx context.Context, httpRequest *http.Request, person uuid.UUID, guest bool, request checkoutRequest) (uuid.UUID, error) {
	tx, err := auth.cartDB.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return uuid.Nil, err
	}
	defer rollbackCart(ctx, tx)

	verifiedPhone, err := lockCheckoutPerson(ctx, tx, person, guest)
	if err != nil {
		return uuid.Nil, err
	}
	previous, err := findCheckoutReceipt(ctx, tx, request.CheckoutKey)
	if err != nil {
		return uuid.Nil, err
	}
	if guest && previous.PersonID != person {
		if err := auth.confirmation.guestLimiter.allow(ctx, person, httpRequest); err != nil {
			return uuid.Nil, err
		}
	}
	address, fingerprint, err := normalizeCheckout(&request)
	if err != nil {
		return uuid.Nil, err
	}
	phone, verified, err := checkoutPhone(verifiedPhone, request.Details.Phone, auth.confirmation.guestEnabled)
	if err != nil {
		return uuid.Nil, err
	}
	if previous.ID != uuid.Nil {
		if previous.PersonID != person || previous.Hash == nil || *previous.Hash != fingerprint {
			return uuid.Nil, errCheckoutKeyConflict
		}
		return previous.ID, tx.Commit(ctx)
	}

	cart, err := checkoutDraft(ctx, tx, person, guest, request)
	if err != nil {
		return uuid.Nil, err
	}
	result, err := checkoutOrderQuote(ctx, tx, request)
	if err != nil {
		return uuid.Nil, err
	}
	if err := persistCheckout(ctx, tx, cart, result, request, person, phone, verified, address, fingerprint); err != nil {
		var dbError *pgconn.PgError
		if errors.As(err, &dbError) && dbError.Code == "23505" && dbError.ConstraintName == "order_checkout_key_key" {
			return uuid.Nil, errCheckoutKeyConflict
		}
		return uuid.Nil, err
	}
	return cart.ID, tx.Commit(ctx)
}

func lockCheckoutPerson(ctx context.Context, tx pgx.Tx, person uuid.UUID, guest bool) (*string, error) {
	where := " WHERE id = $1 AND is_deleted = false"
	if guest {
		where = guestPersonWhere
	}
	var phone *string
	err := tx.QueryRow(ctx, "SELECT verified_phone FROM person"+where+" FOR UPDATE", person).Scan(&phone)
	if errors.Is(err, pgx.ErrNoRows) {
		message := "Войдите в аккаунт"
		if guest {
			message = guestSessionExpired
		}
		return nil, cartRequestError{http.StatusUnauthorized, message}
	}
	return phone, err
}

func checkoutPhone(verified, requested *string, allowUnverified bool) (string, bool, error) {
	if verified != nil {
		if requested != nil && *requested != *verified {
			return "", false, quoteInputError("Подтвердите новый номер перед его изменением")
		}
		return *verified, true, nil
	}
	if allowUnverified && requested != nil {
		return *requested, false, nil
	}
	return "", false, cartRequestError{http.StatusUnauthorized, "Подтвердите номер телефона перед оформлением"}
}

type checkoutReceipt struct {
	ID       uuid.UUID
	PersonID uuid.UUID
	Hash     *string
}

func findCheckoutReceipt(ctx context.Context, tx pgx.Tx, key uuid.UUID) (checkoutReceipt, error) {
	var receipt checkoutReceipt
	err := tx.QueryRow(ctx, `SELECT id, person_id, checkout_request_hash FROM "order" WHERE checkout_key = $1`,
		key).Scan(&receipt.ID, &receipt.PersonID, &receipt.Hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return checkoutReceipt{}, nil
	}
	return receipt, err
}

func checkoutDraft(ctx context.Context, tx pgx.Tx, person uuid.UUID, guest bool, request checkoutRequest) (savedCart, error) {
	if guest {
		cart := savedCart{Revision: 1}
		err := tx.QueryRow(ctx, `
			INSERT INTO "order" (person_id, status, total_price, cart_revision)
			VALUES ($1, 'NotChecked', 0, 1) RETURNING id`, person).Scan(&cart.ID)
		return cart, err
	}

	conflict := cartRequestError{http.StatusConflict, "Корзина изменилась. Проверьте её перед оформлением"}
	cart, err := lockDraftCart(ctx, tx, person, false)
	if errors.Is(err, errCartChanged) {
		return savedCart{}, conflict
	}
	if err != nil {
		return savedCart{}, err
	}
	if cart.ID != request.CartID || cart.Revision != *request.ExpectedRevision || !equalPromoCodes(cart.PromoCode, request.PromoCode) {
		return savedCart{}, conflict
	}
	items, err := savedCartItems(ctx, tx, cart.ID)
	if err != nil {
		return savedCart{}, err
	}
	items, err = normalizeCartItems(items)
	if err != nil {
		return savedCart{}, err
	}
	if !slices.Equal(items, request.Items) {
		return savedCart{}, conflict
	}
	return cart, nil
}

func equalPromoCodes(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func checkoutOrderQuote(ctx context.Context, tx pgx.Tx, request checkoutRequest) (cartQuote, error) {
	quote, err := priceCheckoutItems(ctx, tx, request.Items, request.PromoCode,
		request.Details.DeliveryMethod, request.Details.IsCourierDelivery)
	if err != nil {
		return cartQuote{}, err
	}
	if quote.TotalCents != *request.ExpectedTotalCents {
		return cartQuote{}, cartRequestError{http.StatusConflict, "Стоимость заказа изменилась. Проверьте обновлённый итог перед подтверждением"}
	}
	return quote, nil
}

func priceCheckoutItems(ctx context.Context, tx pgx.Tx, items []cartItem, promoCode *string, method string, courier bool) (cartQuote, error) {
	var variants []uuid.UUID
	for _, item := range items {
		variants = append(variants, item.VariantID)
	}
	if _, err := tx.Exec(ctx, `SELECT v.id FROM product_variant v JOIN product p ON p.id = v.product_id
		WHERE v.id = ANY($1) ORDER BY v.id FOR SHARE OF v, p`, variants); err != nil {
		return cartQuote{}, err
	}
	if promoCode != nil {
		if _, err := tx.Exec(ctx, "SELECT id FROM promo_code WHERE code = $1 FOR UPDATE", *promoCode); err != nil {
			return cartQuote{}, err
		}
	}
	quote, err := quoteCart(ctx, tx, quoteRequest{
		Items: items, PromoCode: promoCode, DeliveryMethod: &method, IsCourierDelivery: &courier,
	})
	if err != nil {
		return cartQuote{}, err
	}
	if len(quote.UnavailableItems) != 0 {
		return cartQuote{}, quoteInputError("Удалите недоступные товары из корзины")
	}
	if quote.PromoCodeError != nil {
		return cartQuote{}, quoteInputError(*quote.PromoCodeError)
	}
	return quote, nil
}

func persistCheckout(ctx context.Context, tx pgx.Tx, cart savedCart, quote cartQuote, request checkoutRequest,
	person uuid.UUID, phone string, verified bool, address *string, fingerprint string) error {
	details, err := json.Marshal(request.Details)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(struct {
		checkoutDetails
		Phone         string `json:"phone"`
		PhoneVerified bool   `json:"phoneVerified"`
	}{*request.Details, phone, verified})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE person SET first_name = $1, second_name = $2, contact_email = $3,
			checkout_details = $4, phone = $5, updated_at = now() WHERE id = $6`,
		request.Details.FirstName, request.Details.SecondName, request.Details.Email, details, phone, person); err != nil {
		return err
	}
	if err := saveConfirmedItems(ctx, tx, cart.ID, quote.Order.Products); err != nil {
		return err
	}

	paymentStatus := "Pending"
	if request.Details.PaymentMethod == "online" {
		paymentStatus = "Processing"
	}
	orderNumber := time.Now().UTC().Format("20060102") + "-" + strings.ToUpper(cart.ID.String()[:8])
	if _, err := tx.Exec(ctx, `
		UPDATE "order" SET status = 'Checked', order_id = $1, payment_status = $2, total_price = $3,
			delivery_cost = $4, discount_amount = $5, promo_code_id = $6, delivery_method = $7,
			delivery_address = $8, delivery_post_address = $9, is_courier_delivery = $10,
			payment_method = $11, comment = $12, contact_snapshot = $13, checkout_key = $14,
			checkout_request_hash = $15, cart_revision = $16, created_at = now(), updated_at = now()
		WHERE id = $17`, orderNumber, paymentStatus, quote.Order.TotalPrice, quote.Order.DeliveryCost,
		quote.Order.DiscountAmount, quote.Order.PromoCodeID, request.Details.DeliveryMethod, address,
		request.Details.DeliveryPostAddress, request.Details.IsCourierDelivery, request.Details.PaymentMethod,
		request.Details.Comment, snapshot, request.CheckoutKey, fingerprint, cart.Revision+1, cart.ID); err != nil {
		return err
	}
	return consumeCheckoutPromo(ctx, tx, cart.ID, person, quote)
}

func consumeCheckoutPromo(ctx context.Context, tx pgx.Tx, orderID, person uuid.UUID, quote cartQuote) error {
	if quote.Order.PromoCodeID == nil {
		return nil
	}

	changed, err := tx.Exec(ctx, `UPDATE promo_code SET used_count = used_count + 1
		WHERE id = $1 AND (usage_limit IS NULL OR used_count < usage_limit)`, quote.Order.PromoCodeID)
	if err != nil {
		return err
	}
	if changed.RowsAffected() != 1 {
		return cartRequestError{http.StatusConflict, "Промокод исчерпан. Проверьте итог заказа"}
	}
	_, err = tx.Exec(ctx, `INSERT INTO promo_code_usage (promo_code_id, order_id, person_id, discount_amount)
		VALUES ($1, $2, $3, $4)`, quote.Order.PromoCodeID, orderID, person, float32(quote.PromoCodeDiscountCents)/100)
	return err
}

func saveConfirmedItems(ctx context.Context, tx pgx.Tx, id uuid.UUID, products []orderProduct) error {
	if _, err := tx.Exec(ctx, "DELETE FROM order_product WHERE order_id = $1", id); err != nil {
		return err
	}
	for _, line := range products {
		_, err := tx.Exec(ctx, `INSERT INTO order_product (order_id, variant_id, quantity, unit_price, unit_cost_price)
			VALUES ($1, $2, $3, $4, $5)`, id, line.Variant.ID, line.Quantity, line.Variant.Price, line.Variant.CostPrice)
		if err != nil {
			return err
		}
	}
	return nil
}
