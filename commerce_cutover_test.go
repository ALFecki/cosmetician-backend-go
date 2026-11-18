package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestCutoverOrderMutationRequiresAccount(t *testing.T) {
	key := []byte("commerce-fixture")
	auth := &accountAuth{key: key, cartDB: nilCartDatabase{}}
	handler := testCommerceHandler(auth, nil)
	for _, route := range []struct{ method, path string }{
		{http.MethodPatch, "/api/order/00000000-0000-4000-8000-000000000113"},
		{http.MethodGet, "/api/order/person/last_non_checked"},
		{http.MethodPost, "/api/promo-code"},
		{http.MethodGet, "/api/statistics/summary"},
	} {
		t.Run(route.path, func(t *testing.T) {
			assertCartStatus(t, newCartRequest(t, handler, route.method, route.path, nil, nil), http.StatusUnauthorized)
		})
	}
}

func TestCutoverPromoCodeWriteContract(t *testing.T) {
	pool := testCartDatabase(t, "testdata/checkout_confirmation_schema.sql")
	key := []byte("commerce-fixture")
	auth := &accountAuth{db: pool, key: key, cartDB: pool}
	handler := testCommerceHandler(auth, nil)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	buyer := profileCookie(t, key, testAccountID, false, false, false)
	path := "/api/promo-code"
	body := `{"code":"CUTOVER-FIXTURE","discountType":"percentage","discountValue":"15","usageLimit":0,"validFrom":"2000-01-01T00:00:00Z","validUntil":"2099-01-01T00:00:00Z","isActive":true}`
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPost, path, body, buyer, "fixture-csrf"), http.StatusUnauthorized)
	var created struct {
		Status string    `json:"status"`
		Data   promoCode `json:"data"`
	}
	readOrderHistory(t, bannerRawRequest(t, handler, http.MethodPost, path, body, moderator, "fixture-csrf"), &created)
	if created.Status != "success" || created.Data.Code != "CUTOVER-FIXTURE" || created.Data.UsageLimit != nil {
		t.Fatalf("unexpected promo code: %+v", created)
	}
	id := created.Data.ID.String()
	assertCartStatus(t, newCartRequest(t, handler, http.MethodGet, path+"/"+id, nil, moderator), http.StatusOK)
	validate := `{"code":"CUTOVER-FIXTURE","orderTotal":100,"productIds":[]}`
	res := bannerRawRequest(t, handler, http.MethodPost, path+"/validate", validate, nil, "")
	assertCartStatus(t, res, http.StatusOK)
	if !strings.Contains(res.Body.String(), `"discountAmount":15`) {
		t.Fatalf("unexpected discount: %s", res.Body)
	}
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPatch, path+"/"+id, `{"isActive":false}`, moderator, "fixture-csrf"), http.StatusOK)
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPost, path+"/validate", validate, nil, ""), http.StatusNotFound)
	assertCartStatus(t, newCartRequest(t, handler, http.MethodDelete, path+"/"+id, nil, moderator), http.StatusOK)
}

func TestCutoverCartAndAdminStatusSharePersistedOrder(t *testing.T) {
	pool := testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/checkout_confirmation_schema.sql")
	key := []byte("commerce-fixture")
	auth := &accountAuth{db: pool, key: key, cartDB: pool, confirmation: &checkoutConfirmation{}}
	handler := testCommerceHandler(auth, nil)
	admin := profileCookie(t, key, testAdminID, true, false, false)
	buyer := profileCookie(t, key, testAccountID, false, false, false)
	var cart struct{ Data order }
	variantID := "00000000-0000-4000-8000-000000000601"
	readOrderHistory(t, newCartRequest(t, handler, http.MethodGet, "/api/order/person/last_non_checked", nil, buyer), &cart)
	path := "/api/order/" + cart.Data.ID.String()
	var updated order
	readOrderHistory(t, newCartRequest(t, handler, http.MethodPost, "/api/order/attach_product/"+variantID, nil, buyer), &updated)
	if len(updated.Products) != 1 || updated.Products[0].Quantity != 1 || updated.ID != cart.Data.ID {
		t.Fatalf("unexpected cart after add: %+v", updated)
	}
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPatch, path, `{"status":"Completed"}`, buyer, "fixture-csrf"), http.StatusBadRequest)
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPatch, path, `{"status":"Completed"}`, admin, "fixture-csrf"), http.StatusOK)
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPatch, path, `{"comment":"changed"}`, buyer, "fixture-csrf"), http.StatusConflict)
}

