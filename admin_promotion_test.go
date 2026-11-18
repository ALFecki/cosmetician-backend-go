package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const adminPromotionPath = "/api/admin/promotion"
const promotionProductA = "00000000-0000-4000-8000-000000000501"
const promotionProductB = "00000000-0000-4000-8000-000000000502"
const promotionArchivedProduct = "00000000-0000-4000-8000-000000000504"
const promotionMissingProduct = "00000000-0000-4000-8000-000000000599"
const promotionLegacyID = "00000000-0000-4000-8000-000000000709"

func TestAdminPromotionAuthorization(t *testing.T) {
	key := []byte("test-only")
	handler := newHandler(nil, nil, &accountAuth{key: key, promotionDB: nilCartDatabase{}}, false)
	customer := profileCookie(t, key, testAccountID, false, false, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, adminPromotionPath},
		{http.MethodGet, adminPromotionPath + "/products?q=test"},
		{http.MethodPost, adminPromotionPath},
		{http.MethodPut, adminPromotionPath + "/" + promotionLegacyID},
		{http.MethodDelete, adminPromotionPath + "/" + promotionLegacyID},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			for _, user := range []*http.Cookie{nil, customer, moderator} {
				csrf := "fixture-csrf"
				if user == moderator {
					csrf = "wrong"
				}
				assertCartStatus(t, bannerRawRequest(t, handler, route.method, route.path, "{}", user, csrf), http.StatusUnauthorized)
			}
		})
	}
	disabled := newHandler(nil, nil, &accountAuth{key: key}, false)
	assertCartStatus(t, newCartRequest(t, disabled, http.MethodGet, adminPromotionPath, nil, moderator), http.StatusNotFound)
}

