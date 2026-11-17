package main

import (
	"bytes"
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

type cartItem struct {
	VariantID uuid.UUID `json:"variantId"`
	Quantity  int32     `json:"quantity"`
}

type quoteRequest struct {
	Items             []cartItem `json:"items"`
	PromoCode         *string    `json:"promoCode"`
	DeliveryMethod    *string    `json:"deliveryMethod"`
	IsCourierDelivery *bool      `json:"isCourierDelivery"`
}

type cartQuote struct {
	Order                  order      `json:"order"`
	Items                  []cartItem `json:"items"`
	UnavailableItems       []cartItem `json:"unavailableItems"`
	PromoCode              *string    `json:"promoCode"`
	PromoCodeError         *string    `json:"promoCodeError"`
	SubtotalCents          int64      `json:"subtotalCents"`
	PromotionDiscountCents int64      `json:"promotionDiscountCents"`
	PromoCodeDiscountCents int64      `json:"promoCodeDiscountCents"`
	TotalCents             int64      `json:"totalCents"`
}

type quoteInputError string

func (err quoteInputError) Error() string {
	return string(err)
}

func cartQuoteHandler(db database, enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isJSONRequest(r) {
			writeText(w, http.StatusBadRequest, "Content type error")
			return
		}
		courier := false
		input := quoteRequest{IsCourierDelivery: &courier}
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
		err := jsonv2.UnmarshalRead(r.Body, &input)
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeText(w, http.StatusRequestEntityTooLarge, "Cart quote request is too large")
			return
		case err != nil || input.Items == nil || input.IsCourierDelivery == nil:
			writeText(w, http.StatusBadRequest, "Invalid cart quote JSON")
			return
		case !enabled:
			writeCheckoutError(w, http.StatusServiceUnavailable, "Новое оформление пока недоступно")
			return
		}
		result, err := quoteCart(r.Context(), db, input)
		var badInput quoteInputError
		switch {
		case errors.As(err, &badInput):
			writeCheckoutError(w, http.StatusBadRequest, badInput.Error())
		case err != nil:
			slog.Error("cart quote database read failed")
			writeCheckoutError(w, http.StatusInternalServerError, "Не удалось рассчитать корзину. Попробуйте ещё раз")
		default:
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, result)
		}
	}
}

func writeCheckoutError(w http.ResponseWriter, status int, message string) {
	w.Header()["Content-Type"] = nil
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message))
}

func normalizeCartItems(items []cartItem) ([]cartItem, error) {
	if len(items) > 100 {
		return nil, quoteInputError("Слишком много товаров в корзине")
	}
	const invalidQuantity = quoteInputError("Количество товара должно быть от 1 до 99")
	quantities := make(map[uuid.UUID]int32, len(items))
	for _, item := range items {
		if item.VariantID == uuid.Nil || item.Quantity < 1 || item.Quantity > 99 {
			return nil, invalidQuantity
		}
		quantities[item.VariantID] += item.Quantity
		if quantities[item.VariantID] > 99 {
			return nil, invalidQuantity
		}
	}
	result := make([]cartItem, 0, len(quantities))
	for id, quantity := range quantities {
		result = append(result, cartItem{VariantID: id, Quantity: quantity})
	}
	slices.SortFunc(result, compareCartItems)
	return result, nil
}

func compareCartItems(a, b cartItem) int {
	return bytes.Compare(a.VariantID[:], b.VariantID[:])
}

func quoteCart(ctx context.Context, db database, input quoteRequest) (cartQuote, error) {
	items, err := normalizeCartItems(input.Items)
	if err != nil {
		return cartQuote{}, err
	}
	promos, err := activePromotions(ctx, db)
	if err != nil {
		return cartQuote{}, err
	}
	products, unavailable, err := loadQuoteProducts(ctx, db, items, promos)
	if err != nil {
		return cartQuote{}, err
	}
	var base float32
	for _, item := range products {
		base += float32(item.Variant.Price * float32(item.Quantity))
	}
	subtotal, err := checkoutCents(base)
	if err != nil {
		return cartQuote{}, err
	}
	promoDiscount, err := checkoutCents(min(base, max(0, cartPromotionDiscount(products, promos, base))))
	if err != nil {
		return cartQuote{}, err
	}
	afterPromotions := max(0, subtotal-promoDiscount)
	result := cartQuote{
		Items: items, UnavailableItems: unavailable, PromoCode: input.PromoCode,
		SubtotalCents: subtotal, PromotionDiscountCents: promoDiscount,
		Order: order{
			orderSummary:      orderSummary{Status: "NotChecked", PaymentStatus: "Pending"},
			Products:          products,
			IsCourierDelivery: input.IsCourierDelivery,
		},
	}
	if err := quotePromoCode(ctx, db, &result, afterPromotions); err != nil {
		return cartQuote{}, err
	}
	method := "pickup"
	if input.DeliveryMethod != nil {
		method = *input.DeliveryMethod
	}
	delivery, err := quoteDeliveryCost(method, *input.IsCourierDelivery, afterPromotions)
	if err != nil {
		return cartQuote{}, err
	}
	result.TotalCents = max(0, afterPromotions-result.PromoCodeDiscountCents) + int64(delivery)*100
	discount := float32(promoDiscount+result.PromoCodeDiscountCents) / 100
	result.Order.DeliveryMethod = &method
	result.Order.DeliveryCost = delivery
	result.Order.TotalPrice = float32(result.TotalCents) / 100
	result.Order.DiscountAmount = &discount
	return result, nil
}

