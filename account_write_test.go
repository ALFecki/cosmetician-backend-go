package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

const testAccountWriteKey = "fixture-only-account-writes-key"

func accountWriteHandler(writes *accountWrites) http.Handler {
	writes.accountAuth.writes = writes
	return newHandler(writes.db, nil, writes.accountAuth, writes.checkoutEnabled)
}

func TestPortedAccountPhoneConfig(t *testing.T) {
	account := &accountAuth{key: []byte(testAccountWriteKey)}
	handler := accountWriteHandler(&accountWrites{accountAuth: account, checkoutEnabled: true, guestEnabled: true})
	response := accountRequest(handler, http.MethodGet, "/api/auth/phone/config", "", nil)
	var config map[string]bool
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &config) != nil ||
		!config["enabled"] || config["phoneLoginAvailable"] || !config["guestCheckoutAvailable"] ||
		response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("phone config = %d %q", response.Code, response.Body)
	}
	phone := accountRequest(handler, http.MethodPost, "/api/auth/phone/start", `{"phone":"+375290004201","purpose":"login"}`, nil)
	assertCartStatus(t, phone, http.StatusServiceUnavailable)
	if len(phone.Result().Cookies()) != 0 {
		t.Fatal("disabled route returned a cookie")
	}
}

func TestPortedAccountRegistrationAndProfile(t *testing.T) {
	writes := accountWritesFixture(t)
	handler := accountWriteHandler(writes)
	registered := accountRequest(handler, http.MethodPost, "/api/register", `{
		"firstName":"Тест","secondName":"Новый","email":"new@example.invalid","phone":"+375290004201",
		"password":"registration-fixture","isAdmin":true,"isModerator":true,"isEmailVerified":true}`, nil)
	parsed := accountPayload(t, registered)
	if user := parsed["user"].(map[string]any); user["isAdmin"] != false || user["isModerator"] != false ||
		user["isEmailVerified"] != false || user["password"] != nil {
		t.Fatal("registration leaked credentials or honored privileged fields")
	}
	cookie := registered.Result().Cookies()[0]
	claims := parsed["claims"].(map[string]any)
	token := claims["csrf_token"].(string)
	personPath := "/api/person/" + claims["id"].(string)
	assertCartStatus(t, accountRequest(handler, http.MethodPost, "/api/login", `{"email":"new@example.invalid","password":"registration-fixture"}`, nil), http.StatusOK)
	assertCartStatus(t, accountWriteRequest(handler, http.MethodPatch, personPath, `{"isEmailVerified":true}`, token, cookie), http.StatusBadRequest)
	assertCartStatus(t, accountWriteRequest(handler, http.MethodPatch, personPath, `null`, token, cookie), http.StatusBadRequest)
	assertCartStatus(t, accountWriteRequest(handler, http.MethodPatch, "/api/person/"+testAdminID, `{"firstName":"Новый"}`, token, cookie), http.StatusForbidden)
	assertCartStatus(t, accountWriteRequest(handler, http.MethodPatch, personPath, `{"firstName":"Обновлён","email":"new2@example.invalid"}`, token, cookie), http.StatusOK)
	assertCartStatus(t, accountWriteRequest(handler, http.MethodPatch, "/api/checkout/profile", `{"firstName":"Новый","secondName":"Профиль"}`, token, cookie), http.StatusBadRequest)
	admin := profileCookie(t, writes.key, testAdminID, true, true, false)
	assertCartStatus(t, accountWriteRequest(handler, http.MethodDelete, personPath, "", "fixture-csrf", admin), http.StatusOK)
	assertCartStatus(t, accountRequest(handler, http.MethodPost, "/api/login", `{"email":"new2@example.invalid","password":"registration-fixture"}`, nil), http.StatusUnauthorized)
}

