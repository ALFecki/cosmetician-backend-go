SkViDerm Go backend
==================

First migration slice: public read-only GET /api/brand, /api/brand/{id},
/api/category, /api/category/{id}, and /api/banner. The Rust backend is
the reference for behavior. This service expects the existing PostgreSQL
schema, connects in read-only mode, and does not run migrations.

Running locally
---------------
Use the Go version specified in go.mod. Set DATABASE_URL for a disposable
database with the existing Rust migrations applied and run:

    go run .

Set HTTP_ADDR to select a listen address (defaults to :8000). To build the
Linux production architecture on another machine:

    docker build --platform linux/amd64 -t cosmetician-backend-go .

Contract tests
--------------
Start a disposable PostgreSQL 17 instance. Set SKVIDERM_TEST_DATABASE_URL
to point to it, then run:

    go test -count=1 -v ./...

The suite uses temporary tables and synthetic data. It compares the Go
HTTP handlers with responses observed from the Rust version pinned in
testdata/catalog_contract.json. The Rust image was exercised with its
original migrations. The Go tests verify both the public response envelope
and the values. Only arrays without a defined ordering are normalized.
GitHub Actions runs the tests with PostgreSQL and builds the Linux image.
