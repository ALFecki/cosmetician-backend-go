SkViDerm Go backend
==================

The Go service implements public storefront reads for brands, categories,
banners, products, variants, promotions, product promo codes and images. The
Rust backend is the behavioral reference. The Go service connects to the
existing PostgreSQL schema in read-only mode. Rust owns the migrations.

Running locally
---------------
Use the Go version specified in go.mod. Set DATABASE_URL for a disposable
database with the existing Rust migrations applied and run:

    go run .

Set HTTP_ADDR to select a listen address (defaults to :8000). The public
/api/images/... route reads files from MinIO when MINIO_ENDPOINT,
MINIO_ACCESS_KEY, MINIO_SECRET_KEY and MINIO_BUCKET are supplied. Provide
an absolute HTTP(S) endpoint and credentials allowed to read the bucket.

Set JWT_SECRET to enable POST /api/login and GET /api/check_auth. For
compatible sessions between Rust and Go, configure the same signing
secret on both services. Cookies are secure by default. Set
SECURE_MODE=false for local HTTP.
To build the Linux production architecture on another machine:

    docker build --platform linux/amd64 -t cosmetician-backend-go .

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
GitHub Actions runs the tests with PostgreSQL and builds the Linux image.
