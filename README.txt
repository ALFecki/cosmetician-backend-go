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
same authenticated session and CSRF header as GET /api/checkout/attempt/{id}.

Set CLIENT_URL to the frontend origin used by Rust to enable the guest
checkout GET endpoints for session restore, order history and checkout
attempts. The signing secret and browser cookies are shared with Rust.
Guest history and completed attempts remain available when new guest
checkout is disabled.

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

Order confirmation
------------------
Enable ORDER_CONFIRMATION_ENABLED with CART_STORAGE_ENABLED to serve
POST /api/checkout. For guest checkout, set GUEST_CHECKOUT_ENABLED=true
with CLIENT_URL and REDIS_URL pointing to the same Redis database used
by Rust for guest admission counters. Set SMS_TRUSTED_PROXY_IP to the
same trusted proxy IP if it supplies X-Real-IP. Account checkout follows
the same verified-phone requirement as Rust when guest checkout is off.

Go saves confirmed orders with the existing checkout key, notification
and payment state. Keep the Rust background jobs active while they
handle order notifications.

Online payments
---------------
Enable ORDER_PAYMENTS_ENABLED to serve POST /api/payments/process and
POST /api/checkout/guest/payments/process with the same response as Rust.
Use the existing JWT_SECRET, CLIENT_URL, ACQUIRING_URL,
ACQUIRING_ORDER_STATUS_URL, ACQUIRING_USERNAME and ACQUIRING_PASSWORD.
The bank URLs must use HTTPS. Route both payment endpoints together.
The existing PostgreSQL payment fields coordinate registration across
Rust and Go. A repeat returns a saved payment link. If the bank outcome
is uncertain, the attempt stays reserved for manual reconciliation.

The optional PAYMENT_STATUS_SYNC_ENABLED worker uses the same bank and
PostgreSQL state. Leave it off while Rust background jobs are running.
The current Rust BACKGROUND_JOBS_ENABLED controls both that payment
worker and order notifications. Keep it enabled until notifications
have their own worker. At handover, enable Go status sync on one replica.

Contract tests
--------------
Start a disposable PostgreSQL 17 instance. Set SKVIDERM_TEST_DATABASE_URL
to point to it, then run:

    go test -count=1 -v ./...

Set SKVIDERM_TEST_REDIS_URL to an isolated Redis instance to also run
the guest admission counter tests.

The suite uses temporary tables and synthetic data. It compares the Go
HTTP handlers with responses observed from the Rust version pinned in
the catalog and product contract fixtures under testdata/. The Rust image
was exercised with its original migrations. The Go tests verify both the public response envelope
and the values. Only arrays without a defined ordering are normalized.
Run the importer tests with:

    python3 -B -m unittest discover -s scripts -p test_legacy_carts.py

GitHub Actions runs the tests with PostgreSQL, Redis and a local HTTPS
bank simulator, and builds the Linux image.