func TestPortedAccountEmailTokens(t *testing.T) {
	writes := accountWritesFixture(t)
	writes.redis = testAccountRedis(t)
	mailer := &testAccountMail{}
	writes.mailer = mailer
	handler := accountWriteHandler(writes)
	email := "test@example.invalid"
	defer cleanupAccountRedis(t, writes.redis, accountTokenKey(email, resetPasswordToken), accountTokenKey(email, resetPasswordToken)+":send",
		accountTokenKey(email, verifyEmailToken), accountTokenKey(email, verifyEmailToken)+":send", accountTokenKey(email, verifyEmailToken)+":attempts")
	query := url.Values{"email": {email}}
	requestReset := "/api/forgot_password?" + query.Encode()
	assertCartStatus(t, accountRequest(handler, http.MethodGet, requestReset, "", nil), http.StatusOK)
	if mailer.recipient != email || !strings.Contains(mailer.body, writes.guestOrigin+"/reset-password?") {
		t.Fatal("reset did not send a link to the account")
	}
	assertCartStatus(t, accountRequest(handler, http.MethodGet, requestReset, "", nil), http.StatusTooManyRequests)
	token, err := writes.redis.Get(t.Context(), accountTokenKey(email, resetPasswordToken)).Result()
	if err != nil {
		t.Fatal(err)
	}
	query.Set("token", token)
	verifyReset := "/api/verify_reset_token?" + query.Encode()
	checked := accountRequest(handler, http.MethodGet, verifyReset, "", nil)
	if got := accountRoutePayload(t, checked)["valid"]; got != true {
		t.Fatalf("generated reset token valid = %v", got)
	}
	resetBody := fmt.Sprintf(`{"email":%q,"token":%q,"newPassword":"new-fixture-only-password"}`, email, token)
	assertCartStatus(t, accountRequest(handler, http.MethodPost, "/api/reset_password", resetBody, nil), http.StatusOK)
	assertCartStatus(t, accountRequest(handler, http.MethodPost, "/api/reset_password", resetBody, nil), http.StatusBadRequest)
	assertCartStatus(t, accountRequest(handler, http.MethodPost, "/api/login", `{"email":"test@example.invalid","password":"test-password"}`, nil), http.StatusUnauthorized)
	assertCartStatus(t, accountRequest(handler, http.MethodPost, "/api/login", `{"email":"test@example.invalid","password":"new-fixture-only-password"}`, nil), http.StatusOK)

	cookie := profileCookie(t, writes.key, testAccountID, false, false, false)
	assertCartStatus(t, accountWriteRequest(handler, http.MethodGet, "/api/verify_email", "", "fixture-csrf", cookie), http.StatusOK)
	code, err := writes.redis.Get(t.Context(), accountTokenKey(email, verifyEmailToken)).Result()
	if err != nil || !strings.Contains(mailer.body, code) || mailer.recipient != email {
		t.Fatal("verification code not sent")
	}
	verifyEmail := "/api/email_verification?code=" + code
	verify := accountWriteRequest(handler, http.MethodGet, verifyEmail, "", "fixture-csrf", cookie)
	if user := accountRoutePayload(t, verify); user["isEmailVerified"] != true {
		t.Fatal("email still unverified")
	}
	assertCartStatus(t, accountWriteRequest(handler, http.MethodGet, verifyEmail, "", "fixture-csrf", cookie), http.StatusBadRequest)
}

