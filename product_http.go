package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func registerProductRoutes(mux *http.ServeMux, db database) {
	mux.HandleFunc("GET /api/product", productListHandler(db))
	mux.HandleFunc("GET /api/product/{id}", productDetailHandler(db))
	mux.HandleFunc("GET /api/product_variant", variantListHandler(db))
	mux.HandleFunc("GET /api/product_variant/{id}", variantDetailHandler(db))
	registerPromotionRoutes(mux, db)
}

func productListHandler(db database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values, err := parseListQuery(r.URL.RawQuery,
			"title", "fullTitle", "description", "composition", "brandId", "categoryId", "countryId",
			"productScopeId", "productScope", "productType", "minPrice", "maxPrice", "minVolume", "maxVolume", "hasPromotion", "sort")
		if err != nil {
			writeQueryError(w, err)
			return
		}
		filter, err := parseListFilter(values)
		if err != nil {
			writeQueryError(w, err)
			return
		}
		query, args, err := productListSQL(values, filter)
		if err != nil {
			writeQueryError(w, err)
			return
		}
		ctx := r.Context()
		rows, err := db.Query(ctx, query, args...)
		if err != nil {
			databaseFailure(w, "list products", err)
			return
		}
		entries, err := pgx.CollectRows(rows, scanProduct)
		if err != nil {
			databaseFailure(w, "scan products", err)
			return
		}
		result := paginated(entries, filter)
		if err := loadProductRelations(ctx, db, result.Data, false); err != nil {
			databaseFailure(w, "load product list relations", err)
			return
		}
		writeJSON(w, result)
	}
}

func productListSQL(values url.Values, filter listFilter) (string, []any, error) {
	if values.Has("productScope") || values.Has("productType") {
		return "", nil, errors.New("unsupported product filter")
	}
	clauses := []string{"TRUE"}
	var args []any
	if !filter.includeDeleted {
		clauses = append(clauses, "p.is_deleted IS NOT TRUE")
	}
	for _, field := range []struct{ key, column string }{
		{"title", "p.title"}, {"fullTitle", "p.full_title"}, {"description", "p.description"},
	} {
		if values.Has(field.key) {
			args = append(args, "%"+strings.ToLower(values.Get(field.key))+"%")
			clauses = append(clauses, fmt.Sprintf("%s ILIKE $%d", field.column, len(args)))
		}
	}
	for _, field := range []struct{ key, condition string }{
		{"brandId", "p.brand_id = ANY($%d)"},
		{"categoryId", "EXISTS (SELECT 1 FROM product_category j WHERE j.product_id = p.id AND j.category_id = ANY($%d))"},
		{"countryId", "EXISTS (SELECT 1 FROM product_country j WHERE j.product_id = p.id AND j.country_id = ANY($%d))"},
		{"productScopeId", "EXISTS (SELECT 1 FROM product_product_scope j WHERE j.product_id = p.id AND j.product_scope_id = ANY($%d))"},
	} {
		if values.Has(field.key) {
			var ids []pgtype.UUID
			for item := range strings.SplitSeq(values.Get(field.key), ",") {
				id, err := parseID(strings.TrimSpace(item))
				if err != nil {
					return "", nil, fmt.Errorf("invalid %s", field.key)
				}
				ids = append(ids, id)
			}
			args = append(args, ids)
			clauses = append(clauses, fmt.Sprintf(field.condition, len(args)))
		}
	}
	// Each bound may match a different variant in the Rust catalog.
	for _, field := range []struct{ key, condition string }{
		{"minPrice", "v.price >= $%d"}, {"maxPrice", "v.price <= $%d"},
		{"minVolume", "v.volume >= $%d"}, {"maxVolume", "v.volume <= $%d"},
	} {
		number, err := queryFloat(values, field.key)
		if err != nil {
			return "", nil, err
		}
		if number != nil {
			args = append(args, *number)
			clauses = append(clauses, fmt.Sprintf(
				"EXISTS (SELECT 1 FROM product_variant v WHERE v.product_id = p.id AND "+field.condition+")", len(args)))
		}
	}
	if values.Has("hasPromotion") {
		switch values.Get("hasPromotion") {
		case "true", "false":
			args = append(args, values.Get("hasPromotion") == "true")
			clauses = append(clauses, fmt.Sprintf(`EXISTS (
                SELECT 1 FROM promotion promo LEFT JOIN product_promotion link ON link.promotion_id = promo.id
                WHERE (promo.target_type = 'all' OR link.product_id = p.id) AND `+activePromotion+`
            ) = $%d`, len(args)))
		default:
			return "", nil, errors.New("provided string was not `true` or `false`")
		}
	}
	return productSelect + " WHERE " + strings.Join(clauses, " AND ") + productOrder(values.Get("sort")), args, nil
}

