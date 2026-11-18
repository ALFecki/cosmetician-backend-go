package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type legacyCartAction int

const (
	legacyCartRead legacyCartAction = iota
	legacyCartAdd
	legacyCartRemove
	legacyCartSetQuantity
	legacyCartBatch
	legacyCartPromo
)

type legacyCartInput struct {
	VariantID  uuid.UUID
	Quantities map[uuid.UUID]int32
	PromoCode  *string
}

func (auth *accountAuth) registerLegacyCart(mux *http.ServeMux, db cartDatabase) {
	mux.HandleFunc("GET /api/order/person/last_non_checked", auth.serveLegacyCart(db, legacyCartRead, true))
	mux.HandleFunc("POST /api/order/attach_product/{id}", auth.serveLegacyCart(db, legacyCartAdd, false))
	mux.HandleFunc("PATCH /api/order/product/{id}/quantity/{quantity}", auth.serveLegacyCart(db, legacyCartSetQuantity, false))
	mux.HandleFunc("PUT /api/order/batch-quantity", auth.serveLegacyCart(db, legacyCartBatch, true))
	mux.HandleFunc("DELETE /api/order/product/{id}", auth.serveLegacyCart(db, legacyCartRemove, false))
	mux.HandleFunc("POST /api/order/apply-promo-code", auth.serveLegacyCart(db, legacyCartPromo, true))
	mux.HandleFunc("GET /api/order/products/sync", auth.requireCommerceAdmin(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, singleResponse{Status: "success"})
	}))
	mux.HandleFunc("GET /api/order/person/checkout", auth.serveLegacyCheckout(db))
}

func (auth *accountAuth) serveLegacyCart(db cartDatabase, action legacyCartAction, wrapped bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		input, ok := readLegacyCartInput(w, r, action)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var entry order
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			result, err := changeLegacyCart(ctx, tx, claims.ID.Bytes, action, input)
			entry = result
			return err
		})
		if err != nil {
			writeCommerceOrderError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if wrapped {
			writeJSON(w, singleResponse{Status: "success", Data: entry})
			return
		}
		writeJSON(w, entry)
	}
}

func readLegacyCartInput(w http.ResponseWriter, r *http.Request, action legacyCartAction) (legacyCartInput, bool) {
	var input legacyCartInput
	switch action {
	case legacyCartAdd, legacyCartRemove, legacyCartSetQuantity:
		id, ok := requestID(w, r)
		if !ok {
			return input, false
		}
		input.VariantID = id.Bytes
		if action == legacyCartSetQuantity {
			amount, err := strconv.ParseInt(r.PathValue("quantity"), 10, 32)
			if err != nil || amount < 1 || amount > 99 {
				writeCommerceOrderError(w, quoteInputError("Количество товара должно быть от 1 до 99"))
				return input, false
			}
			input.Quantities = map[uuid.UUID]int32{input.VariantID: int32(amount)}
		}
	case legacyCartBatch:
		var body struct {
			Variants map[uuid.UUID]int32 `json:"variants"`
		}
		if !decodeCommerceJSON(w, r, &body) {
			return input, false
		}
		if body.Variants == nil || len(body.Variants) > 100 {
			writeCommerceOrderError(w, quoteInputError("Некорректный список товаров"))
			return input, false
		}
		input.Quantities = body.Variants
	case legacyCartPromo:
		var body struct {
			PromoCode string `json:"promoCode"`
		}
		if !decodeCommerceJSON(w, r, &body) {
			return input, false
		}
		body.PromoCode = strings.TrimSpace(body.PromoCode)
		if body.PromoCode == "" {
			writeCommerceOrderError(w, quoteInputError("Укажите промокод"))
			return input, false
		}
		input.PromoCode = &body.PromoCode
	}
	return input, true
}