func testCommerceHandler(auth *accountAuth, mailer accountMailer) http.Handler {
	auth.fullDB = auth.cartDB
	auth.orderMailer = mailer
	return newHandler(auth.db, nil, auth, true)
}

func TestCutoverLegacyCheckoutPersistsPriceAndStats(t *testing.T) {
	pool := testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/checkout_confirmation_schema.sql")
	key := []byte("commerce-fixture")
	auth := &accountAuth{db: pool, key: key, cartDB: pool}
	handler := testCommerceHandler(auth, nil)
	buyer := profileCookie(t, key, testAccountID, false, false, false)
	admin := profileCookie(t, key, testAdminID, true, false, false)
	const variantID = "00000000-0000-4000-8000-000000000601"
	var draft order
	readOrderHistory(t, newCartRequest(t, handler, http.MethodPost, "/api/order/attach_product/"+variantID, nil, buyer), &draft)
	path := "/api/order/" + draft.ID.String()
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPatch, path,
		`{"deliveryMethod":"standart","deliveryAddress":"Минск, улица, дом","paymentMethod":"cash","deliveryCost":0}`, buyer, "fixture-csrf"), http.StatusOK)
	var order order
	readOrderHistory(t, newCartRequest(t, handler, http.MethodGet, "/api/order/person/checkout", nil, buyer), &order)
	if order.ID != draft.ID || order.Status != "Checked" || order.DeliveryCost != 10 || len(order.Products) != 1 || order.TotalPrice != draft.TotalPrice+10 {
		t.Fatalf("unexpected confirmed order: %+v", order)
	}
	assertCartStatus(t, newCartRequest(t, handler, http.MethodGet, "/api/order/person/checkout", nil, buyer), http.StatusConflict)
	assertCartStatus(t, bannerRawRequest(t, handler, http.MethodPatch, path, `{"status":"Completed"}`, admin, "fixture-csrf"), http.StatusOK)

	var snapshot bool
	if err := pool.QueryRow(t.Context(), `SELECT unit_price IS NOT NULL AND unit_cost_price IS NOT NULL
		FROM order_product WHERE order_id = $1`, order.ID).Scan(&snapshot); err != nil || !snapshot {
		t.Fatalf("snapshot missing: %v, %v", snapshot, err)
	}
	var stats struct{ Data commerceStatistics }
	readOrderHistory(t, newCartRequest(t, handler, http.MethodGet, "/api/statistics/summary?significanceCoefficient=2&categoryId%5B%5D=00000000-0000-4000-8000-000000000201", nil, admin), &stats)
	data := stats.Data
	if len(data.BrandProfitability) != 1 || len(data.WeightedROI) != 1 || len(data.InventoryTurnover) != 1 ||
		data.BrandProfitability[0].UnitsSold != 1 || data.WeightedROI[0].SignificanceCoefficient != 2 ||
		data.WeightedROI[0].TotalProfit != 2*data.BrandProfitability[0].TotalProfit {
		t.Fatalf("statistics did not include matching order: %+v", data)
	}
	assertCartStatus(t, newCartRequest(t, handler, http.MethodGet, "/api/statistics/summary?brandId=invalid", nil, admin), http.StatusBadRequest)
}

type testStatusMailer struct {
	to   string
	body string
}

func (mail *testStatusMailer) send(_ context.Context, to, _, body string) error {
	mail.to, mail.body = to, body
	return nil
}

