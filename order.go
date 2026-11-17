package main

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

type orderSummary struct {
	ID             uuid.UUID `json:"id"`
	OrderID        *string   `json:"orderId"`
	PersonID       uuid.UUID `json:"personId"`
	Status         string    `json:"status"`
	PaymentStatus  string    `json:"paymentStatus"`
	PaymentID      *string   `json:"paymentId"`
	PaymentFormURL *string   `json:"paymentFormUrl"`
	TotalPrice     float32   `json:"totalPrice"`
	CreatedAt      apiTime   `json:"createdAt"`
	UpdatedAt      apiTime   `json:"updatedAt"`
}

type order struct {
	orderSummary
	DeliveryCost        int32           `json:"deliveryCost"`
	PromoCodeID         *pgtype.UUID    `json:"promoCodeId"`
	DiscountAmount      *float32        `json:"discountAmount"`
	Products            []orderProduct  `json:"products" db:"-"`
	DeliveryMethod      *string         `json:"deliveryMethod"`
	DeliveryDate        apiTime         `json:"deliveryDate"`
	DeliveryAddress     *string         `json:"deliveryAddress"`
	DeliveryPostAddress *string         `json:"deliveryPostAddress"`
	IsCourierDelivery   *bool           `json:"isCourierDelivery"`
	PaymentMethod       *string         `json:"paymentMethod"`
	Comment             *string         `json:"comment"`
	ContactSnapshot     json.RawMessage `json:"contactSnapshot"`
	CartRevision        int64           `json:"cartRevision"`
	IsDeleted           *bool           `json:"isDeleted"`
}

type orderProduct struct {
	Product             product `json:"product"`
	Variant             variant `json:"variant"`
	Quantity            int32   `json:"quantity"`
	HistoricalPrice     float32 `json:"historicalPrice"`
	HistoricalCostPrice float32 `json:"historicalCostPrice"`
}

const orderSummaryColumns = `o.id, o.order_id, o.person_id, o.status::text, o.payment_status::text,
	o.payment_id, o.payment_form_url, o.total_price, o.created_at, o.updated_at`

const orderSummarySelect = "SELECT " + orderSummaryColumns + ` FROM "order" o`

const orderSelect = "SELECT " + orderSummaryColumns + `, o.delivery_cost, o.promo_code_id, o.discount_amount,
	o.delivery_method, o.delivery_date AT TIME ZONE 'UTC', o.delivery_address, o.delivery_post_address, o.is_courier_delivery,
	o.payment_method, o.comment, o.contact_snapshot, o.cart_revision, o.is_deleted FROM "order" o`

func loadOrderProducts(ctx context.Context, db database, orders []order) error {
	if len(orders) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(orders))
	byID := make(map[uuid.UUID]*order, len(orders))
	for i := range orders {
		entry := &orders[i]
		ids = append(ids, entry.ID)
		byID[entry.ID] = entry
		entry.Products = []orderProduct{}
	}
	lines, err := queryStructs[struct {
		OrderID uuid.UUID
		variant
		Quantity            int32
		HistoricalPrice     float32
		HistoricalCostPrice float32
	}](ctx, db, `
		SELECT op.order_id, v.id, v.product_id, v.volume, v.cost_price, v.price, v.is_default,
		       v.is_deleted, v.created_at, v.updated_at, op.quantity,
		       CASE WHEN op.unit_price IS NOT NULL AND op.unit_cost_price IS NOT NULL THEN op.unit_price
		            ELSE COALESCE(h.price, v.price) END,
		       CASE WHEN op.unit_price IS NOT NULL AND op.unit_cost_price IS NOT NULL THEN op.unit_cost_price
		            ELSE COALESCE(h.cost_price, v.cost_price) END
		FROM order_product op
		JOIN "order" o ON o.id = op.order_id
		JOIN product_variant v ON v.id = op.variant_id
		LEFT JOIN LATERAL (
			SELECT price, cost_price FROM product_variant_price_history
			WHERE variant_id = op.variant_id AND valid_from <= o.created_at
			  AND (valid_to IS NULL OR valid_to > o.created_at)
			ORDER BY valid_from DESC LIMIT 1
		) h ON op.unit_price IS NULL OR op.unit_cost_price IS NULL
		WHERE op.order_id = ANY($1)`, ids)
	if err != nil || len(lines) == 0 {
		return err
	}
	productIDs := make([]pgtype.UUID, 0, len(lines))
	for _, line := range lines {
		productIDs = append(productIDs, line.ProductID)
	}
	promos, err := activePromotions(ctx, db)
	if err != nil {
		return err
	}
	products, err := productsForDetails(ctx, db, productIDs, promos, true)
	if err != nil {
		return err
	}
	for _, line := range lines {
		p, exists := products[line.ProductID]
		if !exists {
			continue
		}
		entry := byID[line.OrderID]
		entry.Products = append(entry.Products, orderProduct{
			Product: p, Variant: line.variant, Quantity: line.Quantity,
			HistoricalPrice: line.HistoricalPrice, HistoricalCostPrice: line.HistoricalCostPrice,
		})
	}
	return nil
}
