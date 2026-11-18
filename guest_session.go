package main

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const guestSessionLifetime = 30 * 24 * time.Hour

type guestSessionResponse struct {
	CSRFToken string  `json:"csrfToken"`
	ExpiresAt int64   `json:"expiresAt"`
	Customer  account `json:"customer"`
}

func (auth *accountAuth) startGuestSession(w http.ResponseWriter, r *http.Request) {
	if !auth.validGuestOrigin(r) {
		writeCheckoutError(w, http.StatusUnauthorized, "Недопустимый источник запроса")
		return
	}
	if auth.confirmation == nil || !auth.confirmation.guestEnabled {
		writeCheckoutError(w, http.StatusServiceUnavailable, "Для нового заказа подтвердите номер телефона")
		return
	}
	if !isJSONRequest(r) {
		writeText(w, http.StatusBadRequest, "Content type error")
		return
	}
	var details struct {
		FirstName      *string `json:"firstName"`
		SecondName     *string `json:"secondName"`
		Email          *string `json:"email"`
		Phone          *string `json:"phone"`
		DeliveryMethod *string `json:"deliveryMethod"`
		PaymentMethod  *string `json:"paymentMethod"`
		OfferConsent   *bool   `json:"offerConsent"`
	}
	err := jsonv2.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBodySize), &details)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeText(w, http.StatusRequestEntityTooLarge, "Guest session request is too large")
		return
	case err != nil || details.FirstName == nil || details.SecondName == nil ||
		details.DeliveryMethod == nil || details.PaymentMethod == nil || details.OfferConsent == nil:
		writeText(w, http.StatusBadRequest, "Invalid guest session JSON")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if claims, ok := auth.parseGuestSession(r); ok {
		auth.respondGuestSession(w, r, claims, true)
		return
	}

	first, second, email, err := validateCheckoutContact(*details.FirstName, *details.SecondName, details.Email)
	if err != nil {
		writeCartError(w, err)
		return
	}
	var rawPhone string
	if details.Phone != nil {
		rawPhone = *details.Phone
	}
	phone, err := normalizeCheckoutPhone(rawPhone)
	if err != nil {
		writeCartError(w, err)
		return
	}
	if auth.guestSessions == nil {
		writeCheckoutError(w, http.StatusServiceUnavailable, "Оформление временно недоступно. Попробуйте позже")
		return
	}
	if err := auth.guestSessions.allowSession(ctx, r); err != nil {
		writeCartError(w, err)
		return
	}
	csrf, err := uuid.NewRandom()
	if err != nil {
		guestSessionWriteFailure(w)
		return
	}
	customer, err := insertGuestCustomer(ctx, auth.cartDB, first, second, email, phone)
	if err != nil {
		guestSessionWriteFailure(w)
		return
	}
	auth.writeGuestSession(w, guestSessionClaims{ID: customer.ID.Bytes, CSRFToken: csrf.String()}, customer, true)
}

func insertGuestCustomer(ctx context.Context, db cartDatabase, first, second string, email *string, phone string) (account, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return account{}, err
	}
	defer rollbackCart(ctx, tx)

	customer, err := scanAccount(tx.QueryRow(ctx, `INSERT INTO person
		(first_name, second_name, phone, contact_email, email, password, is_admin, is_moderator)
		VALUES ($1,$2,$3,$4,NULL,NULL,false,false)
		RETURNING `+accountColumns+", NULL::text", first, second, phone, email))
	if err != nil {
		return account{}, err
	}
	return customer, tx.Commit(ctx)
}

func (auth *accountAuth) respondGuestSession(w http.ResponseWriter, r *http.Request, claims guestSessionClaims, renew bool) {
	customer, err := scanAccount(auth.db.QueryRow(r.Context(), accountSelect+guestPersonWhere, claims.ID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeCheckoutError(w, http.StatusUnauthorized, guestSessionExpired)
	case err != nil:
		orderReadFailure(w)
	default:
		auth.writeGuestSession(w, claims, customer, renew)
	}
}

func (auth *accountAuth) writeGuestSession(w http.ResponseWriter, claims guestSessionClaims, customer account, renew bool) {
	if renew {
		claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(guestSessionLifetime))
		// Rust expects an audience string. The JWT package normally writes an array.
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"id": claims.ID, "csrf_token": claims.CSRFToken, "exp": claims.ExpiresAt.Unix(), "aud": guestAudience,
		}).SignedString(auth.key)
		if err != nil {
			guestSessionWriteFailure(w)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: guestCookieName, Value: token, Path: "/api/checkout/guest", MaxAge: int(guestSessionLifetime.Seconds()),
			Secure: auth.secureCookies, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, guestSessionResponse{CSRFToken: claims.CSRFToken, ExpiresAt: claims.ExpiresAt.Unix(), Customer: customer})
}

func guestSessionWriteFailure(w http.ResponseWriter) {
	slog.Error("guest session creation failed")
	writeCheckoutError(w, http.StatusInternalServerError, "Не удалось начать оформление. Попробуйте ещё раз.")
}
