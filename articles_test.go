package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testArticlesPath = "/api/articles"
const testAdminArticlesPath = "/api/admin/articles"

type articleResponseSummary struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Summary     string     `json:"summary"`
	PublishedAt *time.Time `json:"publishedAt"`
}

type articleResponseDetail struct {
	articleResponseSummary
	Body string `json:"body"`
}

func TestArticlesRequireModeratorAndCSRF(t *testing.T) {
	key := []byte("test-only")
	handler := newHandler(nil, nil, &accountAuth{key: key, fullDB: nilCartDatabase{}}, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	readOnly := newHandler(nil, nil, &accountAuth{key: key}, false)
	assertCartStatus(t, bannerRawRequest(t, readOnly, http.MethodGet, testAdminArticlesPath, "", moderator, "fixture-csrf"), http.StatusNotFound)
	customer := profileCookie(t, key, testAccountID, false, false, false)
	forged := profileCookie(t, []byte("other-key"), testModeratorID, false, true, false)
	id := testAdminArticlesPath + "/" + uuid.NewString()

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, testAdminArticlesPath},
		{http.MethodGet, id},
		{http.MethodPost, testAdminArticlesPath},
		{http.MethodPut, id},
	} {
		for _, identity := range []struct {
			name, csrf string
			cookie     *http.Cookie
		}{
			{"anonymous", "", nil},
			{"customer", "fixture-csrf", customer},
			{"forged moderator", "fixture-csrf", forged},
			{"missing CSRF", "", moderator},
			{"wrong CSRF", "wrong", moderator},
		} {
			t.Run(route.method+" "+route.path+"/"+identity.name, func(t *testing.T) {
				response := bannerRawRequest(t, handler, route.method, route.path, "{}", identity.cookie, identity.csrf)
				assertCartStatus(t, response, http.StatusUnauthorized)
			})
		}
	}
}

func TestArticlePublicationLifecycle(t *testing.T) {
	pool := testCartDatabase(t, "migrations/0004_articles.sql")
	key := []byte("test-only")
	handler := newHandler(pool, nil, &accountAuth{db: pool, fullDB: pool, key: key}, false)
	public := newHandler(pool, nil, nil, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	admin := profileCookie(t, key, testAdminID, true, false, false)

	assertArticleList(t, public, testArticlesPath, nil)
	assertArticleList(t, handler, testAdminArticlesPath, moderator)
	draft := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPost, testAdminArticlesPath,
		`{"title":"Уход за кожей","summary":"","body":"","isPublished":false}`, moderator, "fixture-csrf"))
	if _, err := uuid.Parse(draft.ID); err != nil || draft.Title != "Уход за кожей" ||
		draft.Summary != "" || draft.Body != "" || draft.PublishedAt != nil {
		t.Fatalf("unexpected draft: %+v", draft)
	}
	adminPath, publicPath := testAdminArticlesPath+"/"+draft.ID, testArticlesPath+"/"+draft.ID
	assertArticleList(t, handler, testAdminArticlesPath, moderator, draft.ID)
	assertArticleList(t, public, testArticlesPath, nil)
	assertPublicArticleMissing(t, public, publicPath)
	got := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodGet, adminPath, "", moderator, "fixture-csrf"))
	if got != draft {
		t.Fatalf("draft detail = %+v, want %+v", got, draft)
	}

	published := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPut, adminPath,
		`{"title":"Уход за кожей","summary":"Кратко","body":"Первый абзац\n\n<em>Второй абзац</em>","isPublished":true}`, moderator, "fixture-csrf"))
	if published.PublishedAt == nil || published.Summary != "Кратко" {
		t.Fatalf("unexpected published article: %+v", published)
	}
	got = readArticleDetail(t, bannerRawRequest(t, public, http.MethodGet, publicPath, "", nil, ""))
	if got.ID != draft.ID || got.Title != "Уход за кожей" || got.Summary != "Кратко" ||
		got.Body != "Первый абзац\n\n<em>Второй абзац</em>" || got.PublishedAt == nil || !published.PublishedAt.Equal(*got.PublishedAt) {
		t.Fatalf("unexpected public detail: %+v", got)
	}
	assertArticleList(t, public, testArticlesPath, nil, draft.ID)

	second := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPost, testAdminArticlesPath,
		`{"title":"Вторая","summary":"","body":"Абзац","isPublished":true}`, admin, "fixture-csrf"))
	assertArticleList(t, public, testArticlesPath, nil, second.ID, draft.ID)
	edited := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPut, adminPath,
		`{"title":"Новый заголовок","summary":"Новое описание","body":"Новый текст","isPublished":true}`, moderator, "fixture-csrf"))
	if edited.Title != "Новый заголовок" || edited.Summary != "Новое описание" || edited.Body != "Новый текст" ||
		edited.PublishedAt == nil || !edited.PublishedAt.Equal(*published.PublishedAt) {
		t.Fatalf("unexpected edited article: %+v", edited)
	}
	assertArticleList(t, public, testArticlesPath, nil, second.ID, draft.ID)

	for _, id := range []string{second.ID, draft.ID} {
		unpublished := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPut, testAdminArticlesPath+"/"+id,
			`{"title":"Снята с публикации","summary":"","body":"Текст сохранён","isPublished":false}`, moderator, "fixture-csrf"))
		if unpublished.PublishedAt != nil || unpublished.Body != "Текст сохранён" {
			t.Fatalf("unexpected unpublished article: %+v", unpublished)
		}
		assertPublicArticleMissing(t, public, testArticlesPath+"/"+id)
	}
	assertArticleList(t, public, testArticlesPath, nil)

	republished := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPut, adminPath,
		`{"title":"Снова опубликована","summary":"","body":"Текст сохранён","isPublished":true}`, moderator, "fixture-csrf"))
	if republished.PublishedAt == nil || !republished.PublishedAt.After(*published.PublishedAt) {
		t.Fatalf("unexpected republication timestamp: %+v", republished.PublishedAt)
	}
	assertArticleList(t, public, testArticlesPath, nil, draft.ID)

	for _, id := range []string{"", "invalid", uuid.NewString()} {
		assertCartStatus(t, bannerRawRequest(t, public, http.MethodGet, testArticlesPath+"/"+id, "", nil, ""), http.StatusNotFound)
		assertCartStatus(t, bannerRawRequest(t, handler, http.MethodGet, testAdminArticlesPath+"/"+id, "", moderator, "fixture-csrf"), http.StatusNotFound)
		assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPut, testAdminArticlesPath+"/"+id,
			`{"title":"Название","summary":"","body":"","isPublished":false}`, moderator, "fixture-csrf"), http.StatusNotFound)
	}
}

