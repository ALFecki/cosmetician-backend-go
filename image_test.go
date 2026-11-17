package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPublicImages(t *testing.T) {
	blob := []byte{0x89, 'P', 'N', 'G', 0, 0xff}
	cases := []struct {
		name, path, s3Path, contentType string
		status, s3Status                int
		body                            []byte
	}{
		{"public PNG", "/api/images/fixture.png", "/fixture-bucket/fixture.png", "image/png", 200, 200, blob},
		{"encoded nested WebP key", "/api/images/nested/fixture%20name.webp", "/fixture-bucket/nested/fixture name.webp", "image/webp", 200, 200, blob},
		{"JPEG extension", "/api/images/fixture.jpeg", "/fixture-bucket/fixture.jpeg", "image/jpeg", 200, 200, blob},
		{"GIF extension", "/api/images/fixture.gif", "/fixture-bucket/fixture.gif", "image/gif", 200, 200, blob},
		{"unknown type", "/api/images/fixture.bin", "/fixture-bucket/fixture.bin", "application/octet-stream", 200, 200, blob},
		{"case sensitive extension", "/api/images/fixture.PNG", "/fixture-bucket/fixture.PNG", "application/octet-stream", 200, 200, blob},
		{"missing object", "/api/images/missing.png", "/fixture-bucket/missing.png", "", 404, 404, []byte("File not found")},
		{"nonpublic object", "/api/images/denied.png", "/fixture-bucket/denied.png", "", 404, 403, []byte("File not found")},
		{"dot-dot in key", "/api/images/bad..png", "", "", 400, 0, []byte("Invalid path")},
		{"literal parent segment", "/api/images/../private.png", "", "", 400, 0, []byte("Invalid path")},
		{"encoded parent segment", "/api/images/%2e%2e/private.png", "", "", 400, 0, []byte("Invalid path")},
		{"encoded absolute key", "/api/images/%2Fprivate.png", "", "", 400, 0, []byte("Invalid path")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls, locations atomic.Int32
			s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/fixture-bucket/" && r.URL.Query().Has("location") {
					locations.Add(1)
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte("<LocationConstraint>us-east-1</LocationConstraint>"))
					return
				}
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != tc.s3Path {
					t.Errorf("storage request %s %q, want GET %q", r.Method, r.URL.Path, tc.s3Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-access/") {
					t.Error("storage request must use Signature V4")
				}
				if tc.s3Status != http.StatusOK {
					code := "NoSuchKey"
					if tc.s3Status == http.StatusForbidden {
						code = "AccessDenied"
					}
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(tc.s3Status)
					_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>fixture only</Message></Error>", code)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Last-Modified", "Wed, 01 Jan 2025 00:00:00 GMT")
				w.Header().Set("ETag", `"fixture-etag"`)
				_, _ = w.Write(blob)
			}))
			defer s3.Close()

			images, err := newImageStore(s3.URL, "test-access", "test-secret", "fixture-bucket")
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			newHandler(nil, images, nil, false).ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", response.Code, tc.status, response.Body.Bytes())
			}
			if got := response.Header().Get("Content-Type"); got != tc.contentType {
				t.Errorf("Content-Type = %q, want %q", got, tc.contentType)
			}
			if !bytes.Equal(response.Body.Bytes(), tc.body) {
				t.Errorf("body = %q, want %q", response.Body.Bytes(), tc.body)
			}
			var wantCalls int32
			if tc.s3Path != "" {
				wantCalls = 1
			}
			if got := locations.Load(); got != wantCalls {
				t.Errorf("storage location calls = %d, want %d", got, wantCalls)
			}
			if got := calls.Load(); got != wantCalls {
				t.Errorf("storage calls = %d, want %d", got, wantCalls)
			}
		})
	}
}

func TestImageStoreConfig(t *testing.T) {
	disabled, err := newImageStore("", "", "", "")
	if err != nil || disabled != nil {
		t.Fatal("all empty MinIO settings must disable image routes")
	}
	for _, endpoint := range []string{"", "minio:9000", "ftp://minio:9000", "http://minio:9000/subpath", "http://minio:9000?x=1", "http://user:secret@minio:9000"} {
		if _, err := newImageStore(endpoint, "test-access", "test-secret", "fixture-bucket"); err == nil {
			t.Errorf("accepted invalid MinIO endpoint %q", endpoint)
		}
	}
	if _, err := newImageStore("http://minio:9000", "test-access", "", "fixture-bucket"); err == nil {
		t.Error("accepted incomplete credentials")
	}
}
