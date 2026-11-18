package main

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const promoCodeRoute = "/api/promo-code"
const promoCodeSelect = "SELECT " + promoCodeColumns + " FROM promo_code pc"

type promoCodeInput struct {
	Code              *string         `json:"code"`
	DiscountType      *string         `json:"discountType"`
	DiscountValue     *promoCodeValue `json:"discountValue"`
	MinOrderAmount    *float32        `json:"minOrderAmount"`
	MaxDiscountAmount *float32        `json:"maxDiscountAmount"`
	UsageLimit        *int32          `json:"usageLimit"`
	ValidFrom         *time.Time      `json:"validFrom"`
	ValidUntil        *time.Time      `json:"validUntil"`
	ApplicableTo      json.RawMessage `json:"applicableTo"`
	IsActive          json.RawMessage `json:"isActive"`
}

type promoCodeValue float32

func (value *promoCodeValue) UnmarshalJSON(raw []byte) error {
	text := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
	}
	number, err := strconv.ParseFloat(text, 32)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return quoteInputError("Некорректная сумма скидки")
	}
	*value = promoCodeValue(number)
	return nil
}

func (auth *accountAuth) registerPromoCodes(mux *http.ServeMux, db cartDatabase) {
	mux.HandleFunc("GET "+promoCodeRoute, auth.requireModerator(auth.listPromoCodes))
	mux.HandleFunc("GET "+promoCodeRoute+"/{id}", auth.requireModerator(auth.getPromoCode))
	mux.HandleFunc("POST "+promoCodeRoute, auth.requireModerator(auth.savePromoCode(db, true)))
	mux.HandleFunc("PATCH "+promoCodeRoute+"/{id}", auth.requireModerator(auth.savePromoCode(db, false)))
	mux.HandleFunc("DELETE "+promoCodeRoute+"/{id}", auth.requireModerator(auth.deletePromoCode(db)))
	mux.HandleFunc("POST "+promoCodeRoute+"/validate", auth.validatePromoCode)
}

func (auth *accountAuth) listPromoCodes(w http.ResponseWriter, r *http.Request) {
	entries, err := queryStructs[promoCode](r.Context(), auth.db, promoCodeSelect+" ORDER BY pc.created_at DESC, pc.id")
	if err != nil {
		writePromoCodeError(w, err)
		return
	}
	if entries == nil {
		entries = []promoCode{}
	}
	writeJSON(w, listResponse[promoCode]{Status: "success", Length: len(entries), Data: entries})
}

func (auth *accountAuth) getPromoCode(w http.ResponseWriter, r *http.Request) {
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	entries, err := queryStructs[promoCode](r.Context(), auth.db, promoCodeSelect+" WHERE pc.id = $1", id)
	if err != nil {
		writePromoCodeError(w, err)
		return
	}
	writeOrderDetail(w, entries)
}

func (auth *accountAuth) savePromoCode(db cartDatabase, create bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input promoCodeInput
		if !decodeCommerceJSON(w, r, &input) {
			return
		}
		var id = uuid.New()
		if !create {
			parsed, ok := requestID(w, r)
			if !ok {
				return
			}
			id = parsed.Bytes
		}
		ctx := r.Context()
		var entry promoCode
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			switch {
			case create:
				entry.IsActive = true
			default:
				rows, err := queryStructs[promoCode](ctx, tx, promoCodeSelect+" WHERE pc.id = $1 FOR UPDATE", id)
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					return pgx.ErrNoRows
				}
				entry = rows[0]
			}
			if err := applyPromoCodeInput(&entry, input); err != nil {
				return err
			}
			args := []any{id, entry.Code, entry.DiscountType, entry.DiscountValue, entry.MinOrderAmount,
				entry.MaxDiscountAmount, entry.UsageLimit, entry.ValidFrom.Time, entry.ValidUntil.Time, entry.ApplicableTo, entry.IsActive}
			query := `UPDATE promo_code SET code = $2, discount_type = $3, discount_value = $4, min_order_amount = $5,
				max_discount_amount = $6, usage_limit = $7, valid_from = $8, valid_until = $9, applicable_to = $10,
				is_active = $11, updated_at = now() WHERE id = $1`
			if create {
				query = `INSERT INTO promo_code(id, code, discount_type, discount_value, min_order_amount, max_discount_amount,
					usage_limit, valid_from, valid_until, applicable_to, is_active, used_count, created_at, updated_at)
					VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,0,now(),now())`
			}
			if _, err := tx.Exec(ctx, query, args...); err != nil {
				return err
			}
			rows, err := queryStructs[promoCode](ctx, tx, promoCodeSelect+" WHERE pc.id = $1", id)
			if err == nil {
				entry = rows[0]
			}
			return err
		})
		if err != nil {
			writePromoCodeError(w, err)
			return
		}
		writeJSON(w, singleResponse{Status: "success", Data: entry})
	}
}