func TestPortedAccountPhoneChallenge(t *testing.T) {
	writes := accountWritesFixture(t)
	writes.redis = testAccountRedis(t)
	const number = "+375290004204"
	writes.phone = phoneAuthentication{provider: testAccountSMS{number}, dailyLimit: 200}
	writes.checkoutEnabled = true
	handler := accountWriteHandler(writes)
	defer cleanupAccountRedis(t, writes.redis, "phone:cooldown:"+number, "phone:hour:"+number, "phone:day:"+number, "phone:ip:192.0.2.1", "phone:global:day")
	start := accountRequest(handler, http.MethodPost, "/api/auth/phone/start", `{"phone":"+375 (29) 000-42-04","purpose":"login"}`, nil)
	challenge := accountRoutePayload(t, start)
	challengeID := challenge["challengeId"].(string)
	csrf := challenge["csrfToken"].(string)
	cookie := start.Result().Cookies()[0]
	defer cleanupAccountRedis(t, writes.redis, phoneChallengePrefix+challengeID)
	if cookie.Name != phoneChallengeCookie || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode ||
		cookie.Path != "/api/auth/phone" || challenge["expiresIn"] != float64(300) {
		t.Fatal("wrong phone challenge response")
	}
	verify := fmt.Sprintf(`{"challengeId":%q,"code":"12345"}`, challengeID)
	incorrect := fmt.Sprintf(`{"challengeId":%q,"code":"10000"}`, challengeID)
	assertCartStatus(t, phoneWriteRequest(handler, "/api/auth/phone/verify", verify, csrf, nil), http.StatusUnauthorized)
	assertCartStatus(t, phoneWriteRequest(handler, "/api/auth/phone/verify", incorrect, csrf, cookie), http.StatusBadRequest)
	registered := phoneWriteRequest(handler, "/api/auth/phone/verify", verify, csrf, cookie)
	if got := accountRoutePayload(t, registered)["registrationRequired"]; got != true {
		t.Fatalf("phone verification registrationRequired = %v", got)
	}
	complete := fmt.Sprintf(`{"challengeId":%q,"firstName":" Имя ","secondName":" Фамилия ","email":"contact@example.invalid"}`, challengeID)
	login := phoneWriteRequest(handler, "/api/auth/phone/complete", complete, csrf, cookie)
	payload := accountPayload(t, login)
	user := payload["user"].(map[string]any)
	claims := payload["claims"].(map[string]any)
	if claims["phone_auth"] != true || claims["is_admin"] != false || claims["is_moderator"] != false ||
		user["verifiedPhone"] != number || user["firstName"] != "Имя" || user["contactEmail"] != "contact@example.invalid" || user["email"] != nil {
		t.Fatal("phone login did not create a normal profile")
	}
	assertCartStatus(t, phoneWriteRequest(handler, "/api/auth/phone/complete", complete, csrf, cookie), http.StatusBadRequest)
	accountCookie := login.Result().Cookies()[0]
	accountCSRF := claims["csrf_token"].(string)
	profilePath := "/api/person/" + claims["id"].(string)
	assertCartStatus(t, accountWriteRequest(handler, http.MethodPatch, profilePath, `{"password":"wrong"}`, accountCSRF, accountCookie), http.StatusForbidden)
	assertCartStatus(t, accountWriteRequest(handler, http.MethodPatch, profilePath, `{"phone":"+375290004205"}`, accountCSRF, accountCookie), http.StatusBadRequest)
	profile := accountWriteRequest(handler, http.MethodPatch, "/api/checkout/profile", `{"firstName":"Новое имя","secondName":"Фамилия","email":null}`, accountCSRF, accountCookie)
	if records := accountRoutePayload(t, profile); records["firstName"] != "Новое имя" || records["contactEmail"] != nil || records["verifiedPhone"] != number {
		t.Fatal("phone checkout profile update changed wrong fields")
	}
}

