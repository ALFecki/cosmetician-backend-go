package main

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type product struct {
	ID            pgtype.UUID    `json:"id"`
	Title         string         `json:"title"`
	FullTitle     string         `json:"fullTitle"`
	Description   *string        `json:"description"`
	Image         *string        `json:"image"`
	MethodOfUse   *string        `json:"methodOfUse"`
	Composition   *string        `json:"composition"`
	BrandID       pgtype.UUID    `json:"brandId"`
	Brand         brand          `json:"brand"`
	ProductScopes []productTaxon `json:"productScopes"`
	ProductTypeID pgtype.UUID    `json:"productTypeId"`
	ProductType   productTaxon   `json:"productType"`
	Countries     []country      `json:"countries"`
	Categories    []catalogEntry `json:"categories"`
	Variants      []variant      `json:"variants"`
	Promotions    []promotion    `json:"promotions"`
	PromoCodes    []promoCode    `json:"promoCodes"`
	IsPriceHidden bool           `json:"isPriceHidden"`
	IsDeleted     *bool          `json:"isDeleted"`
	CreatedAt     apiTime        `json:"createdAt"`
	UpdatedAt     apiTime        `json:"updatedAt"`
}

type productTaxon struct {
	ID          pgtype.UUID `json:"id"`
	Title       string      `json:"title"`
	Description *string     `json:"description"`
	CreatedAt   apiTime     `json:"createdAt"`
	UpdatedAt   apiTime     `json:"updatedAt"`
}

type country struct {
	ID        pgtype.UUID `json:"id"`
	Name      string      `json:"name"`
	Code      string      `json:"code"`
	IsDeleted *bool       `json:"isDeleted"`
	CreatedAt apiTime     `json:"createdAt"`
	UpdatedAt apiTime     `json:"updatedAt"`
}

type variant struct {
	ID             pgtype.UUID     `json:"id"`
	ProductID      pgtype.UUID     `json:"productId"`
	Volume         float32         `json:"volume"`
	CostPrice      float64         `json:"costPrice"`
	Price          float64         `json:"price"`
	IsDefault      bool            `json:"isDefault"`
	IsDeleted      *bool           `json:"isDeleted"`
	CreatedAt      apiTime         `json:"createdAt"`
	UpdatedAt      apiTime         `json:"updatedAt"`
	PriceCents     int64           `json:"-"`
	PromotionPrice *promotionPrice `json:"promotionPrice,omitempty" db:"-"`
}

const productSelect = `
    SELECT p.id, p.title, p.full_title, p.description, p.image, p.method_of_use, p.composition,
           p.brand_id, p.product_type_id, p.is_price_hidden, p.is_deleted, p.created_at, p.updated_at,
           b.id, b.title, COALESCE(b.description, ''), b.is_deleted, b.created_at, b.updated_at, b.image, b.manufacturer, b.importer,
           t.id, t.title, t.description, t.created_at, t.updated_at
    FROM product p
    JOIN brand b ON p.brand_id = b.id
    JOIN product_type t ON p.product_type_id = t.id
`

const variantSelect = `
    SELECT id, product_id, volume, cost_price_cents::double precision / 100, price_cents::double precision / 100,
           is_default, is_deleted, created_at, updated_at, price_cents
    FROM product_variant
`

func scanProduct(row pgx.CollectableRow) (product, error) {
	var entry product
	b, t := &entry.Brand, &entry.ProductType
	err := row.Scan(&entry.ID, &entry.Title, &entry.FullTitle, &entry.Description, &entry.Image, &entry.MethodOfUse,
		&entry.Composition, &entry.BrandID, &entry.ProductTypeID, &entry.IsPriceHidden, &entry.IsDeleted, &entry.CreatedAt, &entry.UpdatedAt,
		&b.ID, &b.Title, &b.Description, &b.IsDeleted, &b.CreatedAt, &b.UpdatedAt, &b.Image, &b.Manufacturer, &b.Importer,
		&t.ID, &t.Title, &t.Description, &t.CreatedAt, &t.UpdatedAt)
	entry.Countries = []country{}
	entry.Categories = []catalogEntry{}
	entry.ProductScopes = []productTaxon{}
	entry.Variants = []variant{}
	entry.Promotions = []promotion{}
	entry.PromoCodes = []promoCode{}
	return entry, err
}