func applyPromoCodeInput(entry *promoCode, input promoCodeInput) error {
	if input.Code != nil {
		entry.Code = strings.TrimSpace(*input.Code)
	}
	if input.DiscountType != nil {
		entry.DiscountType = *input.DiscountType
	}
	if input.DiscountValue != nil {
		entry.DiscountValue = float32(*input.DiscountValue)
	}
	if input.MinOrderAmount != nil {
		entry.MinOrderAmount = input.MinOrderAmount
	}
	if input.MaxDiscountAmount != nil {
		entry.MaxDiscountAmount = input.MaxDiscountAmount
	}
	if input.UsageLimit != nil {
		entry.UsageLimit = input.UsageLimit
		if *entry.UsageLimit == 0 {
			entry.UsageLimit = nil
		}
	}
	if input.ValidFrom != nil {
		entry.ValidFrom = apiTime{pgtype.Timestamptz{Time: input.ValidFrom.UTC(), Valid: true}}
	}
	if input.ValidUntil != nil {
		entry.ValidUntil = apiTime{pgtype.Timestamptz{Time: input.ValidUntil.UTC(), Valid: true}}
	}
	if len(input.IsActive) != 0 {
		switch string(input.IsActive) {
		case "true", `"true"`:
			entry.IsActive = true
		case "false", `"false"`:
			entry.IsActive = false
		default:
			return quoteInputError("Некорректный статус промокода")
		}
	}
	if len(input.ApplicableTo) != 0 {
		raw := []byte(input.ApplicableTo)
		if raw[0] == '"' {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return quoteInputError("Некорректные товары промокода")
			}
			raw = []byte(text)
		}
		if err := json.Unmarshal(raw, &entry.ApplicableTo); err != nil {
			return quoteInputError("Некорректные товары промокода")
		}
	}
	if entry.Code == "" || !validCheckoutText(entry.Code, 50) ||
		(entry.DiscountType != "fixed" && entry.DiscountType != "percentage") ||
		entry.DiscountValue <= 0 || entry.DiscountValue > maxCheckoutAmount ||
		(entry.DiscountType == "percentage" && entry.DiscountValue > 100) ||
		!entry.ValidFrom.Valid || !entry.ValidUntil.Valid || !entry.ValidFrom.Time.Before(entry.ValidUntil.Time) ||
		(entry.UsageLimit != nil && *entry.UsageLimit < 0) {
		return quoteInputError("Проверьте условия промокода")
	}
	for _, amount := range []*float32{entry.MinOrderAmount, entry.MaxDiscountAmount} {
		if amount != nil {
			if _, err := checkoutCents(*amount); err != nil {
				return err
			}
		}
	}
	return nil
}

func (auth *accountAuth) deletePromoCode(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			var used int32
			err := tx.QueryRow(ctx, "SELECT COALESCE(used_count, 0) FROM promo_code WHERE id = $1 FOR UPDATE", id).Scan(&used)
			if err != nil {
				return err
			}
			var referenced bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM "order" WHERE promo_code_id = $1)
				OR EXISTS(SELECT 1 FROM promo_code_usage WHERE promo_code_id = $1)`, id).Scan(&referenced)
			if err != nil {
				return err
			}
			if used > 0 || referenced {
				return cartRequestError{http.StatusConflict, "Промокод связан с заказом. Его можно выключить"}
			}
			_, err = tx.Exec(ctx, "DELETE FROM promo_code WHERE id = $1", id)
			return err
		})
		if err != nil {
			writePromoCodeError(w, err)
			return
		}
		writeJSON(w, struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}{"success", "Promo code deleted successfully"})
	}
}

func (auth *accountAuth) validatePromoCode(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code       string   `json:"code"`
		OrderTotal *float32 `json:"orderTotal"`
	}
	if !decodeCommerceJSON(w, r, &input) {
		return
	}
	if input.OrderTotal == nil || strings.TrimSpace(input.Code) == "" {
		writePromoCodeError(w, quoteInputError("Проверьте промокод и сумму заказа"))
		return
	}
	total, err := checkoutCents(*input.OrderTotal)
	if err != nil {
		writePromoCodeError(w, err)
		return
	}
	result := cartQuote{PromoCode: &input.Code}
	if err := quotePromoCode(r.Context(), auth.db, &result, total); err != nil {
		writePromoCodeError(w, err)
		return
	}
	if result.Order.PromoCodeID == nil && result.PromoCodeError != nil && *result.PromoCodeError == "Промокод недоступен" {
		writePromoCodeError(w, pgx.ErrNoRows)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, struct {
		IsValid        bool    `json:"isValid"`
		DiscountAmount float32 `json:"discountAmount"`
		Message        *string `json:"message"`
	}{result.Order.PromoCodeID != nil, float32(result.PromoCodeDiscountCents) / 100, result.PromoCodeError})
}

func writePromoCodeError(w http.ResponseWriter, err error) {
	var dbErr *pgconn.PgError
	var input quoteInputError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeCheckoutError(w, http.StatusNotFound, "Промокод не найден")
	case errors.As(err, &dbErr) && dbErr.Code == "23505":
		writeCheckoutError(w, http.StatusConflict, "Такой промокод уже есть")
	case errors.As(err, &input):
		writeCheckoutError(w, http.StatusBadRequest, input.Error())
	default:
		writeCartError(w, err)
	}
}

func decodeCommerceJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !isJSONRequest(r) {
		writeText(w, http.StatusBadRequest, "Content type error")
		return false
	}
	var raw json.RawMessage
	err := jsonv2.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBodySize), &raw)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeText(w, http.StatusRequestEntityTooLarge, "Request is too large")
	case err != nil || len(raw) == 0 || raw[0] != '{' || jsonv2.Unmarshal(raw, dst) != nil:
		writeText(w, http.StatusBadRequest, "Invalid request JSON")
	default:
		return true
	}
	return false
}
