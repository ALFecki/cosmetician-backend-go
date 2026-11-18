package main

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type dictionaryWrite struct {
	table    string
	resource catalogResource
	admin    bool
}

type dictionaryInput struct {
	Title        *string `json:"title"`
	Description  *string `json:"description"`
	Image        *string `json:"image"`
	Manufacturer *string `json:"manufacturer"`
	Importer     *string `json:"importer"`
	Name         *string `json:"name"`
	Code         *string `json:"code"`
	IsDeleted    bool    `json:"isDeleted"`
}

func (auth *accountAuth) registerCatalogWrites(mux *http.ServeMux, db cartDatabase) {
	for _, dictionary := range []dictionaryWrite{
		{"brand", brands, false}, {"category", categories, false}, {"country", countries, false},
		{"product_type", productTypes, true}, {"product_scope", productScopes, true},
	} {
		path := "/api/" + dictionary.table
		guard := auth.requireModerator
		if dictionary.admin {
			guard = auth.requireCatalogAdmin
		}
		mux.HandleFunc("POST "+path, guard(dictionary.save(db)))
		mux.HandleFunc("PATCH "+path+"/{id}", guard(dictionary.save(db)))
		mux.HandleFunc("DELETE "+path+"/{id}", guard(deleteCatalogRow(db, dictionary.table, false)))
		mux.HandleFunc("DELETE "+path+"/cascade/{id}", auth.requireCatalogAdmin(deleteCatalogRow(db, dictionary.table, true)))
	}
	mux.HandleFunc("POST /api/product", auth.requireModerator(saveCatalogProduct(db)))
	mux.HandleFunc("PATCH /api/product/{id}", auth.requireModerator(saveCatalogProduct(db)))
	mux.HandleFunc("DELETE /api/product/{id}", auth.requireModerator(deleteCatalogRow(db, "product", false)))
	mux.HandleFunc("DELETE /api/product/cascade/{id}", auth.requireCatalogAdmin(deleteCatalogProductOrVariant(db, true)))
	mux.HandleFunc("POST /api/product_variant", auth.requireModerator(saveCatalogVariant(db)))
	mux.HandleFunc("PATCH /api/product_variant/{id}", auth.requireModerator(saveCatalogVariant(db)))
	mux.HandleFunc("DELETE /api/product_variant/{id}", auth.requireModerator(archiveCatalogVariant(db)))
	mux.HandleFunc("DELETE /api/product_variant/cascade/{id}", auth.requireCatalogAdmin(deleteCatalogProductOrVariant(db, false)))
}

func (auth *accountAuth) requireCatalogAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}
		if !*claims.IsAdmin {
			writeText(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}

func readCatalogInput[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var input T
	if !isJSONRequest(r) {
		writeText(w, http.StatusBadRequest, "Content type error")
		return input, false
	}
	var raw jsontext.Value
	err := jsonv2.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBodySize), &raw)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeText(w, http.StatusRequestEntityTooLarge, "Catalog request is too large")
		return input, false
	}
	if err != nil || raw.Kind() != '{' || jsonv2.Unmarshal(raw, &input) != nil {
		writeText(w, http.StatusBadRequest, "Invalid catalog JSON")
		return input, false
	}
	return input, true
}

func (dictionary dictionaryWrite) save(db cartDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		input, ok := readCatalogInput[dictionaryInput](w, r)
		if !ok {
			return
		}
		create := r.Method == http.MethodPost
		if create && (dictionary.table == "country" && (input.Name == nil || input.Code == nil) ||
			dictionary.table != "country" && input.Title == nil) {
			writeText(w, http.StatusBadRequest, "Отсутствуют обязательные поля")
			return
		}
		id := uuid.New()
		if !create {
			rowID, ok := requestID(w, r)
			if !ok {
				return
			}
			id = rowID.Bytes
		}

		query, args := dictionary.statement(id, input, create)
		var entry any
		ctx := r.Context()
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			var savedID uuid.UUID
			err := tx.QueryRow(ctx, query+" RETURNING id", args...).Scan(&savedID)
			if err != nil {
				return err
			}
			entry, err = dictionary.resource.scan(tx.QueryRow(ctx, dictionary.resource.fromSQL+" WHERE id = $1", id))
			return err
		})
		writeCatalogResult(w, entry, err)
	}
}

