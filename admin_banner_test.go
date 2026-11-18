package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

const adminBannerPath = "/api/admin/banner"
const hiddenTestBanner = "00000000-0000-4000-8000-000000000303"

func TestAdminBannerAuthorization(t *testing.T) {
	key := []byte("test-only")
	auth := &accountAuth{key: key, bannerDB: nilCartDatabase{}}
	handler := newHandler(nil, nil, auth, false)
	customer := profileCookie(t, key, testAccountID, false, false, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	wrongKey := profileCookie(t, []byte("separate-fixture-key"), testAdminID, true, false, false)
	for _, route := range []struct {
		method, path string
	}{
		{http.MethodGet, adminBannerPath},
		{http.MethodGet, adminBannerPath + "/" + hiddenTestBanner},
		{http.MethodPost, adminBannerPath},
		{http.MethodPatch, adminBannerPath + "/" + hiddenTestBanner},
		{http.MethodDelete, adminBannerPath + "/" + hiddenTestBanner},
	} {
		for _, tc := range []struct {
			name, csrf string
			cookie     *http.Cookie
		}{
			{"no cookie", "", nil},
			{"customer", "fixture-csrf", customer},
			{"invalid signature", "fixture-csrf", wrongKey},
			{"moderator without csrf", "", moderator},
			{"moderator wrong csrf", "wrong", moderator},
		} {
			t.Run(route.method+"/"+tc.name, func(t *testing.T) {
				response := bannerRawRequest(t, handler, route.method, route.path, "{}", tc.cookie, tc.csrf)
				assertCartStatus(t, response, http.StatusUnauthorized)
			})
		}
	}
	disabled := newHandler(nil, nil, &accountAuth{key: key}, false)
	assertCartStatus(t, newCartRequest(t, disabled, http.MethodGet, adminBannerPath, nil, moderator), http.StatusNotFound)
}

func TestAdminBannerContract(t *testing.T) {
	pool := testCartDatabase(t)
	key := []byte("test-only")
	handler := newHandler(pool, nil, &accountAuth{db: pool, bannerDB: pool, key: key}, false)
	admin := profileCookie(t, key, testAdminID, true, false, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)

	list := newCartRequest(t, handler, http.MethodGet, adminBannerPath+"?limit=1&includeDeleted=false", nil, admin)
	var banners listResponse[banner]
	readOrderHistory(t, list, &banners)
	if banners.Status != "success" || banners.Length != 4 || banners.Page != nil || banners.Limit != nil ||
		banners.Data[0].ID.String() != hiddenTestBanner || list.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("admin list differs from Rust: expected all banners sorted by order and ID")
	}
	public := func() listResponse[banner] {
		t.Helper()
		var result listResponse[banner]
		readOrderHistory(t, newCartRequest(t, handler, http.MethodGet, "/api/banner", nil, nil), &result)
		return result
	}
	if public().Length != 3 {
		t.Fatal("a hidden banner is present in the public feed")
	}

	var single struct {
		Status string `json:"status"`
		Data   banner `json:"data"`
	}
	readOrderHistory(t, newCartRequest(t, handler, http.MethodGet, adminBannerPath+"/"+hiddenTestBanner, nil, moderator), &single)
	if single.Status != "success" || single.Data.IsActive || single.Data.ID.String() != hiddenTestBanner {
		t.Fatal("moderator did not see the hidden record")
	}

	create := `{"image":"/images/testing/banner.jpg","ignored":"field"}`
	readOrderHistory(t, bannerRawRequest(t, handler, http.MethodPost, adminBannerPath, create, moderator, "fixture-csrf"), &single)
	created := single.Data
	if single.Status != "success" || !created.ID.Valid || created.IsActive || created.SortOrder != 0 ||
		created.Title != "" || created.Image != "/images/testing/banner.jpg" || !created.UpdatedAt.Valid {
		t.Fatalf("unexpected defaults: %+v", created)
	}

	id := adminBannerPath + "/" + created.ID.String()
	var wg sync.WaitGroup
	for _, payload := range []string{
		`{"title":"Новый слайд","isActive":true}`,
		`{"subtitle":"Новая подпись","sortOrder":1,"buttonText":"В каталог","buttonLink":"/products"}`,
	} {
		wg.Go(func() {
			response := bannerRawRequest(t, handler, http.MethodPatch, id, payload, moderator, "fixture-csrf")
			if response.Code != http.StatusOK {
				t.Errorf("concurrent edit failed: %d %s", response.Code, response.Body)
			}
		})
	}
	wg.Wait()
	feed := public()
	if feed.Length != 4 || feed.Data[0].ID != created.ID || feed.Data[0].Title != "Новый слайд" ||
		feed.Data[0].Subtitle != "Новая подпись" || feed.Data[0].ButtonLink != "/products" {
		t.Fatal("concurrent partial edits overwrote fields or changed sorting")
	}

	bad := bannerRawRequest(t, handler, http.MethodPatch, id, `{"buttonLink":""}`, moderator, "fixture-csrf")
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "Укажите и текст, и ссылку кнопки") {
		t.Fatal("invalid resulting banner was not rejected")
	}
	readOrderHistory(t, bannerRawRequest(t, handler, http.MethodPatch, id,
		`{"isActive":false,"title":null}`, moderator, "fixture-csrf"), &single)
	if single.Data.IsActive || single.Data.Title != "Новый слайд" || single.Data.ButtonLink != "/products" || public().Length != 3 {
		t.Fatal("hide or rollback did not preserve the previous banner data")
	}

	legacyURL := "https://example.invalid/sale-50%off"
	readOrderHistory(t, bannerRawRequest(t, handler, http.MethodPatch, id,
		`{"buttonLink":"`+legacyURL+`"}`, moderator, "fixture-csrf"), &single)
	readOrderHistory(t, bannerRawRequest(t, handler, http.MethodPatch, id,
		`{"isActive":true}`, moderator, "fixture-csrf"), &single)
	if single.Data.ButtonLink != legacyURL || !single.Data.IsActive {
		t.Fatal("an existing Rust link was rejected or rewritten when updating the banner")
	}

	var removed map[string]string
	readOrderHistory(t, newCartRequest(t, handler, http.MethodDelete, id, nil, admin), &removed)
	if removed["status"] != "success" || len(removed) != 1 {
		t.Fatal("unexpected delete response")
	}
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		assertCartStatus(t, bannerRawRequest(t, handler, method, id, `{}`, admin, "fixture-csrf"), http.StatusNotFound)
	}
	for _, id := range []string{uuid.NewString(), "wrong-id"} {
		assertCartStatus(t, newCartRequest(t, handler, http.MethodGet, adminBannerPath+"/"+id, nil, moderator), http.StatusNotFound)
	}
}

