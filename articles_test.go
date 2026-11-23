package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testArticlesPath = "/api/articles"
const testAdminArticlesPath = "/api/admin/articles"

type articleResponseSummary struct {
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Summary          string            `json:"summary"`
	PublishedAt      *time.Time        `json:"publishedAt"`
	CoverImage       *testArticleImage `json:"coverImage"`
	FeaturedPosition *int              `json:"featuredPosition"`
}

type articleResponseDetail struct {
	articleResponseSummary
	Body       string                 `json:"body"`
	BodyImages []testArticleBodyImage `json:"bodyImages"`
}

type testArticleImage struct {
	Image string `json:"image"`
	Alt   string `json:"alt"`
}

type testArticleBodyImage struct {
	testArticleImage
	Caption        string `json:"caption"`
	AfterParagraph int    `json:"afterParagraph"`
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
		{http.MethodPut, testAdminArticlesPath + "/featured"},
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
	pool := testCartDatabase(t, "migrations/0004_articles.sql", "migrations/0005_article_media.sql")
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
		draft.Summary != "" || draft.Body != "" || draft.PublishedAt != nil ||
		draft.CoverImage != nil || draft.FeaturedPosition != nil || draft.BodyImages == nil || len(draft.BodyImages) != 0 {
		t.Fatalf("unexpected draft: %+v", draft)
	}
	adminPath, publicPath := testAdminArticlesPath+"/"+draft.ID, testArticlesPath+"/"+draft.ID
	assertArticleList(t, handler, testAdminArticlesPath, moderator, draft.ID)
	assertArticleList(t, public, testArticlesPath, nil)
	assertPublicArticleMissing(t, public, publicPath)
	got := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodGet, adminPath, "", moderator, "fixture-csrf"))
	if !reflect.DeepEqual(got, draft) {
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
	pool := testCartDatabase(t, "migrations/0004_articles.sql", "migrations/0005_article_media.sql")
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

func assertArticleList(t *testing.T, handler http.Handler, path string, cookie *http.Cookie, ids ...string) []articleResponseSummary {
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
	return list.Data
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

func TestArticleMediaRoundTrip(t *testing.T) {
	pool := testCartDatabase(t, "migrations/0004_articles.sql", "migrations/0005_article_media.sql")
	key := []byte("test-only")
	handler := newHandler(pool, nil, &accountAuth{db: pool, fullDB: pool, key: key}, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)

	first := testArticleImage{Image: "/images/00000000-0000-4000-8000-000000000001.jpg", Alt: "Обложка"}
	second := testArticleImage{Image: "/images/00000000-0000-4000-8000-000000000002.webp", Alt: "До и после"}
	images := []testArticleBodyImage{
		{testArticleImage: second, AfterParagraph: 0},
		{testArticleImage: first, Caption: "Подпись", AfterParagraph: 3},
		{testArticleImage: second, AfterParagraph: 3},
	}
	payload := `{"title":"Статья с фотографиями","summary":"Кратко","body":"Первый\r\n   \rВторой\nТретий","isPublished":true,
		"media":{"coverImage":{"image":"/images/00000000-0000-4000-8000-000000000001.jpg","alt":"Обложка"},"bodyImages":[
			{"image":"/images/00000000-0000-4000-8000-000000000002.webp","alt":"До и после","afterParagraph":0},
			{"image":"/images/00000000-0000-4000-8000-000000000001.jpg","alt":"Обложка","caption":"Подпись","afterParagraph":3},
			{"image":"/images/00000000-0000-4000-8000-000000000002.webp","alt":"До и после","afterParagraph":3}]}}`
	saved := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPost, testAdminArticlesPath, payload, moderator, "fixture-csrf"))
	if saved.CoverImage == nil || *saved.CoverImage != first || !slices.Equal(saved.BodyImages, images) {
		t.Fatalf("media not returned in save response: %+v", saved)
	}
	publicPath := testArticlesPath + "/" + saved.ID
	adminPath := testAdminArticlesPath + "/" + saved.ID
	for _, read := range []struct {
		listPath, detailPath string
		cookie               *http.Cookie
	}{
		{testArticlesPath, publicPath, nil},
		{testAdminArticlesPath, adminPath, moderator},
	} {
		list := assertArticleList(t, handler, read.listPath, read.cookie, saved.ID)
		if list[0].CoverImage == nil || *list[0].CoverImage != first {
			t.Fatalf("cover not returned in list: %+v", list[0])
		}
		detail := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodGet, read.detailPath, "", read.cookie, "fixture-csrf"))
		if detail.CoverImage == nil || *detail.CoverImage != first || !slices.Equal(detail.BodyImages, images) {
			t.Fatalf("media not returned in detail: %+v", detail)
		}
	}

	oldClient := articlePayload(t, "Другой заголовок", "Кратко", "Один абзац", true)
	edited := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPut, adminPath, oldClient, moderator, "fixture-csrf"))
	if edited.Body != "Один абзац" || edited.CoverImage == nil || *edited.CoverImage != first || !slices.Equal(edited.BodyImages, images) {
		t.Fatalf("legacy update did not preserve media: %+v", edited)
	}
	cleared := readArticleDetail(t, bannerRawRequest(t, handler, http.MethodPut, adminPath,
		strings.TrimSuffix(oldClient, "}")+`,"media":{"coverImage":null,"bodyImages":[]}}`, moderator, "fixture-csrf"))
	if cleared.CoverImage != nil || cleared.BodyImages == nil || len(cleared.BodyImages) != 0 {
		t.Fatalf("media not cleared: %+v", cleared)
	}
}

