package main

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type cartDatabase interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

type cartAction int

const (
	readCart cartAction = iota
	replaceCart
	mergeCart
	addCartItem
)

type cartRequest struct {
	CartID           uuid.UUID  `json:"cartId"`
	Items            []cartItem `json:"items"`
	ExpectedRevision *int64     `json:"expectedRevision"`
	PromoCode        *string    `json:"promoCode"`
	MergeID          uuid.UUID  `json:"mergeId"`
	OperationID      uuid.UUID  `json:"operationId"`
	VariantID        uuid.UUID  `json:"variantId"`
}

type savedCart struct {
	ID        uuid.UUID
	Revision  int64
	PromoCode *string
}

type cartRequestError struct {
	status  int
	message string
}

func (err cartRequestError) Error() string { return err.message }

var errCartChanged = cartRequestError{http.StatusConflict, "Корзина изменилась. Обновите её"}

func (auth *accountAuth) registerCartRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/checkout/cart", auth.cartHandler(readCart))
	mux.HandleFunc("PUT /api/checkout/cart", auth.cartHandler(replaceCart))
	mux.HandleFunc("POST /api/checkout/cart/merge", auth.cartHandler(mergeCart))
	mux.HandleFunc("POST /api/checkout/cart/items", auth.cartHandler(addCartItem))
	mux.HandleFunc("POST /api/logout", auth.logout)
}

func (auth *accountAuth) cartHandler(action cartAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}
		var request cartRequest
		if action != readCart {
			if !isJSONRequest(r) {
				writeText(w, http.StatusBadRequest, "Content type error")
				return
			}
			body := http.MaxBytesReader(w, r.Body, maxJSONBodySize)
			err := jsonv2.UnmarshalRead(body, &request)
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				writeText(w, http.StatusRequestEntityTooLarge, "Cart request is too large")
				return
			case err != nil || !request.valid(action):
				writeText(w, http.StatusBadRequest, "Invalid cart JSON")
				return
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		quote, err := changeCart(ctx, auth.cartDB, claims.ID.Bytes, action, request)
		if err != nil {
			writeCartError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, struct {
			cartQuote
			ItemAddAvailable bool `json:"itemAddAvailable"`
		}{quote, true})
	}
}

func (request cartRequest) valid(action cartAction) bool {
	switch action {
	case replaceCart:
		return request.CartID != uuid.Nil && request.Items != nil && request.ExpectedRevision != nil
	case mergeCart:
		return request.MergeID != uuid.Nil && request.Items != nil
	case addCartItem:
		return request.CartID != uuid.Nil && request.VariantID != uuid.Nil && request.OperationID != uuid.Nil
	default:
		return true
	}
}

func writeCartError(w http.ResponseWriter, err error) {
	var invalid quoteInputError
	var rejected cartRequestError
	switch {
	case errors.As(err, &invalid):
		writeCheckoutError(w, http.StatusBadRequest, invalid.Error())
	case errors.As(err, &rejected):
		if rejected.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "60")
		}
		writeCheckoutError(w, rejected.status, rejected.message)
	case errors.Is(err, context.DeadlineExceeded):
		writeCheckoutError(w, http.StatusServiceUnavailable, "Корзина временно недоступна. Попробуйте ещё раз")
	default:
		slog.Error("cart transaction failed")
		writeCheckoutError(w, http.StatusInternalServerError, "Не удалось сохранить корзину. Попробуйте ещё раз")
	}
}