func (dictionary dictionaryWrite) statement(id uuid.UUID, input dictionaryInput, create bool) (string, []any) {
	table := dictionary.table
	if create {
		switch table {
		case "country":
			return "INSERT INTO country (id, name, code, is_deleted, created_at, updated_at) VALUES ($1, $2, $3, $4, now(), NULL)",
				[]any{id, input.Name, input.Code, input.IsDeleted}
		case "brand":
			return "INSERT INTO brand (id, title, description, image, manufacturer, importer, is_deleted, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, now(), NULL)",
				[]any{id, input.Title, input.Description, input.Image, input.Manufacturer, input.Importer, input.IsDeleted}
		default:
			return "INSERT INTO " + table + " (id, title, description, is_deleted, created_at, updated_at) VALUES ($1, $2, $3, $4, now(), NULL)",
				[]any{id, input.Title, input.Description, input.IsDeleted && !dictionary.admin}
		}
	}

	switch table {
	case "country":
		return "UPDATE country SET name = COALESCE($2, name), code = COALESCE($3, code), updated_at = now() WHERE id = $1",
			[]any{id, input.Name, input.Code}
	case "brand":
		return `UPDATE brand SET title = COALESCE($2, title), description = COALESCE($3, description),
			image = COALESCE($4, image), manufacturer = COALESCE($5, manufacturer), importer = COALESCE($6, importer), updated_at = now() WHERE id = $1`,
			[]any{id, input.Title, input.Description, input.Image, input.Manufacturer, input.Importer}
	default:
		return "UPDATE " + table + " SET title = COALESCE($2, title), description = COALESCE($3, description), updated_at = now() WHERE id = $1",
			[]any{id, input.Title, input.Description}
	}
}

func deleteCatalogRow(db cartDatabase, table string, purge bool) http.HandlerFunc {
	query := "UPDATE " + table + " SET is_deleted = true WHERE id = $1"
	if purge {
		query = "DELETE FROM " + table + " WHERE id = $1"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
			var savedID uuid.UUID
			err := tx.QueryRow(ctx, query+" RETURNING id", id).Scan(&savedID)
			var dbErr *pgconn.PgError
			if purge && errors.As(err, &dbErr) && dbErr.Code == "23503" {
				return errCatalogReference
			}
			return err
		})
		writeCatalogResult(w, nil, err)
	}
}

var errCatalogReference = errors.New("Запись используется в каталоге")

var errCatalogOrderReference = errors.New("Вариант используется в заказе. Его можно убрать в архив.")

func writeCatalogResult(w http.ResponseWriter, entry any, err error) {
	if err == nil {
		if entry == nil {
			w.WriteHeader(http.StatusOK)
		} else {
			writeJSON(w, singleResponse{Status: "success", Data: entry})
		}
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if errors.Is(err, errCatalogOrderReference) || errors.Is(err, errCatalogReference) {
		writeText(w, http.StatusConflict, err.Error())
		return
	}
	var dbErr *pgconn.PgError
	if errors.As(err, &dbErr) {
		switch dbErr.Code {
		case "23505":
			writeText(w, http.StatusConflict, "Такая запись уже существует")
			return
		case "23503", "23502", "22001", "23514":
			writeText(w, http.StatusBadRequest, "Проверьте значения и связанные записи")
			return
		}
		slog.Error("catalog write failed", "sqlstate", dbErr.Code)
	} else {
		slog.Error("catalog write failed")
	}
	writeText(w, http.StatusInternalServerError, "Не удалось сохранить изменения каталога")
}
