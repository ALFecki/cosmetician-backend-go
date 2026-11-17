package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestAccountAttemptRequiresSession(t *testing.T) {
	handler := newHandler(nil, nil, &accountAuth{key: []byte("test-only")}, false)
	response := profileRequest(handler, "/api/checkout/attempt/"+testAccountID, nil, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("recovery without account session = %d %q, want 401", response.Code, response.Body)
	}
}

const (
	firstGuestID  = "00000000-0000-4000-8000-000000000611"
	secondGuestID = "00000000-0000-4000-8000-000000000612"
	guestOrigin   = "https://skviderm.by"
	guestCSRF     = "guest-fixture-csrf"
	guestReadPath = "/api/checkout/guest"
)

func TestGuestReadRejectsInvalidSession(t *testing.T) {
	key := []byte("test-only")
	handler := newHandler(nil, nil, &accountAuth{key: key, guestOrigin: guestOrigin}, false)
	future := time.Now().Add(time.Hour).Unix()
	active := guestHistoryCookie(t, key, firstGuestID, guestAudience, future)
	for _, tc := range []struct {
		name, path, origin, csrf string
		cookie                   *http.Cookie
	}{
		{"no session", "/session", "", "", nil},
		{"wrong origin", "/session", "https://example.invalid", "", active},
		{"expired", "/session", "", "", guestHistoryCookie(t, key, firstGuestID, guestAudience, time.Now().Add(-time.Second).Unix())},
		{"unexpected audience", "/session", "", "", guestHistoryCookie(t, key, firstGuestID, "test-other", future)},
		{"signature", "/session", "", "", guestHistoryCookie(t, []byte("other-test-key"), firstGuestID, guestAudience, future)},
		{"missing csrf", "/orders", "", "", active},
		{"wrong csrf", "/orders", guestOrigin, "other", active},
		{"account cookie", "/orders", guestOrigin, guestCSRF, profileCookie(t, key, testAccountID, false, false, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := guestHistoryRequest(handler, tc.path, tc.cookie, tc.origin, tc.csrf)
			assertCartStatus(t, response, http.StatusUnauthorized)
		})
	}

	active.Name = "jwt"
	assertCartStatus(t, profileRequest(handler, "/api/checkout/attempt/"+testAccountID, active, guestCSRF), http.StatusUnauthorized)
}

