package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type testGuestOrderLimiter struct {
	calls  atomic.Int32
	reject atomic.Bool
}

func (limiter *testGuestOrderLimiter) allow(_ context.Context, _ uuid.UUID, _ *http.Request) error {
	limiter.calls.Add(1)
	if limiter.reject.Load() {
		return cartRequestError{http.StatusTooManyRequests, "Слишком много попыток. Попробуйте позже"}
	}
	return nil
}

func TestCheckoutConfirmation(t *testing.T) {
	pool := testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/guest_history_rows.sql", "testdata/checkout_confirmation_schema.sql")
	key := []byte("test-only")
	limit := &testGuestOrderLimiter{}
	auth := &accountAuth{
		db: pool, cartDB: pool, key: key, guestOrigin: guestOrigin,
		confirmation: &checkoutConfirmation{guestEnabled: true, guestLimiter: limit},
	}
	handler := newHandler(pool, nil, auth, true)
	account := profileCookie(t, key, testAccountID, false, false, false)
	admin := profileCookie(t, key, testAdminID, true, true, false)
	guest := guestHistoryCookie(t, key, firstGuestID, guestAudience, time.Now().Add(time.Hour).Unix())
	variant := uuid.MustParse("00000000-0000-4000-8000-000000000601")
	items := []cartItem{{VariantID: variant, Quantity: 2}}

	const cartPath = "/api/checkout/cart"
	cart := parseSavedQuote(t, newCartRequest(t, handler, http.MethodGet, cartPath, nil, account))
	promo := "FIXTURE-ONLY"
	cartRequest := cartRequest{CartID: cart.Order.ID, ExpectedRevision: &cart.Order.CartRevision, Items: items, PromoCode: &promo}
	response := newCartRequest(t, handler, http.MethodPut, cartPath, cartRequest, account)
	var saved cartQuote
	readOrderHistory(t, response, &saved)

	phone := "+375290000001"
	request := checkoutRequest{
		CartID: saved.Order.ID, ExpectedRevision: &saved.Order.CartRevision, CheckoutKey: uuid.New(),
		Items: items, PromoCode: &promo, ExpectedTotalCents: &saved.TotalCents,
		Details: &checkoutDetails{Phone: &phone, FirstName: "Тест", SecondName: "Покупатель",
			DeliveryMethod: "pickup", PaymentMethod: "cash", OfferConsent: true},
	}
	accountPath := "/api/checkout"
	subtotal := saved.TotalCents + 1
	changedPrice := request
	changedPrice.ExpectedTotalCents = &subtotal
	assertCartStatus(t, newCartRequest(t, handler, http.MethodPost, accountPath, changedPrice, account), http.StatusConflict)
	oldRevision := *request.ExpectedRevision - 1
	staleCart := request
	staleCart.ExpectedRevision = &oldRevision
	assertCartStatus(t, newCartRequest(t, handler, http.MethodPost, accountPath, staleCart, account), http.StatusConflict)
	assertCartStatus(t, newCartRequest(t, handler, http.MethodPost, accountPath, request, nil), http.StatusUnauthorized)

	var outstanding sync.WaitGroup
	for range 10 {
		outstanding.Go(func() {
			response := newCartRequest(t, handler, http.MethodPost, accountPath, request, account)
			if response.Code != http.StatusOK {
				t.Errorf("checkout status = %d, want 200: %s", response.Code, response.Body)
				return
			}
			var result order
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Error(err)
				return
			}
			if result.ID != request.CartID || result.Status != "Checked" || result.PaymentStatus != "Pending" ||
				len(result.Products) != 1 || result.Products[0].HistoricalPrice != 90.13 ||
				result.Products[0].Quantity != 2 || result.TotalPrice != saved.Order.TotalPrice {
				t.Error("checkout returned an inconsistent order")
			}
		})
	}
	outstanding.Wait()
	var used, logged int
	if err := pool.QueryRow(t.Context(), `
		SELECT used_count, (SELECT count(*) FROM promo_code_usage) FROM promo_code WHERE code = $1`, promo).Scan(&used, &logged); err != nil {
		t.Fatal(err)
	}
	if used != 1 || logged != 1 {
		t.Fatalf("repeated checkout consumed promo %d times, logged %d", used, logged)
	}
	assertCartStatus(t, newCartRequest(t, handler, http.MethodPost, accountPath, request, admin), http.StatusConflict)
	otherDetails := *request.Details
	message := "Другая просьба"
	otherDetails.Comment = &message
	conflict := request
	conflict.Details = &otherDetails
	assertCartStatus(t, newCartRequest(t, handler, http.MethodPost, accountPath, conflict, account), http.StatusConflict)

	guestPhone := "+375290000011"
	guestDetails := *request.Details
	guestDetails.Phone = &guestPhone
	guestDetails.PaymentMethod = "online"
	guestPrice, err := quoteCart(t.Context(), pool, quoteRequest{Items: items, IsCourierDelivery: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	guestRequest := checkoutRequest{
		CartID: uuid.New(), ExpectedRevision: &revision, CheckoutKey: uuid.New(), Items: items,
		Details: &guestDetails, ExpectedTotalCents: &guestPrice.TotalCents,
	}
	guestID := uuid.Nil
	for range 2 {
		var original order
		readOrderHistory(t, guestCheckoutRequest(t, handler, guestRequest, guest, guestCSRF), &original)
		if original.ID == uuid.Nil || original.PersonID.String() != firstGuestID ||
			original.PaymentStatus != "Processing" || original.PaymentID != nil || original.PaymentFormURL != nil {
			t.Fatal("guest checkout returned the wrong owner or payment state")
		}
		if guestID != uuid.Nil && original.ID != guestID {
			t.Fatal("guest retry created another order")
		}
		guestID = original.ID
	}
	if limit.calls.Load() != 1 {
		t.Fatalf("guest retries reserved %d quota entries, want one", limit.calls.Load())
	}
	assertCartStatus(t, guestCheckoutRequest(t, handler, guestRequest, guest, "wrong"), http.StatusUnauthorized)
	guestRequest.CheckoutKey = uuid.New()
	invalidGuest := guestRequest
	invalidDetails := *guestRequest.Details
	invalidDetails.OfferConsent = false
	invalidGuest.Details = &invalidDetails
	assertCartStatus(t, guestCheckoutRequest(t, handler, invalidGuest, guest, guestCSRF), http.StatusBadRequest)
	if limit.calls.Load() != 2 {
		t.Fatalf("invalid new checkout left the quota count at %d, want 2", limit.calls.Load())
	}
	limit.reject.Store(true)
	rejected := guestCheckoutRequest(t, handler, guestRequest, guest, guestCSRF)
	assertCartStatus(t, rejected, http.StatusTooManyRequests)
	if rejected.Header().Get("Retry-After") != "60" {
		t.Fatal("throttled checkout did not return Retry-After")
	}
}

func guestCheckoutRequest(t *testing.T, handler http.Handler, body checkoutRequest, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, guestReadPath, bytes.NewReader(payload))
	req.AddCookie(cookie)
	req.Header.Set("Origin", guestOrigin)
	req.Header.Set("X-Guest-CSRF", csrf)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}
