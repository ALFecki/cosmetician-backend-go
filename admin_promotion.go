package main

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const adminPromotionRoute = "/api/admin/promotion"
const promotionProductLimit = 200
const promotionSearchLimit = 25

// The Rust schema requires finite, non-null timestamps even for inactive drafts.
var missingPromotionDate = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)

type adminPromotion struct {
	promotion
	IsDraft bool `json:"isDraft"`
}

const adminPromotionSelect = "SELECT " + promotionColumns + `,
	COALESCE(array_agg(link.product_id ORDER BY link.product_id) FILTER (WHERE link.product_id IS NOT NULL), '{}'::uuid[]),
	promo.is_draft
	FROM promotion promo LEFT JOIN product_promotion link ON link.promotion_id = promo.id
`

func (auth *accountAuth) registerAdminPromotions(mux *http.ServeMux) {
	detail := adminPromotionRoute + "/{id}"
	mux.HandleFunc("GET "+adminPromotionRoute, auth.requireModerator(auth.adminPromotionList))
	mux.HandleFunc("POST "+adminPromotionRoute, auth.requireModerator(auth.saveAdminPromotion))
	mux.HandleFunc("PUT "+detail, auth.requireModerator(auth.saveAdminPromotion))
	mux.HandleFunc("DELETE "+detail, auth.requireModerator(auth.deleteAdminPromotion))
	mux.HandleFunc("GET "+adminPromotionRoute+"/products", auth.requireModerator(auth.adminPromotionProducts))
}

func (auth *accountAuth) adminPromotionList(w http.ResponseWriter, r *http.Request) {
	entries, err := queryStructs[adminPromotion](r.Context(), auth.db, adminPromotionSelect+
		" GROUP BY promo.id ORDER BY promo.updated_at DESC NULLS LAST, promo.id")
	if err != nil {
		writeAdminPromotionError(w, err)
		return
	}
	for i := range entries {
		entries[i].restoreDraftDates()
	}
	writeJSON(w, listResponse[adminPromotion]{Status: "success", Data: entries, Length: len(entries)})
}

func getAdminPromotion(ctx context.Context, db database, id pgtype.UUID) (adminPromotion, error) {
	rows, err := db.Query(ctx, adminPromotionSelect+" WHERE promo.id = $1 GROUP BY promo.id", id)
	if err != nil {
		return adminPromotion{}, err
	}
	entry, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[adminPromotion])
	entry.restoreDraftDates()
	return entry, err
}

func (entry *adminPromotion) restoreDraftDates() {
	if entry.IsDraft {
		if entry.ValidFrom.Time.Equal(missingPromotionDate) {
			entry.ValidFrom = apiTime{}
		}
		if entry.ValidUntil.Time.Equal(missingPromotionDate) {
			entry.ValidUntil = apiTime{}
		}
	}
}

type promotionEditorInput struct {
	Title         *string        `json:"title"`
	Description   jsontext.Value `json:"description"`
	DiscountType  *string        `json:"discountType"`
	DiscountValue *float32       `json:"discountValue"`
	TargetType    *string        `json:"targetType"`
	ProductIDs    []pgtype.UUID  `json:"productIds"`
	ValidFrom     jsontext.Value `json:"validFrom"`
	ValidUntil    jsontext.Value `json:"validUntil"`
	IsDraft       *bool          `json:"isDraft"`
	IsActive      *bool          `json:"isActive"`
}

func (input promotionEditorInput) campaign(create bool) (adminPromotion, error) {
	var entry adminPromotion
	if input.IsDraft == nil || input.Title == nil || (!create || !*input.IsDraft) &&
		(input.DiscountType == nil || input.DiscountValue == nil || input.TargetType == nil || input.ProductIDs == nil ||
			len(input.ValidFrom) == 0 || len(input.ValidUntil) == 0 || input.IsActive == nil) {
		return entry, invalidPromotion("Заполните поля акции")
	}
	entry.Title = *input.Title
	entry.IsDraft = *input.IsDraft
	entry.DiscountType = "percentage"
	entry.TargetType = "all"
	if input.IsActive != nil {
		entry.IsActive = *input.IsActive
	}
	if input.DiscountType != nil {
		entry.DiscountType = *input.DiscountType
	}
	if input.DiscountValue != nil {
		entry.DiscountValue = *input.DiscountValue
	}
	if input.TargetType != nil {
		entry.TargetType = *input.TargetType
	}
	if len(input.Description) > 0 && jsonv2.Unmarshal(input.Description, &entry.Description) != nil {
		return entry, invalidPromotion("Описание должно быть строкой")
	}
	if len(input.ProductIDs) > promotionProductLimit {
		return entry, invalidPromotion("Выберите не больше 200 товаров")
	}
	for _, id := range input.ProductIDs {
		if !id.Valid {
			return entry, invalidPromotion("Некорректный идентификатор товара")
		}
	}
	slices.SortFunc(input.ProductIDs, func(a, b pgtype.UUID) int { return strings.Compare(a.String(), b.String()) })
	entry.ProductIDs = slices.Compact(input.ProductIDs)
	if !readPromotionDate(input.ValidFrom, &entry.ValidFrom) || !readPromotionDate(input.ValidUntil, &entry.ValidUntil) {
		return entry, invalidPromotion("Укажите корректные даты акции")
	}
	return entry, nil
}