func TestAdminPromotionLifecycle(t *testing.T) {
	pool, handler, admin := adminPromotionFixture(t)
	var result struct {
		Status string `json:"status"`
		Data   struct {
			ID            string   `json:"id"`
			Title         string   `json:"title"`
			Description   *string  `json:"description"`
			DiscountType  string   `json:"discountType"`
			DiscountValue float32  `json:"discountValue"`
			TargetType    string   `json:"targetType"`
			ProductIDs    []string `json:"productIds"`
			ValidFrom     *string  `json:"validFrom"`
			ValidUntil    *string  `json:"validUntil"`
			IsDraft       bool     `json:"isDraft"`
			IsActive      bool     `json:"isActive"`
		} `json:"data"`
	}
	draft := bannerRawRequest(t, handler, http.MethodPost, adminPromotionPath, `{"title":"Новая акция","isDraft":true,"description":"Тест"}`, admin, "fixture-csrf")
	readOrderHistory(t, draft, &result)
	if result.Status != "success" || result.Data.ID == "" || !result.Data.IsDraft || result.Data.IsActive ||
		result.Data.ValidFrom != nil || result.Data.ValidUntil != nil || result.Data.DiscountValue != 0 || len(result.Data.ProductIDs) != 0 {
		t.Fatalf("invalid title-only draft: %s", draft.Body)
	}
	id := result.Data.ID
	path := adminPromotionPath + "/" + id
	adminList := newCartRequest(t, handler, http.MethodGet, adminPromotionPath, nil, admin)
	assertCartStatus(t, adminList, http.StatusOK)
	if !strings.Contains(adminList.Body.String(), id) {
		t.Fatal("admin list is missing the draft")
	}
	for _, publicHandler := range []http.Handler{handler, newHandler(pool, nil, nil, false)} {
		publicList := newCartRequest(t, publicHandler, http.MethodGet, "/api/promotion", nil, nil)
		assertCartStatus(t, publicList, http.StatusOK)
		if strings.Contains(publicList.Body.String(), id) {
			t.Fatal("public list includes a draft")
		}
		assertCartStatus(t, newCartRequest(t, publicHandler, http.MethodGet, "/api/promotion/"+id, nil, nil), http.StatusNotFound)
	}

	var active bool
	if err := pool.QueryRow(t.Context(), `SELECT is_active FROM promotion WHERE id = $1::text::uuid`, id).Scan(&active); err != nil || active {
		t.Fatalf("legacy readers may treat a draft as active: is_active=%v, err=%v", active, err)
	}

	body := `{"title":"На два товара","targetType":"product","productIds":["%s","%s"],"discountType":"fixed","discountValue":2.25,"validFrom":"2020-01-01T03:00:00+03:00","validUntil":"2099-01-01T00:00:00Z","isDraft":false,"isActive":true}`
	publish := bannerRawRequest(t, handler, http.MethodPut, path, fmt.Sprintf(body, promotionProductA, promotionProductB), admin, "fixture-csrf")
	readOrderHistory(t, publish, &result)
	if result.Data.IsDraft || !result.Data.IsActive || result.Data.DiscountType != "fixed" || result.Data.DiscountValue != 2.25 ||
		result.Data.Description == nil || *result.Data.Description != "Тест" || result.Data.ValidFrom == nil || *result.Data.ValidFrom != "2020-01-01T00:00:00Z" ||
		strings.Join(result.Data.ProductIDs, ",") != promotionProductA+","+promotionProductB {
		t.Fatalf("unexpected published campaign: %s", publish.Body)
	}

	invalid := strings.Replace(fmt.Sprintf(body, promotionProductA, promotionMissingProduct), "На два товара", "Невалидная акция", 1)
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPut, path, invalid, admin, "fixture-csrf"), http.StatusBadRequest)
	var title string
	var links int
	if err := pool.QueryRow(t.Context(), `SELECT title, (SELECT count(*) FROM product_promotion WHERE promotion_id = p.id) FROM promotion p WHERE id = $1::text::uuid`, id).Scan(&title, &links); err != nil || title != "На два товара" || links != 2 {
		t.Fatalf("failed edit changed title or relations: title=%q, links=%d, err=%v", title, links, err)
	}

	scheduled := `{"title":"На весь каталог","description":null,"targetType":"all","productIds":[],"discountType":"percentage","discountValue":15,"validFrom":"2100-01-01T00:00:00Z","validUntil":"2199-01-01T00:00:00Z","isDraft":false,"isActive":true}`
	readOrderHistory(t, bannerRawRequest(t, handler, http.MethodPut, path, scheduled, admin, "fixture-csrf"), &result)
	if !result.Data.IsActive || result.Data.IsDraft || result.Data.Description != nil || len(result.Data.ProductIDs) != 0 || result.Data.TargetType != "all" || result.Data.ValidFrom == nil || *result.Data.ValidFrom != "2100-01-01T00:00:00Z" {
		t.Fatalf("unexpected scheduled campaign: %+v", result.Data)
	}

	pause := strings.Replace(scheduled, `"isActive":true`, `"isActive":false`, 1)
	readOrderHistory(t, bannerRawRequest(t, handler, http.MethodPut, path, pause, admin, "fixture-csrf"), &result)
	if result.Data.IsActive || result.Data.IsDraft {
		t.Fatalf("unexpected paused campaign: %+v", result.Data)
	}

	var list struct {
		Status string `json:"status"`
		Length int    `json:"length"`
		Data   []struct {
			ID      string `json:"id"`
			IsDraft *bool  `json:"isDraft"`
		} `json:"data"`
	}
	readOrderHistory(t, newCartRequest(t, handler, http.MethodGet, adminPromotionPath, nil, admin), &list)
	if list.Status != "success" || list.Length != 7 || list.Data[0].ID != id || list.Data[0].IsDraft == nil || *list.Data[0].IsDraft {
		t.Fatalf("unexpected campaign list: %+v", list)
	}
	var deleted map[string]string
	readOrderHistory(t, newCartRequest(t, handler, http.MethodDelete, path, nil, admin), &deleted)
	if deleted["status"] != "success" || len(deleted) != 1 {
		t.Fatalf("unexpected delete response: %v", deleted)
	}
	assertCartStatus(t, newCartRequest(t, handler, http.MethodDelete, path, nil, admin), http.StatusNotFound)
}

