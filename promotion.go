package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

type promotion struct {
	ID            pgtype.UUID   `json:"id"`
	Title         string        `json:"title"`
	Description   *string       `json:"description"`
	DiscountType  string        `json:"discountType"`
	DiscountValue float32       `json:"discountValue"`
	TargetType    string        `json:"targetType"`
	MinQuantity   *int32        `json:"minQuantity"`
	BuyQuantity   *int32        `json:"buyQuantity"`
	GetQuantity   *int32        `json:"getQuantity"`
	Priority      int32         `json:"priority"`
	ValidFrom     apiTime       `json:"validFrom"`
	ValidUntil    apiTime       `json:"validUntil"`
	IsActive      bool          `json:"isActive"`
	CreatedAt     apiTime       `json:"createdAt"`
	UpdatedAt     apiTime       `json:"updatedAt"`
	ProductIDs    []pgtype.UUID `json:"productIds"`
}

type promoCode struct {
	ID                pgtype.UUID   `json:"id"`
	Code              string        `json:"code"`
	DiscountType      string        `json:"discountType"`
	DiscountValue     float32       `json:"discountValue"`
	MinOrderAmount    *float32      `json:"minOrderAmount"`
	MaxDiscountAmount *float32      `json:"maxDiscountAmount"`
	UsageLimit        *int32        `json:"usageLimit"`
	UsedCount         int32         `json:"usedCount"`
	ValidFrom         apiTime       `json:"validFrom"`
	ValidUntil        apiTime       `json:"validUntil"`
	IsActive          bool          `json:"isActive"`
	ApplicableTo      []pgtype.UUID `json:"applicableTo"`
	CreatedAt         apiTime       `json:"createdAt"`
	UpdatedAt         apiTime       `json:"updatedAt"`
}

type productCodeRow struct {
	Owner pgtype.UUID
	promoCode
}

const promotionColumns = `
    promo.id, promo.title, promo.description, promo.discount_type::text, promo.discount_value,
    promo.target_type::text, promo.min_quantity, promo.buy_quantity, promo.get_quantity,
    promo.priority, promo.valid_from, promo.valid_until, promo.is_active, promo.created_at, promo.updated_at
`
const promoCodeColumns = `
    pc.id, pc.code, pc.discount_type::text, pc.discount_value, pc.min_order_amount, pc.max_discount_amount,
    pc.usage_limit, pc.used_count, pc.valid_from, pc.valid_until, pc.is_active, pc.applicable_to, pc.created_at, pc.updated_at
`
const activePromotion = "promo.is_active AND promo.valid_from <= NOW() AND promo.valid_until >= NOW()"
const activePromoCode = "pc.is_active AND pc.valid_from <= NOW() AND pc.valid_until >= NOW()"
const promotionSelect = "SELECT " + promotionColumns + `,
    COALESCE(array_agg(link.product_id) FILTER (WHERE link.product_id IS NOT NULL), '{}'::uuid[])
    FROM promotion promo LEFT JOIN product_promotion link ON link.promotion_id = promo.id
`

func productPromotions(ctx context.Context, db database, id pgtype.UUID) ([]promotion, error) {
	return queryStructs[promotion](ctx, db, promotionSelect+
		" WHERE (promo.target_type = 'all' OR link.product_id = $1) AND "+activePromotion+
		" GROUP BY promo.id ORDER BY promo.priority DESC", id)
}

func loadListPromotions(ctx context.Context, db database, ids []pgtype.UUID, byID map[pgtype.UUID]*product) error {
	rows, err := queryStructs[struct {
		Owner pgtype.UUID
		promotion
	}](ctx, db, "SELECT requested.product_id, "+promotionColumns+`,
        CASE WHEN promo.target_type = 'all' THEN '{}'::uuid[] ELSE ARRAY[requested.product_id] END
        FROM unnest($1::uuid[]) AS requested(product_id)
        JOIN promotion promo ON promo.target_type = 'all' OR EXISTS (
            SELECT 1 FROM product_promotion link
            WHERE link.promotion_id = promo.id AND link.product_id = requested.product_id
        )
        WHERE `+activePromotion+" ORDER BY promo.priority DESC", ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		entry := byID[row.Owner]
		entry.Promotions = append(entry.Promotions, row.promotion)
	}
	return nil
}

func productPromoCodes(ctx context.Context, db database, id pgtype.UUID) ([]promoCode, error) {
	return queryStructs[promoCode](ctx, db, "SELECT "+promoCodeColumns+`
        FROM promo_code pc JOIN product_promo_code link ON link.promo_code_id = pc.id
        WHERE link.product_id = $1 AND `+activePromoCode, id)
}
