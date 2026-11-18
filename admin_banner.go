package main

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const adminBannerRoute = "/api/admin/banner"

type bannerInput struct {
	Title      *string `json:"title"`
	Subtitle   *string `json:"subtitle"`
	Image      *string `json:"image"`
	ButtonText *string `json:"buttonText"`
	ButtonLink *string `json:"buttonLink"`
	SortOrder  *int32  `json:"sortOrder"`
	IsActive   *bool   `json:"isActive"`
}

func (auth *accountAuth) registerAdminBanners(mux *http.ServeMux) {
	detail := adminBannerRoute + "/{id}"
	mux.HandleFunc("GET "+adminBannerRoute, auth.requireModerator(auth.adminBannerList))
	mux.HandleFunc("GET "+detail, auth.requireModerator(auth.adminBannerDetail))
	mux.HandleFunc("POST "+adminBannerRoute, auth.requireModerator(auth.saveAdminBanner))
	mux.HandleFunc("PATCH "+detail, auth.requireModerator(auth.saveAdminBanner))
	mux.HandleFunc("DELETE "+detail, auth.requireModerator(auth.deleteAdminBanner))
}

func (auth *accountAuth) requireModerator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}
		if !*claims.IsAdmin && !*claims.IsModerator {
			writeText(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}

func (auth *accountAuth) adminBannerList(w http.ResponseWriter, r *http.Request) {
	records, err := listBanners(r.Context(), auth.db, true)
	if err != nil {
		writeBannerError(w, err)
		return
	}
	writeJSON(w, listResponse[banner]{Status: "success", Length: len(records), Data: records})
}

func (auth *accountAuth) adminBannerDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	entry, err := scanBanner(auth.db.QueryRow(r.Context(), "SELECT "+bannerColumns+" FROM banner WHERE id = $1", id))
	writeBannerResult(w, entry, err)
}

func readBannerInput(w http.ResponseWriter, r *http.Request, create bool) (bannerInput, bool) {
	var fields bannerInput
	if !isJSONRequest(r) {
		writeText(w, http.StatusBadRequest, "Content type error")
		return fields, false
	}
	var raw jsontext.Value
	err := jsonv2.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBodySize), &raw)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeText(w, http.StatusRequestEntityTooLarge, "Banner request is too large")
		return fields, false
	}
	if err != nil || raw.Kind() != '{' || jsonv2.Unmarshal(raw, &fields) != nil {
		writeText(w, http.StatusBadRequest, "Invalid banner JSON")
		return fields, false
	}
	if create {
		var members map[string]jsontext.Value
		if err := jsonv2.Unmarshal(raw, &members); err != nil || fields.Image == nil {
			writeText(w, http.StatusBadRequest, "Invalid banner JSON")
			return fields, false
		}
		for _, name := range []string{"title", "subtitle", "buttonText", "buttonLink", "sortOrder", "isActive"} {
			if members[name].Kind() == 'n' {
				writeText(w, http.StatusBadRequest, "Invalid banner JSON")
				return fields, false
			}
		}
	}
	return fields, true
}

func (input bannerInput) apply(entry *banner) {
	setBannerField(&entry.Title, input.Title)
	setBannerField(&entry.Subtitle, input.Subtitle)
	setBannerField(&entry.Image, input.Image)
	setBannerField(&entry.ButtonText, input.ButtonText)
	setBannerField(&entry.ButtonLink, input.ButtonLink)
	setBannerField(&entry.SortOrder, input.SortOrder)
	setBannerField(&entry.IsActive, input.IsActive)
}

func setBannerField[T any](dest, value *T) {
	if value != nil {
		*dest = *value
	}
}

