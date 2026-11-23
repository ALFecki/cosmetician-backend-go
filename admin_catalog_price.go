package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type catalogPriceSelection struct {
	BrandID           *uuid.UUID  `json:"brandId,omitempty"`
	ProductIDs        []uuid.UUID `json:"productIds,omitempty"`
	ChangeBasisPoints int32       `json:"changeBasisPoints"`
}

type catalogPriceApplyRequest struct {
	catalogPriceSelection
	OperationID uuid.UUID `json:"operationId"`
	Revision    string    `json:"revision"`
}

type catalogPriceItem struct {
	VariantID     uuid.UUID `json:"variantId"`
	ProductID     uuid.UUID `json:"productId"`
	Title         string    `json:"title"`
	Volume        float32   `json:"volume"`
	OldPriceCents int64     `json:"oldPriceCents"`
	NewPriceCents int64     `json:"newPriceCents" db:"-"`
}

type catalogPricePreview struct {
	OperationID  uuid.UUID          `json:"operationId"`
	Revision     string             `json:"revision"`
	Items        []catalogPriceItem `json:"items"`
	ChangedCount int                `json:"changedCount"`
}

type catalogPriceReceipt struct {
	UpdatedCount int `json:"updatedCount"`
}

const maxCatalogPriceCents = maxCheckoutAmount * 100

var errCatalogPriceRevision = errors.New("Цены изменились. Рассчитайте их заново")

func registerCatalogPrices(mux *http.ServeMux, auth *accountAuth, db cartDatabase) {
	const path = "/api/admin/catalog-prices"
	mux.HandleFunc("GET "+path, auth.requireCatalogAdmin(func(w http.ResponseWriter, r *http.Request) {
		writeCatalogResult(w, struct {
			Enabled bool `json:"enabled"`
		}{Enabled: true}, nil)
	}))
	mux.HandleFunc("POST "+path+"/preview", auth.requireCatalogAdmin(previewCatalogPrices(auth.db)))
	mux.HandleFunc("POST "+path+"/apply", auth.requireCatalogAdmin(applyCatalogPrices(db)))
}

func (input *catalogPriceSelection) normalize() error {
	switch {
	case (input.BrandID == nil) == (input.ProductIDs == nil):
		return quoteInputError("Выберите бренд или список товаров")
	case input.BrandID != nil && *input.BrandID == uuid.Nil:
		return quoteInputError("Некорректный идентификатор бренда")
	case input.ProductIDs != nil && (len(input.ProductIDs) == 0 || slices.Contains(input.ProductIDs, uuid.Nil)):
		return quoteInputError("Выберите хотя бы один товар с действительным идентификатором")
	case input.ChangeBasisPoints == 0 || input.ChangeBasisPoints < -10000:
		return quoteInputError("Укажите изменение цены не ниже −100% и отличное от нуля")
	}
	slices.SortFunc(input.ProductIDs, compareCatalogPriceIDs)
	input.ProductIDs = slices.Compact(input.ProductIDs)
	return nil
}

func compareCatalogPriceIDs(a, b uuid.UUID) int {
	return bytes.Compare(a[:], b[:])
}

func previewCatalogPrices(db database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		input, ok := readCatalogInput[catalogPriceSelection](w, r)
		if !ok {
			return
		}
		if err := input.normalize(); err != nil {
			writeCatalogPriceResult(w, nil, err)
			return
		}
		preview, err := calculateCatalogPrices(r.Context(), db, input, false)
		if err == nil {
			err = validateNewCatalogPrices(preview.Items)
		}
		preview.OperationID = uuid.New()
		writeCatalogPriceResult(w, preview, err)
	}
}

func applyCatalogPrices(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		input, ok := readCatalogInput[catalogPriceApplyRequest](w, r)
		if !ok {
			return
		}
		if err := input.normalize(); err != nil {
			writeCatalogPriceResult(w, nil, err)
			return
		}
		revision, err := hex.DecodeString(input.Revision)
		if err != nil || len(revision) != sha256.Size || input.OperationID == uuid.Nil {
			writeCatalogPriceResult(w, nil, quoteInputError("Некорректные данные предпросмотра"))
			return
		}
		ctx := r.Context()
		var result catalogPriceReceipt
		err = pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			result, err = persistCatalogPrices(ctx, tx, input)
			return err
		})
		writeCatalogPriceResult(w, result, err)
	}
}