func TestAdminBannerValidation(t *testing.T) {
	key := []byte("test-only")
	handler := newHandler(nil, nil, &accountAuth{key: key, bannerDB: nilCartDatabase{}}, false)
	admin := profileCookie(t, key, testAdminID, true, false, false)
	for _, tc := range []struct {
		name, body, message string
	}{
		{"image missing", `{}`, "Invalid banner JSON"},
		{"null title", `{"image":"assets/test.png","title":null}`, "Invalid banner JSON"},
		{"invalid order", `{"image":"assets/test.png","sortOrder":10001}`, "Порядок должен быть от 0 до 10000"},
		{"image path", `{"image":"assets/../test.png"}`, "Выберите загруженное изображение"},
		{"image outside store", `{"image":"https://example.invalid/test.png"}`, "Выберите загруженное изображение"},
		{"invisible button", `{"image":"assets/test.png","buttonText":" ","buttonLink":"/products"}`, "Укажите и текст, и ссылку кнопки"},
		{"unescaped space", `{"image":"assets/test.png","buttonText":"Перейти","buttonLink":"/two words"}`, "Некорректная ссылка"},
		{"external protocol-relative link", `{"image":"assets/test.png","buttonText":"Перейти","buttonLink":"//example.invalid"}`, "Допустим путь внутри сайта или ссылка http/https"},
		{"title too long", `{"image":"assets/test.png","title":"` + strings.Repeat("ё", 256) + `"}`, "Заголовок: максимум 255 символов"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := bannerRawRequest(t, handler, http.MethodPost, adminBannerPath, tc.body, admin, "fixture-csrf")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), tc.message) {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body)
			}
		})
	}
}

func bannerRawRequest(t *testing.T, handler http.Handler, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
