package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

func TestCartQuoteDisabled(t *testing.T) {
	response := accountRequest(newHandler(nil, nil, nil, false), http.MethodPost, "/api/checkout/cart/quote", `{"items":[]}`, nil)
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "Новое оформление пока недоступно" {
		t.Fatalf("disabled quote = %d %q, want 503", response.Code, response.Body)
	}
}

func TestCartQuoteContract(t *testing.T) {
	ctx, tx := catalogFixture(t, "catalog_schema.sql", "catalog_rows.sql", "product_schema.sql", "product_rows.sql", "cart_quote_rows.sql")
	fixture, err := os.ReadFile("testdata/cart_quote_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		RustCommit string
		Cases      []struct {
			Name        string
			Body        json.RawMessage
			Enabled     bool
			FullProduct bool
			Expected    struct {
				Status       int
				ContentType  string
				CacheControl string
				Body         *string
				JSON         json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(fixture, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.RustCommit == "" || len(contract.Cases) == 0 {
		t.Fatal("missing Rust reference revision or scenarios")
	}
	enabled, disabled := newHandler(tx, nil, nil, true), newHandler(tx, nil, nil, false)
	for _, tc := range contract.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			handler := enabled
			if !tc.Enabled {
				handler = disabled
			}
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/checkout/cart/quote", bytes.NewReader(tc.Body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.Expected.Status || response.Header().Get("Cache-Control") != tc.Expected.CacheControl ||
				response.Header().Get("Content-Type") != tc.Expected.ContentType {
				t.Fatalf("unexpected status or headers: %d %v", response.Code, response.Header())
			}
			if tc.Expected.Body != nil && response.Body.String() != *tc.Expected.Body {
				t.Fatalf("body = %q, want %q", response.Body, *tc.Expected.Body)
			}
			if len(tc.Expected.JSON) == 0 {
				return
			}

			got := decodeContract(t, response.Body.Bytes(), false, false)
			want := decodeContract(t, tc.Expected.JSON, false, false)
			if !tc.FullProduct {
				items := got["order"].(map[string]any)["products"].([]any)
				for _, value := range items {
					line := value.(map[string]any)
					item := line["product"].(map[string]any)
					line["product"] = map[string]any{"id": item["id"]}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("quote differs from Rust: got %v, want %v", got, want)
			}
		})
	}
	var used int
	if err := tx.QueryRow(ctx, "SELECT used_count FROM promo_code WHERE code = 'PERCENT'").Scan(&used); err != nil || used != 0 {
		t.Fatalf("quote changed promo code usage to %d or failed to read it: %v", used, err)
	}
}

func TestQuoteDeliveryThreshold(t *testing.T) {
	for _, tc := range []struct {
		method   string
		courier  bool
		subtotal int64
		cost     int32
	}{
		{"standart", false, 24_999, 10},
		{"standart", false, 25_000, 0},
		{"post", true, 24_999, 10},
		{"post", true, 25_000, 0},
		{"post", false, 24_999, 0},
	} {
		got, err := quoteDeliveryCost(tc.method, tc.courier, tc.subtotal)
		if err != nil || got != tc.cost {
			t.Errorf("delivery cost for %+v = %d (%v)", tc, got, err)
		}
	}
}