func queryStructs[T any](ctx context.Context, db database, query string, args ...any) ([]T, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[T])
}

func loadProductRelations(ctx context.Context, db database, entries []product) error {
	if len(entries) == 0 {
		return nil
	}
	ids := make([]pgtype.UUID, 0, len(entries))
	byID := make(map[pgtype.UUID]*product, len(entries))
	for i := range entries {
		id := entries[i].ID
		ids = append(ids, id)
		byID[id] = &entries[i]
	}

	countries, err := queryStructs[struct {
		Owner pgtype.UUID
		country
	}](ctx, db, `
        SELECT j.product_id, c.id, c.name, c.code, c.is_deleted, c.created_at, c.updated_at
        FROM country c JOIN product_country j ON j.country_id = c.id
        WHERE j.product_id = ANY($1) AND c.is_deleted IS NOT TRUE
    `, ids)
	if err != nil {
		return err
	}
	for _, row := range countries {
		entry := byID[row.Owner]
		entry.Countries = append(entry.Countries, row.country)
	}

	categories, err := queryStructs[struct {
		Owner pgtype.UUID
		catalogEntry
	}](ctx, db, `
        SELECT j.product_id, c.id, c.title, COALESCE(c.description, ''), c.is_deleted, c.created_at, c.updated_at
        FROM category c JOIN product_category j ON j.category_id = c.id
        WHERE j.product_id = ANY($1) AND c.is_deleted IS NOT TRUE
    `, ids)
	if err != nil {
		return err
	}
	for _, row := range categories {
		entry := byID[row.Owner]
		entry.Categories = append(entry.Categories, row.catalogEntry)
	}

	scopes, err := queryStructs[struct {
		Owner pgtype.UUID
		productTaxon
	}](ctx, db, `
        SELECT j.product_id, s.id, s.title, s.description, s.created_at, s.updated_at
        FROM product_scope s JOIN product_product_scope j ON j.product_scope_id = s.id
        WHERE j.product_id = ANY($1) AND s.is_deleted IS NOT TRUE
    `, ids)
	if err != nil {
		return err
	}
	for _, row := range scopes {
		entry := byID[row.Owner]
		entry.ProductScopes = append(entry.ProductScopes, row.productTaxon)
	}

	variants, err := queryStructs[variant](ctx, db, variantSelect+
		" WHERE product_id = ANY($1) ORDER BY volume ASC, is_default DESC", ids)
	if err != nil {
		return err
	}
	for _, item := range variants {
		entry := byID[item.ProductID]
		entry.Variants = append(entry.Variants, item)
	}
	codes, err := queryStructs[productCodeRow](ctx, db, `
        SELECT j.product_id, `+promoCodeColumns+`
        FROM promo_code pc JOIN product_promo_code j ON j.promo_code_id = pc.id
        WHERE j.product_id = ANY($1) AND `+activePromoCode, ids)
	if err != nil {
		return err
	}
	for _, row := range codes {
		entry := byID[row.Owner]
		entry.PromoCodes = append(entry.PromoCodes, row.promoCode)
	}
	return nil
}

func productsForDetails(ctx context.Context, db database, ids []pgtype.UUID, promos []promotion, includeDeleted bool) (map[pgtype.UUID]product, error) {
	query := productSelect + " WHERE p.id = ANY($1)"
	if !includeDeleted {
		query += " AND p.is_deleted IS NOT TRUE"
	}
	rows, err := db.Query(ctx, query, ids)
	if err != nil {
		return nil, err
	}
	entries, err := pgx.CollectRows(rows, scanProduct)
	if err != nil {
		return nil, err
	}
	if err := loadProductRelations(ctx, db, entries); err != nil {
		return nil, err
	}
	productsByID := make(map[pgtype.UUID]product, len(entries))
	for _, p := range entries {
		for _, promo := range promos {
			switch {
			case promo.TargetType == "all":
				p.Promotions = append(p.Promotions, promo)
			case slices.Contains(promo.ProductIDs, p.ID):
				promo.ProductIDs = []pgtype.UUID{p.ID}
				p.Promotions = append(p.Promotions, promo)
			}
		}
		productsByID[p.ID] = p
	}
	return productsByID, nil
}