func changeCart(ctx context.Context, db cartDatabase, person uuid.UUID, action cartAction, request cartRequest) (cartQuote, error) {
	if action == replaceCart || action == mergeCart {
		items, err := normalizeCartItems(request.Items)
		if err != nil {
			return cartQuote{}, err
		}
		request.Items = items
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return cartQuote{}, err
	}
	defer rollbackCart(ctx, tx)

	cart, err := lockCart(ctx, tx, person, action == readCart || action == mergeCart)
	if err != nil {
		return cartQuote{}, err
	}
	if (action == replaceCart || action == addCartItem) && request.CartID != cart.ID {
		return cartQuote{}, errCartChanged
	}
	if action == replaceCart && *request.ExpectedRevision != cart.Revision {
		return cartQuote{}, errCartChanged
	}

	switch action {
	case replaceCart:
		cart.PromoCode = request.PromoCode
	default:
		items, err := savedCartItems(ctx, tx, cart.ID)
		if err != nil {
			return cartQuote{}, err
		}
		switch action {
		case readCart:
			request.Items = items
		case mergeCart:
			seen, err := seenCartMerge(ctx, tx, person, request.MergeID)
			if err != nil {
				return cartQuote{}, err
			}
			if seen {
				action = readCart
				request.Items = items
			} else {
				request.Items = append(items, request.Items...)
			}
		case addCartItem:
			replayed, err := replayedCartAdd(ctx, tx, cart.ID, request)
			if err != nil {
				return cartQuote{}, err
			}
			request.Items = items
			if replayed {
				action = readCart
			} else {
				request.Items, err = incrementCartItem(items, request.VariantID)
				if err != nil {
					return cartQuote{}, err
				}
			}
		}
	}

	courier := false
	quote, err := quoteCart(ctx, tx, quoteRequest{Items: request.Items, PromoCode: cart.PromoCode, IsCourierDelivery: &courier})
	if err != nil {
		return cartQuote{}, err
	}
	quote.Order.ID, quote.Order.PersonID, quote.Order.CartRevision = cart.ID, person, cart.Revision
	if action != readCart {
		quote.Order.CartRevision++
		if err := saveCartChange(ctx, tx, action, request, quote); err != nil {
			return cartQuote{}, err
		}
	}
	return quote, tx.Commit(ctx)
}

func rollbackCart(ctx context.Context, tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(cleanup)
}

func lockCart(ctx context.Context, tx pgx.Tx, person uuid.UUID, create bool) (savedCart, error) {
	var owner uuid.UUID
	err := tx.QueryRow(ctx, "SELECT id FROM person WHERE id = $1 AND is_deleted = false FOR UPDATE", person).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return savedCart{}, cartRequestError{http.StatusUnauthorized, "Войдите в аккаунт"}
	}
	if err != nil {
		return savedCart{}, err
	}
	return lockDraftCart(ctx, tx, person, create)
}

func savedCartItems(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]cartItem, error) {
	return queryStructs[cartItem](ctx, tx,
		"SELECT variant_id, quantity FROM order_product WHERE order_id = $1 ORDER BY variant_id", id)
}

func lockDraftCart(ctx context.Context, tx pgx.Tx, person uuid.UUID, create bool) (savedCart, error) {
	carts, err := queryStructs[savedCart](ctx, tx, `
		SELECT o.id, o.cart_revision, p.code FROM "order" o
		LEFT JOIN promo_code p ON p.id = o.promo_code_id
		WHERE o.person_id = $1 AND o.status = 'NotChecked' AND o.is_deleted = false AND o.checkout_key IS NULL
		ORDER BY o.created_at DESC LIMIT 2 FOR UPDATE OF o`, person)
	if err != nil {
		return savedCart{}, err
	}
	switch len(carts) {
	case 0:
		if !create {
			return savedCart{}, errCartChanged
		}
		cart := savedCart{Revision: 1}
		err := tx.QueryRow(ctx, `
			INSERT INTO "order" (person_id, status, total_price, cart_revision)
			VALUES ($1, 'NotChecked', 0, 1) RETURNING id`, person).Scan(&cart.ID)
		return cart, err
	case 1:
		if carts[0].Revision == 0 {
			return savedCart{}, cartRequestError{http.StatusServiceUnavailable, "Корзина временно недоступна"}
		}
		return carts[0], nil
	default:
		return savedCart{}, cartRequestError{http.StatusConflict, "Найдено несколько корзин. Обратитесь в магазин для их восстановления"}
	}
}