func TestGuestHistoryContract(t *testing.T) {
	ctx, tx := catalogFixture(t, "account_schema.sql", "account_rows.sql", "catalog_schema.sql", "catalog_rows.sql",
		"product_schema.sql", "product_rows.sql", "cart_storage_schema.sql", "order_history_schema.sql", "guest_history_rows.sql")
	key := []byte("test-only")
	handler := newHandler(tx, nil, &accountAuth{db: tx, key: key, guestOrigin: guestOrigin}, false)
	expires := time.Now().Add(time.Hour).Unix()
	guest := guestHistoryCookie(t, key, firstGuestID, guestAudience, expires)
	otherGuest := guestHistoryCookie(t, key, secondGuestID, guestAudience, expires)
	archivedGuest := guestHistoryCookie(t, key, "00000000-0000-4000-8000-000000000613", guestAudience, expires)
	account := profileCookie(t, key, testAccountID, false, false, false)
	admin := profileCookie(t, key, testAdminID, true, true, false)
	const (
		first        = "00000000-0000-4000-8000-000000000921"
		next         = "00000000-0000-4000-8000-000000000922"
		firstKey     = "00000000-0000-4000-8000-000000000981"
		accountKey   = "00000000-0000-4000-8000-000000000986"
		accountOrder = "00000000-0000-4000-8000-000000000926"
	)

	response := guestHistoryRequest(handler, "/session", guest, guestOrigin, "")
	var restored struct {
		CSRFToken string
		ExpiresAt int64
		Customer  struct {
			ID       uuid.UUID
			Password json.RawMessage
		}
	}
	readOrderHistory(t, response, &restored)
	if restored.Customer.ID.String() != firstGuestID || len(restored.Customer.Password) != 0 || restored.CSRFToken != guestCSRF ||
		restored.ExpiresAt != expires || len(response.Result().Cookies()) != 0 {
		t.Fatal("restored session differs from the signed claims or contains unexpected fields")
	}
	assertCartStatus(t, guestHistoryRequest(handler, "/session", archivedGuest, "", ""), http.StatusUnauthorized)
	accountAsGuest := guestHistoryCookie(t, key, testAccountID, guestAudience, expires)
	assertCartStatus(t, guestHistoryRequest(handler, "/orders", accountAsGuest, guestOrigin, guestCSRF), http.StatusUnauthorized)

	for _, tc := range []struct {
		query string
		page  int32
		limit int32
		ids   []string
	}{
		{"", 1, 50, []string{first, next}},
		{"?page=1&limit=1", 1, 1, []string{first}},
		{"?page=2&limit=1", 2, 1, []string{next}},
		{"?page=3&limit=1", 3, 1, []string{}},
		{"?page=-1&limit=0", 1, 1, []string{first}},
		{"?limit=101&includeDeleted=true&personId=" + secondGuestID, 1, 100, []string{first, next}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			var page listResponse[orderSummary]
			response := guestHistoryRequest(handler, "/orders"+tc.query, guest, guestOrigin, guestCSRF)
			readOrderHistory(t, response, &page)
			if page.Status != "success" || page.Length != 2 || page.Page == nil || *page.Page != tc.page ||
				page.Limit == nil || *page.Limit != tc.limit || page.Data == nil || len(page.Data) != len(tc.ids) {
				t.Fatalf("unexpected history page: %+v", page)
			}
			for i, expected := range tc.ids {
				if page.Data[i].ID.String() != expected {
					t.Fatalf("order %d = %s, want %s", i, page.Data[i].ID, expected)
				}
			}
		})
	}
	assertCartStatus(t, guestHistoryRequest(handler, "/orders?limit=no", guest, guestOrigin, guestCSRF), http.StatusBadRequest)

	for _, path := range []string{"/orders/" + first, "/attempt/" + firstKey} {
		assertCartStatus(t, guestHistoryRequest(handler, path, otherGuest, guestOrigin, guestCSRF), http.StatusNotFound)
	}
	for _, path := range []string{"/orders/00000000-0000-4000-8000-000000000923", "/orders/00000000-0000-4000-8000-000000000924",
		"/attempt/00000000-0000-4000-8000-000000000983", "/attempt/00000000-0000-4000-8000-000000000984"} {
		assertCartStatus(t, guestHistoryRequest(handler, path, guest, "", guestCSRF), http.StatusNotFound)
	}

	var detail struct {
		Status string
		Data   order
	}
	readOrderHistory(t, guestHistoryRequest(handler, "/orders/"+first, guest, guestOrigin, guestCSRF), &detail)
	if detail.Status != "success" || detail.Data.ID.String() != first {
		t.Fatal("unexpected guest order")
	}
	var recovered order
	readOrderHistory(t, guestHistoryRequest(handler, "/attempt/"+firstKey, guest, "", guestCSRF), &recovered)
	if recovered.ID != detail.Data.ID || len(recovered.Products) != 1 || recovered.Products[0].HistoricalPrice != 90.13 {
		t.Fatal("guest checkout recovery did not return the original order")
	}

	accountAttempt := "/api/checkout/attempt/"
	readOrderHistory(t, profileRequest(handler, accountAttempt+accountKey, account, "fixture-csrf"), &recovered)
	if recovered.ID.String() != accountOrder {
		t.Fatal("account checkout recovery returned the wrong order")
	}
	assertCartStatus(t, profileRequest(handler, accountAttempt+accountKey, admin, "fixture-csrf"), http.StatusNotFound)
	assertCartStatus(t, profileRequest(handler, accountAttempt+firstKey, account, "fixture-csrf"), http.StatusNotFound)

	if _, err := tx.Exec(ctx, `UPDATE "order" SET person_id = $1 WHERE id = $2`, testAccountID, first); err != nil {
		t.Fatal(err)
	}
	assertCartStatus(t, guestHistoryRequest(handler, "/orders/"+first, guest, guestOrigin, guestCSRF), http.StatusNotFound)
	assertCartStatus(t, guestHistoryRequest(handler, "/attempt/"+firstKey, guest, guestOrigin, guestCSRF), http.StatusNotFound)
}

func guestHistoryCookie(t *testing.T, key []byte, id, audience string, expires int64) *http.Cookie {
	t.Helper()
	cookie := signAccountCookie(t, jwt.SigningMethodHS256, key, jwt.MapClaims{
		"id": id, "aud": audience, "csrf_token": guestCSRF, "exp": expires,
	})
	cookie.Name = guestCookieName
	return cookie
}

func guestHistoryRequest(handler http.Handler, path string, cookie *http.Cookie, origin, csrf string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, guestReadPath+path, nil)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if csrf != "" {
		request.Header.Set("X-Guest-CSRF", csrf)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
