package main

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const guestAudience = "skviderm-guest-checkout"
const guestCookieName = "guest_checkout"
const guestSessionExpired = "Не удалось открыть заказ. Свяжитесь с магазином — мы поможем."

const guestPersonWhere = ` WHERE id = $1 AND email IS NULL AND password IS NULL AND verified_phone IS NULL
	AND is_admin = false AND is_moderator = false AND is_deleted = false`

const guestOrderWhere = ` WHERE o.person_id = $1 AND o.status <> 'NotChecked' AND o.is_deleted = false
	AND EXISTS (SELECT 1 FROM person` + guestPersonWhere + ")"

type guestSessionClaims struct {
	jwt.RegisteredClaims
	ID        uuid.UUID `json:"id"`
	CSRFToken string    `json:"csrf_token"`
}

func (auth *accountAuth) registerCheckoutReads(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/checkout/attempt/{id}", auth.checkoutAttempt)
	if auth.guestOrigin != "" {
		mux.HandleFunc("GET /api/checkout/guest/session", auth.restoreGuest)
		mux.HandleFunc("POST /api/checkout/guest/session", auth.startGuestSession)
		mux.HandleFunc("GET /api/checkout/guest/orders", auth.guestOrders)
		mux.HandleFunc("GET /api/checkout/guest/orders/{id}", auth.guestOrder(false))
		mux.HandleFunc("GET /api/checkout/guest/attempt/{id}", auth.guestOrder(true))
	}
}

func (auth *accountAuth) validGuestOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	return len(origins) == 0 || (len(origins) == 1 && origins[0] == auth.guestOrigin)
}

func (auth *accountAuth) parseGuestSession(r *http.Request) (guestSessionClaims, bool) {
	var claims guestSessionClaims
	cookie, err := r.Cookie(guestCookieName)
	if err != nil {
		return claims, false
	}
	_, err = jwt.ParseWithClaims(cookie.Value, &claims, auth.verificationKey,
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired(), jwt.WithAudience(guestAudience))
	return claims, err == nil && claims.ID != uuid.Nil && claims.CSRFToken != "" && len(claims.Audience) == 1
}

func (auth *accountAuth) guestSession(w http.ResponseWriter, r *http.Request, requireCSRF bool) (guestSessionClaims, bool) {
	if !auth.validGuestOrigin(r) {
		writeCheckoutError(w, http.StatusUnauthorized, "Недопустимый источник запроса")
		return guestSessionClaims{}, false
	}
	claims, ok := auth.parseGuestSession(r)
	if !ok || (requireCSRF && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Guest-CSRF")), []byte(claims.CSRFToken)) != 1) {
		writeCheckoutError(w, http.StatusUnauthorized, guestSessionExpired)
		return claims, false
	}
	return claims, true
}

func (auth *accountAuth) restoreGuest(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.guestSession(w, r, false)
	if ok {
		auth.respondGuestSession(w, r, claims, false)
	}
}

func (auth *accountAuth) guestOrderOwner(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	claims, ok := auth.guestSession(w, r, true)
	if !ok {
		return uuid.Nil, false
	}
	var id uuid.UUID
	err := auth.db.QueryRow(r.Context(), "SELECT id FROM person"+guestPersonWhere, claims.ID).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeCheckoutError(w, http.StatusUnauthorized, guestSessionExpired)
	case err != nil:
		orderReadFailure(w)
	default:
		return id, true
	}
	return uuid.Nil, false
}

func (auth *accountAuth) guestOrders(w http.ResponseWriter, r *http.Request) {
	values, err := parseOrderListQuery(r.URL.RawQuery)
	if err != nil {
		writeQueryError(w, err)
		return
	}
	page, limit, err := guestOrderPage(values)
	if err != nil {
		writeQueryError(w, err)
		return
	}
	person, ok := auth.guestOrderOwner(w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	offset := int64(page-1) * int64(limit)
	entries, err := queryStructs[orderSummary](ctx, auth.db, orderSummarySelect+guestOrderWhere+
		" ORDER BY o.created_at DESC, o.id LIMIT $2 OFFSET $3", person, limit, offset)
	if err != nil {
		orderReadFailure(w)
		return
	}
	var count int
	if err := auth.db.QueryRow(ctx, `SELECT count(*) FROM "order" o`+guestOrderWhere, person).Scan(&count); err != nil {
		orderReadFailure(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, listResponse[orderSummary]{Status: "success", Data: entries, Length: count, Page: &page, Limit: &limit})
}

func guestOrderPage(values url.Values) (int32, int32, error) {
	var page, limit int32 = 1, 50
	var err error
	if values.Has("page") {
		page, err = parseQueryInt(values.Get("page"))
		if err != nil {
			return 0, 0, err
		}
	}
	if values.Has("limit") {
		limit, err = parseQueryInt(values.Get("limit"))
		if err != nil {
			return 0, 0, err
		}
	}
	if values.Has("includeDeleted") {
		if _, err := parseQueryBool(values.Get("includeDeleted")); err != nil {
			return 0, 0, err
		}
	}
	for _, field := range []string{"status", "paymentStatus", "totalPrice", "deliveryCost", "isCourierDelivery", "deliveryDate"} {
		if values.Has(field) {
			if _, _, err := orderFilterArgument(values, field); err != nil {
				return 0, 0, err
			}
		}
	}
	return max(1, page), min(100, max(1, limit)), nil
}

func (auth *accountAuth) guestOrder(attempt bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		person, ok := auth.guestOrderOwner(w, r)
		if !ok {
			return
		}
		where := guestOrderWhere + " AND o.id = $2"
		if attempt {
			where = guestOrderWhere + " AND o.checkout_key = $2"
		}
		auth.readCheckoutOrder(w, r, where, person, uuid.UUID(id.Bytes), !attempt)
	}
}

func (auth *accountAuth) checkoutAttempt(w http.ResponseWriter, r *http.Request) {
	key, ok := requestID(w, r)
	if !ok {
		return
	}
	claims, ok := auth.protectedSession(w, r)
	if !ok {
		return
	}
	auth.readCheckoutOrder(w, r, " WHERE o.person_id = $1 AND o.checkout_key = $2 AND o.is_deleted = false", uuid.UUID(claims.ID.Bytes), uuid.UUID(key.Bytes), false)
}

func (auth *accountAuth) readCheckoutOrder(w http.ResponseWriter, r *http.Request, where string, owner, id uuid.UUID, envelope bool) {
	ctx := r.Context()
	entries, err := queryStructs[order](ctx, auth.db, orderSelect+where, owner, id)
	if err != nil {
		orderReadFailure(w)
		return
	}
	if len(entries) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := loadOrderProducts(ctx, auth.db, entries); err != nil {
		orderReadFailure(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if envelope {
		writeJSON(w, singleResponse{Status: "success", Data: entries[0]})
	} else {
		writeJSON(w, entries[0])
	}
}
