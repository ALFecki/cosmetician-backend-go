package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	fromSQL string
	scan    func(pgx.Row) (any, error)
}

var brands = catalogResource{
	fromSQL: "SELECT id, title, description, is_deleted, created_at, updated_at, image, manufacturer, importer FROM brand",
	scan:    scanBrand,
}

var categories = catalogResource{
	fromSQL: "SELECT id, title, description, is_deleted, created_at, updated_at FROM category",
	scan:    scanCategory,
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

func listCatalog(ctx context.Context, db database, resource catalogResource, filter listFilter) ([]any, error) {
	query := resource.fromSQL + " WHERE TRUE"
	if !filter.includeDeleted {
		query += " AND (is_deleted IS NULL OR is_deleted = false)"
	}
	var args []any
	if filter.title != nil {
		args = append(args, "%"+*filter.title+"%")
		query += fmt.Sprintf(" AND title ILIKE $%d", len(args))
	}
	if filter.description != nil {
		args = append(args, "%"+*filter.description+"%")
		query += fmt.Sprintf(" AND description ILIKE $%d", len(args))
	}
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

func listBanners(ctx context.Context, db database) ([]banner, error) {
	const query = `
        SELECT id, title, subtitle, image, button_text, button_link, sort_order, is_active, created_at, updated_at
        FROM banner
        WHERE is_active
        ORDER BY sort_order, id
    `
	rows, err := db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]banner, 0)
	for rows.Next() {
		var value banner
		err := rows.Scan(&value.ID, &value.Title, &value.Subtitle, &value.Image, &value.ButtonText, &value.ButtonLink,
			&value.SortOrder, &value.IsActive, &value.CreatedAt, &value.UpdatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
