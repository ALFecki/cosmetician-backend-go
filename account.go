package main

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"
)

type accountAuth struct {
	db            database
	key           []byte
	secureCookies bool
}

type account struct {
	ID              pgtype.UUID     `json:"id"`
	FirstName       string          `json:"firstName"`
	SecondName      string          `json:"secondName"`
	Surname         *string         `json:"surname"`
	Email           *string         `json:"email"`
	IsEmailVerified *bool           `json:"isEmailVerified"`
	Phone           string          `json:"phone"`
	VerifiedPhone   *string         `json:"verifiedPhone"`
	PhoneVerifiedAt apiTime         `json:"phoneVerifiedAt"`
	ContactEmail    *string         `json:"contactEmail"`
	CheckoutDetails json.RawMessage `json:"checkoutDetails"`
	Password        *string         `json:"-"`
	Image           *string         `json:"image"`
	IsModerator     bool            `json:"isModerator"`
	IsAdmin         bool            `json:"isAdmin"`
	IsDeleted       *bool           `json:"isDeleted"`
	CreatedAt       apiTime         `json:"createdAt"`
	UpdatedAt       apiTime         `json:"updatedAt"`
}

type accountClaims struct {
	jwt.RegisteredClaims
	ID          pgtype.UUID `json:"id"`
	Email       *string     `json:"email"`
	IsModerator *bool       `json:"is_moderator"`
	IsAdmin     *bool       `json:"is_admin"`
	CSRFToken   string      `json:"csrf_token"`
	PhoneAuth   bool        `json:"phone_auth"`
}

type accountResponse struct {
	Claims accountClaims `json:"claims"`
	User   account       `json:"user"`
}

const accountColumns = `id, first_name, second_name, surname, email, is_email_verified, phone,
	verified_phone, phone_verified_at, contact_email, checkout_details, image,
	is_moderator, is_admin, is_deleted, created_at, updated_at`

func scanAccount(row pgx.Row) (account, error) {
	var user account
	err := row.Scan(&user.ID, &user.FirstName, &user.SecondName, &user.Surname, &user.Email,
		&user.IsEmailVerified, &user.Phone, &user.VerifiedPhone, &user.PhoneVerifiedAt,
		&user.ContactEmail, &user.CheckoutDetails, &user.Image, &user.IsModerator, &user.IsAdmin,
		&user.IsDeleted, &user.CreatedAt, &user.UpdatedAt, &user.Password)
	return user, err
}

func (auth *accountAuth) login(w http.ResponseWriter, r *http.Request) {
	if !isJSONRequest(r) {
		writeText(w, http.StatusBadRequest, "Content type error")
		return
	}
	var credentials struct {
		Email    *string `json:"email"`
		Password *string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	if err := jsonv2.UnmarshalRead(r.Body, &credentials); err != nil || credentials.Email == nil || credentials.Password == nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeText(w, http.StatusRequestEntityTooLarge, "Login request is too large")
		} else {
			writeText(w, http.StatusBadRequest, "Invalid login JSON")
		}
		return
	}

	user, err := scanAccount(auth.db.QueryRow(r.Context(),
		"SELECT "+accountColumns+", password FROM person WHERE email = $1 AND is_deleted IS NOT TRUE", *credentials.Email))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("read account for login failed")
	}
	password := *credentials.Password
	// Rust bcrypt uses only the first 72 bytes.
	password = password[:min(len(password), 72)]
	if err != nil || user.Password == nil || bcrypt.CompareHashAndPassword([]byte(*user.Password), []byte(password)) != nil {
		writeAccountUnauthorized(w, "Invalid email or password")
		return
	}
	csrfToken, err := uuid.NewRandom()
	if err != nil {
		writeText(w, http.StatusInternalServerError, "Can't create token")
		return
	}
	claims := accountClaims{
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour))},
		ID:               user.ID, Email: user.Email, IsModerator: &user.IsModerator, IsAdmin: &user.IsAdmin,
		CSRFToken: csrfToken.String(),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.key)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "Can't create token")
		return
	}
	cookie := http.Cookie{Name: "jwt", Value: token, Path: "/", HttpOnly: true, Secure: auth.secureCookies}
	if auth.secureCookies {
		cookie.SameSite = http.SameSiteNoneMode
	}
	http.SetCookie(w, &cookie)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, accountResponse{Claims: claims, User: user})
}

func (auth *accountAuth) session(r *http.Request) (accountClaims, error) {
	var claims accountClaims
	cookie, err := r.Cookie("jwt")
	if err != nil {
		return claims, errors.New("No token")
	}
	_, err = jwt.ParseWithClaims(cookie.Value, &claims, auth.verificationKey,
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired(), jwt.WithLeeway(time.Minute))
	if err == nil && claims.Audience != nil {
		return claims, errors.New("Invalid token: InvalidAudience")
	}
	if err != nil || !claims.ID.Valid || claims.CSRFToken == "" ||
		claims.Email == nil || claims.IsAdmin == nil || claims.IsModerator == nil {
		reason := "InvalidToken"
		switch {
		case errors.Is(err, jwt.ErrTokenSignatureInvalid):
			reason = "InvalidSignature"
		case errors.Is(err, jwt.ErrTokenExpired):
			reason = "ExpiredSignature"
		}
		return claims, errors.New("Invalid token: " + reason)
	}
	return claims, nil
}

func (auth *accountAuth) check(w http.ResponseWriter, r *http.Request) {
	claims, err := auth.session(r)
	if err != nil {
		writeAccountUnauthorized(w, err.Error())
		return
	}
	user, err := auth.accountByID(r.Context(), claims.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("read account for session failed")
		}
		writeAccountUnauthorized(w, "User not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, accountResponse{Claims: claims, User: user})
}

const accountSelect = "SELECT " + accountColumns + ", NULL::text FROM person"

func (auth *accountAuth) accountByID(ctx context.Context, id pgtype.UUID) (account, error) {
	return scanAccount(auth.db.QueryRow(ctx, accountSelect+" WHERE id = $1", id))
}

func (auth *accountAuth) verificationKey(_ *jwt.Token) (any, error) {
	return auth.key, nil
}

func writeAccountUnauthorized(w http.ResponseWriter, message string) {
	w.Header()["Content-Type"] = nil
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(message))
}