func readPromotionDate(raw jsontext.Value, dest *apiTime) bool {
	if len(raw) == 0 || raw.Kind() == 'n' {
		return true
	}
	var stamp time.Time
	if jsonv2.Unmarshal(raw, &stamp) != nil || stamp.UTC().Year() < 2 || stamp.UTC().Year() > 9999 {
		return false
	}
	*dest = apiTime{pgtype.Timestamptz{Time: stamp.UTC().Truncate(time.Microsecond), Valid: true}}
	return true
}

func storedPromotionDate(stamp apiTime) time.Time {
	if stamp.Valid {
		return stamp.Time
	}
	return missingPromotionDate
}

func (entry adminPromotion) validate() error {
	switch {
	case strings.TrimSpace(entry.Title) == "" || utf8.RuneCountInString(entry.Title) > 255 || strings.ContainsRune(entry.Title, 0):
		return invalidPromotion("Укажите название акции до 255 символов")
	case entry.Description != nil && strings.ContainsRune(*entry.Description, 0):
		return invalidPromotion("Некорректное описание акции")
	case entry.TargetType != "all" && entry.TargetType != "product":
		return invalidPromotion("Выберите весь каталог или отдельные товары")
	case entry.TargetType == "product" && !entry.IsDraft && len(entry.ProductIDs) == 0:
		return invalidPromotion("Выберите хотя бы один товар")
	case entry.DiscountType != "percentage" && entry.DiscountType != "fixed":
		return invalidPromotion("Некорректный вид скидки")
	case entry.DiscountType == "percentage" && (entry.DiscountValue < 0 || entry.DiscountValue > 100):
		return invalidPromotion("Процент скидки должен быть от 0 до 100")
	case !entry.IsDraft && promotionSavingCents(maxCheckoutAmount*100, entry.DiscountType, entry.DiscountValue) == 0:
		return invalidPromotion("Укажите скидку больше нуля с точностью до сотых")
	case entry.IsDraft && entry.IsActive:
		return invalidPromotion("Черновик акции нельзя включить")
	case !entry.IsDraft && (!entry.ValidFrom.Valid || !entry.ValidUntil.Valid):
		return invalidPromotion("Укажите даты начала и окончания акции")
	case entry.ValidFrom.Valid && entry.ValidUntil.Valid && !entry.ValidFrom.Time.Before(entry.ValidUntil.Time):
		return invalidPromotion("Дата окончания должна быть позже даты начала акции")
	}
	if entry.DiscountType == "fixed" && entry.DiscountValue != 0 {
		_, err := checkoutCents(entry.DiscountValue)
		if err != nil || promotionSavingCents(1, "fixed", entry.DiscountValue) == 0 {
			return invalidPromotion("Укажите корректную сумму скидки в BYN")
		}
	}
	return nil
}

func (auth *accountAuth) saveAdminPromotion(w http.ResponseWriter, r *http.Request) {
	if !isJSONRequest(r) {
		writeText(w, http.StatusBadRequest, "Content type error")
		return
	}
	var input promotionEditorInput
	if err := jsonv2.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBodySize), &input); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeText(w, http.StatusRequestEntityTooLarge, "Promotion request is too large")
		} else {
			writeAdminPromotionError(w, invalidPromotion("Некорректные данные акции"))
		}
		return
	}
	create := r.Method == http.MethodPost
	entry, err := input.campaign(create)
	if err != nil {
		writeAdminPromotionError(w, err)
		return
	}
	var id pgtype.UUID
	if !create {
		var ok bool
		if id, ok = requestID(w, r); !ok {
			return
		}
	}

	var saved adminPromotion
	ctx := r.Context()
	err = pgx.BeginTxFunc(ctx, auth.promotionDB, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		if create {
			var err error
			id, err = insertAdminPromotion(ctx, tx, entry)
			if err != nil {
				return err
			}
		} else if err := updateAdminPromotion(ctx, tx, id, entry, len(input.Description) > 0); err != nil {
			return err
		}
		var err error
		saved, err = getAdminPromotion(ctx, tx, id)
		return err
	})
	if err != nil {
		writeAdminPromotionError(w, err)
		return
	}
	writeJSON(w, singleResponse{Status: "success", Data: saved})
}

