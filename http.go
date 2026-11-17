package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type listResponse[T any] struct {
	Status string `json:"status"`
	Length int    `json:"length"`
	Data   []T    `json:"data"`
	Page   *int32 `json:"page"`
	Limit  *int32 `json:"limit"`
}

type singleResponse struct {
	Status string `json:"status"`
	Data   any    `json:"data"`
}

func newHandler(db database) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/banner", func(w http.ResponseWriter, r *http.Request) {
		entries, err := listBanners(r.Context(), db)
		if err != nil {
			databaseFailure(w, "list banners", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, listResponse[banner]{Status: "success", Length: len(entries), Data: entries})
	})
	registerCatalog(mux, db, "/api/brand", brands)
	registerCatalog(mux, db, "/api/category", categories)
	return storefrontCORS(mux)
}

func registerCatalog(mux *http.ServeMux, db database, path string, resource catalogResource) {
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseListFilter(r.URL.RawQuery)
		if err != nil {
			writeText(w, http.StatusBadRequest, "Query deserialize error: "+err.Error())
			return
		}
		entries, err := listCatalog(r.Context(), db, resource, filter)
		if err != nil {
			databaseFailure(w, "list "+path, err)
			return
		}
		result := listResponse[any]{Status: "success", Length: len(entries), Data: entries}
		if filter.limit != nil {
			page, limit := int64(filter.page), int64(*filter.limit)
			start := min(int64(len(entries)), (page-1)*limit)
			end := min(int64(len(entries)), start+limit)
			result.Data = entries[start:end]
			result.Page, result.Limit = &filter.page, filter.limit
		}
		writeJSON(w, result)
	})
	mux.HandleFunc("GET "+path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"))
		if err != nil {
			writeText(w, http.StatusNotFound, "UUID parsing failed: "+err.Error())
			return
		}
		entry, err := resource.scan(db.QueryRow(r.Context(), resource.fromSQL+" WHERE id = $1", id))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			w.WriteHeader(http.StatusNotFound)
		case err != nil:
			databaseFailure(w, "get "+path, err)
		default:
			writeJSON(w, singleResponse{Status: "success", Data: entry})
		}
	})
}

type listFilter struct {
	title, description *string
	includeDeleted     bool
	page               int32
	limit              *int32
}

func parseListFilter(raw string) (listFilter, error) {
	filter := listFilter{page: 1}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return filter, errors.New("invalid query parameters")
	}
	for _, key := range []string{"id", "title", "description", "includeDeleted", "page", "limit"} {
		if len(values[key]) > 1 {
			return filter, fmt.Errorf("duplicate field `%s`", key)
		}
	}
	if values.Has("id") {
		return filter, fmt.Errorf("invalid type: string %q, expected a sequence", values.Get("id"))
	}
	if values.Has("title") {
		value := strings.ToLower(values.Get("title"))
		filter.title = &value
	}
	if values.Has("description") {
		value := strings.ToLower(values.Get("description"))
		filter.description = &value
	}
	if values.Has("includeDeleted") {
		switch values.Get("includeDeleted") {
		case "true":
			filter.includeDeleted = true
		case "false":
		default:
			return filter, errors.New("provided string was not `true` or `false`")
		}
	}
	if values.Has("page") {
		page, err := parseQueryInt(values.Get("page"))
		if err != nil {
			return filter, err
		}
		if page == 0 && values.Has("limit") {
			return filter, errors.New("page must be at least 1")
		}
		filter.page = max(1, page)
	}
	if values.Has("limit") {
		limit, err := parseQueryInt(values.Get("limit"))
		if err != nil {
			return filter, err
		}
		if limit < 0 {
			return filter, errors.New("limit must be non-negative")
		}
		filter.limit = &limit
	}
	return filter, nil
}

func parseQueryInt(value string) (int32, error) {
	number, err := strconv.ParseInt(value, 10, 32)
	switch {
	case err == nil:
		return int32(number), nil
	case errors.Is(err, strconv.ErrRange) && strings.HasPrefix(value, "-"):
		return 0, errors.New("number too small to fit in target type")
	case errors.Is(err, strconv.ErrRange):
		return 0, errors.New("number too large to fit in target type")
	case value == "":
		return 0, errors.New("cannot parse integer from empty string")
	default:
		return 0, errors.New("invalid digit found in string")
	}
}

func parseID(value string) (pgtype.UUID, error) {
	var id pgtype.UUID
	switch {
	case strings.HasPrefix(value, "urn:uuid:"):
		value = strings.TrimPrefix(value, "urn:uuid:")
		if len(value) != 36 {
			return id, errors.New("invalid length")
		}
	case strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}"):
		value = value[1 : len(value)-1]
		if len(value) != 36 {
			return id, errors.New("invalid length")
		}
	}
	for index, char := range value {
		if len(value) == 36 && (index == 8 || index == 13 || index == 18 || index == 23) {
			if char != '-' {
				return id, errors.New("invalid hyphen position")
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return id, fmt.Errorf("invalid character: found `%c` at %d", char, index)
		}
	}
	if err := id.Scan(value); err != nil {
		return id, errors.New("invalid length")
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Warn("write HTTP response", "error", err)
	}
}

func writeText(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(message))
}

func databaseFailure(w http.ResponseWriter, operation string, err error) {
	slog.Error("catalog query failed", "operation", operation, "error", err)
	writeText(w, http.StatusInternalServerError, "Не удалось загрузить данные каталога")
}
