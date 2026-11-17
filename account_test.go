package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const testAccountID = "00000000-0000-4000-8000-000000000601"

func TestAccountSessionRejections(t *testing.T) {
	key := []byte("fixture-secret-no-production")
	handler := newHandler(nil, nil, &accountAuth{key: key})
	for _, tc := range []struct {
		name, message string
		cookie        *http.Cookie
	}{
		{"missing", "No token", nil},
		{"malformed", "Invalid token: InvalidToken", &http.Cookie{Name: "jwt", Value: "invalid"}},
		{"expired", "Invalid token: ExpiredSignature", legacyAccountCookie(t, jwt.SigningMethodHS256, key, time.Now().Add(-2*time.Minute))},
		{"missing expiration", "Invalid token: InvalidToken", legacyAccountCookie(t, jwt.SigningMethodHS256, key, time.Time{})},
		{"wrong key", "Invalid token: InvalidSignature", legacyAccountCookie(t, jwt.SigningMethodHS256, []byte("another-fixture-key"), time.Now().Add(time.Hour))},
		{"different algorithm", "Invalid token: InvalidSignature", legacyAccountCookie(t, jwt.SigningMethodHS384, key, time.Now().Add(time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := accountRequest(handler, http.MethodGet, "/api/check_auth", "", tc.cookie)
			if response.Code != http.StatusUnauthorized || response.Body.String() != tc.message {
				t.Fatalf("check_auth = %d %q, want 401 %q", response.Code, response.Body, tc.message)
			}
		})
	}
}

func TestAccountSessionRequiresAccountClaims(t *testing.T) {
	key := []byte("fixture-secret-no-production")
	handler := newHandler(nil, nil, &accountAuth{key: key})
	for _, tc := range []struct {
		name, message string
		claims        jwt.MapClaims
	}{
		{"guest session", "Invalid token: InvalidAudience", jwt.MapClaims{"aud": "skviderm-guest-checkout"}},
		{"missing roles and email", "Invalid token: InvalidToken", jwt.MapClaims{}},
		{"account with unexpected audience", "Invalid token: InvalidAudience", jwt.MapClaims{
			"aud": "other-service", "email": "test@example.invalid", "is_admin": false, "is_moderator": false,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.claims["id"] = testAccountID
			tc.claims["csrf_token"] = "fixture-csrf"
			tc.claims["exp"] = time.Now().Add(time.Hour).Unix()
			cookie := signAccountCookie(t, jwt.SigningMethodHS256, key, tc.claims)
			response := accountRequest(handler, http.MethodGet, "/api/check_auth", "", cookie)
			if response.Code != http.StatusUnauthorized || response.Body.String() != tc.message {
				t.Fatalf("check_auth = %d %q, want 401 %q", response.Code, response.Body, tc.message)
			}
		})
	}
}

func TestLoginRequestValidation(t *testing.T) {
	handler := newHandler(nil, nil, &accountAuth{key: []byte("test-only")})
	for _, body := range []string{
		`{"email":"test@example.invalid"}`,
		`{"Email":"test@example.invalid","password":"test-password"}`,
		`{"email":"test@example.invalid","password":null}`,
		`{"email":"test@example.invalid","password":"wrong","password":"test-password"}`,
		`{"email":"test@example.invalid","password":"test-password"} null`,
	} {
		response := accountRequest(handler, http.MethodPost, "/api/login", body, nil)
		if response.Code != http.StatusBadRequest {
			t.Errorf("login with %q = %d, want 400", body, response.Code)
		}
	}
}

func TestAccountContract(t *testing.T) {
	dsn := os.Getenv("SKVIDERM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SKVIDERM_TEST_DATABASE_URL to run the PostgreSQL contract tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test PostgreSQL:", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, name := range []string{"account_schema.sql", "account_rows.sql"} {
		sql, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	key := []byte("fixture-secret-no-production")
	for _, tc := range []struct {
		name, email, password string
		admin, secure         bool
		code                  int
	}{
		{"customer", "test@example.invalid", "test-password", false, true, 200},
		{"admin", "admin@example.invalid", "test-password", true, false, 200},
		{"legacy long password", "long@example.invalid", strings.Repeat("x", 100), false, true, 200},
		{"missing account", "missing@example.invalid", "test-password", false, true, 401},
		{"different case", "Test@example.invalid", "test-password", false, true, 401},
		{"wrong password", "test@example.invalid", "incorrect", false, true, 401},
		{"archived", "archived@example.invalid", "test-password", false, true, 401},
		{"phone account", "phone@example.invalid", "test-password", false, true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := newHandler(tx, nil, &accountAuth{db: tx, key: key, secureCookies: tc.secure})
			body, err := json.Marshal(map[string]any{"email": tc.email, "password": tc.password, "ignored": true})
			if err != nil {
				t.Fatal(err)
			}
			response := accountRequest(handler, http.MethodPost, "/api/login", string(body), nil)
			if response.Code != tc.code {
				t.Fatalf("login = %d %q, want %d", response.Code, response.Body, tc.code)
			}
			cookies := response.Result().Cookies()
			if tc.code == http.StatusUnauthorized {
				if len(cookies) != 0 || response.Body.String() != "Invalid email or password" {
					t.Fatalf("login failure returned cookies or unexpected body: %q", response.Body)
				}
				return
			}
			if len(cookies) != 1 {
				t.Fatal("login did not return one cookie")
			}
			cookie := cookies[0]
			if cookie.Name != "jwt" || !cookie.HttpOnly || cookie.Path != "/" || cookie.Secure != tc.secure ||
				(cookie.SameSite == http.SameSiteNoneMode) != tc.secure || cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
				t.Fatalf("unexpected jwt cookie attributes")
			}
			profile := accountPayload(t, response)
			claims := profile["claims"].(map[string]any)
			user := profile["user"].(map[string]any)
			if claims["is_admin"] != tc.admin || claims["is_moderator"] != tc.admin ||
				claims["phone_auth"] != false || user["isAdmin"] != tc.admin || user["email"] != tc.email ||
				claims["id"] != user["id"] || claims["email"] != user["email"] {
				t.Fatal("incorrect account profile or roles")
			}
			csrf, err := uuid.Parse(claims["csrf_token"].(string))
			if err != nil || csrf.Version() != 4 || time.Until(time.Unix(int64(claims["exp"].(float64)), 0)) < 23*time.Hour+59*time.Minute {
				t.Fatal("incorrect session CSRF value or expiration")
			}
			if got := accountPayload(t, accountRequest(handler, http.MethodGet, "/api/check_auth", "", cookie)); !reflect.DeepEqual(got, profile) {
				t.Fatal("session verification did not return the same claims and profile")
			}
		})
	}
	t.Run("session predating phone checkout", func(t *testing.T) {
		handler := newHandler(tx, nil, &accountAuth{db: tx, key: key})
		cookie := legacyAccountCookie(t, jwt.SigningMethodHS256, key, time.Now().Add(-20*time.Second))
		profile := accountPayload(t, accountRequest(handler, http.MethodGet, "/api/check_auth", "", cookie))
		claims := profile["claims"].(map[string]any)
		user := profile["user"].(map[string]any)
		if claims["phone_auth"] != false || user["createdAt"] != "2025-08-09T10:11:12.123450Z" || user["updatedAt"] != nil {
			t.Fatal("unexpected legacy profile or claims")
		}
	})
}

func legacyAccountCookie(t *testing.T, method jwt.SigningMethod, key []byte, expires time.Time) *http.Cookie {
	t.Helper()
	claims := jwt.MapClaims{
		"id": testAccountID, "email": "test@example.invalid", "is_admin": false,
		"is_moderator": false, "csrf_token": "fixture-csrf",
	}
	if !expires.IsZero() {
		claims["exp"] = expires.Unix()
	}
	return signAccountCookie(t, method, key, claims)
}

func signAccountCookie(t *testing.T, method jwt.SigningMethod, key []byte, claims jwt.MapClaims) *http.Cookie {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "jwt", Value: token}
}

func accountRequest(handler http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func accountPayload(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected account response: %d %q", response.Code, response.Body)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["user"].(map[string]any)["password"]; present {
		t.Fatal("account response exposed a password hash")
	}
	return payload
}
