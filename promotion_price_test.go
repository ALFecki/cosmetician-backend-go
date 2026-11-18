package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestBestPromotionPrice(t *testing.T) {
	productID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	otherID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	sale := func(id, scope, kind string, value float32, priority int32) promotion {
		return promotion{ID: pgtype.UUID{Bytes: uuid.MustParse("00000000-0000-4000-8000-000000000" + id), Valid: true},
			TargetType: scope, DiscountType: kind, DiscountValue: value, Priority: priority, ProductIDs: []pgtype.UUID{productID}}
	}
	percent := sale("701", "all", "percentage", 20.5, 0)
	fixed := sale("702", "product", "fixed", 12.25, 50)
	tieLater := sale("709", "product", "fixed", 12.25, 50)
	unrelated := sale("703", "product", "fixed", 900, 999)
	unrelated.ProductIDs = []pgtype.UUID{otherID}
	legacy := sale("704", "brand", "fixed", 999, 50)
	for _, tc := range []struct {
		name   string
		base   int64
		promos []promotion
		final  int64
		saving int64
		winner *pgtype.UUID
	}{
		{"wins over higher priority", 9013, []promotion{fixed, unrelated, legacy, percent}, 7165, 1848, &percent.ID},
		{"winning offer can differ across volumes", 2990, []promotion{percent, fixed}, 1765, 1225, &fixed.ID},
		{"stable UUID tie", 2990, []promotion{tieLater, fixed}, 1765, 1225, &fixed.ID},
		{"fixed reduction capped", 400, []promotion{fixed}, 0, 400, &fixed.ID},
		{"without a matching offer", 9013, []promotion{unrelated, legacy}, 9013, 0, nil},
		{"half cent rounds up", 101, []promotion{sale("705", "all", "percentage", 50, 1)}, 50, 51, &pgtype.UUID{Bytes: uuid.MustParse("00000000-0000-4000-8000-000000000705"), Valid: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := bestPromotionPrice(tc.base, productID, tc.promos)
			if result.FinalCents != tc.final || result.SavingCents != tc.saving ||
				!samePromotionID(result.PromotionID, tc.winner) {
				t.Fatalf("unexpected price: %+v", result)
			}
		})
	}
}

