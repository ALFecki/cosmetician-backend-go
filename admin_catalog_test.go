package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestAdminCatalogPermissions(t *testing.T) {
	key := []byte("test-only")
	handler := catalogWriteTestHandler(&accountAuth{key: key}, nilCartDatabase{})
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	customer := profileCookie(t, key, testAccountID, false, false, false)
	for _, tc := range []struct {
		method, path, csrf string
		cookie             *http.Cookie
	}{
		{"POST", "/api/product", "", nil},
		{"POST", "/api/product_variant", "fixture-csrf", customer},
		{"PATCH", "/api/brand/" + uuid.NewString(), "wrong", moderator},
		{"POST", "/api/product_scope", "fixture-csrf", moderator},
		{"DELETE", "/api/category/cascade/" + uuid.NewString(), "fixture-csrf", moderator},
	} {
		assertCartStatus(t, bannerRawRequest(t, handler, tc.method, tc.path, "{}", tc.cookie, tc.csrf), http.StatusUnauthorized)
	}
}

func TestAdminCatalogWriteLifecycle(t *testing.T) {
	pool := testCartDatabase(t, "testdata/admin_catalog_constraints.sql")
	key := []byte("test-only")
	handler := catalogWriteTestHandler(&accountAuth{db: pool, key: key}, pool)
	admin := profileCookie(t, key, testAdminID, true, false, false)
	makeRequest := func(t *testing.T, method, path, body string) map[string]any {
		t.Helper()
		response := bannerRawRequest(t, handler, method, path, body, admin, "fixture-csrf")
		assertCartStatus(t, response, http.StatusOK)
		if response.Body.Len() == 0 {
			return nil
		}
		var wrapper struct {
			Status string         `json:"status"`
			Data   map[string]any `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wrapper); err != nil || wrapper.Status != "success" {
			t.Fatalf("invalid response for %s %s: %s", method, path, response.Body)
		}
		return wrapper.Data
	}

	for _, endpoint := range []string{"brand", "category", "product_scope", "product_type", "country"} {
		t.Run(endpoint, func(t *testing.T) {
			path := "/api/" + endpoint
			body := `{"title":"Новый справочник"}`
			field, value := "title", "Новое название"
			patch := `{"title":"Новое название"}`
			if endpoint == "country" {
				body, patch = `{"name":"Новая страна","code":"ZZ"}`, `{"code":"ZY"}`
				field, value = "code", "ZY"
			}
			created := makeRequest(t, "POST", path, body)
			if (endpoint == "brand" || endpoint == "category") && created["description"] != "" {
				t.Fatalf("missing optional description must remain readable: %#v", created)
			}
			id, ok := created["id"].(string)
			if !ok {
				t.Fatal("missing generated ID")
			}
			if edited := makeRequest(t, "PATCH", path+"/"+id, patch); edited[field] != value {
				t.Fatalf("unexpected edited dictionary: %#v", edited)
			}
			if endpoint == "country" {
				assertCartStatus(t, bannerRawRequest(t, handler, "POST", path, `{"name":"Другая страна","code":"ZY"}`, admin, "fixture-csrf"), http.StatusConflict)
			}
			makeRequest(t, "DELETE", path+"/"+id, "")
			if restored := makeRequest(t, "GET", path+"/"+id, ""); endpoint != "product_type" && endpoint != "product_scope" && restored["isDeleted"] != true {
				t.Fatalf("archive erased row instead of preserving it: %#v", restored)
			}
			makeRequest(t, "DELETE", path+"/cascade/"+id, "")
			assertCartStatus(t, bannerRawRequest(t, handler, "GET", path+"/"+id, "", admin, "fixture-csrf"), http.StatusNotFound)
		})
	}

	const brandID = "00000000-0000-4000-8000-000000000101"
	const typeID = "00000000-0000-4000-8000-000000000401"
	const categoryID = "00000000-0000-4000-8000-000000000201"
	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "UPDATE brand SET description = NULL WHERE id = $1", uuid.MustParse(brandID)); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), "UPDATE category SET description = NULL WHERE id = $1", uuid.MustParse(categoryID))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	productBody := fmt.Sprintf(`{"title":"Сыворотка","fullTitle":"Сыворотка тест","description":"Описание","brandId":%q,"productTypeId":%q,"categoryIds":"[\"%s\"]","countryIds":"[]","productScopeIds":"[]","isPriceHidden":false}`, brandID, typeID, categoryID)
	created := makeRequest(t, "POST", "/api/product", productBody)
	productID := created["id"].(string)
	if categories := created["categories"].([]any); len(categories) != 1 || categories[0].(map[string]any)["id"] != categoryID ||
		categories[0].(map[string]any)["description"] != "" || created["brand"].(map[string]any)["description"] != "" {
		t.Fatalf("create did not return selected category and brand: %#v", created)
	}

	invalid := fmt.Sprintf(`{"title":"Потеря не допускается","categoryIds":"[\"%s\"]"}`, uuid.NewString())
	productPath := "/api/product/" + productID
	assertCartStatus(t, bannerRawRequest(t, handler, "PATCH", productPath, invalid, admin, "fixture-csrf"), http.StatusBadRequest)
	current := makeRequest(t, "GET", productPath, "")
	if current["title"] != "Сыворотка" || len(current["categories"].([]any)) != 1 {
		t.Fatalf("failed write changed product or links: %#v", current)
	}

	makeRequest(t, "DELETE", productPath, "")
	restored := makeRequest(t, "PATCH", productPath, `{"isDeleted":false,"categoryIds":"[]"}`)
	if restored["isDeleted"] != false || len(restored["categories"].([]any)) != 0 || restored["title"] != "Сыворотка" {
		t.Fatalf("restore and clear relations: %#v", restored)
	}

	makeVariant := func(price int) string {
		t.Helper()
		body := fmt.Sprintf(`{"productId":%q,"volume":30,"price":%d,"costPrice":10}`, productID, price)
		return makeRequest(t, "POST", "/api/product_variant", body)["id"].(string)
	}
	variantA, variantB := makeVariant(25), makeVariant(35)
	changed := makeRequest(t, "PATCH", "/api/product_variant/"+variantA, `{"price":20}`)
	if changed["price"] != float64(20) || makeRequest(t, "GET", "/api/product_variant/"+variantB, "")["price"] != float64(35) {
		t.Fatalf("edit must apply to a single variant: %#v", changed)
	}
	archivedPath := "/api/product_variant/" + variantB
	if archived := makeRequest(t, "DELETE", archivedPath, ""); archived["isDeleted"] != true {
		t.Fatalf("archive response missing archived variant: %#v", archived)
	}
	var open, closed int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE valid_to IS NULL), count(*) FILTER (WHERE valid_to IS NOT NULL)
		FROM product_variant_price_history WHERE variant_id = $1`, uuid.MustParse(variantB)).Scan(&open, &closed); err != nil || open != 0 || closed != 1 {
		t.Fatalf("price history after archive open=%d closed=%d, err=%v", open, closed, err)
	}

	err = pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `WITH saved_order AS (INSERT INTO "order" (person_id, status, total_price) VALUES ($1, 'Completed', 20) RETURNING id)
			INSERT INTO order_product (order_id, variant_id, quantity, unit_price) SELECT id, $2, 1, 20 FROM saved_order`, uuid.MustParse(testAccountID), uuid.MustParse(variantA))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/product_variant/cascade/" + variantA, "/api/product/cascade/" + productID} {
		assertCartStatus(t, bannerRawRequest(t, handler, "DELETE", path, "", admin, "fixture-csrf"), http.StatusConflict)
	}
	var retained int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM order_product WHERE variant_id = $1", uuid.MustParse(variantA)).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("historical order line was lost: retained=%d, err=%v", retained, err)
	}
	makeRequest(t, "DELETE", "/api/product_variant/cascade/"+variantB, "")
}

func catalogWriteTestHandler(auth *accountAuth, db cartDatabase) http.Handler {
	mux := http.NewServeMux()
	auth.registerCatalogWrites(mux, db)
	mux.Handle("/", newHandler(auth.db, nil, auth, false))
	return mux
}
