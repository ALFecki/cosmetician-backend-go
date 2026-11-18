package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type catalogProductInput struct {
	Title         *string         `json:"title"`
	FullTitle     *string         `json:"fullTitle"`
	Description   *string         `json:"description"`
	Image         *string         `json:"image"`
	MethodOfUse   *string         `json:"methodOfUse"`
	Composition   *string         `json:"composition"`
	BrandID       *pgtype.UUID    `json:"brandId"`
	ProductTypeID *pgtype.UUID    `json:"productTypeId"`
	CategoryIDs   *catalogLinkIDs `json:"categoryIds"`
	CountryIDs    *catalogLinkIDs `json:"countryIds"`
	ScopeIDs      *catalogLinkIDs `json:"productScopeIds"`
	IsPriceHidden *bool           `json:"isPriceHidden"`
	IsDeleted     *bool           `json:"isDeleted"`
}

type catalogLinkIDs []pgtype.UUID

func (ids *catalogLinkIDs) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var encoded string
		if err := json.Unmarshal(data, &encoded); err != nil {
			return err
		}
		if id, err := parseID(encoded); err == nil {
			*ids = []pgtype.UUID{id}
			return nil
		}
		data = []byte(encoded)
	}
	var values []pgtype.UUID
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	if values == nil {
		return fmt.Errorf("product links must be an array")
	}
	seen := make(map[pgtype.UUID]struct{}, len(values))
	for _, id := range values {
		if _, exists := seen[id]; exists || !id.Valid {
			return fmt.Errorf("invalid or repeated product link")
		}
		seen[id] = struct{}{}
	}
	*ids = values
	return nil
}

func saveCatalogProduct(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		input, ok := readCatalogInput[catalogProductInput](w, r)
		if !ok {
			return
		}
		create := r.Method == http.MethodPost
		var id pgtype.UUID
		if create {
			if input.Title == nil || input.BrandID == nil || input.ProductTypeID == nil || input.Description == nil ||
				input.IsPriceHidden == nil || input.CategoryIDs == nil || input.ScopeIDs == nil || input.CountryIDs == nil {
				writeText(w, http.StatusBadRequest, "Отсутствуют обязательные поля товара")
				return
			}
			id = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		} else {
			id, ok = requestID(w, r)
			if !ok {
				return
			}
		}

		ctx := r.Context()
		var result product
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			if err := saveProductFields(ctx, tx, id, input, create); err != nil {
				return err
			}
			if err := saveProductLinks(ctx, tx, id, input); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, productSelect+" WHERE p.id = $1", id)
			if err != nil {
				return err
			}
			result, err = pgx.CollectExactlyOneRow(rows, scanProduct)
			if err != nil {
				return err
			}
			entries := []product{result}
			if err := loadProductRelations(ctx, tx, entries); err != nil {
				return err
			}
			entries[0].Promotions, err = productPromotions(ctx, tx, id)
			if err != nil {
				return err
			}
			if promotionPricingEnabled(ctx) {
				applyProductPromotionPrices(entries)
			}
			result = entries[0]
			return nil
		})
		writeCatalogResult(w, result, err)
	}
}

func saveProductFields(ctx context.Context, tx pgx.Tx, id pgtype.UUID, input catalogProductInput, create bool) error {
	query := `UPDATE product SET title = COALESCE($2, title), full_title = COALESCE($3, full_title),
		description = COALESCE($4, description), image = COALESCE($5, image), method_of_use = COALESCE($6, method_of_use),
		composition = COALESCE($7, composition), brand_id = COALESCE($8, brand_id),
		product_type_id = COALESCE($9, product_type_id), is_price_hidden = COALESCE($10, is_price_hidden),
		is_deleted = COALESCE($11, is_deleted), updated_at = now() WHERE id = $1 RETURNING id`
	if create {
		query = `INSERT INTO product (id, title, full_title, description, image, method_of_use, composition,
			brand_id, product_type_id, is_price_hidden, is_deleted, created_at, updated_at)
			VALUES ($1, $2::varchar, COALESCE($3::varchar, $2::varchar), $4, $5, $6, $7, $8, $9, $10, COALESCE($11, false), now(), NULL) RETURNING id`
	}
	var savedID uuid.UUID
	return tx.QueryRow(ctx, query, id, input.Title, input.FullTitle, input.Description, input.Image,
		input.MethodOfUse, input.Composition, input.BrandID, input.ProductTypeID,
		input.IsPriceHidden, input.IsDeleted).Scan(&savedID)
}

func saveProductLinks(ctx context.Context, tx pgx.Tx, id pgtype.UUID, input catalogProductInput) error {
	for _, links := range []struct {
		ids    *catalogLinkIDs
		table  string
		column string
	}{
		{input.CategoryIDs, "product_category", "category_id"},
		{input.CountryIDs, "product_country", "country_id"},
		{input.ScopeIDs, "product_product_scope", "product_scope_id"},
	} {
		if links.ids == nil {
			continue
		}
		if _, err := tx.Exec(ctx, "DELETE FROM "+links.table+" WHERE product_id = $1", id); err != nil {
			return err
		}
		if len(*links.ids) == 0 {
			continue
		}
		query := "INSERT INTO " + links.table + " (id, product_id, " + links.column + ") SELECT gen_random_uuid(), $1, id FROM unnest($2::uuid[]) AS id"
		if _, err := tx.Exec(ctx, query, id, []pgtype.UUID(*links.ids)); err != nil {
			return err
		}
	}
	return nil
}