func TestPromotionPricesAcrossCatalogQuoteAndCheckout(t *testing.T) {
	pool := testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/guest_history_rows.sql",
		"testdata/checkout_confirmation_schema.sql", "testdata/cart_quote_rows.sql")
	key := []byte("test-only")
	auth := &accountAuth{db: pool, cartDB: pool, key: key, guestOrigin: guestOrigin,
		confirmation: &checkoutConfirmation{guestEnabled: true, guestLimiter: &testGuestOrderLimiter{}}}
	legacy := newHandler(pool, nil, auth, true)
	enabled := *auth
	enabled.promotionDB = pool
	handler := newHandler(pool, nil, &enabled, true)

	const productPath = "/api/product/00000000-0000-4000-8000-000000000501"
	largeID := uuid.MustParse("00000000-0000-4000-8000-000000000601")
	smallID := uuid.MustParse("00000000-0000-4000-8000-000000000602")
	var oldResponse struct{ Data product }
	readPricedProduct(t, newCartRequest(t, legacy, http.MethodGet, productPath, nil, nil), &oldResponse)
	for _, variant := range oldResponse.Data.Variants {
		if variant.PromotionPrice != nil {
			t.Fatal("previous pricing contract changed while feature was off")
		}
	}

	var response struct{ Data product }
	readPricedProduct(t, newCartRequest(t, handler, http.MethodGet, productPath, nil, nil), &response)
	prices := map[uuid.UUID]*promotionPrice{}
	for _, v := range response.Data.Variants {
		prices[v.ID.Bytes] = v.PromotionPrice
	}
	if large, small := prices[largeID], prices[smallID]; large == nil || small == nil || large.FinalCents != 7165 || small.FinalCents != 1765 {
		t.Fatal("public product returned incorrect promotion prices")
	}

	var catalog listResponse[product]
	readPricedProduct(t, newCartRequest(t, handler, http.MethodGet, "/api/product?page=1&limit=20", nil, nil), &catalog)
	var listed *promotionPrice
	for _, entry := range catalog.Data {
		for _, v := range entry.Variants {
			if v.ID.Bytes == smallID {
				listed = v.PromotionPrice
			}
		}
	}
	if listed == nil || listed.FinalCents != prices[smallID].FinalCents {
		t.Fatal("catalog and product details disagree on price")
	}
	var vResponse struct{ Data variant }
	readPricedProduct(t, newCartRequest(t, handler, http.MethodGet, "/api/product_variant/"+largeID.String(), nil, nil), &vResponse)
	if amount := vResponse.Data.PromotionPrice; amount == nil || amount.FinalCents != prices[largeID].FinalCents ||
		!samePromotionID(amount.PromotionID, prices[largeID].PromotionID) {
		t.Fatal("direct variant endpoint and product details disagree on price")
	}

	readPricedProduct(t, newCartRequest(t, handler, http.MethodGet,
		"/api/product_variant/00000000-0000-4000-8000-000000000607", nil, nil), &vResponse)
	if price := vResponse.Data.PromotionPrice; price == nil || price.FinalCents != 1000 || price.SavingCents != 0 {
		t.Fatal("a variant of an archived product was advertised with a sale price")
	}

	items := []cartItem{{VariantID: largeID, Quantity: 2}, {VariantID: smallID, Quantity: 1}}
	code := "PERCENT"
	input := quoteRequest{Items: items, PromoCode: &code, IsCourierDelivery: new(bool)}
	var quote cartQuote
	readOrderHistory(t, newCartRequest(t, handler, http.MethodPost, "/api/checkout/cart/quote", input, nil), &quote)
	if quote.SubtotalCents != 21016 || quote.PromotionDiscountCents != 4921 || quote.PromoCodeDiscountCents != 2000 || quote.TotalCents != 14095 {
		t.Fatalf("quote amounts: subtotal=%d automatic=%d code=%d total=%d", quote.SubtotalCents, quote.PromotionDiscountCents, quote.PromoCodeDiscountCents, quote.TotalCents)
	}
	for _, line := range quote.Order.Products {
		fromQuote, expected := line.Variant.PromotionPrice, prices[line.Variant.ID.Bytes]
		if fromQuote == nil || fromQuote.FinalCents != expected.FinalCents || !samePromotionID(fromQuote.PromotionID, expected.PromotionID) {
			t.Fatal("cart and product page disagree on variant price")
		}
	}

	guest := guestHistoryCookie(t, key, firstGuestID, guestAudience, time.Now().Add(time.Hour).Unix())
	request := checkoutRequest{
		CartID: uuid.New(), CheckoutKey: uuid.New(), ExpectedRevision: new(int64), ExpectedTotalCents: &quote.TotalCents,
		Items: items, PromoCode: &code, Details: &checkoutDetails{Phone: new("+375290000011"), FirstName: "Тест",
			SecondName: "Покупатель", DeliveryMethod: "pickup", PaymentMethod: "cash", OfferConsent: true},
	}
	var placed order
	readOrderHistory(t, guestCheckoutRequest(t, handler, request, guest, guestCSRF), &placed)
	if placed.TotalPrice != 140.95 || placed.DiscountAmount == nil || *placed.DiscountAmount != 69.21 {
		t.Fatalf("checkout did not persist the quote total and discount: %v", placed.TotalPrice)
	}
	for _, line := range placed.Products {
		if line.Variant.PromotionPrice != nil || line.HistoricalPrice != line.Variant.Price {
			t.Fatal("checkout replaced historical prices with live promotion metadata")
		}
	}

	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE promotion SET is_active = false WHERE target_type IN ('all', 'product')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var replayed order
	readOrderHistory(t, guestCheckoutRequest(t, handler, request, guest, guestCSRF), &replayed)
	if replayed.ID != placed.ID || replayed.TotalPrice != placed.TotalPrice {
		t.Fatal("disabling promotions repriced an already confirmed order")
	}
	request.CartID, request.CheckoutKey = uuid.New(), uuid.New()
	assertCartStatus(t, guestCheckoutRequest(t, handler, request, guest, guestCSRF), http.StatusConflict)
}

func samePromotionID(a, b *pgtype.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func readPricedProduct(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	assertCartStatus(t, response, http.StatusOK)
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}