func TestAdminPromotionLegacyTargetPause(t *testing.T) {
	pool, handler, admin := adminPromotionFixture(t)
	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO promotion (id, title, description, discount_type, discount_value,
			target_type, target_ids, priority, min_quantity, buy_quantity, get_quantity, valid_from, valid_until, is_active, created_at, updated_at)
			VALUES ('00000000-0000-4000-8000-000000000709', 'Legacy', 'Description', 'fixed', 5, 'brand',
			ARRAY['00000000-0000-4000-8000-000000000101']::uuid[], 9, 2, 3, 1,
			'2020-01-01T00:00:00.111111Z', '2099-01-01T00:00:00Z', true, now(), now());
			INSERT INTO product_promotion VALUES ('00000000-0000-4000-8000-000000000501','00000000-0000-4000-8000-000000000709');`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	path := adminPromotionPath + "/" + promotionLegacyID
	body := `{"title":"Legacy","discountType":"fixed","discountValue":5,"targetType":"brand","productIds":["` + promotionProductA + `"],"validFrom":"2020-01-01T00:00:00.111111Z","validUntil":"2099-01-01T00:00:00Z","isDraft":false,"isActive":false}`
	invalid := strings.Replace(body, `"targetType":"brand"`, `"targetType":"product"`, 1)
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPut, path, invalid, admin, "fixture-csrf"), http.StatusBadRequest)
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPut, path, body, admin, "fixture-csrf"), http.StatusOK)
	var preserved bool
	if err := pool.QueryRow(t.Context(), `SELECT NOT p.is_active AND NOT p.is_draft AND p.title = 'Legacy'
		AND p.description = 'Description' AND p.target_type = 'brand' AND p.priority = 9
		AND p.discount_value = 5 AND p.min_quantity = 2 AND p.buy_quantity = 3 AND p.get_quantity = 1
		AND p.target_ids = ARRAY['00000000-0000-4000-8000-000000000101']::uuid[]
		AND p.valid_from = '2020-01-01T00:00:00.111111Z' AND p.valid_until = '2099-01-01T00:00:00Z'
		AND EXISTS (SELECT 1 FROM product_promotion WHERE promotion_id = p.id AND product_id = '00000000-0000-4000-8000-000000000501')
		FROM promotion p WHERE id = $1::text::uuid`, promotionLegacyID).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("legacy pause overwrote stored fields: preserved=%v, err=%v", preserved, err)
	}
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPut, path, strings.Replace(body, `"isActive":false`, `"isActive":true`, 1), admin, "fixture-csrf"), http.StatusBadRequest)
}

func TestAdminPromotionValidation(t *testing.T) {
	_, handler, admin := adminPromotionFixture(t)
	valid := `{"title":"Акция","discountType":"percentage","discountValue":10,"targetType":"all","productIds":[],"validFrom":"2020-01-01T00:00:00Z","validUntil":"2099-01-01T00:00:00Z","isDraft":false,"isActive":true}`
	for _, tc := range []struct{ name, body string }{
		{"empty title", strings.Replace(valid, "Акция", " ", 1)},
		{"percentage exceeds 100", strings.Replace(valid, `"discountValue":10`, `"discountValue":101`, 1)},
		{"percentage rounds to zero", strings.Replace(valid, `"discountValue":10`, `"discountValue":0.004`, 1)},
		{"published with zero discount", strings.Replace(valid, `"discountValue":10`, `"discountValue":0`, 1)},
		{"invalid dates", strings.Replace(valid, `2099-01-01`, `2000-01-01`, 1)},
		{"published without date", strings.Replace(valid, `"2099-01-01T00:00:00Z"`, "null", 1)},
		{"product scope without products", strings.Replace(valid, `"targetType":"all"`, `"targetType":"product"`, 1)},
		{"unsupported scope", strings.Replace(valid, `"targetType":"all"`, `"targetType":"brand"`, 1)},
		{"draft cannot be active", strings.Replace(valid, `"isDraft":false`, `"isDraft":true`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPost, adminPromotionPath, tc.body, admin, "fixture-csrf"), http.StatusBadRequest)
		})
	}
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPut, adminPromotionPath+"/00000000-0000-4000-8000-000000000701",
		`{"isActive":false}`, admin, "fixture-csrf"), http.StatusBadRequest)
	fixed := strings.Replace(valid, `"discountType":"percentage"`, `"discountType":"fixed"`, 1)
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPost, adminPromotionPath,
		strings.Replace(fixed, `"discountValue":10`, `"discountValue":0.004`, 1), admin, "fixture-csrf"), http.StatusBadRequest)
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPost, adminPromotionPath,
		strings.Replace(fixed, `"discountValue":10`, `"discountValue":0.005`, 1), admin, "fixture-csrf"), http.StatusOK)
}

func TestAdminPromotionProductSearch(t *testing.T) {
	pool, handler, admin := adminPromotionFixture(t)
	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE product_variant SET price = 0.01 WHERE id = '00000000-0000-4000-8000-000000000603';
			UPDATE product_variant SET price = 0 WHERE id = '00000000-0000-4000-8000-000000000604';
			INSERT INTO product_variant (id, product_id, volume, price, cost_price, is_default, is_deleted, created_at)
			VALUES ('00000000-0000-4000-8000-000000000607', '00000000-0000-4000-8000-000000000504', 15, 10, 8, true, false, now());`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	type match struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		FullTitle  string `json:"fullTitle"`
		BrandTitle string `json:"brandTitle"`
		PriceCents *int64 `json:"priceCents"`
	}
	get := func(query string) []match {
		t.Helper()
		var result listResponse[match]
		readOrderHistory(t, newCartRequest(t, handler, http.MethodGet, adminPromotionPath+"/products"+query, nil, admin), &result)
		if result.Status != "success" || result.Length != len(result.Data) || result.Data == nil {
			t.Fatalf("invalid search envelope: %+v", result)
		}
		return result.Data
	}
	matches := get("?q=" + url.QueryEscape("КРЕМ"))
	if len(matches) != 1 || matches[0].ID != promotionProductA || matches[0].Title != "Альфа" ||
		matches[0].FullTitle != "Альфа: крем для рук" || matches[0].BrandTitle == "" || matches[0].PriceCents == nil || *matches[0].PriceCents != 2990 {
		t.Fatalf("search by full title returned wrong base price or product: %+v", matches)
	}
	if matches = get("?q=" + url.QueryEscape(matches[0].BrandTitle)); len(matches) != 2 {
		t.Fatalf("brand search should find two active products: %+v", matches)
	}
	if matches = get("?q=" + url.QueryEscape("Архив")); len(matches) != 0 {
		t.Fatalf("search returned archived rows: %+v", matches)
	}
	if matches = get("?ids=" + promotionArchivedProduct + "," + promotionProductA); len(matches) != 2 || matches[0].ID != promotionArchivedProduct || matches[0].PriceCents != nil {
		t.Fatalf("selected-ID lookup lost an archived product or reported a missing price: %+v", matches)
	}
	if matches = get("?ids=" + promotionProductB); len(matches) != 1 || matches[0].PriceCents == nil || *matches[0].PriceCents != 0 {
		t.Fatalf("a product priced at zero was reported as missing a price: %+v", matches)
	}
	if matches = get(""); len(matches) != 3 {
		t.Fatalf("initial preview missed active products: %+v", matches)
	}
	for _, query := range []string{"?ids=broken", "?q=alpha&ids=" + promotionProductA, "?q=" + strings.Repeat("x", 121), "?ids=" + strings.Repeat(promotionProductA+",", 200) + promotionProductA} {
		assertCartStatus(t, newCartRequest(t, handler, http.MethodGet, adminPromotionPath+"/products"+query, nil, admin), http.StatusBadRequest)
	}
}

func adminPromotionFixture(t *testing.T) (*pgxpool.Pool, http.Handler, *http.Cookie) {
	t.Helper()
	pool := testCartDatabase(t, "migrations/0002_promotion_draft.sql")
	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `ALTER TABLE product_promotion
			ADD FOREIGN KEY (product_id) REFERENCES product(id) ON DELETE CASCADE,
			ADD FOREIGN KEY (promotion_id) REFERENCES promotion(id) ON DELETE CASCADE`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("test-only")
	handler := newHandler(pool, nil, &accountAuth{db: pool, promotionDB: pool, key: key}, false)
	return pool, handler, profileCookie(t, key, testModeratorID, false, true, false)
}