func changeLegacyCart(ctx context.Context, tx pgx.Tx, person uuid.UUID, action legacyCartAction, input legacyCartInput) (order, error) {
	cart, err := lockCart(ctx, tx, person, action == legacyCartRead || action == legacyCartAdd)
	if err != nil {
		return order{}, err
	}
	items, err := savedCartItems(ctx, tx, cart.ID)
	if err != nil {
		return order{}, err
	}
	switch action {
	case legacyCartAdd:
		items, err = incrementCartItem(items, input.VariantID)
		if err != nil {
			return order{}, err
		}
	case legacyCartRemove:
		index := cartItemIndex(items, input.VariantID)
		if index < 0 {
			return order{}, pgx.ErrNoRows
		}
		items = append(items[:index], items[index+1:]...)
	case legacyCartSetQuantity, legacyCartBatch:
		for id, quantity := range input.Quantities {
			if quantity < 1 || quantity > 99 {
				return order{}, quoteInputError("Количество товара должно быть от 1 до 99")
			}
			index := cartItemIndex(items, id)
			if index < 0 {
				return order{}, pgx.ErrNoRows
			}
			items[index].Quantity = quantity
		}
	case legacyCartPromo:
		cart.PromoCode = input.PromoCode
	}

	courier := false
	quote, err := quoteCart(ctx, tx, quoteRequest{Items: items, PromoCode: cart.PromoCode, IsCourierDelivery: &courier})
	if err != nil {
		return order{}, err
	}
	if action == legacyCartPromo && quote.PromoCodeError != nil {
		return order{}, quoteInputError(*quote.PromoCodeError)
	}
	if action == legacyCartAdd {
		for _, unavailable := range quote.UnavailableItems {
			if unavailable.VariantID == input.VariantID {
				return order{}, quoteInputError("Этот товар сейчас недоступен")
			}
		}
	}
	quote.Order.ID, quote.Order.PersonID, quote.Order.CartRevision = cart.ID, person, cart.Revision
	if action != legacyCartRead {
		quote.Order.CartRevision++
		if err := saveCartChange(ctx, tx, replaceCart, cartRequest{}, quote); err != nil {
			return order{}, err
		}
	}

	rows, err := queryStructs[order](ctx, tx, orderSelect+" WHERE o.id = $1", cart.ID)
	if err != nil {
		return order{}, err
	}
	if len(rows) != 1 {
		return order{}, pgx.ErrNoRows
	}
	entry := rows[0]
	entry.Products = quote.Order.Products
	entry.TotalPrice = quote.Order.TotalPrice
	entry.DiscountAmount = quote.Order.DiscountAmount
	entry.PromoCodeID = quote.Order.PromoCodeID
	return entry, nil
}

func cartItemIndex(items []cartItem, variant uuid.UUID) int {
	for i, item := range items {
		if item.VariantID == variant {
			return i
		}
	}
	return -1
}

func (auth *accountAuth) serveLegacyCheckout(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		paid, err := optionalBoolFlag(r.URL.Query().Get("withPayment"))
		if err != nil {
			writeCommerceOrderError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var entry order
		err = pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			record, err := auth.confirmLegacyCart(ctx, tx, claims.ID.Bytes, paid)
			entry = record
			return err
		})
		if err != nil {
			writeCommerceOrderError(w, err)
			return
		}
		if auth.confirmation != nil && auth.confirmation.notifications != nil {
			auth.confirmation.notifications.deliverLogged(r.Context(), entry.ID)
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, entry)
	}
}

func (auth *accountAuth) confirmLegacyCart(ctx context.Context, tx pgx.Tx, person uuid.UUID, paid bool) (order, error) {
	verified, err := lockCheckoutPerson(ctx, tx, person, false)
	if err != nil {
		return order{}, err
	}
	if auth.confirmation != nil && !auth.confirmation.guestEnabled && verified == nil {
		return order{}, cartRequestError{http.StatusUnauthorized, "Подтвердите номер телефона перед оформлением"}
	}
	cart, err := lockDraftCart(ctx, tx, person, false)
	if err != nil {
		return order{}, err
	}
	items, err := savedCartItems(ctx, tx, cart.ID)
	if err != nil {
		return order{}, err
	}
	if len(items) == 0 {
		return order{}, quoteInputError("Добавьте товары в корзину")
	}
	method := "pickup"
	courier := false
	if err := tx.QueryRow(ctx, `SELECT COALESCE(delivery_method, 'pickup'), COALESCE(is_courier_delivery, false)
		FROM "order" WHERE id = $1`, cart.ID).Scan(&method, &courier); err != nil {
		return order{}, err
	}
	quote, err := priceCheckoutItems(ctx, tx, items, cart.PromoCode, method, courier)
	if err != nil {
		return order{}, err
	}
	if err := saveConfirmedItems(ctx, tx, cart.ID, quote.Order.Products); err != nil {
		return order{}, err
	}

	paymentStatus := "Pending"
	if paid {
		paymentStatus = "Processing"
	}
	number := time.Now().UTC().Format("20060102") + "-" + strings.ToUpper(cart.ID.String()[:4])
	if _, err := tx.Exec(ctx, `UPDATE "order" SET status = 'Checked', order_id = $1, payment_status = $2,
		total_price = $3, discount_amount = $4, promo_code_id = $5, delivery_cost = $6, delivery_method = $7,
		is_courier_delivery = $8, payment_method = CASE WHEN $9 THEN 'online' ELSE payment_method END,
		cart_revision = cart_revision + 1, updated_at = now() WHERE id = $10`,
		number, paymentStatus, quote.Order.TotalPrice, quote.Order.DiscountAmount, quote.Order.PromoCodeID,
		quote.Order.DeliveryCost, method, courier, paid, cart.ID); err != nil {
		return order{}, err
	}
	if err := consumeCheckoutPromo(ctx, tx, cart.ID, person, quote); err != nil {
		return order{}, err
	}
	record, err := readCommerceOrder(ctx, tx, cart.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return order{}, errCartChanged
	}
	return record, err
}
