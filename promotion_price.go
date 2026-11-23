package main

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type promotionPrice struct {
	FinalCents  int64        `json:"finalCents"`
	SavingCents int64        `json:"savingCents"`
	PromotionID *pgtype.UUID `json:"promotionId"`
}

type promotionPricingKey struct{}

func withPromotionPricing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), promotionPricingKey{}, true)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func promotionPricingEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(promotionPricingKey{}).(bool)
	return enabled
}

func bestPromotionPrice(baseCents int64, productID pgtype.UUID, promos []promotion) promotionPrice {
	result := promotionPrice{FinalCents: baseCents}
	var winner *promotion
	for i := range promos {
		promo := &promos[i]
		if promo.TargetType != "all" && (promo.TargetType != "product" || !slices.Contains(promo.ProductIDs, productID)) {
			continue
		}
		saving := promotionSavingCents(baseCents, promo.DiscountType, promo.DiscountValue)
		if saving == 0 || saving < result.SavingCents {
			continue
		}
		if saving == result.SavingCents && winner != nil &&
			(promo.Priority < winner.Priority || (promo.Priority == winner.Priority && bytes.Compare(promo.ID.Bytes[:], winner.ID.Bytes[:]) >= 0)) {
			continue
		}
		winner = promo
		result.SavingCents = saving
	}
	if winner != nil {
		result.PromotionID = &winner.ID
		result.FinalCents -= result.SavingCents
	}
	return result
}

func promotionSavingCents(baseCents int64, kind string, storedValue float32) int64 {
	value := float64(storedValue)
	if baseCents <= 0 || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	// Use the decimal value visible to the editor before converting to cents or basis points.
	value, _ = strconv.ParseFloat(strconv.FormatFloat(value, 'f', -1, 32), 64)
	switch kind {
	case "percentage":
		basisPoints := int64(math.Round(min(100, value) * 100))
		return (baseCents*basisPoints + 5000) / 10000
	case "fixed":
		return int64(min(float64(baseCents), math.Round(value*100)))
	default:
		return 0
	}
}

func applyProductPromotionPrices(entries []product) {
	for i := range entries {
		entry := &entries[i]
		promos := entry.Promotions
		if entry.IsDeleted != nil && *entry.IsDeleted {
			promos = nil
		}
		setVariantPromotionPrices(entry.Variants, promos)
	}
}

func loadVariantPromotionPrices(ctx context.Context, db database, variants []variant) error {
	if len(variants) == 0 {
		return nil
	}
	promos, err := activePromotions(ctx, db)
	if err != nil {
		return err
	}
	ids := make([]pgtype.UUID, 0, len(variants))
	for _, entry := range variants {
		ids = append(ids, entry.ProductID)
	}
	rows, err := db.Query(ctx, "SELECT id FROM product WHERE id = ANY($1) AND is_deleted IS NOT TRUE", ids)
	if err != nil {
		return err
	}
	activeProducts, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
	if err != nil {
		return err
	}
	eligible := make(map[pgtype.UUID]bool, len(activeProducts))
	for _, id := range activeProducts {
		eligible[id] = true
	}
	for i := range variants {
		offers := promos
		if !eligible[variants[i].ProductID] {
			offers = nil
		}
		setVariantPromotionPrice(&variants[i], offers)
	}
	return nil
}

func setVariantPromotionPrices(variants []variant, promos []promotion) {
	for i := range variants {
		setVariantPromotionPrice(&variants[i], promos)
	}
}

func setVariantPromotionPrice(entry *variant, promos []promotion) {
	if entry.IsDeleted != nil && *entry.IsDeleted {
		promos = nil
	}
	price := bestPromotionPrice(entry.PriceCents, entry.ProductID, promos)
	entry.PromotionPrice = &price
}