func insertAdminPromotion(ctx context.Context, tx pgx.Tx, entry adminPromotion) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := entry.validate(); err != nil {
		return id, err
	}
	if entry.TargetType == "all" && len(entry.ProductIDs) != 0 {
		return id, invalidPromotion("Для всего каталога список товаров должен быть пустым")
	}
	err := tx.QueryRow(ctx, `INSERT INTO promotion (id, title, description, discount_type, discount_value,
		target_type, priority, valid_from, valid_until, is_active, is_draft, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3::text::discount_type, $4, $5::text::target_type, 0, $6, $7, $8, $9, now(), now())
		RETURNING id`, strings.TrimSpace(entry.Title), entry.Description, entry.DiscountType, entry.DiscountValue,
		entry.TargetType, storedPromotionDate(entry.ValidFrom), storedPromotionDate(entry.ValidUntil), entry.IsActive, entry.IsDraft).Scan(&id)
	if err == nil && len(entry.ProductIDs) > 0 {
		err = addAdminPromotionProducts(ctx, tx, id, entry.ProductIDs)
	}
	return id, err
}

func updateAdminPromotion(ctx context.Context, tx pgx.Tx, id pgtype.UUID, entry adminPromotion, changeDescription bool) error {
	if err := tx.QueryRow(ctx, `SELECT id FROM promotion WHERE id = $1 FOR UPDATE`, id).Scan(&id); err != nil {
		return err
	}
	current, err := getAdminPromotion(ctx, tx, id)
	if err != nil {
		return err
	}
	if !changeDescription {
		entry.Description = current.Description
	}
	if !entry.IsActive && !entry.IsDraft && !current.IsDraft && entry.sameCampaign(current) {
		_, err := tx.Exec(ctx, `UPDATE promotion SET is_active = FALSE, updated_at = now() WHERE id = $1`, id)
		return err
	}
	if current.TargetType != "all" && current.TargetType != "product" {
		return invalidPromotion("Старую акцию по бренду или категории можно только приостановить")
	}
	if err := entry.validate(); err != nil {
		return err
	}
	keepCatalogLinks := entry.TargetType == "all" && current.TargetType == "all"
	if entry.TargetType == "all" && len(entry.ProductIDs) > 0 &&
		(!keepCatalogLinks || !slices.Equal(entry.ProductIDs, current.ProductIDs)) {
		return invalidPromotion("Для всего каталога список товаров должен быть пустым")
	}
	_, err = tx.Exec(ctx, `UPDATE promotion SET title = $1, description = $2, discount_type = $3::text::discount_type,
		discount_value = $4, target_type = $5::text::target_type, valid_from = $6, valid_until = $7,
		is_active = $8, is_draft = $9, updated_at = now() WHERE id = $10`, strings.TrimSpace(entry.Title), entry.Description,
		entry.DiscountType, entry.DiscountValue, entry.TargetType, storedPromotionDate(entry.ValidFrom),
		storedPromotionDate(entry.ValidUntil), entry.IsActive, entry.IsDraft, id)
	if err != nil || keepCatalogLinks || slices.Equal(entry.ProductIDs, current.ProductIDs) {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM product_promotion WHERE promotion_id = $1`, id); err != nil {
		return err
	}
	return addAdminPromotionProducts(ctx, tx, id, entry.ProductIDs)
}

func (entry adminPromotion) sameCampaign(current adminPromotion) bool {
	if entry.Description != current.Description &&
		(entry.Description == nil || current.Description == nil || *entry.Description != *current.Description) {
		return false
	}
	return entry.Title == current.Title && entry.DiscountType == current.DiscountType && entry.DiscountValue == current.DiscountValue &&
		entry.TargetType == current.TargetType && entry.ValidFrom.Valid && entry.ValidUntil.Valid &&
		entry.ValidFrom.Time.Equal(current.ValidFrom.Time) && entry.ValidUntil.Time.Equal(current.ValidUntil.Time) &&
		slices.Equal(entry.ProductIDs, current.ProductIDs)
}

func addAdminPromotionProducts(ctx context.Context, tx pgx.Tx, id pgtype.UUID, products []pgtype.UUID) error {
	if len(products) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO product_promotion (promotion_id, product_id) SELECT $1, unnest($2::uuid[])`, id, products)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return invalidPromotion("Один из выбранных товаров не найден")
	}
	return err
}

