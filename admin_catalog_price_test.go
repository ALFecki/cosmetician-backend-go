package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const catalogPricePath = "/api/admin/catalog-prices"

func TestCatalogPriceCapabilityRequiresAdmin(t *testing.T) {
	key := []byte("test-only")
	handler := catalogWriteTestHandler(&accountAuth{key: key}, nilCartDatabase{})
	admin := profileCookie(t, key, testAdminID, true, false, false)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, catalogPricePath},
		{http.MethodPost, catalogPricePath + "/preview"},
		{http.MethodPost, catalogPricePath + "/apply"},
	} {
		assertCartStatus(t, bannerRawRequest(t, handler, route.method, route.path, "{}", moderator, "fixture-csrf"), http.StatusUnauthorized)
		assertCartStatus(t, bannerRawRequest(t, handler, route.method, route.path, "{}", admin, "wrong-csrf"), http.StatusUnauthorized)
	}
	response := bannerRawRequest(t, handler, http.MethodGet, catalogPricePath, "", admin, "fixture-csrf")
	assertCartStatus(t, response, http.StatusOK)
	var reply struct {
		Status string
		Data   struct{ Enabled bool }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil || reply.Status != "success" || !reply.Data.Enabled {
		t.Fatalf("capability response = %s, error = %v", response.Body, err)
	}
}

func TestCatalogPriceApplyUpdatesEachVariantOnce(t *testing.T) {
	const (
		brandID  = "00000000-0000-4000-8000-000000000101"
		variantA = "00000000-0000-4000-8000-000000000601"
		variantB = "00000000-0000-4000-8000-000000000602"
		archived = "00000000-0000-4000-8000-000000000603"
	)
	pool := testCartDatabase(t)
	request := catalogPriceTestRequest(t, pool)
	if err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "UPDATE product_variant SET price = 99.9 WHERE id = $1", uuid.MustParse(variantA))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	input := fmt.Sprintf(`"brandId":%q,"changeBasisPoints":500`, brandID)
	preview := catalogPriceTestData(t, request("/preview", "{"+input+"}"), http.StatusOK)
	var plan struct {
		OperationID  uuid.UUID
		Revision     string
		ChangedCount int
		Items        []struct {
			VariantID     string
			OldPriceCents int64
			NewPriceCents int64
		}
	}
	if err := json.Unmarshal(preview, &plan); err != nil || plan.OperationID == uuid.Nil || plan.Revision == "" ||
		plan.ChangedCount != 2 || len(plan.Items) != 2 ||
		plan.Items[0].VariantID != variantA || plan.Items[0].OldPriceCents != 9990 || plan.Items[0].NewPriceCents != 10490 ||
		plan.Items[1].VariantID != variantB || plan.Items[1].OldPriceCents != 2990 || plan.Items[1].NewPriceCents != 3140 {
		t.Fatalf("preview = %s, error = %v", preview, err)
	}
	apply := fmt.Sprintf(`{%s,"operationId":%q,"revision":%q}`, input, plan.OperationID, plan.Revision)
	for range 2 {
		var receipt struct{ UpdatedCount int }
		result := catalogPriceTestData(t, request("/apply", apply), http.StatusOK)
		if err := json.Unmarshal(result, &receipt); err != nil || receipt.UpdatedCount != 2 {
			t.Fatalf("receipt = %s, error = %v", result, err)
		}
	}

	for _, want := range []struct {
		id           string
		retail, cost int64
		revisions    int
	}{
		{variantA, 10490, 6005, 2},
		{variantB, 3140, 1930, 1},
		{archived, 20000, 9000, 0},
	} {
		var retail, cost int64
		var revisions int
		err := pool.QueryRow(t.Context(), `SELECT price_cents, cost_price_cents,
			(SELECT count(*) FROM product_variant_price_history WHERE variant_id = v.id)
			FROM product_variant v WHERE id = $1`, uuid.MustParse(want.id)).Scan(&retail, &cost, &revisions)
		if err != nil || retail != want.retail || cost != want.cost || revisions != want.revisions {
			t.Fatalf("variant %s: retail=%d cost=%d revisions=%d error=%v", want.id, retail, cost, revisions, err)
		}
	}
	ctx := context.WithValue(t.Context(), promotionPricingKey{}, true)
	courier := false
	quote, err := quoteCart(ctx, pool, quoteRequest{Items: []cartItem{{VariantID: uuid.MustParse(variantA), Quantity: 1}}, IsCourierDelivery: &courier})
	if err != nil || quote.SubtotalCents != 10490 || quote.PromotionDiscountCents != 2150 || quote.TotalCents != 8340 {
		t.Fatalf("quote subtotal=%d discount=%d total=%d, error=%v", quote.SubtotalCents, quote.PromotionDiscountCents, quote.TotalCents, err)
	}
}

