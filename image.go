package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type imageStore struct {
	client *minio.Core
	bucket string
}

func newImageStore(endpoint, accessKey, secretKey, bucket string) (*imageStore, error) {
	if endpoint == "" && accessKey == "" && secretKey == "" && bucket == "" {
		return nil, nil
	}
	if endpoint == "" || accessKey == "" || secretKey == "" || bucket == "" {
		return nil, errors.New("all MinIO settings are required for image serving")
	}

	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || (parsed.Path != "" && parsed.Path != "/") ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("invalid MINIO_ENDPOINT")
	}
	client, err := minio.NewCore(parsed.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       parsed.Scheme == "https",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, errors.New("cannot initialize MinIO image client")
	}
	return &imageStore{client: client, bucket: bucket}, nil
}

func (store *imageStore) routes(next http.Handler) http.Handler {
	// ServeMux redirects paths with literal dot segments before the handler can reject the key.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, matched := strings.CutPrefix(r.URL.Path, "/api/images/")
		if !matched {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		store.serve(w, r, name)
	})
}

func (store *imageStore) serve(w http.ResponseWriter, r *http.Request, name string) {
	if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		writeImageError(w, http.StatusBadRequest, "Invalid path")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	object, _, _, err := store.client.GetObject(ctx, store.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		writeImageError(w, http.StatusNotFound, "File not found")
		return
	}
	defer object.Close()
	body, err := io.ReadAll(object)
	if err != nil {
		writeImageError(w, http.StatusNotFound, "File not found")
		return
	}
	w.Header().Set("Content-Type", imageContentType(name))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

func imageContentType(name string) string {
	extension := path.Ext(name)
	if path.Base(name) != extension {
		switch extension {
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".png":
			return "image/png"
		case ".gif":
			return "image/gif"
		case ".webp":
			return "image/webp"
		}
	}
	return "application/octet-stream"
}

func writeImageError(w http.ResponseWriter, code int, message string) {
	w.Header()["Content-Type"] = nil
	w.WriteHeader(code)
	_, _ = w.Write([]byte(message))
}