func (auth *accountAuth) deleteAdminPromotion(w http.ResponseWriter, r *http.Request) {
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	err := pgx.BeginTxFunc(ctx, auth.promotionDB, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM promotion WHERE id = $1`, id)
		if err == nil && tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return err
	})
	if err != nil {
		writeAdminPromotionError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "success"})
}

type promotionSearchProduct struct {
	ID         pgtype.UUID `json:"id"`
	Title      string      `json:"title"`
	FullTitle  string      `json:"fullTitle"`
	BrandTitle string      `json:"brandTitle"`
	PriceCents *int64      `json:"priceCents"`
}

const promotionSearchSelect = `SELECT p.id, p.title, p.full_title, b.title, v.price
	FROM product p JOIN brand b ON b.id = p.brand_id
	LEFT JOIN LATERAL (
		SELECT min(price) AS price FROM product_variant
		WHERE product_id = p.id AND is_deleted IS NOT TRUE AND price >= 0
	) v ON p.is_deleted IS NOT TRUE`

func (auth *accountAuth) adminPromotionProducts(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["q"]) > 1 || len(query["ids"]) > 1 || query.Has("q") && query.Has("ids") {
		writeAdminPromotionError(w, invalidPromotion("Укажите строку поиска или список товаров"))
		return
	}
	var filter string
	var args []any
	if query.Has("ids") {
		ids, err := promotionSearchIDs(query.Get("ids"))
		if err != nil {
			writeAdminPromotionError(w, err)
			return
		}
		filter = ` WHERE p.id = ANY($1::uuid[]) ORDER BY array_position($1, p.id)`
		args = []any{ids}
	} else {
		q := strings.TrimSpace(query.Get("q"))
		if utf8.RuneCountInString(q) > 120 || !utf8.ValidString(q) || strings.ContainsRune(q, 0) {
			writeAdminPromotionError(w, invalidPromotion("Некорректная строка поиска"))
			return
		}
		filter = ` WHERE p.is_deleted IS NOT TRUE AND strpos(lower(concat_ws(' ', p.title, p.full_title, b.title)), lower($1)) > 0
			ORDER BY p.title, p.id LIMIT $2`
		args = []any{q, promotionSearchLimit}
	}
	rows, err := queryStructs[struct {
		ID         pgtype.UUID
		Title      string
		FullTitle  string
		BrandTitle string
		Price      *float32
	}](r.Context(), auth.db, promotionSearchSelect+filter, args...)
	if err != nil {
		writeAdminPromotionError(w, err)
		return
	}
	entries := make([]promotionSearchProduct, 0, len(rows))
	for _, row := range rows {
		entry := promotionSearchProduct{ID: row.ID, Title: row.Title, FullTitle: row.FullTitle, BrandTitle: row.BrandTitle}
		if row.Price != nil {
			if cents, err := checkoutCents(*row.Price); err == nil {
				entry.PriceCents = &cents
			}
		}
		entries = append(entries, entry)
	}
	writeJSON(w, listResponse[promotionSearchProduct]{Status: "success", Data: entries, Length: len(entries)})
}

func promotionSearchIDs(raw string) ([]pgtype.UUID, error) {
	if strings.Count(raw, ",") >= promotionProductLimit {
		return nil, invalidPromotion("Выберите не больше 200 товаров")
	}
	ids := make([]pgtype.UUID, 0, strings.Count(raw, ",")+1)
	if raw == "" {
		return ids, nil
	}
	for value := range strings.SplitSeq(raw, ",") {
		id, err := parseID(value)
		if err != nil {
			return nil, invalidPromotion("Некорректный идентификатор товара")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

type invalidPromotion string

func (err invalidPromotion) Error() string { return string(err) }

func writeAdminPromotionError(w http.ResponseWriter, err error) {
	var invalid invalidPromotion
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		w.WriteHeader(http.StatusNotFound)
	case errors.As(err, &invalid):
		writeCheckoutError(w, http.StatusBadRequest, invalid.Error())
	default:
		slog.Error("admin promotion query failed")
		writeCheckoutError(w, http.StatusInternalServerError, "Не удалось сохранить или загрузить акцию")
	}
}
