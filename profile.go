package main

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (auth *accountAuth) protectedSession(w http.ResponseWriter, r *http.Request) (accountClaims, bool) {
	claims, err := auth.session(r)
	headers := r.Header.Values("X-CSRF-Token")
	switch {
	case err != nil:
		writeText(w, http.StatusUnauthorized, "Unauthorized")
	case len(headers) == 0:
		writeText(w, http.StatusUnauthorized, "CSRF token not found")
	case subtle.ConstantTimeCompare([]byte(headers[0]), []byte(claims.CSRFToken)) != 1:
		writeText(w, http.StatusUnauthorized, "Invalid CSRF token")
	default:
		return claims, true
	}
	return claims, false
}

func (auth *accountAuth) profile(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.protectedSession(w, r)
	if !ok {
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	if id != claims.ID && !*claims.IsAdmin {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	user, err := auth.accountByID(r.Context(), id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("read account profile failed")
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, singleResponse{Status: "success", Data: user})
}

func (auth *accountAuth) listProfiles(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.protectedSession(w, r)
	if !ok {
		return
	}
	if !*claims.IsAdmin {
		writeText(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	values, err := parseListQuery(r.URL.RawQuery, true, "first_name", "second_name", "surname", "email", "phone", "isEmailVerified")
	if err != nil {
		writeQueryError(w, err)
		return
	}
	filter, err := parseListFilter(values)
	if err != nil {
		writeQueryError(w, err)
		return
	}
	var verified *bool
	if values.Has("isEmailVerified") {
		value, err := parseQueryBool(values.Get("isEmailVerified"))
		if err != nil {
			writeQueryError(w, err)
			return
		}
		verified = &value
	}

	rows, err := auth.db.Query(r.Context(), accountSelect+`
		WHERE ($1 OR is_deleted IS NOT TRUE)
		  AND ($2::text IS NULL OR first_name ILIKE $2)
		  AND ($3::text IS NULL OR second_name ILIKE $3)
		  AND ($4::text IS NULL OR surname ILIKE $4)
		  AND ($5::text IS NULL OR email ILIKE $5)
		  AND ($6::text IS NULL OR phone ILIKE $6)
		  AND ($7::boolean IS NULL OR is_email_verified = $7)`,
		filter.includeDeleted, profilePattern(values, "first_name"), profilePattern(values, "second_name"),
		profilePattern(values, "surname"), profilePattern(values, "email"), profilePattern(values, "phone"), verified)
	if err != nil {
		profileListFailure(w)
		return
	}
	profiles, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (account, error) {
		return scanAccount(row)
	})
	if err != nil {
		profileListFailure(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, paginated(profiles, filter))
}

func profilePattern(values url.Values, field string) *string {
	if !values.Has(field) {
		return nil
	}
	pattern := "%" + strings.ToLower(values.Get(field)) + "%"
	return &pattern
}

func profileListFailure(w http.ResponseWriter) {
	slog.Error("read account list failed")
	writeText(w, http.StatusInternalServerError, "Could not read account list")
}