func TestCatalogPriceConcurrentOperationsAndReceiptRetry(t *testing.T) {
	pool := testCartDatabase(t)
	request := catalogPriceTestRequest(t, pool)
	const input = `"productIds":["00000000-0000-4000-8000-000000000501"],"changeBasisPoints":500`
	first := catalogPriceTestApplyBody(t, request, input)
	second := catalogPriceTestApplyBody(t, request, input)

	type applyResponse struct {
		index int
		*httptest.ResponseRecorder
	}
	responses := make(chan applyResponse, 3)
	for i, body := range []string{first, first, second} {
		go func() {
			responses <- applyResponse{i, request("/apply", body)}
		}()
	}
	var statuses [3]int
	for range statuses {
		response := <-responses
		statuses[response.index] = response.Code
	}
	if statuses != [3]int{http.StatusOK, http.StatusOK, http.StatusConflict} &&
		statuses != [3]int{http.StatusConflict, http.StatusConflict, http.StatusOK} {
		t.Fatalf("concurrent statuses = %v", statuses)
	}
	var price int64
	var receipts, revisions int
	err := pool.QueryRow(t.Context(), `SELECT price_cents, (SELECT count(*) FROM catalog_price_operation),
		(SELECT count(*) FROM product_variant_price_history WHERE variant_id = v.id)
		FROM product_variant v WHERE id = '00000000-0000-4000-8000-000000000601'`).Scan(&price, &receipts, &revisions)
	if err != nil || price != 9464 || receipts != 1 || revisions != 1 {
		t.Fatalf("price=%d receipts=%d revisions=%d, error=%v", price, receipts, revisions, err)
	}
}

func TestCatalogPriceApplyRejectsRevisionAfterLegacyPriceReverts(t *testing.T) {
	pool := testCartDatabase(t)
	request := catalogPriceTestRequest(t, pool)
	const input = `"productIds":["00000000-0000-4000-8000-000000000501"],"changeBasisPoints":500`
	apply := catalogPriceTestApplyBody(t, request, input)
	catalogPriceFixtureWrite(t, pool, `UPDATE product_variant SET price = 90.14 WHERE id = '00000000-0000-4000-8000-000000000601';
		UPDATE product_variant SET price = 90.13 WHERE id = '00000000-0000-4000-8000-000000000601';`)
	assertCartStatus(t, request("/apply", apply), http.StatusConflict)
}

func TestCatalogPriceApplyRollsBackPriceAndHistoryOnReceiptFailure(t *testing.T) {
	pool := testCartDatabase(t)
	request := catalogPriceTestRequest(t, pool)
	const input = `"productIds":["00000000-0000-4000-8000-000000000501"],"changeBasisPoints":500`
	apply := catalogPriceTestApplyBody(t, request, input)
	catalogPriceFixtureWrite(t, pool, `ALTER TABLE catalog_price_operation ADD CONSTRAINT fail_receipt CHECK (updated_count = 0)`)
	assertCartStatus(t, request("/apply", apply), http.StatusBadRequest)
	var prices []int64
	var revisions int
	err := pool.QueryRow(t.Context(), `SELECT array_agg(price_cents ORDER BY id),
		(SELECT count(*) FROM product_variant_price_history)
		FROM product_variant WHERE product_id = '00000000-0000-4000-8000-000000000501'`).Scan(&prices, &revisions)
	if err != nil || len(prices) != 3 || prices[0] != 9013 || prices[1] != 2990 || prices[2] != 20000 || revisions != 0 {
		t.Fatalf("prices=%v revisions=%d, error=%v", prices, revisions, err)
	}
	catalogPriceFixtureWrite(t, pool, `ALTER TABLE catalog_price_operation DROP CONSTRAINT fail_receipt`)
	assertCartStatus(t, request("/apply", apply), http.StatusOK)
}

func TestCatalogPriceMigrationPreservesPlacedOrderLines(t *testing.T) {
	pool := testCartDatabase(t, "testdata/catalog_price_legacy_rows.sql")
	request := catalogPriceTestRequest(t, pool)
	const input = `"productIds":["00000000-0000-4000-8000-000000000501","00000000-0000-4000-8000-000000000504"],"changeBasisPoints":500`
	apply := catalogPriceTestApplyBody(t, request, input)
	result := catalogPriceTestData(t, request("/apply", apply), http.StatusOK)
	if string(result) != `{"updatedCount":2}` {
		t.Fatalf("unexpected receipt includes archived product: %s", result)
	}

	placed, err := readCommerceOrder(t.Context(), pool, uuid.MustParse("00000000-0000-4000-8000-000000000901"))
	if err != nil || placed.TotalPrice != 222.23 || placed.PaymentStatus != "Completed" || len(placed.Products) != 3 {
		t.Fatalf("placed order total=%v payment=%s lines=%d, error=%v", placed.TotalPrice, placed.PaymentStatus, len(placed.Products), err)
	}
	want := map[string][2]float32{
		"00000000-0000-4000-8000-000000000601": {90.13, 60.05},
		"00000000-0000-4000-8000-000000000602": {21.5, 13.25},
		"00000000-0000-4000-8000-000000000603": {20.2, 10.1},
	}
	for _, line := range placed.Products {
		prices := [2]float32{line.HistoricalPrice, line.HistoricalCostPrice}
		if expected := want[line.Variant.ID.String()]; expected != prices {
			t.Fatalf("variant %s prices=%v want=%v", line.Variant.ID, prices, expected)
		}
	}
	var unmodified bool
	err = pool.QueryRow(t.Context(), `SELECT unit_price IS NULL AND unit_cost_price IS NULL
		FROM order_product WHERE order_id = '00000000-0000-4000-8000-000000000902'`).Scan(&unmodified)
	if err != nil || !unmodified {
		t.Fatalf("draft order snapshots changed, error=%v", err)
	}
}