func TestPortedAccountGuestOrdersRequireMatchingVerifiedPhone(t *testing.T) {
	writes := accountWritesFixture(t, "testdata/order_history_schema.sql", "testdata/guest_history_rows.sql", "testdata/checkout_confirmation_schema.sql")
	ctx := t.Context()
	tx, err := writes.store.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE person SET verified_phone='+375290000011', phone_verified_at=now() WHERE id=$1`, testAccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE "order" SET contact_snapshot='{"phone":"+375290000011"}' WHERE id='00000000-0000-4000-8000-000000000921'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	handler := accountWriteHandler(writes)
	accountCookie := profileCookie(t, writes.key, testAccountID, false, false, false)
	guest := guestHistoryCookie(t, writes.key, firstGuestID, guestAudience, time.Now().Add(time.Hour).Unix())
	claim := func(csrf string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/checkout/guest/claim", nil)
		request.AddCookie(accountCookie)
		request.AddCookie(guest)
		request.Header.Set("Origin", writes.guestOrigin)
		request.Header.Set("X-Guest-CSRF", csrf)
		request.Header.Set("X-CSRF-Token", "fixture-csrf")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	assertCartStatus(t, claim("wrong"), http.StatusUnauthorized)
	if data := accountRoutePayload(t, claim(guestCSRF)); data["claimed"] != float64(1) {
		t.Fatalf("claimed = %v", data)
	}
	if data := accountRoutePayload(t, claim(guestCSRF)); data["claimed"] != float64(0) {
		t.Fatalf("replayed claim = %v", data)
	}
}

func accountWritesFixture(t *testing.T, files ...string) *accountWrites {
	t.Helper()
	pool := testCartDatabase(t, files...)
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackCart(t.Context(), tx)
	if _, err := tx.Exec(t.Context(), `ALTER TABLE person ALTER COLUMN id SET DEFAULT gen_random_uuid(),
		ALTER COLUMN is_admin SET DEFAULT false, ALTER COLUMN is_moderator SET DEFAULT false,
		ALTER COLUMN is_deleted SET DEFAULT false, ALTER COLUMN created_at SET DEFAULT now(),
		ALTER COLUMN updated_at SET DEFAULT now(), ADD UNIQUE(verified_phone)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	auth := &accountAuth{db: pool, key: []byte(testAccountWriteKey), guestOrigin: guestOrigin, secureCookies: true}
	return &accountWrites{accountAuth: auth, store: pool}
}

func testAccountRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("SKVIDERM_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set SKVIDERM_TEST_REDIS_URL to run the Redis contract tests")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func cleanupAccountRedis(t *testing.T, client *redis.Client, keys ...string) {
	t.Helper()
	if err := client.Del(t.Context(), keys...).Err(); err != nil {
		t.Error(err)
	}
}

func accountWriteRequest(handler http.Handler, method, path, body, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", token)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func phoneWriteRequest(handler http.Handler, path, body, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Phone-CSRF", csrf)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type testAccountMail struct {
	recipient string
	body      string
}

func (mail *testAccountMail) send(_ context.Context, recipient, _, body string) error {
	mail.recipient, mail.body = recipient, body
	return nil
}

type testAccountSMS struct{ phone string }

func (provider testAccountSMS) sendCode(_ context.Context, phone string) (string, error) {
	if phone != provider.phone {
		return "", errors.New("invalid synthetic recipient")
	}
	return "fixture-code", nil
}

func (provider testAccountSMS) checkCode(_ context.Context, hash, code string) (string, error) {
	if hash == "fixture-code" && code == "12345" {
		return provider.phone, nil
	}
	return "", nil
}

func accountRoutePayload(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	assertCartStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("account route response could be cached")
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestPersonPurgePreservesOrderHistory(t *testing.T) {
	writes := accountWritesFixture(t, "testdata/order_history_schema.sql", "testdata/guest_history_rows.sql", "testdata/checkout_confirmation_schema.sql")
	handler := accountWriteHandler(writes)
	admin := profileCookie(t, writes.key, testAdminID, true, true, false)
	cascade := accountWriteRequest(handler, http.MethodDelete, "/api/person/cascade/"+firstGuestID, "", "fixture-csrf", admin)
	assertCartStatus(t, cascade, http.StatusConflict)
	archive := accountWriteRequest(handler, http.MethodDelete, "/api/person/"+firstGuestID, "", "fixture-csrf", admin)
	assertCartStatus(t, archive, http.StatusOK)
	var archived bool
	var remaining int
	if err := writes.db.QueryRow(t.Context(), `SELECT is_deleted,
		(SELECT count(*) FROM "order" WHERE person_id = $1) FROM person WHERE id = $1`, firstGuestID).Scan(&archived, &remaining); err != nil ||
		!archived || remaining == 0 {
		t.Fatalf("archive retained account=%v orders=%d: %v", archived, remaining, err)
	}
}