func (auth *accountAuth) saveAdminBanner(w http.ResponseWriter, r *http.Request) {
	create := r.Method == http.MethodPost
	input, ok := readBannerInput(w, r, create)
	if !ok {
		return
	}

	var entry banner
	var result banner
	ctx := r.Context()
	if create {
		input.apply(&entry)
		if err := validateBanner(entry); err != nil {
			writeBannerError(w, err)
			return
		}
		err := pgx.BeginTxFunc(ctx, auth.bannerDB, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			var err error
			result, err = scanBanner(tx.QueryRow(ctx, `INSERT INTO banner
				(title, subtitle, image, button_text, button_link, sort_order, is_active)
				VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+bannerColumns,
				entry.Title, entry.Subtitle, entry.Image, entry.ButtonText, entry.ButtonLink, entry.SortOrder, entry.IsActive))
			return err
		})
		writeBannerResult(w, result, err)
		return
	}

	id, ok := requestID(w, r)
	if !ok {
		return
	}
	err := pgx.BeginTxFunc(ctx, auth.bannerDB, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		entry, err := scanBanner(tx.QueryRow(ctx, "SELECT "+bannerColumns+" FROM banner WHERE id = $1 FOR UPDATE", id))
		if err != nil {
			return err
		}
		input.apply(&entry)
		if err := validateBanner(entry); err != nil {
			return err
		}
		result, err = scanBanner(tx.QueryRow(ctx, `UPDATE banner SET title = $1, subtitle = $2, image = $3,
			button_text = $4, button_link = $5, sort_order = $6, is_active = $7, updated_at = now()
			WHERE id = $8 RETURNING `+bannerColumns, entry.Title, entry.Subtitle, entry.Image, entry.ButtonText,
			entry.ButtonLink, entry.SortOrder, entry.IsActive, id))
		return err
	})
	writeBannerResult(w, result, err)
}

func (auth *accountAuth) deleteAdminBanner(w http.ResponseWriter, r *http.Request) {
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	err := pgx.BeginTxFunc(ctx, auth.bannerDB, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM banner WHERE id = $1", id)
		if err == nil && tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return err
	})
	if err != nil {
		writeBannerError(w, err)
		return
	}
	writeJSON(w, struct {
		Status string `json:"status"`
	}{"success"})
}

func writeBannerResult(w http.ResponseWriter, entry banner, err error) {
	if err != nil {
		writeBannerError(w, err)
		return
	}
	writeJSON(w, singleResponse{Status: "success", Data: entry})
}

type invalidBanner string

func (err invalidBanner) Error() string { return string(err) }

func writeBannerError(w http.ResponseWriter, err error) {
	var invalid invalidBanner
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		w.WriteHeader(http.StatusNotFound)
	case errors.As(err, &invalid):
		writeCheckoutError(w, http.StatusBadRequest, invalid.Error())
	default:
		slog.Error("banner query failed")
		writeCheckoutError(w, http.StatusInternalServerError, "Не удалось сохранить или загрузить баннер")
	}
}

func validateBanner(entry banner) error {
	for _, field := range []struct {
		label, value string
		max          int
	}{
		{"Заголовок", entry.Title, 255},
		{"Подпись", entry.Subtitle, 1000},
		{"Текст кнопки", entry.ButtonText, 100},
		{"Ссылка", entry.ButtonLink, 2048},
	} {
		if utf8.RuneCountInString(field.value) > field.max {
			return invalidBanner(fmt.Sprintf("%s: максимум %d символов", field.label, field.max))
		}
	}
	if entry.SortOrder < 0 || entry.SortOrder > 10000 {
		return invalidBanner("Порядок должен быть от 0 до 10000")
	}
	if !validBannerImage(entry.Image) {
		return invalidBanner("Выберите загруженное изображение")
	}
	if (strings.TrimSpace(entry.ButtonText) == "") != (entry.ButtonLink == "") {
		return invalidBanner("Укажите и текст, и ссылку кнопки либо очистите оба поля")
	}
	link := entry.ButtonLink
	if strings.ContainsFunc(link, func(r rune) bool { return r == '\\' || unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return invalidBanner("Некорректная ссылка")
	}
	if link == "" || (strings.HasPrefix(link, "/") && !strings.HasPrefix(link, "//")) {
		return nil
	}
	if !validBannerWebLink(link) {
		return invalidBanner("Допустим путь внутри сайта или ссылка http/https")
	}
	return nil
}

func validBannerWebLink(link string) bool {
	scheme, remainder, _ := strings.Cut(link, ":")
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	authority := strings.TrimLeft(remainder, "/")
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	// Rust accepts a literal percent sign in the path and stores the original link.
	parsed, err := url.Parse(scheme + "://" + authority)
	return err == nil && parsed.Hostname() != ""
}

func validBannerImage(image string) bool {
	if len(image) > 1024 {
		return false
	}
	path, ok := strings.CutPrefix(image, "/images/")
	if !ok {
		path, ok = strings.CutPrefix(image, "assets/")
	}
	if !ok {
		return false
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsFunc(part, invalidBannerPathCharacter) {
			return false
		}
	}
	return true
}

func invalidBannerPathCharacter(r rune) bool {
	return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-')
}