func TestCatalogPricePreviewValidationAndNoChange(t *testing.T) {
	pool := testCartDatabase(t)
	request := catalogPriceTestRequest(t, pool)
	const brand = `"brandId":"00000000-0000-4000-8000-000000000101"`
	for _, invalid := range []string{
		`{"changeBasisPoints":500}`,
		`{` + brand + `,"productIds":[],"changeBasisPoints":500}`,
		`{"productIds":[],"changeBasisPoints":500}`,
		`{` + brand + `,"changeBasisPoints":0}`,
		`{` + brand + `,"changeBasisPoints":-10001}`,
		`{` + brand + `,"changeBasisPoints":1.5}`,
		`{` + brand + `,"changeBasisPoints":2147483648}`,
		`{` + brand + `,"changeBasisPoints":2147483647}`,
	} {
		assertCartStatus(t, request("/preview", invalid), http.StatusBadRequest)
	}
	catalogPriceFixtureWrite(t, pool, `UPDATE product_variant SET price = 0.01
		WHERE product_id = '00000000-0000-4000-8000-000000000501' AND is_deleted IS NOT TRUE`)
	apply := catalogPriceTestApplyBody(t, request, brand+`,"changeBasisPoints":1`)
	result := catalogPriceTestData(t, request("/apply", apply), http.StatusOK)
	if string(result) != `{"updatedCount":0}` {
		t.Fatalf("rounded no-op receipt = %s", result)
	}
	var revisions int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM product_variant_price_history").Scan(&revisions); err != nil || revisions != 2 {
		t.Fatalf("rounded no-op created history: revisions=%d error=%v", revisions, err)
	}
}

func TestCatalogPriceRejectsPricesThatCannotBeMirroredForRollback(t *testing.T) {
	pool := testCartDatabase(t)
	catalogPriceFixtureWrite(t, pool, `UPDATE product_variant SET price = 131072
		WHERE id = '00000000-0000-4000-8000-000000000601'`)
	request := catalogPriceTestRequest(t, pool)
	// 131072.00 increased by 0.02% rounds to 131098.21. REAL rounds to 131098.203125.
	assertCartStatus(t, request("/preview", `{"brandId":"00000000-0000-4000-8000-000000000101","changeBasisPoints":2}`), http.StatusBadRequest)

	key := []byte("test-only")
	handler := catalogWriteTestHandler(&accountAuth{db: pool, key: key}, pool)
	admin := profileCookie(t, key, testAdminID, true, false, false)
	response := bannerRawRequest(t, handler, http.MethodPatch, "/api/product_variant/00000000-0000-4000-8000-000000000601",
		`{"price":131098.21}`, admin, "fixture-csrf")
	assertCartStatus(t, response, http.StatusBadRequest)
}

func catalogPriceTestRequest(t *testing.T, pool *pgxpool.Pool) func(string, string) *httptest.ResponseRecorder {
	t.Helper()
	key := []byte("test-only")
	handler := catalogWriteTestHandler(&accountAuth{db: pool, key: key}, pool)
	admin := profileCookie(t, key, testAdminID, true, false, false)
	return func(path, body string) *httptest.ResponseRecorder {
		return bannerRawRequest(t, handler, http.MethodPost, catalogPricePath+path, body, admin, "fixture-csrf")
	}
}

func catalogPriceTestData(t *testing.T, response *httptest.ResponseRecorder, status int) json.RawMessage {
	t.Helper()
	assertCartStatus(t, response, status)
	var reply struct {
		Status string
		Data   json.RawMessage
	}
	if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil || reply.Status != "success" {
		t.Fatalf("catalog price response = %s, error = %v", response.Body, err)
	}
	return reply.Data
}

func catalogPriceTestApplyBody(t *testing.T, request func(string, string) *httptest.ResponseRecorder, input string) string {
	t.Helper()
	result := catalogPriceTestData(t, request("/preview", "{"+input+"}"), http.StatusOK)
	var plan struct {
		OperationID uuid.UUID
		Revision    string
	}
	if err := json.Unmarshal(result, &plan); err != nil || plan.OperationID == uuid.Nil || plan.Revision == "" {
		t.Fatalf("invalid preview = %s, error = %v", result, err)
	}
	return fmt.Sprintf(`{%s,"operationId":%q,"revision":%q}`, input, plan.OperationID, plan.Revision)
}

func catalogPriceFixtureWrite(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), sql)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