func TestArticleWriteValidatesMedia(t *testing.T) {
	pool := testCartDatabase(t, "migrations/0004_articles.sql", "migrations/0005_article_media.sql")
	key := []byte("test-only")
	handler := newHandler(pool, nil, &accountAuth{db: pool, fullDB: pool, key: key}, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	basePayload := strings.TrimSuffix(articlePayload(t, "Заголовок", "", "Абзац", true), "}")
	const validPath = "/images/00000000-0000-4000-8000-000000000001.png"
	const validBodyImage = `{"image":"` + validPath + `","alt":"Фото","afterParagraph":1}`
	for _, test := range []struct{ name, media string }{
		{"null media", `null`},
		{"omitted cover", `{"bodyImages":[]}`},
		{"omitted illustrations", `{"coverImage":null}`},
		{"null illustrations", `{"coverImage":null,"bodyImages":null}`},
		{"remote image", `{"coverImage":{"image":"https://example.net` + validPath + `","alt":"Фото"},"bodyImages":[]}`},
		{"non-upload path", `{"coverImage":{"image":"/images/file.png","alt":"Фото"},"bodyImages":[]}`},
		{"SVG", `{"coverImage":{"image":"` + strings.Replace(validPath, ".png", ".svg", 1) + `","alt":"Фото"},"bodyImages":[]}`},
		{"image URL query", `{"coverImage":{"image":"` + validPath + `?x","alt":"Фото"},"bodyImages":[]}`},
		{"blank alt", `{"coverImage":{"image":"` + validPath + `","alt":" \t"},"bodyImages":[]}`},
		{"oversize Unicode alt", `{"coverImage":{"image":"` + validPath + `","alt":"` + strings.Repeat("я", 201) + `"},"bodyImages":[]}`},
		{"missing position", `{"coverImage":null,"bodyImages":[{"image":"` + validPath + `","alt":"Фото"}]}`},
		{"fractional position", `{"coverImage":null,"bodyImages":[` + strings.Replace(validBodyImage, "1}", "0.5}", 1) + `]}`},
		{"negative position", `{"coverImage":null,"bodyImages":[` + strings.Replace(validBodyImage, "1}", "-1}", 1) + `]}`},
		{"position outside text", `{"coverImage":null,"bodyImages":[` + strings.Replace(validBodyImage, "1}", "2}", 1) + `]}`},
		{"positions out of order", `{"coverImage":null,"bodyImages":[` + validBodyImage + "," + strings.Replace(validBodyImage, "1}", "0}", 1) + `]}`},
		{"oversize caption", `{"coverImage":null,"bodyImages":[` + strings.TrimSuffix(validBodyImage, "}") + `,"caption":"` + strings.Repeat("я", 241) + `"}]}`},
		{"too many illustrations", `{"coverImage":null,"bodyImages":[` + strings.Repeat(validBodyImage+",", 10) + validBodyImage + `]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := bannerRawRequest(t, handler, http.MethodPost, testAdminArticlesPath, basePayload+`,"media":`+test.media+"}", moderator, "fixture-csrf")
			assertCartStatus(t, response, http.StatusBadRequest)
		})
	}
	assertArticleList(t, handler, testAdminArticlesPath, moderator)
}

func TestFeaturedArticlesReplaceOnlyWithPublishedArticles(t *testing.T) {
	pool := testCartDatabase(t, "migrations/0004_articles.sql", "migrations/0005_article_media.sql")
	key := []byte("test-only")
	handler := newHandler(pool, nil, &accountAuth{db: pool, fullDB: pool, key: key}, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	save := func(path, method string, published bool) articleResponseDetail {
		return readArticleDetail(t, bannerRawRequest(t, handler, method, path,
			articlePayload(t, "Заголовок", "", "Текст", published), moderator, "fixture-csrf"))
	}
	first := save(testAdminArticlesPath, http.MethodPost, true).ID
	second := save(testAdminArticlesPath, http.MethodPost, true).ID
	third := save(testAdminArticlesPath, http.MethodPost, true).ID
	draft := save(testAdminArticlesPath, http.MethodPost, false).ID
	featuredPath := testAdminArticlesPath + "/featured"
	update := func(ids ...string) {
		t.Helper()
		payload, err := json.Marshal(map[string][]string{"ids": ids})
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Status string `json:"status"`
			Data   struct {
				IDs []string `json:"ids"`
			} `json:"data"`
		}
		readArticleResponse(t, bannerRawRequest(t, handler, http.MethodPut, featuredPath, string(payload), moderator, "fixture-csrf"), &response)
		if response.Status != "success" || response.Data.IDs == nil || !slices.Equal(response.Data.IDs, ids) {
			t.Fatalf("unexpected featured response: %+v", response)
		}
	}
	hasPositions := func(ids []string, positions ...int) {
		t.Helper()
		list := assertArticleList(t, handler, testArticlesPath, nil, ids...)
		for index, position := range positions {
			got := list[index].FeaturedPosition
			if (position == 0 && got != nil) || (position != 0 && (got == nil || *got != position)) {
				t.Fatalf("article %s featuredPosition = %v, want %d (0 means null)", list[index].ID, got, position)
			}
		}
	}

	update(first, third, second)
	hasPositions([]string{third, second, first}, 2, 3, 1)
	update(second, first)
	hasPositions([]string{third, second, first}, 0, 1, 2)
	for _, test := range []struct{ name, payload string }{
		{"missing ids", `{}`},
		{"null ids", `{"ids":null}`},
		{"invalid id", `{"ids":["not-a-uuid"]}`},
		{"duplicate ids", `{"ids":["` + third + `","` + strings.ToUpper(third) + `"]}`},
		{"unknown id", `{"ids":["` + third + `","` + uuid.NewString() + `"]}`},
		{"draft", `{"ids":["` + third + `","` + draft + `"]}`},
		{"too many", `{"ids":["` + third + `","` + second + `","` + first + `","` + draft + `"]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPut, featuredPath, test.payload, moderator, "fixture-csrf"), http.StatusBadRequest)
			hasPositions([]string{third, second, first}, 0, 1, 2)
		})
	}
	if detail := save(testAdminArticlesPath+"/"+second, http.MethodPut, false); detail.FeaturedPosition != nil {
		t.Fatal("unpublished article is still featured in the save response")
	}
	assertPublicArticleMissing(t, handler, testArticlesPath+"/"+second)
	hasPositions([]string{third, first}, 0, 2)
	save(testAdminArticlesPath+"/"+second, http.MethodPut, true)
	hasPositions([]string{second, third, first}, 0, 0, 2)
	update([]string{}...)
	hasPositions([]string{second, third, first}, 0, 0, 0)
}
