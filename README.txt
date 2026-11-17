SkViDerm Go backend
==================

The Go service implements public storefront reads for brands, categories,
banners, products, variants, promotions, product promo codes and images. The
Rust backend is the behavioral reference. The Go service connects to the
existing PostgreSQL schema. Transactions are read-only by default.
Apply the Rust migrations before the Go migrations under migrations/.

Running locally
---------------
Use the Go version specified in go.mod. Set DATABASE_URL for a disposable
database with the existing Rust migrations applied and run:

    go run .

Set HTTP_ADDR to select a listen address (defaults to :8000). The public
/api/images/... route reads files from MinIO when MINIO_ENDPOINT,
MINIO_ACCESS_KEY, MINIO_SECRET_KEY and MINIO_BUCKET are supplied. Provide
an absolute HTTP(S) endpoint and credentials allowed to read the bucket.

Set JWT_SECRET to enable POST /api/login, GET /api/check_auth and the
protected GET /api/person and GET /api/person/{id} routes. For compatible
sessions between Rust and Go, configure the same signing secret on both
services. Cookies are secure by default. Set SECURE_MODE=false for local
HTTP. The GET /api/order and /api/order_minimal lists and details use the
same authenticated session and CSRF header.

Set PHONE_CHECKOUT_ENABLED=true to enable POST /api/checkout/cart/quote.
The route recalculates prices and availability from the catalog and does
not create a cart or consume a promo code.

To build the Linux production architecture on another machine:

    docker build --platform linux/amd64 -t cosmetician-backend-go .

Account cart storage
--------------------
Apply migrations/0001_cart_add_operation.sql. Enable CART_STORAGE_ENABLED
with JWT_SECRET and PHONE_CHECKOUT_ENABLED=true. The service opens
explicit read-write transactions for the authenticated GET and PUT
/api/checkout/cart, POST /api/checkout/cart/merge and
POST /api/checkout/cart/items. POST /api/logout clears the session cookie.
Route these account cart endpoints together. Checkout in Rust shares
the PostgreSQL cart and locks the same customer row.

For a new environment, start with an empty database or already migrated
carts. Before switching an existing environment, pause legacy cart and
checkout writes and the Redis cart synchronization job. Keep a backup,
then run the commands with the connection for the store being migrated:

    python3 scripts/legacy_carts.py snapshot /secure/path/carts.jsonl -- -h <redis-host>
    python3 scripts/legacy_carts.py import /secure/path/carts.jsonl
    python3 scripts/legacy_carts.py import /secure/path/carts.jsonl --apply

Use REDISCLI_AUTH for Redis authentication and the PostgreSQL client
environment (PGHOST, PGPORT, PGDATABASE, PGUSER and PGPASSFILE) for import.
The first import command validates and reports counts. The second applies
the snapshot in one transaction. A retry uses the same snapshot. Redis
keys are retained. Go returns 503 if an active legacy cart still needs
reconciliation.

Contract tests
--------------
Start a disposable PostgreSQL 17 instance. Set SKVIDERM_TEST_DATABASE_URL
to point to it, then run:

    go test -count=1 -v ./...

The suite uses temporary tables and synthetic data. It compares the Go
HTTP handlers with responses observed from the Rust version pinned in
the catalog and product contract fixtures under testdata/. The Rust image
was exercised with its original migrations. The Go tests verify both the public response envelope
and the values. Only arrays without a defined ordering are normalized.
Run the importer tests with:

    python3 -B -m unittest discover -s scripts -p test_legacy_carts.py

GitHub Actions runs the tests with PostgreSQL and builds the Linux image.