func calculateCatalogPrices(ctx context.Context, db database, input catalogPriceSelection, lock bool) (catalogPricePreview, error) {
	query := `SELECT v.id, p.id, p.title, v.volume, v.price_cents, v.updated_at
		FROM product_variant v JOIN product p ON p.id = v.product_id
		WHERE v.is_deleted IS NOT TRUE AND p.is_deleted IS NOT TRUE
		  AND (p.brand_id = $1 OR p.id = ANY($2))
		ORDER BY v.id`
	if lock {
		query += " FOR NO KEY UPDATE OF v FOR SHARE OF p"
	}
	rows, err := queryStructs[struct {
		catalogPriceItem
		UpdatedAt apiTime
	}](ctx, db, query, input.BrandID, input.ProductIDs)
	if err != nil {
		return catalogPricePreview{}, err
	}

	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode(input); err != nil {
		return catalogPricePreview{}, err
	}
	preview := catalogPricePreview{Items: make([]catalogPriceItem, 0, len(rows))}
	for _, row := range rows {
		item := row.catalogPriceItem
		item.NewPriceCents = (item.OldPriceCents*(10000+int64(input.ChangeBasisPoints)) + 5000) / 10000
		if item.OldPriceCents != item.NewPriceCents {
			preview.ChangedCount++
		}
		preview.Items = append(preview.Items, item)
		if err := encoder.Encode(struct {
			VariantID, ProductID uuid.UUID
			OldCents, NewCents   int64
			UpdatedAt            apiTime
		}{item.VariantID, item.ProductID, item.OldPriceCents, item.NewPriceCents, row.UpdatedAt}); err != nil {
			return catalogPricePreview{}, err
		}
	}
	preview.Revision = hex.EncodeToString(hash.Sum(nil))
	return preview, nil
}

func validateNewCatalogPrices(items []catalogPriceItem) error {
	for _, item := range items {
		if item.NewPriceCents > maxCatalogPriceCents {
			return quoteInputError("Новая цена превышает 1 000 000 BYN")
		}
		price := float64(item.NewPriceCents) / 100
		legacy := float64(float32(price))
		visible, _ := strconv.ParseFloat(strconv.FormatFloat(legacy, 'f', -1, 32), 64)
		if price != visible || int64(math.Round(legacy*100)) != item.NewPriceCents {
			return quoteInputError("Новую цену нельзя сохранить с точностью до копейки")
		}
	}
	return nil
}

func persistCatalogPrices(ctx context.Context, tx pgx.Tx, input catalogPriceApplyRequest) (catalogPriceReceipt, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return catalogPriceReceipt{}, err
	}
	digest := sha256.Sum256(body)

	// Serialize retries even when the selector matches no variants.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 70901))", input.OperationID.String()); err != nil {
		return catalogPriceReceipt{}, err
	}
	var previousDigest []byte
	var receipt catalogPriceReceipt
	err = tx.QueryRow(ctx, "SELECT request_digest, updated_count FROM catalog_price_operation WHERE operation_id = $1", input.OperationID).
		Scan(&previousDigest, &receipt.UpdatedCount)
	switch {
	case err == nil:
		if !bytes.Equal(previousDigest, digest[:]) {
			return catalogPriceReceipt{}, errCatalogPriceRevision
		}
		return receipt, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return catalogPriceReceipt{}, err
	}

	preview, err := calculateCatalogPrices(ctx, tx, input.catalogPriceSelection, true)
	if err != nil {
		return catalogPriceReceipt{}, err
	}
	if preview.Revision != input.Revision {
		return catalogPriceReceipt{}, errCatalogPriceRevision
	}
	if err := validateNewCatalogPrices(preview.Items); err != nil {
		return catalogPriceReceipt{}, err
	}
	if preview.ChangedCount > 0 {
		ids := make([]uuid.UUID, 0, preview.ChangedCount)
		prices := make([]int64, 0, preview.ChangedCount)
		for _, item := range preview.Items {
			if item.NewPriceCents != item.OldPriceCents {
				ids = append(ids, item.VariantID)
				prices = append(prices, item.NewPriceCents)
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE product_variant v SET price_cents = c.price_cents
			FROM unnest($1::uuid[], $2::bigint[]) AS c(id, price_cents) WHERE v.id = c.id`, ids, prices)
		if err != nil {
			return catalogPriceReceipt{}, err
		}
		if tag.RowsAffected() != int64(preview.ChangedCount) {
			return catalogPriceReceipt{}, errCatalogPriceRevision
		}
	}
	_, err = tx.Exec(ctx, "INSERT INTO catalog_price_operation (operation_id, request_digest, updated_count) VALUES ($1, $2, $3)",
		input.OperationID, digest[:], preview.ChangedCount)
	return catalogPriceReceipt{UpdatedCount: preview.ChangedCount}, err
}

func writeCatalogPriceResult(w http.ResponseWriter, result any, err error) {
	var badInput quoteInputError
	switch {
	case errors.As(err, &badInput):
		writeText(w, http.StatusBadRequest, badInput.Error())
	case errors.Is(err, errCatalogPriceRevision):
		writeText(w, http.StatusConflict, err.Error())
	default:
		writeCatalogResult(w, result, err)
	}
}