func seenCartMerge(ctx context.Context, tx pgx.Tx, person, mergeID uuid.UUID) (bool, error) {
	var seen bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM "order" WHERE person_id = $1 AND $2 = ANY(cart_merge_ids))`,
		person, mergeID).Scan(&seen)
	return seen, err
}

func replayedCartAdd(ctx context.Context, tx pgx.Tx, cartID uuid.UUID, request cartRequest) (bool, error) {
	var variantID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT variant_id FROM cart_add_operation WHERE cart_id = $1 AND operation_id = $2`,
		cartID, request.OperationID).Scan(&variantID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	case variantID != request.VariantID:
		return false, cartRequestError{http.StatusConflict, "Идентификатор добавления уже использован для другого товара"}
	default:
		return true, nil
	}
}

func incrementCartItem(items []cartItem, id uuid.UUID) ([]cartItem, error) {
	items, err := normalizeCartItems(items)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].VariantID == id {
			if items[i].Quantity == 99 {
				return nil, quoteInputError("Количество товара должно быть от 1 до 99")
			}
			items[i].Quantity++
			return items, nil
		}
	}
	return append(items, cartItem{VariantID: id, Quantity: 1}), nil
}

func saveCartChange(ctx context.Context, tx pgx.Tx, action cartAction, request cartRequest, quote cartQuote) error {
	id := quote.Order.ID
	var mergeID *uuid.UUID
	switch action {
	case addCartItem:
		if slices.ContainsFunc(quote.UnavailableItems, func(item cartItem) bool { return item.VariantID == request.VariantID }) {
			return quoteInputError("Этот товар сейчас недоступен")
		}
		for _, item := range quote.Items {
			if item.VariantID == request.VariantID {
				if err := updateCartItem(ctx, tx, id, item); err != nil {
					return err
				}
				break
			}
		}
		_, err := tx.Exec(ctx, "INSERT INTO cart_add_operation (cart_id, operation_id, variant_id) VALUES ($1, $2, $3)",
			id, request.OperationID, request.VariantID)
		if err != nil {
			return err
		}
	case replaceCart, mergeCart:
		if err := replaceCartItems(ctx, tx, id, quote.Items); err != nil {
			return err
		}
		if action == mergeCart {
			mergeID = &request.MergeID
		}
	}

	_, err := tx.Exec(ctx, `
		UPDATE "order" SET total_price = $1, discount_amount = $2, promo_code_id = $3, cart_revision = $4,
			updated_at = now(), cart_merge_ids = CASE WHEN $5::uuid IS NULL THEN cart_merge_ids ELSE array_append(cart_merge_ids, $5) END
		WHERE id = $6`, quote.Order.TotalPrice, quote.Order.DiscountAmount, quote.Order.PromoCodeID, quote.Order.CartRevision, mergeID, id)
	return err
}

func updateCartItem(ctx context.Context, tx pgx.Tx, cartID uuid.UUID, item cartItem) error {
	if _, err := tx.Exec(ctx, "DELETE FROM order_product WHERE order_id = $1 AND variant_id = $2", cartID, item.VariantID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "INSERT INTO order_product (order_id, variant_id, quantity) VALUES ($1, $2, $3)",
		cartID, item.VariantID, item.Quantity)
	return err
}

func replaceCartItems(ctx context.Context, tx pgx.Tx, cartID uuid.UUID, items []cartItem) error {
	variantIDs := make([]uuid.UUID, 0, len(items))
	quantities := make([]int32, 0, len(items))
	for _, item := range items {
		variantIDs = append(variantIDs, item.VariantID)
		quantities = append(quantities, item.Quantity)
	}
	var found int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM product_variant WHERE id = ANY($1)", variantIDs).Scan(&found); err != nil {
		return err
	}
	if found != len(items) {
		return quoteInputError("Удалите недоступные товары из корзины и повторите перенос")
	}
	if _, err := tx.Exec(ctx, "DELETE FROM order_product WHERE order_id = $1", cartID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO order_product (order_id, variant_id, quantity)
		SELECT $1, variant_id, quantity FROM unnest($2::uuid[], $3::integer[]) AS item(variant_id, quantity)`,
		cartID, variantIDs, quantities)
	return err
}