func productOrder(sort string) string {
	var variantColumn string
	switch {
	case strings.Contains(sort, "price"):
		variantColumn = "price"
	case strings.Contains(sort, "volume"):
		variantColumn = "volume"
	}
	direction := "ASC"
	if variantColumn != "" {
		if strings.HasPrefix(sort, "-") {
			direction = "DESC"
		}
		return fmt.Sprintf(" ORDER BY (SELECT v.%s FROM product_variant v WHERE v.product_id = p.id ORDER BY v.is_default DESC, v.%s %s LIMIT 1) %s",
			variantColumn, variantColumn, direction, direction)
	}
	sort = strings.TrimSpace(sort)
	if field, descending := strings.CutPrefix(sort, "-"); descending {
		direction = "DESC"
		sort = field
	}
	switch sort {
	case "title", "created_at", "updated_at":
		return " ORDER BY p." + sort + " " + direction
	default:
		return ""
	}
}

func queryFloat(values url.Values, key string) (*float32, error) {
	if !values.Has(key) {
		return nil, nil
	}
	number, err := strconv.ParseFloat(values.Get(key), 32)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, errors.New("invalid float literal")
	}
	if math.IsInf(number, 0) || math.IsNaN(number) {
		return nil, nil
	}
	value := float32(number)
	return &value, nil
}

func productDetailHandler(db database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		rows, err := db.Query(ctx, productSelect+" WHERE p.id = $1", id)
		if err != nil {
			databaseFailure(w, "get product", err)
			return
		}
		entries, err := pgx.CollectRows(rows, scanProduct)
		if err != nil {
			databaseFailure(w, "scan product", err)
			return
		}
		if len(entries) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := loadProductRelations(ctx, db, entries, true); err != nil {
			databaseFailure(w, "load product details", err)
			return
		}
		writeJSON(w, singleResponse{Status: "success", Data: entries[0]})
	}
}

func variantListHandler(db database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values, err := parseListQuery(r.URL.RawQuery, "productId", "volume", "price")
		if err != nil {
			writeQueryError(w, err)
			return
		}
		filter, err := parseListFilter(values)
		if err != nil {
			writeQueryError(w, err)
			return
		}
		clauses := []string{"TRUE"}
		var args []any
		if !filter.includeDeleted {
			clauses = append(clauses, "is_deleted IS NOT TRUE")
		}
		if values.Has("productId") {
			id, err := parseID(values.Get("productId"))
			if err != nil {
				writeQueryError(w, errors.New("invalid productId"))
				return
			}
			args = append(args, id)
			clauses = append(clauses, "product_id = $1")
		}
		for _, key := range []string{"volume", "price"} {
			if !values.Has(key) {
				writeQueryError(w, fmt.Errorf("missing field `%s`", key))
				return
			}
			number, err := queryFloat(values, key)
			if err != nil {
				writeQueryError(w, err)
				return
			}
			if number != nil {
				args = append(args, *number)
				clauses = append(clauses, fmt.Sprintf("%s = $%d", key, len(args)))
			}
		}
		entries, err := queryStructs[variant](r.Context(), db, variantSelect+" WHERE "+strings.Join(clauses, " AND "), args...)
		if err != nil {
			databaseFailure(w, "list product variants", err)
			return
		}
		writeJSON(w, paginated(entries, filter))
	}
}

func variantDetailHandler(db database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		entries, err := queryStructs[variant](r.Context(), db, variantSelect+" WHERE id = $1", id)
		if err != nil {
			databaseFailure(w, "get product variant", err)
			return
		}
		if len(entries) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, singleResponse{Status: "success", Data: entries[0]})
	}
}

type promotionError struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func registerPromotionRoutes(mux *http.ServeMux, db database) {
	mux.HandleFunc("GET /api/product/{id}/promotions", func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		entries, err := productPromotions(r.Context(), db, id)
		if err != nil {
			promotionFailure(w, "Failed to get promotions", err)
			return
		}
		writeJSON(w, listResponse[promotion]{Status: "success", Length: len(entries), Data: entries})
	})
	mux.HandleFunc("GET /api/product/{id}/promo-codes", func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		entries, err := productPromoCodes(r.Context(), db, id)
		if err != nil {
			promotionFailure(w, "Failed to get promo codes", err)
			return
		}
		writeJSON(w, listResponse[promoCode]{Status: "success", Length: len(entries), Data: entries})
	})
	mux.HandleFunc("GET /api/promotion", func(w http.ResponseWriter, r *http.Request) {
		entries, err := queryStructs[promotion](r.Context(), db, promotionSelect+" GROUP BY promo.id")
		if err != nil {
			promotionFailure(w, "Failed to get promotions", err)
			return
		}
		writeJSON(w, listResponse[promotion]{Status: "success", Length: len(entries), Data: entries})
	})
	mux.HandleFunc("GET /api/promotion/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		entries, err := queryStructs[promotion](r.Context(), db, promotionSelect+" WHERE promo.id = $1 GROUP BY promo.id", id)
		if err != nil {
			promotionFailure(w, "Failed to get promotion", err)
			return
		}
		if len(entries) == 0 {
			writeJSONStatus(w, http.StatusNotFound, promotionError{Status: "error", Message: "Promotion not found"})
			return
		}
		writeJSON(w, singleResponse{Status: "success", Data: entries[0]})
	})
}

func promotionFailure(w http.ResponseWriter, message string, err error) {
	slog.Error("promotion query failed", "error", err)
	writeJSONStatus(w, http.StatusInternalServerError, promotionError{Status: "error", Message: message})
}
