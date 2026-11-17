package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestCatalogContract(t *testing.T) {
	dsn := os.Getenv("SKVIDERM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SKVIDERM_TEST_DATABASE_URL to run the PostgreSQL contract tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test PostgreSQL:", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, file := range []string{"testdata/catalog_schema.sql", "testdata/catalog_rows.sql"} {
		sql, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}

	fixture, err := os.ReadFile("testdata/catalog_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		RustCommit string
		Cases      []struct {
			Name, Path, ContentType, CacheControl, Body                             string
			Method, AllowOrigin, AllowCredentials, AllowMethods, AllowHeaders, Vary string
			RequestHeaders                                                          map[string]string
			Unordered, HideBanners                                                  bool
			Status                                                                  int
			JSON                                                                    json.RawMessage
		}
	}
	if err := json.Unmarshal(fixture, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.RustCommit == "" || len(contract.Cases) == 0 {
		t.Fatal("missing Rust reference revision or scenarios")
	}
	handler := newHandler(tx)
	for _, tc := range contract.Cases {
		t.Run(tc.Path+" "+tc.Name, func(t *testing.T) {
			if tc.HideBanners {
				if _, err := tx.Exec(ctx, "UPDATE pg_temp.banner SET is_active = false"); err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			method := tc.Method
			if method == "" {
				method = http.MethodGet
			}
			request := httptest.NewRequestWithContext(ctx, method, tc.Path, nil)
			for key, value := range tc.RequestHeaders {
				request.Header.Set(key, value)
			}
			handler.ServeHTTP(response, request)
			if response.Code != tc.Status {
				t.Fatalf("status = %d, want %d (body %s)", response.Code, tc.Status, response.Body)
			}
			if got := response.Header().Get("Cache-Control"); got != tc.CacheControl {
				t.Errorf("Cache-Control = %q, want %q", got, tc.CacheControl)
			}
			if got := strings.SplitN(response.Header().Get("Content-Type"), ";", 2)[0]; got != strings.SplitN(tc.ContentType, ";", 2)[0] {
				t.Errorf("Content-Type = %q, want %q", got, tc.ContentType)
			}
			if len(tc.RequestHeaders) > 0 {
				for name, expected := range map[string]string{
					"Access-Control-Allow-Origin":      tc.AllowOrigin,
					"Access-Control-Allow-Credentials": tc.AllowCredentials,
					"Access-Control-Allow-Methods":     tc.AllowMethods,
					"Access-Control-Allow-Headers":     tc.AllowHeaders,
					"Vary":                             tc.Vary,
				} {
					actual := strings.Join(response.Header().Values(name), ",")
					if !reflect.DeepEqual(headerTokens(actual), headerTokens(expected)) {
						t.Errorf("%s = %q, want %q", name, actual, expected)
					}
				}
			}
			if tc.JSON == nil {
				if response.Body.String() != tc.Body {
					t.Errorf("body = %q, want %q", response.Body, tc.Body)
				}
				return
			}
			got := decodeContract(t, response.Body.Bytes(), tc.Unordered)
			want := decodeContract(t, tc.JSON, tc.Unordered)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("body = %s\nwant = %s", response.Body, tc.JSON)
			}
		})
	}
}

func decodeContract(t *testing.T, data []byte, unordered bool) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if unordered {
		items := result["data"].([]any)
		slices.SortFunc(items, func(a, b any) int {
			return strings.Compare(a.(map[string]any)["id"].(string), b.(map[string]any)["id"].(string))
		})
	}
	return result
}

func headerTokens(value string) []string {
	tokens := strings.Split(strings.ToLower(value), ",")
	for index, token := range tokens {
		tokens[index] = strings.TrimSpace(token)
	}
	sort.Strings(tokens)
	return tokens
}
