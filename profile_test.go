package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testAdminID     = "00000000-0000-4000-8000-000000000602"
	testPhoneID     = "00000000-0000-4000-8000-000000000603"
	testArchivedID  = "00000000-0000-4000-8000-000000000604"
	testLongPassID  = "00000000-0000-4000-8000-000000000605"
	testModeratorID = "00000000-0000-4000-8000-000000000606"
	testUnknownID   = "00000000-0000-4000-8000-000000000999"
)

func TestProfileAuthorization(t *testing.T) {
	key := []byte("fixture-secret-no-production")
	handler := newHandler(nil, nil, &accountAuth{key: key}, false)
	customer := profileCookie(t, key, testAccountID, false, false, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	admin := profileCookie(t, key, testAdminID, true, true, false)
	wrongKey := profileCookie(t, []byte("other-fixture-key"), testAdminID, true, true, false)

	for _, tc := range []struct {
		name, path, csrf, body string
		cookie                 *http.Cookie
		status                 int
	}{
		{"no session for list", "/api/person", "", "Unauthorized", nil, 401},
		{"no session for detail", "/api/person/" + testAccountID, "", "Unauthorized", nil, 401},
		{"no header", "/api/person/" + testAccountID, "", "CSRF token not found", customer, 401},
		{"incorrect header", "/api/person", "wrong", "Invalid CSRF token", admin, 401},
		{"invalid signature", "/api/person", "fixture-csrf", "Unauthorized", wrongKey, 401},
		{"customer list", "/api/person", "fixture-csrf", "Unauthorized", customer, 401},
		{"moderator list", "/api/person", "fixture-csrf", "Unauthorized", moderator, 401},
		{"customer reads another profile", "/api/person/" + testPhoneID, "fixture-csrf", "", customer, 403},
		{"customer reads missing profile", "/api/person/" + testUnknownID, "fixture-csrf", "", customer, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := profileRequest(handler, tc.path, tc.cookie, tc.csrf)
			if response.Code != tc.status || response.Body.String() != tc.body {
				t.Fatalf("%s = %d %q, want %d %q", tc.path, response.Code, response.Body, tc.status, tc.body)
			}
		})
	}
}

func TestProfileContract(t *testing.T) {
	tx := accountFixture(t)
	key := []byte("fixture-secret-no-production")
	handler := newHandler(tx, nil, &accountAuth{db: tx, key: key}, false)
	customer := profileCookie(t, key, testAccountID, false, false, false)
	phone := profileCookie(t, key, testPhoneID, false, false, true)
	admin := profileCookie(t, key, testAdminID, true, true, false)

	for _, tc := range []struct {
		name, id string
		cookie   *http.Cookie
		status   int
	}{
		{"owner", testAccountID, customer, 200},
		{"phone owner", testPhoneID, phone, 200},
		{"admin reads another profile", testPhoneID, admin, 200},
		{"admin reads archived profile", testArchivedID, admin, 200},
		{"admin reads missing profile", testUnknownID, admin, 404},
		{"malformed ID", "not-uuid", admin, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := profileRequest(handler, "/api/person/"+tc.id, tc.cookie, "fixture-csrf")
			if response.Code != tc.status {
				t.Fatalf("unexpected response: %d %q", response.Code, response.Body)
			}
			if tc.status != 200 {
				return
			}
			var payload struct {
				Status string
				Data   struct {
					ID string
				}
			}
			readProfilePayload(t, response, &payload)
			if payload.Status != "success" || payload.Data.ID != tc.id {
				t.Fatalf("unexpected profile: %+v", payload)
			}
		})
	}

	active := []string{testAccountID, testAdminID, testPhoneID, testLongPassID, testModeratorID}
	for _, tc := range []struct {
		query     string
		ids       []string
		total     int
		pageLimit []int
	}{
		{"", active, 5, nil},
		{"includeDeleted=true", []string{testAccountID, testAdminID, testPhoneID, testArchivedID, testLongPassID, testModeratorID}, 6, nil},
		{"first_name=%D0%A2%D0%95%D0%A1", []string{testAccountID, testAdminID, testPhoneID, testLongPassID}, 4, nil},
		{"second_name=%D0%9F%D0%90%D0%A0%D0%9E%D0%9B", []string{testLongPassID}, 1, nil},
		{"firstName=missing", active, 5, nil},
		{"email=PHONE%40&phone=0000003", []string{testPhoneID}, 1, nil},
		{"isEmailVerified=true", []string{testAdminID}, 1, nil},
		{"isEmailVerified=false", []string{testAccountID, testPhoneID, testLongPassID}, 3, nil},
		{"email=missing", []string{}, 0, nil},
		{"limit=2&page=2", []string{testPhoneID, testLongPassID}, 5, []int{2, 2}},
		{"limit=2&page=999", []string{}, 5, []int{999, 2}},
	} {
		t.Run("list/"+tc.query, func(t *testing.T) {
			response := profileRequest(handler, "/api/person?"+tc.query, admin, "fixture-csrf")
			var payload struct {
				Status string
				Length int
				Page   *int
				Limit  *int
				Data   []struct {
					ID string
				}
			}
			readProfilePayload(t, response, &payload)
			actual := make([]string, 0, len(payload.Data))
			for _, person := range payload.Data {
				actual = append(actual, person.ID)
			}
			slices.Sort(actual)
			if payload.Status != "success" || payload.Data == nil || payload.Length != tc.total || !slices.Equal(actual, tc.ids) {
				t.Fatalf("list returned IDs %v and length %d, want %v and %d", actual, payload.Length, tc.ids, tc.total)
			}
			if len(tc.pageLimit) == 0 {
				if payload.Page != nil || payload.Limit != nil {
					t.Fatal("expected null pagination")
				}
			} else if payload.Page == nil || payload.Limit == nil || *payload.Page != tc.pageLimit[0] || *payload.Limit != tc.pageLimit[1] {
				t.Fatal("expected pagination for requested page and limit")
			}
		})
	}

	for _, query := range []string{"id=" + testAccountID, "isEmailVerified=yes", "isEmailVerified=true&isEmailVerified=false", "page=0&limit=2"} {
		response := profileRequest(handler, "/api/person?"+query, admin, "fixture-csrf")
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid query %s: %d", query, response.Code)
		}
	}
}

func profileCookie(t *testing.T, key []byte, id string, admin, moderator, phone bool) *http.Cookie {
	t.Helper()
	return signAccountCookie(t, jwt.SigningMethodHS256, key, jwt.MapClaims{
		"id": id, "email": "test@example.invalid", "is_admin": admin, "is_moderator": moderator,
		"csrf_token": "fixture-csrf", "phone_auth": phone, "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func profileRequest(handler http.Handler, path string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func readProfilePayload(t *testing.T, response *httptest.ResponseRecorder, want any) {
	t.Helper()
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected response: %d %q", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), `"password":`) {
		t.Fatal("profile response exposed a password hash")
	}
	if err := json.Unmarshal(response.Body.Bytes(), want); err != nil {
		t.Fatal(err)
	}
}