type catalogVariantInput struct {
	ProductID *pgtype.UUID `json:"productId"`
	Volume    *float32     `json:"volume"`
	CostPrice *float32     `json:"costPrice"`
	Price     *float32     `json:"price"`
	IsDefault *bool        `json:"isDefault"`
	IsDeleted *bool        `json:"isDeleted"`
}

func saveCatalogVariant(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		input, ok := readCatalogInput[catalogVariantInput](w, r)
		if !ok {
			return
		}
		create := r.Method == http.MethodPost
		if create && (input.ProductID == nil || input.Volume == nil || input.CostPrice == nil || input.Price == nil) {
			writeText(w, http.StatusBadRequest, "Отсутствуют обязательные поля варианта")
			return
		}
		for _, value := range []*float32{input.Volume, input.CostPrice, input.Price} {
			if value != nil && *value < 0 {
				writeText(w, http.StatusBadRequest, "Объём и цены не могут быть отрицательными")
				return
			}
		}

		var id pgtype.UUID
		if create {
			id = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		} else {
			id, ok = requestID(w, r)
			if !ok {
				return
			}
		}
		query, args := variantWriteStatement(id, input, create)
		ctx := r.Context()
		var result variant
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			var savedID uuid.UUID
			if err := tx.QueryRow(ctx, query, args...).Scan(&savedID); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, variantSelect+" WHERE id = $1", id)
			if err != nil {
				return err
			}
			result, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[variant])
			return err
		})
		writeCatalogResult(w, result, err)
	}
}

func variantWriteStatement(id pgtype.UUID, input catalogVariantInput, create bool) (string, []any) {
	if create {
		return `INSERT INTO product_variant (id, product_id, volume, cost_price, price,
			is_default, is_deleted, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, COALESCE($6, false), COALESCE($7, false), now(), NULL) RETURNING id`,
			[]any{id, input.ProductID, input.Volume, input.CostPrice, input.Price, input.IsDefault, input.IsDeleted}
	}
	var priceUpdates []string
	args := []any{id, input.ProductID, input.Volume, input.IsDefault}
	if input.CostPrice != nil {
		args = append(args, *input.CostPrice)
		priceUpdates = append(priceUpdates, fmt.Sprintf("cost_price = $%d, ", len(args)))
	}
	if input.Price != nil {
		args = append(args, *input.Price)
		priceUpdates = append(priceUpdates, fmt.Sprintf("price = $%d, ", len(args)))
	}
	return `UPDATE product_variant SET product_id = COALESCE($2, product_id), volume = COALESCE($3, volume),
		is_default = COALESCE($4, is_default), ` + strings.Join(priceUpdates, "") + "updated_at = now() WHERE id = $1 RETURNING id", args
}

func archiveCatalogVariant(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		var entry variant
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			var savedID uuid.UUID
			if err := tx.QueryRow(ctx, "UPDATE product_variant SET is_deleted = true WHERE id = $1 RETURNING id", id).Scan(&savedID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE product_variant_price_history SET valid_to = now(), updated_at = now()
				WHERE variant_id = $1 AND valid_to IS NULL`, id); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, variantSelect+" WHERE id = $1", id)
			if err != nil {
				return err
			}
			entry, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[variant])
			return err
		})
		writeCatalogResult(w, entry, err)
	}
}

func deleteCatalogProductOrVariant(db cartDatabase, wholeProduct bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			if wholeProduct {
				var productID uuid.UUID
				if err := tx.QueryRow(ctx, "SELECT id FROM product WHERE id = $1 FOR UPDATE", id).Scan(&productID); err != nil {
					return err
				}
			}
			whereColumn := "id"
			if wholeProduct {
				whereColumn = "product_id"
			}
			// Keep checkout inserts from racing the reference check and the cascading DELETE.
			rows, err := tx.Query(ctx, "SELECT id FROM product_variant WHERE "+whereColumn+" = $1 ORDER BY id FOR UPDATE", id)
			if err != nil {
				return err
			}
			variantIDs, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
			if err != nil {
				return err
			}
			if len(variantIDs) == 0 && !wholeProduct {
				return pgx.ErrNoRows
			}
			var used bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM order_product WHERE variant_id = ANY($1))", variantIDs).Scan(&used); err != nil {
				return err
			}
			if used {
				return errCatalogOrderReference
			}
			table := "product_variant"
			if wholeProduct {
				table = "product"
			}
			_, err = tx.Exec(ctx, "DELETE FROM "+table+" WHERE id = $1", id)
			return err
		})
		writeCatalogResult(w, nil, err)
	}
}