func TestCutoverStatusEmailUsesStoredContact(t *testing.T) {
	pool := testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/checkout_confirmation_schema.sql")
	key := []byte("commerce-fixture")
	auth := &accountAuth{db: pool, key: key, cartDB: pool}
	mail := &testStatusMailer{}
	handler := testCommerceHandler(auth, mail)
	admin := profileCookie(t, key, testAdminID, true, false, false)
	buyer := profileCookie(t, key, testAccountID, false, false, false)
	const orderID = "00000000-0000-4000-8000-000000000125"
	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO "order" (id, person_id, status, order_id, total_price, contact_snapshot)
			VALUES($1, $2, 'Checked', 'FIXTURE-125', 100, '{"email":"snapshot@example.invalid"}')`, orderID, testAccountID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	params := url.Values{"to": {testAccountID}, "orderId": {orderID}, "status": {"В обработке"}}
	path := "/api/email/send_status_update?" + params.Encode()
	assertCartStatus(t, newCartRequest(t, handler, http.MethodGet, path, nil, buyer), http.StatusUnauthorized)
	assertCartStatus(t, newCartRequest(t, handler, http.MethodHead, path, nil, admin), http.StatusMethodNotAllowed)
	if mail.to != "" {
		t.Fatal("unauthorized or HEAD request sent an email")
	}
	assertCartStatus(t, newCartRequest(t, handler, http.MethodGet, path, nil, admin), http.StatusOK)
	if mail.to != "snapshot@example.invalid" || !strings.Contains(mail.body, "FIXTURE-125") {
		t.Fatalf("unexpected email destination or order: %+v", mail)
	}
}

func TestCutoverAdminCreateAndArchiveWithOrderEnums(t *testing.T) {
	pool := testCartDatabase(t, "testdata/order_history_schema.sql", "testdata/checkout_confirmation_schema.sql")
	key := []byte("commerce-fixture")
	auth := &accountAuth{db: pool, key: key, cartDB: pool}
	handler := testCommerceHandler(auth, nil)
	admin := profileCookie(t, key, testAdminID, true, false, false)
	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `CREATE TYPE order_status AS ENUM ('NotChecked','Checked','Processing','InDelivery','Delivered','Completed','Cancelled');
			CREATE TYPE payment_status AS ENUM ('Pending','Processing','Completed','Failed');
			ALTER TABLE "order" ALTER COLUMN payment_status DROP DEFAULT;
			ALTER TABLE "order" ALTER COLUMN payment_status TYPE payment_status USING payment_status::payment_status;
			ALTER TABLE "order" ALTER COLUMN payment_status SET DEFAULT 'Pending';
			ALTER TABLE "order" ALTER COLUMN status TYPE order_status USING status::order_status`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Data order }
	input := `{"personId":"` + testAccountID + `","status":"Checked","totalPrice":42,"deliveryCost":10,"deliveryMethod":"standart"}`
	readOrderHistory(t, bannerRawRequest(t, handler, http.MethodPost, "/api/order", input, admin, "fixture-csrf"), &result)
	if result.Data.PaymentStatus != "Pending" || result.Data.TotalPrice != 42 || result.Data.DeliveryCost != 10 {
		t.Fatalf("unexpected status or price: %+v", result.Data)
	}
	path := "/api/order/" + result.Data.ID.String()
	readOrderHistory(t, bannerRawRequest(t, handler, http.MethodPatch, path, `{"status":"Completed"}`, admin, "fixture-csrf"), &result)
	if result.Data.Status != "Completed" {
		t.Fatalf("unexpected order status: %s", result.Data.Status)
	}
	readOrderHistory(t, newCartRequest(t, handler, http.MethodDelete, path, nil, admin), &result)
	if result.Data.IsDeleted == nil || !*result.Data.IsDeleted || result.Data.TotalPrice != 42 {
		t.Fatalf("archive lost saved order: %+v", result.Data)
	}
}