func TestArticleWriteValidatesTextAndJSON(t *testing.T) {
	pool := testCartDatabase(t, "migrations/0004_articles.sql")
	key := []byte("test-only")
	handler := newHandler(pool, nil, &accountAuth{db: pool, fullDB: pool, key: key}, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)

	for _, test := range []struct {
		name, body string
	}{
		{"empty title", articlePayload(t, "", "", "", false)},
		{"blank title", articlePayload(t, " \n\t", "", "", false)},
		{"long title", articlePayload(t, strings.Repeat("ю", 161), "", "", false)},
		{"long summary", articlePayload(t, "Заголовок", strings.Repeat("ю", 351), "", false)},
		{"long body", articlePayload(t, "Заголовок", "", strings.Repeat("🧴", 30001), false)},
		{"empty published body", articlePayload(t, "Заголовок", "", "", true)},
		{"blank published body", articlePayload(t, "Заголовок", "", " \r\n\t", true)},
		{"NUL in text", articlePayload(t, "Заголовок", "", "Текст\x00статьи", false)},
		{"missing body", `{"title":"Заголовок","summary":"","isPublished":false}`},
		{"nullable summary", `{"title":"Заголовок","summary":null,"body":"","isPublished":false}`},
		{"nullable publication flag", `{"title":"Заголовок","summary":"","body":"","isPublished":null}`},
		{"wrong publication flag type", `{"title":"Заголовок","summary":"","body":"","isPublished":"false"}`},
		{"duplicate publication flag", `{"title":"Заголовок","summary":"","body":"","isPublished":true,"isPublished":false}`},
		{"trailing JSON", `{"title":"Заголовок","summary":"","body":"","isPublished":false}{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := bannerRawRequest(t, handler, http.MethodPost, testAdminArticlesPath, test.body, moderator, "fixture-csrf")
			assertCartStatus(t, response, http.StatusBadRequest)
		})
	}
	assertArticleList(t, handler, testAdminArticlesPath, moderator)

	title, summary, body := strings.Repeat("ю", 160), strings.Repeat("я", 350), strings.Repeat("🧴", 30000)
	atLimit := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPost, testAdminArticlesPath,
		articlePayload(t, title, summary, body, true), moderator, "fixture-csrf"))
	if atLimit.Title != title || atLimit.Summary != summary || atLimit.Body != body || atLimit.PublishedAt == nil {
		t.Fatal("text at the Unicode character limit was not preserved")
	}
	adminPath := testAdminArticlesPath + "/" + atLimit.ID
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPut, adminPath,
		articlePayload(t, "Новый заголовок", "Новое описание", "", true), moderator, "fixture-csrf"), http.StatusBadRequest)
	persisted := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodGet, adminPath, "", moderator, "fixture-csrf"))
	if persisted.Title != title || persisted.Summary != summary || persisted.Body != body ||
		persisted.PublishedAt == nil || !persisted.PublishedAt.Equal(*atLimit.PublishedAt) {
		t.Fatal("invalid update changed the existing article")
	}
}

func articlePayload(t *testing.T, title, summary, body string, isPublished bool) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"title": title, "summary": summary, "body": body, "isPublished": isPublished})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func assertPublicArticleMissing(t *testing.T, handler http.Handler, path string) {
	t.Helper()
	response := bannerRawRequest(t, handler, http.MethodGet, path, "", nil, "")
	assertCartStatus(t, response, http.StatusNotFound)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing article responses must disable caching")
	}
}

func assertArticleList(t *testing.T, handler http.Handler, path string, cookie *http.Cookie, ids ...string) {
	t.Helper()
	var list struct {
		Status string                   `json:"status"`
		Length int                      `json:"length"`
		Data   []articleResponseSummary `json:"data"`
	}
	readArticleResponse(t, bannerRawRequest(t, handler, http.MethodGet, path, "", cookie, "fixture-csrf"), &list)
	if list.Status != "success" || list.Length != len(ids) || len(list.Data) != len(ids) || list.Data == nil {
		t.Fatalf("unexpected list response: %+v", list)
	}
	for i, id := range ids {
		if list.Data[i].ID != id {
			t.Fatalf("article %d ID = %s, want %s", i, list.Data[i].ID, id)
		}
	}
}

func readArticleDetail(t *testing.T, response *httptest.ResponseRecorder) articleResponseDetail {
	t.Helper()
	var detail struct {
		Status string                `json:"status"`
		Data   articleResponseDetail `json:"data"`
	}
	readArticleResponse(t, response, &detail)
	if detail.Status != "success" {
		t.Fatalf("article detail status = %q", detail.Status)
	}
	return detail.Data
}

func readArticleResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	assertCartStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("article response must disable caching")
	}
	decoder := json.NewDecoder(response.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
}
