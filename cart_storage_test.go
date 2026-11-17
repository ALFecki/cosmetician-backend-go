package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newCartRequest(t *testing.T, handler http.Handler, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
		req.Header.Set("X-CSRF-Token", "fixture-csrf")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCartWriteRequiresSession(t *testing.T) {
	handler := newHandler(nil, nil, &accountAuth{cartDB: nilCartDatabase{}}, true)
	response := newCartRequest(t, handler, http.MethodPost, "/api/checkout/cart/items",
		map[string]uuid.UUID{"cartId": uuid.New(), "variantId": uuid.New(), "operationId": uuid.New()}, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("add without session = %d %s", response.Code, response.Body)
	}
}

type nilCartDatabase struct{ cartDatabase }

func TestCartStorage(t *testing.T) {
	pool := testCartDatabase(t)
	key := []byte("fixture-secret-no-production")
	auth := &accountAuth{db: pool, cartDB: pool, key: key, secureCookies: true}
	handler := newHandler(pool, nil, auth, true)
	cookie := profileCookie(t, key, testAccountID, false, false, false)
	const cartPath = "/api/checkout/cart"
	one := uuid.MustParse("00000000-0000-4000-8000-000000000601")
	two := uuid.MustParse("00000000-0000-4000-8000-000000000604")
	archived := uuid.MustParse("00000000-0000-4000-8000-000000000603")
	read := func() cartQuote {
		return parseSavedQuote(t, newCartRequest(t, handler, http.MethodGet, cartPath, nil, cookie))
	}

	cart := read()
	if cart.Order.CartRevision != 1 || len(cart.Items) != 0 {
		t.Fatalf("new cart = %+v", cart)
	}
	add := cartRequest{CartID: cart.Order.ID, VariantID: one, OperationID: uuid.New()}
	addItem := func(values cartRequest) *httptest.ResponseRecorder {
		return newCartRequest(t, handler, http.MethodPost, cartPath+"/items", values, cookie)
	}

	var requests sync.WaitGroup
	for i := range 32 {
		request := add
		if i < 8 {
			request.VariantID = two
			request.OperationID = uuid.New()
		}
		requests.Go(func() {
			response := addItem(request)
			if response.Code != http.StatusOK {
				t.Errorf("parallel add = %d %s", response.Code, response.Body)
			}
		})
	}
	requests.Wait()
	cart = read()
	if cart.Order.CartRevision != 10 || !sameCartItems(cart.Items, cartItem{one, 1}, cartItem{two, 8}) {
		t.Fatalf("after concurrent additions, revision = %d, items = %v", cart.Order.CartRevision, cart.Items)
	}

	conflict := add
	conflict.VariantID = two
	assertCartStatus(t, addItem(conflict), http.StatusConflict)
	unavailable := add
	unavailable.VariantID, unavailable.OperationID = archived, uuid.New()
	assertCartStatus(t, addItem(unavailable), http.StatusBadRequest)
	otherAccount := profileCookie(t, key, testAdminID, false, false, false)
	assertCartStatus(t, newCartRequest(t, handler, http.MethodPost, cartPath+"/items", add, otherAccount), http.StatusConflict)
	if cart.Order.CartRevision != read().Order.CartRevision {
		t.Fatal("rejected additions changed the revision")
	}

	failing := add
	failing.OperationID = uuid.MustParse("00000000-0000-4000-8000-000000000666")
	assertCartStatus(t, addItem(failing), http.StatusInternalServerError)
	if got := read(); got.Order.CartRevision != cart.Order.CartRevision || !sameCartItems(got.Items, cartItem{one, 1}, cartItem{two, 8}) {
		t.Fatalf("a failure inserting the operation receipt left a partial write: %v", got.Items)
	}

	var concurrent sync.WaitGroup
	var addStatus, replaceStatus int
	addAlongsideReplace := cartRequest{CartID: cart.Order.ID, VariantID: two, OperationID: uuid.New()}
	freshReplace := cartRequest{CartID: cart.Order.ID, ExpectedRevision: &cart.Order.CartRevision, Items: []cartItem{{one, 20}, {two, 8}}}
	concurrent.Go(func() { addStatus = addItem(addAlongsideReplace).Code })
	concurrent.Go(func() {
		replaceStatus = newCartRequest(t, handler, http.MethodPut, cartPath, freshReplace, cookie).Code
	})
	concurrent.Wait()
	cart = read()
	expectedOne := int32(1)
	if replaceStatus == http.StatusOK {
		expectedOne = 20
	} else if replaceStatus != http.StatusConflict {
		t.Fatalf("concurrent replacement status = %d", replaceStatus)
	}
	if addStatus != http.StatusOK || !sameCartItems(cart.Items, cartItem{one, expectedOne}, cartItem{two, 9}) {
		t.Fatalf("concurrent replace and add returned %d/%d, items: %v", replaceStatus, addStatus, cart.Items)
	}

	stale := cart.Order.CartRevision - 1
	replace := cartRequest{CartID: cart.Order.ID, ExpectedRevision: &stale, Items: []cartItem{{one, 99}}}
	assertCartStatus(t, newCartRequest(t, handler, http.MethodPut, cartPath, replace, cookie), http.StatusConflict)
	replace.ExpectedRevision = &cart.Order.CartRevision
	cart = parseSavedQuote(t, newCartRequest(t, handler, http.MethodPut, cartPath, replace, cookie))
	var limitRequests sync.WaitGroup
	for range 16 {
		limitRequests.Go(func() {
			overLimit := add
			overLimit.OperationID = uuid.New()
			if response := addItem(overLimit); response.Code != http.StatusBadRequest {
				t.Errorf("add above limit = %d %s", response.Code, response.Body)
			}
		})
	}
	limitRequests.Wait()
	if got := read(); got.Order.CartRevision != cart.Order.CartRevision || !sameCartItems(got.Items, cartItem{one, 99}) {
		t.Fatalf("exceeding limit changed the cart: %v", got.Items)
	}

	merge := cartRequest{MergeID: uuid.New(), Items: []cartItem{{two, 2}}}
	for range 2 {
		cart = parseSavedQuote(t, newCartRequest(t, handler, http.MethodPost, cartPath+"/merge", merge, cookie))
		if !sameCartItems(cart.Items, cartItem{one, 99}, cartItem{two, 2}) {
			t.Fatalf("guest merge items = %v", cart.Items)
		}
	}

	logout := newCartRequest(t, handler, http.MethodPost, "/api/logout", nil, cookie)
	assertCartStatus(t, logout, http.StatusOK)
	cookies := logout.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "jwt" || cookies[0].Value != "" || cookies[0].MaxAge >= 0 ||
		!cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteNoneMode || cookies[0].Path != "/" {
		t.Fatalf("logout did not expire the session cookie: %+v", cookies)
	}

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackCart(t.Context(), tx)
	var receipts int
	if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM cart_add_operation WHERE cart_id = $1", add.CartID).Scan(&receipts); err != nil || receipts != 10 {
		t.Fatalf("added %d receipts, want 10: %v", receipts, err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE "order" SET status = 'Checked' WHERE id = $1`, cart.Order.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertCartStatus(t, addItem(add), http.StatusConflict)
	next := parseSavedQuote(t, newCartRequest(t, handler, http.MethodPost, cartPath+"/merge", merge, cookie))
	if next.Order.ID == cart.Order.ID || len(next.Items) != 0 {
		t.Fatalf("replay repopulated cart: %+v", next.Items)
	}
	assertCartStatus(t, addItem(add), http.StatusConflict)
}

func assertCartStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body)
	}
}

func sameCartItems(actual []cartItem, expected ...cartItem) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i, item := range actual {
		if item != expected[i] {
			return false
		}
	}
	return true
}

func parseSavedQuote(t *testing.T, response *httptest.ResponseRecorder) cartQuote {
	t.Helper()
	assertCartStatus(t, response, http.StatusOK)
	var result struct {
		Order            order      `json:"order"`
		Items            []cartItem `json:"items"`
		ItemAddAvailable bool       `json:"itemAddAvailable"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.ItemAddAvailable || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("stored cart did not advertise item adds or disable caching")
	}
	return cartQuote{Order: result.Order, Items: result.Items}
}

func testCartDatabase(t *testing.T, additionalFixtures ...string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SKVIDERM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SKVIDERM_TEST_DATABASE_URL to run the PostgreSQL contract tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "cart_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(cleanup)
	})
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema+"; SET search_path TO "+schema); err != nil {
		t.Fatal(err)
	}
	fixtures := []string{
		"testdata/account_schema.sql", "testdata/account_rows.sql",
		"testdata/catalog_schema.sql", "testdata/catalog_rows.sql",
		"testdata/product_schema.sql", "testdata/product_rows.sql",
		"testdata/cart_storage_schema.sql", "migrations/0001_cart_add_operation.sql",
	}
	for _, path := range append(fixtures, additionalFixtures...) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		schemaSQL := strings.NewReplacer("CREATE TEMP TABLE", "CREATE TABLE", "pg_temp.", "").Replace(string(raw))
		if _, err := admin.Exec(ctx, schemaSQL); err != nil {
			t.Fatalf("apply %s: %v", path, err)
		}
	}
	if _, err := admin.Exec(ctx, `ALTER TABLE cart_add_operation ADD CONSTRAINT test_receipt_failure
		CHECK (operation_id <> '00000000-0000-4000-8000-000000000666')`); err != nil {
		t.Fatal(err)
	}

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	config.MaxConns = 5
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
