package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReadinessFailsWithoutDatabase(t *testing.T) {
	response := accountRequest(newHandler(nil, nil, nil, false), http.MethodGet, "/api/health/ready", "", nil)
	if response.Code != http.StatusServiceUnavailable || strings.TrimSpace(response.Body.String()) != `{"status":"unavailable"}` {
		t.Fatalf("readiness = %d %s, want 503 unavailable", response.Code, response.Body)
	}
}

func TestOperationalRoutesStoreSharedMaintenanceState(t *testing.T) {
	redisURL := os.Getenv("SKVIDERM_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set SKVIDERM_TEST_REDIS_URL to an isolated Redis instance")
	}
	pool := testCartDatabase(t)
	cache, err := newOperationsRedis(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	const key = "maintenance_mode"
	defer cache.Del(context.Background(), key)
	signingKey := []byte("fixture-operations-key")
	auth := &accountAuth{db: pool, key: signingKey, operationsRedis: cache}
	handler := newHandler(pool, nil, auth, false)
	admin := profileCookie(t, signingKey, testAccountID, true, false, false)
	moderator := profileCookie(t, signingKey, testAccountID, false, true, false)

	adminRequest := func(path string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf {
			r.Header.Set("X-CSRF-Token", "fixture-csrf")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := adminRequest("/api/health/ready", nil, false); got.Code != 200 ||
		strings.TrimSpace(got.Body.String()) != `{"status":"ready"}` {
		t.Fatalf("healthy dependencies = %d %s", got.Code, got.Body)
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		csrf   bool
	}{{nil, true}, {admin, false}, {moderator, true}} {
		if got := adminRequest("/api/maintenance/enable", tc.cookie, tc.csrf); got.Code != http.StatusUnauthorized {
			t.Fatalf("maintenance without admin and CSRF = %d %s", got.Code, got.Body)
		}
	}
	if got := adminRequest("/api/maintenance/enable?ttl=120", admin, true); got.Code != 200 {
		t.Fatalf("enable = %d %s", got.Code, got.Body)
	}
	if got, err := cache.Get(t.Context(), key).Result(); err != nil || got != "enabled" {
		t.Fatalf("legacy Redis maintenance key = %q: %v", got, err)
	}
	if ttl, err := cache.TTL(t.Context(), key).Result(); err != nil || ttl < 119*time.Second || ttl > 120*time.Second {
		t.Fatalf("legacy TTL = %v: %v", ttl, err)
	}
	if err := cache.Set(t.Context(), key, "enabled", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if got := adminRequest("/api/maintenance/status", nil, false); got.Code != 200 ||
		strings.TrimSpace(got.Body.String()) != `{"status":"enabled","enabled":true,"ttl":0}` {
		t.Fatalf("persistent status = %d %s", got.Code, got.Body)
	}
	if got := adminRequest("/api/maintenance/disable", admin, true); got.Code != 200 {
		t.Fatalf("disable = %d %s", got.Code, got.Body)
	}
	if got := adminRequest("/api/maintenance/status", nil, false); got.Code != 200 ||
		strings.TrimSpace(got.Body.String()) != `{"status":"disabled","enabled":false,"ttl":null}` {
		t.Fatalf("disabled status = %d %s", got.Code, got.Body)
	}
}

func TestPostPlacementsSortedAndDecoded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		payload string
		want    int
	}{
		{"active placements", 200, `[{"id":7,"number":128,"address":"Минск, \\u0443л. Полевая","longitude":"27.5","latitude":"53.9"},{"id":4,"number":17,"address":"Минск, Площадь","longitude":"27.5","latitude":"53.9"}]`, 2},
		{"bad upstream status", 503, "unavailable", 0},
		{"malformed response", 200, "invalid", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: placementTestTransport{status: tc.status, payload: tc.payload}}
			request := httptest.NewRequest(http.MethodGet, "/api/post_placements", nil)
			response := httptest.NewRecorder()
			postPlacementsHandler(client).ServeHTTP(response, request)
			var result listResponse[postPlacement]
			err := json.Unmarshal(response.Body.Bytes(), &result)
			if response.Code != 200 || err != nil || result.Status != "success" || result.Length != tc.want ||
				result.Data == nil || len(result.Data) != tc.want || result.Page != nil || result.Limit != nil {
				t.Fatalf("placements = %d %s: %v", response.Code, response.Body, err)
			}
			if tc.want == 2 && (result.Data[0].Number != 17 || result.Data[1].Number != 128 ||
				result.Data[1].Address != "Минск, ул. Полевая" || result.Data[1].Longitude != "27.5") {
				t.Fatalf("placements not sorted or normalized: %+v", result.Data)
			}
		})
	}
}

type placementTestTransport struct {
	status  int
	payload string
}

func (tr placementTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet || r.URL.String() != "https://evropochta.by/mvc/application/ops/placement/" {
		panic("unexpected upstream request")
	}
	return &http.Response{StatusCode: tr.status, Body: io.NopCloser(strings.NewReader(tr.payload))}, nil
}

func TestLogoutAvailableWithoutPhoneCheckout(t *testing.T) {
	key := []byte("fixture-legacy-logout-key")
	auth := &accountAuth{key: key}
	handler := newHandler(nil, nil, auth, false)
	r := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	r.AddCookie(profileCookie(t, key, testAccountID, false, false, false))
	r.Header.Set("X-CSRF-Token", "fixture-csrf")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	cookies := w.Result().Cookies()
	if w.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Name != "jwt" || cookies[0].MaxAge >= 0 {
		t.Fatalf("legacy logout = %d %v", w.Code, cookies)
	}
}
