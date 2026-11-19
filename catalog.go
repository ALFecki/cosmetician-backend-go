package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type database interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type apiTime struct {
	pgtype.Timestamptz
}

func (value apiTime) MarshalJSON() ([]byte, error) {
	if !value.Valid {
		return []byte("null"), nil
	}
	stamp := value.Time.UTC()
	layout := time.RFC3339
	switch {
	case stamp.Nanosecond()%1_000_000 != 0:
		layout = "2006-01-02T15:04:05.000000Z"
	case stamp.Nanosecond() != 0:
		layout = "2006-01-02T15:04:05.000Z"
	}
	return json.Marshal(stamp.Format(layout))
}

type catalogEntry struct {
	ID          pgtype.UUID `json:"id"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	IsDeleted   *bool       `json:"isDeleted"`
	CreatedAt   apiTime     `json:"createdAt"`
	UpdatedAt   apiTime     `json:"updatedAt"`
}

type brand struct {
	catalogEntry
	Image        *string `json:"image"`
	Manufacturer *string `json:"manufacturer"`
	Importer     *string `json:"importer"`
}

type banner struct {
	ID         pgtype.UUID `json:"id"`
	Title      string      `json:"title"`
	Subtitle   string      `json:"subtitle"`
	Image      string      `json:"image"`
	ButtonText string      `json:"buttonText"`
	ButtonLink string      `json:"buttonLink"`
	SortOrder  int32       `json:"sortOrder"`
	IsActive   bool        `json:"isActive"`
	CreatedAt  apiTime     `json:"createdAt"`
	UpdatedAt  apiTime     `json:"updatedAt"`
}

type catalogResource struct {
	fromSQL  string
	scan     func(pgx.Row) (any, error)
	filters  []string
	paginate bool
}

var titleFilters = []string{"title", "description"}
var taxonFilters = []string{"title", "description", "createdAt", "updatedAt"}

var brands = catalogResource{
	fromSQL:  "SELECT id, title, COALESCE(description, ''), is_deleted, created_at, updated_at, image, manufacturer, importer FROM brand",
	scan:     scanBrand,
	filters:  titleFilters,
	paginate: true,
}

var categories = catalogResource{
	fromSQL:  "SELECT id, title, COALESCE(description, ''), is_deleted, created_at, updated_at FROM category",
	scan:     scanCategory,
	filters:  titleFilters,
	paginate: true,
}

var countries = catalogResource{
	fromSQL:  "SELECT id, name, code, is_deleted, created_at, updated_at FROM country",
	scan:     scanCountry,
	filters:  []string{"name", "code"},
	paginate: true,
}

const taxonSelect = "SELECT id, title, description, created_at, updated_at FROM "

var productScopes = catalogResource{
	fromSQL: taxonSelect + "product_scope",
	scan:    scanProductTaxon,
	filters: taxonFilters,
}

var productTypes = catalogResource{
	fromSQL: taxonSelect + "product_type",
	scan:    scanProductTaxon,
	filters: taxonFilters,
}

func scanBrand(row pgx.Row) (any, error) {
	var value brand
	err := row.Scan(&value.ID, &value.Title, &value.Description, &value.IsDeleted, &value.CreatedAt, &value.UpdatedAt,
		&value.Image, &value.Manufacturer, &value.Importer)
	return value, err
}

func scanCategory(row pgx.Row) (any, error) {
	var value catalogEntry
	err := row.Scan(&value.ID, &value.Title, &value.Description, &value.IsDeleted, &value.CreatedAt, &value.UpdatedAt)
	return value, err
}

func scanCountry(row pgx.Row) (any, error) {
	var value country
	err := row.Scan(&value.ID, &value.Name, &value.Code, &value.IsDeleted, &value.CreatedAt, &value.UpdatedAt)
	return value, err
}

func scanProductTaxon(row pgx.Row) (any, error) {
	var value productTaxon
	err := row.Scan(&value.ID, &value.Title, &value.Description, &value.CreatedAt, &value.UpdatedAt)
	return value, err
}

func catalogListSQL(resource catalogResource, values url.Values, includeDeleted bool) (string, []any, error) {
	query := resource.fromSQL + " WHERE TRUE"
	if !includeDeleted {
		query += " AND (is_deleted IS NULL OR is_deleted = false)"
	}
	var args []any
	for _, key := range resource.filters {
		if !values.Has(key) {
			continue
		}
		column, value := key, values.Get(key)
		switch key {
		case "createdAt", "updatedAt":
			stamp, err := normalizeCatalogDate(value)
			if err != nil {
				return "", nil, err
			}
			value = stamp
			// Rust matches the serialized timestamp against PostgreSQL's text representation.
			column = "CAST(created_at AS TEXT)"
			if key == "updatedAt" {
				column = "CAST(updated_at AS TEXT)"
			}
		}
		args = append(args, "%"+strings.ToLower(value)+"%")
		query += fmt.Sprintf(" AND %s ILIKE $%d", column, len(args))
	}
	return query, args, nil
}

func normalizeCatalogDate(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) > 10 && value[10] == ' ' {
		value = value[:10] + "T" + value[11:]
	}
	if zone, found := strings.CutSuffix(value, "UTC"); found {
		value = strings.TrimSpace(zone) + "Z"
	}
	if strings.Contains(value, ",") {
		return "", errors.New("input contains invalid characters")
	}
	offset := strings.LastIndexAny(value, "+-")
	if offset > 10 && len(value)-offset == 5 {
		value = value[:offset+3] + ":" + value[offset+3:]
	}
	leapSecond := len(value) >= 19 && value[17:19] == "60"
	if leapSecond {
		value = value[:17] + "59" + value[19:]
	}
	stamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parseErr := err.(*time.ParseError)
		switch {
		case strings.Contains(parseErr.Message, "out of range"):
			return "", errors.New("input is out of range")
		case strings.HasPrefix(parseErr.Message, ": extra text:"):
			return "", errors.New("trailing input")
		case parseErr.ValueElem == "", strings.Trim(parseErr.ValueElem, "0123456789+-:") == "":
			return "", errors.New("premature end of input")
		default:
			return "", errors.New("input contains invalid characters")
		}
	}
	if offset > 10 && (value[offset+1:offset+3] > "23" || value[offset+4:] > "59") {
		return "", errors.New("input is out of range")
	}
	stamp = stamp.UTC()
	if leapSecond {
		return stamp.Format("2006-01-02T15:04:") + "60" + stamp.Format(".999999999Z"), nil
	}
	return stamp.Format(time.RFC3339Nano), nil
}

func listCatalog(ctx context.Context, db database, resource catalogResource, query string, args []any) ([]any, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]any, 0)
	for rows.Next() {
		entry, err := resource.scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

const bannerColumns = "id, title, subtitle, image, button_text, button_link, sort_order, is_active, created_at, updated_at"

func listBanners(ctx context.Context, db database, includeHidden, includeHome bool) ([]banner, error) {
	entries, err := queryStructs[banner](ctx, db, "SELECT "+bannerColumns+
		" FROM banner WHERE ($1 OR is_active OR id = $3) AND ($2 OR id <> $3) ORDER BY sort_order, id",
		includeHidden, includeHome, defaultHomeBanner.ID)
	if err != nil || !includeHome {
		return entries, err
	}
	for i, entry := range entries {
		if entry.ID == defaultHomeBanner.ID {
			if !includeHidden && !entry.IsActive {
				return slices.Delete(entries, i, i+1), nil
			}
			return entries, nil
		}
	}
	return append([]banner{defaultHomeBanner}, entries...), nil
}

func scanBanner(row pgx.Row) (banner, error) {
	var value banner
	err := row.Scan(&value.ID, &value.Title, &value.Subtitle, &value.Image, &value.ButtonText,
		&value.ButtonLink, &value.SortOrder, &value.IsActive, &value.CreatedAt, &value.UpdatedAt)
	return value, err
}
