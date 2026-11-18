package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

const maxImageUploadRequest = 50 << 20

func (auth *accountAuth) registerImageUpload(mux *http.ServeMux, images *imageStore) {
	if images == nil {
		return
	}
	mux.HandleFunc("POST /api/upload/image", func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}
		if !*claims.IsAdmin && !*claims.IsModerator {
			writeText(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		images.upload(w, r)
	})
}

func (store *imageStore) upload(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > maxImageUploadRequest {
		writeText(w, http.StatusRequestEntityTooLarge, "Image upload is too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageUploadRequest)
	form, err := r.MultipartReader()
	if err != nil {
		writeText(w, http.StatusBadRequest, "Expected multipart/form-data")
		return
	}
	part, err := form.NextPart()
	if err != nil {
		writeUploadFormError(w, err)
		return
	}
	if part.FormName() != "file" || part.FileName() == "" {
		writeText(w, http.StatusBadRequest, "Expected an image file")
		return
	}

	data, err := io.ReadAll(part)
	if err != nil {
		writeUploadFormError(w, err)
		return
	}
	if _, err := form.NextPart(); err != io.EOF {
		writeUploadFormError(w, err)
		return
	}

	contentType := http.DetectContentType(data)
	var extension string
	switch contentType {
	case "image/jpeg":
		extension = ".jpg"
	case "image/png":
		extension = ".png"
	case "image/gif":
		extension = ".gif"
	case "image/webp":
		extension = ".webp"
	default:
		writeText(w, http.StatusBadRequest, "Поддерживаются изображения JPEG, PNG, GIF и WebP")
		return
	}

	key := uuid.NewString() + extension
	digest := sha256.Sum256(data)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	_, err = store.client.PutObject(ctx, store.bucket, key, bytes.NewReader(data), int64(len(data)), "",
		hex.EncodeToString(digest[:]), minio.PutObjectOptions{ContentType: contentType, DisableContentSha256: true})
	if err != nil {
		slog.Error("image upload failed", "storageCode", minio.ToErrorResponse(err).Code)
		writeText(w, http.StatusInternalServerError, "Failed to upload file")
		return
	}

	writeJSON(w, singleResponse{Status: "success", Data: struct {
		Path string `json:"path"`
	}{Path: "/images/" + key}})
}

func writeUploadFormError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeText(w, http.StatusRequestEntityTooLarge, "Image upload is too large")
		return
	}
	writeText(w, http.StatusBadRequest, "Expected one image file")
}
