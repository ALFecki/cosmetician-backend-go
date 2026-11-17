package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestOrderHistoryRequiresSession(t *testing.T) {
	handler := newHandler(nil, nil, &accountAuth{key: []byte("test-only")}, false)
	for _, path := range []string{"/api/order", "/api/order/" + testAccountID, "/api/order_minimal", "/api/order_minimal/" + testAccountID} {
		response := profileRequest(handler, path, nil, "")
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d %q, want 401", path, response.Code, response.Body)
		}
	}
}

func TestOrderHistoryContract(t *testing.T) {
	_, tx := catalogFixture(t, "account_schema.sql", "account_rows.sql", "catalog_schema.sql", "catalog_rows.sql",
		"product_schema.sql", "product_rows.sql", "cart_quote_rows.sql", "cart_storage_schema.sql",
		"order_history_schema.sql", "order_history_rows.sql")
	key := []byte("test-only")
	handler := newHandler(tx, nil, &accountAuth{db: tx, key: key}, false)
	customer := profileCookie(t, key, testAccountID, false, false, false)
	admin := profileCookie(t, key, testAdminID, true, true, false)
	const (
		first    = "00000000-0000-4000-8000-000000000901"
		archived = "00000000-0000-4000-8000-000000000902"
		draft    = "00000000-0000-4000-8000-000000000903"
		other    = "00000000-0000-4000-8000-000000000904"
	)

	for _, tc := range []struct {
		path   string
		cookie *http.Cookie
		status int
		ids    []string
	}{
		{"/api/order_minimal", customer, 200, []string{first, draft}},
		{"/api/order", customer, 200, []string{first, draft}},
		{"/api/order?personId=" + testAdminID, customer, 200, []string{first, draft}},
		{"/api/order_minimal?personId=" + testAdminID, customer, 403, nil},
		{"/api/order_minimal?personId=" + testAdminID, admin, 200, []string{other}},
		{"/api/order_minimal?personId=" + testAccountID + "&includeDeleted=true", admin, 200, []string{first, archived, draft}},
		{"/api/order?status=Checked&paymentStatus=Completed&deliveryCost=10&isCourierDelivery=true", admin, 200, []string{first}},
		{"/api/order_minimal?status=Unknown", customer, 400, nil},
		{"/api/order?paymentStatus=Unknown", customer, 400, nil},
		{"/api/order_minimal?personId=invalid", admin, 400, nil},
		{"/api/order_minimal?orderId=20250801-COFFEE&paymentId=bank-42", admin, 200, []string{first}},
		{"/api/order_minimal?deliveryAddress=%D0%90%D0%94%D0%A0%D0%95%D0%A1", admin, 200, []string{first}},
		{"/api/order_minimal?status=Checked", customer, 200, []string{first, draft}},
		{"/api/order_minimal?limit=1&page=3&status=Checked", admin, 200, []string{}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := profileRequest(handler, tc.path, tc.cookie, "fixture-csrf")
			assertCartStatus(t, response, tc.status)
			if tc.status != 200 {
				return
			}
			var result listResponse[struct {
				ID string `json:"id"`
			}]
			readOrderHistory(t, response, &result)
			ids := make([]string, 0, len(result.Data))
			for _, entry := range result.Data {
				ids = append(ids, entry.ID)
			}
			slices.Sort(ids)
			if result.Status != "success" || result.Data == nil || result.Length != len(tc.ids) || !slices.Equal(ids, tc.ids) {
				t.Fatalf("orders = %v, length = %d, want %v", ids, result.Length, tc.ids)
			}
		})
	}

	for _, base := range []string{"/api/order/", "/api/order_minimal/"} {
		assertCartStatus(t, profileRequest(handler, base+other, customer, "fixture-csrf"), http.StatusForbidden)
		assertCartStatus(t, profileRequest(handler, base+testUnknownID, customer, "fixture-csrf"), http.StatusNotFound)
		assertCartStatus(t, profileRequest(handler, base+"invalid-id", customer, "fixture-csrf"), http.StatusNotFound)
		assertCartStatus(t, profileRequest(handler, base+first, customer, "wrong"), http.StatusUnauthorized)
		assertCartStatus(t, profileRequest(handler, base+other, admin, "fixture-csrf"), http.StatusOK)
		assertCartStatus(t, profileRequest(handler, base+archived, customer, "fixture-csrf"), http.StatusOK)
	}

	var result struct {
		Data order `json:"data"`
	}
	readOrderHistory(t, profileRequest(handler, "/api/order/"+first, customer, "fixture-csrf"), &result)
	if !result.Data.DeliveryDate.Valid || result.Data.DeliveryDate.Time.UTC().Format("2006-01-02T15:04:05Z") != "2025-08-02T14:30:00Z" {
		t.Fatalf("unexpected delivery date: %v", result.Data.DeliveryDate)
	}
	if len(result.Data.Products) != 3 {
		t.Fatalf("order products = %d, want 3", len(result.Data.Products))
	}
	expected := map[string][2]float32{
		"00000000-0000-4000-8000-000000000601": {49.5, 18.25},
		"00000000-0000-4000-8000-000000000602": {21.5, 13.25},
		"00000000-0000-4000-8000-000000000604": {49.99, 36.45},
	}
	for _, line := range result.Data.Products {
		id := line.Variant.ID.String()
		want, exists := expected[id]
		got := [2]float32{line.HistoricalPrice, line.HistoricalCostPrice}
		if !exists || got != want {
			t.Errorf("variant %s historical prices = %v, want %v", id, got, want)
		}
		delete(expected, id)
	}
	readOrderHistory(t, profileRequest(handler, "/api/order/"+other, admin, "fixture-csrf"), &result)
	if len(result.Data.Products) != 1 || result.Data.Products[0].Product.IsDeleted == nil || !*result.Data.Products[0].Product.IsDeleted {
		t.Fatal("archived product is missing from order history")
	}

	var summary struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	readOrderHistory(t, profileRequest(handler, "/api/order_minimal/"+first, customer, "fixture-csrf"), &summary)
	for _, key := range []string{"products", "contactSnapshot", "comment", "deliveryAddress"} {
		if _, present := summary.Data[key]; present {
			t.Errorf("minimal history unexpectedly included %s", key)
		}
	}
}

func readOrderHistory(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	assertCartStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("history response may be cached")
	}
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}