func loadQuoteProducts(ctx context.Context, db database, items []cartItem, promos []promotion) ([]orderProduct, []cartItem, error) {
	products := make([]orderProduct, 0, len(items))
	unavailable := make([]cartItem, 0)
	if len(items) == 0 {
		return products, unavailable, nil
	}

	variantIDs := make([]pgtype.UUID, 0, len(items))
	for _, item := range items {
		variantIDs = append(variantIDs, pgtype.UUID{Bytes: item.VariantID, Valid: true})
	}
	variants, err := queryStructs[variant](ctx, db, variantSelect+" WHERE id = ANY($1) AND is_deleted IS NOT TRUE", variantIDs)
	if err != nil {
		return nil, nil, err
	}
	variantsByID := make(map[uuid.UUID]variant, len(variants))
	productIDs := make([]pgtype.UUID, 0, len(variants))
	for _, v := range variants {
		variantsByID[v.ID.Bytes] = v
		productIDs = append(productIDs, v.ProductID)
	}

	productsByID, err := productsForDetails(ctx, db, productIDs, promos, false)
	if err != nil {
		return nil, nil, err
	}

	for _, item := range items {
		v := variantsByID[item.VariantID]
		p, exists := productsByID[v.ProductID]
		if !exists {
			unavailable = append(unavailable, item)
			continue
		}
		if _, err := checkoutCents(v.Price); err != nil {
			return nil, nil, err
		}
		products = append(products, orderProduct{
			Product: p, Variant: v, Quantity: item.Quantity,
			HistoricalPrice: v.Price, HistoricalCostPrice: v.CostPrice,
		})
	}
	return products, unavailable, nil
}

func cartPromotionDiscount(items []orderProduct, promos []promotion, subtotal float32) float32 {
	var total float32
	for _, promo := range promos {
		var applicable float32
		switch promo.TargetType {
		case "all":
			applicable = subtotal
		case "product":
			for _, item := range items {
				if slices.Contains(promo.ProductIDs, item.Product.ID) {
					applicable += float32(item.Variant.Price * float32(item.Quantity))
				}
			}
		}
		// Legacy brand and category rules query persisted order items, which a quote has none of.
		if applicable != 0 {
			total += priceDiscount(promo.DiscountType, promo.DiscountValue, applicable)
		}
	}
	return total
}

func quotePromoCode(ctx context.Context, db database, result *cartQuote, afterPromotions int64) error {
	if result.PromoCode == nil {
		return nil
	}
	code := strings.TrimSpace(*result.PromoCode)
	if code == "" {
		return nil
	}
	if len(code) > 50 {
		return quoteInputError("Некорректный промокод")
	}
	codes, err := queryStructs[promoCode](ctx, db, "SELECT "+promoCodeColumns+
		" FROM promo_code pc WHERE pc.code = $1 AND "+activePromoCode, code)
	if err != nil {
		return err
	}
	var message string
	if len(codes) == 0 {
		message = "Промокод недоступен"
	} else {
		promo := codes[0]
		base := float32(afterPromotions) / 100
		switch {
		case promo.UsageLimit != nil && promo.UsedCount >= *promo.UsageLimit:
			message = "Промокод исчерпан"
		case promo.MinOrderAmount != nil && base < *promo.MinOrderAmount:
			message = "Минимальная сумма заказа: " + strconv.FormatFloat(float64(*promo.MinOrderAmount), 'f', -1, 32)
		default:
			discount := priceDiscount(promo.DiscountType, promo.DiscountValue, base)
			if promo.MaxDiscountAmount != nil {
				discount = min(discount, *promo.MaxDiscountAmount)
			}
			cents, err := checkoutCents(max(0, discount))
			if err != nil {
				return err
			}
			result.PromoCodeDiscountCents = min(cents, afterPromotions)
			result.Order.PromoCodeID = &promo.ID
			return nil
		}
	}
	result.PromoCodeError = &message
	return nil
}

func checkoutCents(amount float32) (int64, error) {
	if math.IsNaN(float64(amount)) || amount < 0 || amount > 1_000_000 {
		return 0, quoteInputError("Некорректная сумма")
	}
	return int64(math.Round(float64(amount) * 100)), nil
}

func priceDiscount(kind string, value, total float32) float32 {
	if kind == "percentage" {
		return total * (value / 100)
	}
	return value
}

func quoteDeliveryCost(method string, courier bool, subtotalCents int64) (int32, error) {
	switch method {
	case "pickup":
		return 0, nil
	case "standart", "post":
		if subtotalCents >= 25_000 || (method == "post" && !courier) {
			return 0, nil
		}
		return 10, nil
	default:
		return 0, quoteInputError("Invalid delivery method")
	}
}
