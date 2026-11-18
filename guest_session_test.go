package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const guestStartPath = "/api/checkout/guest/session"
const guestContactFixture = `{"firstName":" Тест ","secondName":" Покупатель ","phone":"+375 (29) 000-00-17","email":" guest@example.invalid ","deliveryMethod":"self","paymentMethod":"cash","offerConsent":true}`

func TestGuestSessionDisabledAndOrigin(t *testing.T) {
	auth := &accountAuth{key: []byte("fixture-guest-session-key"), guestOrigin: "https://skviderm.by", cartDB: nil,
		confirmation: &checkoutConfirmation{guestEnabled: true}}
	for _, tc := range []struct {
		name, origin string
		enabled      bool
		status       int
	}{
		{"reject foreign origin", "https://outside.example.invalid", true, http.StatusUnauthorized},
		{"disabled", auth.guestOrigin, false, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth.confirmation.guestEnabled = tc.enabled
			handler := newHandler(nil, nil, auth, true)
			response := guestStartRequest(handler, guestContactFixture, tc.origin, nil)
			if response.Code != tc.status {
				t.Fatalf("guest session = %d %s, want %d", response.Code, response.Body, tc.status)
			}
			if len(response.Result().Cookies()) != 0 {
				t.Fatal("rejected request created a cookie")
			}
		})
	}
}

func TestGuestSessionIssueRenewAndLimit(t *testing.T) {
	redisURL := os.Getenv("SKVIDERM_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set SKVIDERM_TEST_REDIS_URL to an isolated Redis instance")
	}
	pool := testCartDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `ALTER TABLE person ALTER COLUMN id SET DEFAULT gen_random_uuid(),
		ALTER COLUMN is_deleted SET DEFAULT false, ALTER COLUMN created_at SET DEFAULT now()`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	limiter, err := newGuestOrderLimiter(redisURL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Close()
	hourKey, dayKey := "guest-checkout:sessions:hour:192.0.2.200", "guest-checkout:sessions:day:192.0.2.200"
	defer limiter.client.Del(context.Background(), hourKey, dayKey)
	key := []byte("fixture-guest-session-key")
	auth := &accountAuth{db: pool, key: key, cartDB: pool, guestSessions: limiter, guestOrigin: "https://skviderm.by", secureCookies: true,
		confirmation: &checkoutConfirmation{guestEnabled: true, guestLimiter: limiter}}
	handler := newHandler(pool, nil, auth, true)

	response := guestStartRequest(handler, `{"firstName":"A","secondName":"B","phone":"123","deliveryMethod":"self","paymentMethod":"cash","offerConsent":true}`, auth.guestOrigin, nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid phone = %d %s", response.Code, response.Body)
	}
	if err := limiter.client.Set(ctx, hourKey, 19, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := limiter.client.Set(ctx, dayKey, 49, 24*time.Hour).Err(); err != nil {
		t.Fatal(err)
	}

	response = guestStartRequest(handler, guestContactFixture, auth.guestOrigin, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("new guest = %d %s", response.Code, response.Body)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != guestCookieName || !cookie.Secure || !cookie.HttpOnly ||
		cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/api/checkout/guest" || cookie.MaxAge != 30*86400 {
		t.Fatal("guest cookie attributes differ from Rust")
	}
	var issued struct {
		CSRFToken string
		ExpiresAt int64
		Customer  account
	}
	if err := json.Unmarshal(response.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Customer.FirstName != "Тест" || issued.Customer.SecondName != "Покупатель" ||
		issued.Customer.Phone != "+375290000017" || issued.Customer.ContactEmail == nil ||
		*issued.Customer.ContactEmail != "guest@example.invalid" || issued.Customer.Email != nil ||
		issued.Customer.VerifiedPhone != nil || issued.CSRFToken == "" || issued.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("customer or session not normalized: %+v", issued)
	}
	var rustClaims struct {
		jwt.RegisteredClaims
		Audience  string    `json:"aud"`
		ID        uuid.UUID `json:"id"`
		CSRFToken string    `json:"csrf_token"`
	}
	token, err := jwt.ParseWithClaims(cookie.Value, &rustClaims, func(*jwt.Token) (any, error) { return key, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid || rustClaims.Audience != guestAudience || rustClaims.ID != issued.Customer.ID.Bytes ||
		rustClaims.CSRFToken != issued.CSRFToken {
		t.Fatalf("cookie must decode as legacy Rust claims: %v", err)
	}

	restored := accountRequest(handler, http.MethodGet, guestStartPath, "", cookie)
	if restored.Code != http.StatusOK || len(restored.Result().Cookies()) != 0 {
		t.Fatalf("restore = %d %s", restored.Code, restored.Body)
	}
	response = guestStartRequest(handler, guestContactFixture, auth.guestOrigin, cookie)
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 ||
		!strings.Contains(response.Body.String(), issued.CSRFToken) {
		t.Fatalf("renew = %d %s", response.Code, response.Body)
	}
	assertRedisCounter(t, ctx, limiter, dayKey, 50, 24*time.Hour)
	response = guestStartRequest(handler, guestContactFixture, auth.guestOrigin, nil)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit = %d %s", response.Code, response.Body)
	}
	var guests int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM person WHERE contact_email = $1", "guest@example.invalid").Scan(&guests); err != nil || guests != 1 {
		t.Fatalf("created %d guests, want 1: %v", guests, err)
	}
}

func guestStartRequest(handler http.Handler, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, guestStartPath, strings.NewReader(body))
	request.RemoteAddr = "192.0.2.200:1234"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
