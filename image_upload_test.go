package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

func TestAdminImageUpload(t *testing.T) {
	var pngFile bytes.Buffer
	if err := png.Encode(&pngFile, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	objects := make(map[string][]byte)
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "GET" && r.URL.Path == "/fixture-bucket/" && r.URL.Query().Has("location") {
			_, _ = w.Write([]byte("<LocationConstraint>us-east-1</LocationConstraint>"))
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-access/") {
			t.Error("storage request must use Signature V4")
		}
		if r.Method == "PUT" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if r.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, pngFile.Bytes()) {
				t.Errorf("the storage write changed the image or media type: %d bytes", len(body))
			}
			if _, exists := objects[r.URL.Path]; exists {
				t.Errorf("upload overwrote an object: %s", r.URL.Path)
			}
			objects[r.URL.Path] = body
			w.Header().Set("ETag", `"fixture-upload"`)
			return
		}

		body, exists := objects[r.URL.Path]
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"fixture-upload"`)
		w.Header().Set("Last-Modified", "Wed, 01 Jan 2025 00:00:00 GMT")
		_, _ = w.Write(body)
	}))
	defer s3.Close()

	images, err := newImageStore(s3.URL, "test-access", "test-secret", "fixture-bucket")
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("test-only")
	auth := &accountAuth{key: key}
	handler := imageUploadTestHandler(auth, images)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)
	customer := profileCookie(t, key, testAccountID, false, false, false)

	request := func(body []byte, name, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
		return sendAdminImageUpload(t, handler, body, name, csrf, cookie)
	}
	assertCartStatus(t, request(pngFile.Bytes(), "fixture.png", "", nil), http.StatusUnauthorized)
	assertCartStatus(t, request(pngFile.Bytes(), "fixture.png", "fixture-csrf", customer), http.StatusUnauthorized)
	assertCartStatus(t, request(pngFile.Bytes(), "fixture.png", "wrong", moderator), http.StatusUnauthorized)
	assertCartStatus(t, request([]byte("<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"), "fixture.png", "fixture-csrf", moderator), http.StatusBadRequest)

	for range 2 {
		result := request(pngFile.Bytes(), "../fixture.jpeg", "fixture-csrf", moderator)
		assertCartStatus(t, result, http.StatusOK)
		var response struct {
			Status string `json:"status"`
			Data   struct {
				Path string `json:"path"`
			} `json:"data"`
		}
		if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Status != "success" || !strings.HasPrefix(response.Data.Path, "/images/") ||
			!strings.HasSuffix(response.Data.Path, ".png") || strings.Contains(response.Data.Path, "fixture") {
			t.Fatalf("unexpected upload response: %s", result.Body)
		}
		read := httptest.NewRecorder()
		handler.ServeHTTP(read, httptest.NewRequestWithContext(t.Context(), "GET", "/api"+response.Data.Path, nil))
		assertCartStatus(t, read, http.StatusOK)
		if read.Header().Get("Content-Type") != "image/png" || !bytes.Equal(read.Body.Bytes(), pngFile.Bytes()) {
			t.Fatal("the public URL did not return the uploaded PNG")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(objects) != 2 {
		t.Fatalf("wanted only 2 successful uploads, got %d", len(objects))
	}
}

func TestAdminImageUploadMinIO(t *testing.T) {
	endpoint := os.Getenv("SKVIDERM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set SKVIDERM_TEST_MINIO_ENDPOINT to an isolated MinIO")
	}
	images, err := newImageStore(endpoint, "fixture-minio", "fixture-minio-test-only", "catalog-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := images.client.MakeBucket(ctx, images.bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	defer images.client.RemoveBucket(ctx, images.bucket)
	key := []byte("test-only")
	auth := &accountAuth{key: key}
	handler := imageUploadTestHandler(auth, images)
	moderator := profileCookie(t, key, testModeratorID, false, true, false)

	var pngFile bytes.Buffer
	if err := png.Encode(&pngFile, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	upload := sendAdminImageUpload(t, handler, pngFile.Bytes(), "real.png", "fixture-csrf", moderator)
	var payload struct {
		Data struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	readOrderHistory(t, upload, &payload)
	objectKey, ok := strings.CutPrefix(payload.Data.Path, "/images/")
	if !ok {
		t.Fatalf("unexpected URL: %q", payload.Data.Path)
	}
	defer images.client.RemoveObject(ctx, images.bucket, objectKey, minio.RemoveObjectOptions{})
	download := httptest.NewRecorder()
	handler.ServeHTTP(download, httptest.NewRequestWithContext(ctx, "GET", "/api"+payload.Data.Path, nil))
	assertCartStatus(t, download, http.StatusOK)
	if !bytes.Equal(download.Body.Bytes(), pngFile.Bytes()) || download.Header().Get("Content-Type") != "image/png" {
		t.Fatal("MinIO did not return the stored PNG")
	}
}

func sendAdminImageUpload(t *testing.T, handler http.Handler, body []byte, name, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var data bytes.Buffer
	form := multipart.NewWriter(&data)
	part, err := form.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/upload/image", &data)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrf)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestAdminImageUploadLimit(t *testing.T) {
	key := []byte("test-only")
	auth := &accountAuth{key: key}
	handler := imageUploadTestHandler(auth, &imageStore{})
	admin := profileCookie(t, key, testAdminID, true, false, false)
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/upload/image", errOnRead{})
	req.ContentLength = 51 << 20
	req.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	req.Header.Set("X-CSRF-Token", "fixture-csrf")
	req.AddCookie(admin)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	assertCartStatus(t, response, http.StatusRequestEntityTooLarge)
}

type errOnRead struct{}

func (errOnRead) Read([]byte) (int, error) { return 0, errors.New("body should not be read") }

func imageUploadTestHandler(auth *accountAuth, images *imageStore) http.Handler {
	mux := http.NewServeMux()
	auth.registerImageUpload(mux, images)
	mux.Handle("/", newHandler(nil, images, auth, false))
	return mux
}
