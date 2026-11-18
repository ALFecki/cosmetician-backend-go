package main

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

type accountWrites struct {
	*accountAuth
	store           cartDatabase
	redis           *redis.Client
	mailer          accountMailer
	phone           phoneAuthentication
	checkoutEnabled bool
	guestEnabled    bool
}

func newAccountWrites(auth *accountAuth, db cartDatabase, checkoutEnabled, guestEnabled bool) (*accountWrites, error) {
	options, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		return nil, errors.New("account writes require a valid REDIS_URL")
	}
	mailer, err := newAccountMailer(os.Getenv("EMAIL_LOGIN"), os.Getenv("EMAIL_PASSWORD"))
	if err != nil {
		return nil, err
	}
	phone, err := configuredPhoneAuthentication(checkoutEnabled)
	if err != nil {
		return nil, err
	}
	return &accountWrites{accountAuth: auth, store: db, redis: redis.NewClient(options), mailer: mailer, phone: phone, checkoutEnabled: checkoutEnabled, guestEnabled: guestEnabled}, nil
}

func (writes *accountWrites) Close() error {
	return writes.redis.Close()
}

func (writes *accountWrites) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/register", writes.registerAccount)
	mux.HandleFunc("GET /api/forgot_password", writes.sendResetPassword)
	mux.HandleFunc("GET /api/verify_reset_token", writes.verifyResetPassword)
	mux.HandleFunc("POST /api/reset_password", writes.resetAccountPassword)
	mux.HandleFunc("GET /api/verify_email", writes.sendEmailVerification)
	mux.HandleFunc("GET /api/email_verification", writes.verifyAccountEmail)
	mux.HandleFunc("POST /api/person", writes.createPerson)
	mux.HandleFunc("PATCH /api/person/{id}", writes.patchPerson)
	mux.HandleFunc("DELETE /api/person/{id}", writes.deletePerson(false))
	mux.HandleFunc("DELETE /api/person/cascade/{id}", writes.deletePerson(true))
	mux.HandleFunc("PATCH /api/checkout/profile", writes.saveCheckoutProfile)
	mux.HandleFunc("POST /api/checkout/guest/claim", writes.claimGuestOrders)
	mux.HandleFunc("GET /api/auth/phone/config", writes.phoneConfig)
	mux.HandleFunc("POST /api/auth/phone/start", writes.startPhone)
	mux.HandleFunc("POST /api/auth/phone/verify", writes.verifyPhone)
	mux.HandleFunc("POST /api/auth/phone/complete", writes.completePhone)
}

func readAccountInput[T any](w http.ResponseWriter, r *http.Request, input *T) bool {
	if !isJSONRequest(r) {
		writeText(w, http.StatusBadRequest, "Content type error")
		return false
	}
	var decoded *T
	if err := jsonv2.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBodySize), &decoded); err != nil || decoded == nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeText(w, http.StatusRequestEntityTooLarge, "Request is too large")
		} else {
			writeText(w, http.StatusBadRequest, "Invalid JSON")
		}
		return false
	}
	*input = *decoded
	return true
}

type personCreateInput struct {
	FirstName       *string `json:"firstName"`
	SecondName      *string `json:"secondName"`
	Surname         *string `json:"surname"`
	Email           *string `json:"email"`
	IsEmailVerified *bool   `json:"isEmailVerified"`
	Phone           *string `json:"phone"`
	Password        *string `json:"password"`
	IsAdmin         bool    `json:"isAdmin"`
	IsModerator     bool    `json:"isModerator"`
	IsDeleted       bool    `json:"isDeleted"`
	Image           *string `json:"image"`
}

type personPatchInput struct {
	FirstName       *string `json:"firstName"`
	SecondName      *string `json:"secondName"`
	Surname         *string `json:"surname"`
	Email           *string `json:"email"`
	Phone           *string `json:"phone"`
	Password        *string `json:"password"`
	Image           *string `json:"image"`
	IsEmailVerified *bool   `json:"isEmailVerified"`
}

const returnAccount = " RETURNING " + accountColumns + ", NULL::text"

func (writes *accountWrites) registerAccount(w http.ResponseWriter, r *http.Request) {
	input, ok := readPersonCreate(w, r)
	if !ok {
		return
	}
	input.IsEmailVerified = new(bool)
	input.IsAdmin, input.IsModerator, input.IsDeleted = false, false, false
	user, err := writes.insertPerson(r.Context(), input)
	if err != nil {
		writePersonError(w, err)
		return
	}
	writes.writeSession(w, user, false)
}

func (writes *accountWrites) createPerson(w http.ResponseWriter, r *http.Request) {
	if !writes.admin(w, r) {
		return
	}
	input, ok := readPersonCreate(w, r)
	if !ok {
		return
	}
	user, err := writes.insertPerson(r.Context(), input)
	writePersonResult(w, user, err, true)
}

func readPersonCreate(w http.ResponseWriter, r *http.Request) (personCreateInput, bool) {
	var input personCreateInput
	if !readAccountInput(w, r, &input) {
		return input, false
	}
	if input.FirstName == nil || input.SecondName == nil || input.Email == nil || input.Phone == nil || input.Password == nil ||
		!validMailRecipient(*input.Email) || !validWirePassword(*input.Password) {
		writeText(w, http.StatusBadRequest, "Invalid person data")
		return input, false
	}
	return input, true
}

func validWirePassword(raw string) bool {
	return len(raw) > 0 && len(raw) <= 1024
}

func hashAccountPassword(raw string) (string, error) {
	raw = raw[:min(len(raw), 72)]
	hash, err := bcrypt.GenerateFromPassword([]byte(raw), 12)
	return string(hash), err
}

func (writes *accountWrites) insertPerson(ctx context.Context, input personCreateInput) (account, error) {
	password, err := hashAccountPassword(*input.Password)
	if err != nil {
		return account{}, err
	}
	tx, err := writes.store.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return account{}, err
	}
	defer rollbackCart(ctx, tx)
	user, err := scanAccount(tx.QueryRow(ctx, `INSERT INTO person
		(first_name, second_name, surname, email, phone, password, is_email_verified, is_admin, is_moderator, is_deleted, image)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`+returnAccount,
		input.FirstName, input.SecondName, input.Surname, input.Email, input.Phone, password,
		input.IsEmailVerified, input.IsAdmin, input.IsModerator, input.IsDeleted, input.Image))
	if err != nil {
		return account{}, err
	}
	return user, tx.Commit(ctx)
}

func (writes *accountWrites) writeSession(w http.ResponseWriter, user account, phoneAuth bool) {
	email := ""
	if user.Email != nil {
		email = *user.Email
	}
	admin, moderator := !phoneAuth && user.IsAdmin, !phoneAuth && user.IsModerator
	claims := accountClaims{
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour))},
		ID:               user.ID, Email: &email, IsAdmin: &admin, IsModerator: &moderator, PhoneAuth: phoneAuth, CSRFToken: uuid.NewString(),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(writes.key)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "Can't create token")
		return
	}
	http.SetCookie(w, writes.sessionCookie(token))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, accountResponse{Claims: claims, User: user})
}

func (writes *accountWrites) admin(w http.ResponseWriter, r *http.Request) bool {
	claims, ok := writes.protectedSession(w, r)
	if !ok {
		return false
	}
	if !*claims.IsAdmin || claims.PhoneAuth {
		writeText(w, http.StatusUnauthorized, "Unauthorized")
		return false
	}
	var allowed bool
	if err := writes.db.QueryRow(r.Context(), "SELECT is_admin AND is_deleted IS NOT TRUE FROM person WHERE id=$1", claims.ID).Scan(&allowed); err != nil || !allowed {
		writeText(w, http.StatusUnauthorized, "Unauthorized")
		return false
	}
	return true
}

func (writes *accountWrites) patchPerson(w http.ResponseWriter, r *http.Request) {
	claims, ok := writes.protectedSession(w, r)
	if !ok {
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	if id != claims.ID {
		if !*claims.IsAdmin || claims.PhoneAuth {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !writes.admin(w, r) {
			return
		}
	}
	var input personPatchInput
	if !readAccountInput(w, r, &input) {
		return
	}
	switch {
	case claims.PhoneAuth && (input.Email != nil || input.Password != nil):
		writeText(w, http.StatusForbidden, "Use password sign-in to change login credentials")
		return
	case input.IsEmailVerified != nil:
		writeText(w, http.StatusBadRequest, "Email verification requires a verification code")
		return
	case input.Email != nil && !validMailRecipient(*input.Email):
		writeText(w, http.StatusBadRequest, "Invalid email")
		return
	case input.Password != nil && !validWirePassword(*input.Password):
		writeText(w, http.StatusBadRequest, "Invalid password")
		return
	}
	if input.Password != nil {
		hash, err := hashAccountPassword(*input.Password)
		if err != nil {
			writePersonError(w, err)
			return
		}
		input.Password = &hash
	}
	user, err := writes.updatePerson(r.Context(), id, input)
	writePersonResult(w, user, err, true)
}

func (writes *accountWrites) updatePerson(ctx context.Context, id pgtype.UUID, input personPatchInput) (account, error) {
	tx, err := writes.store.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return account{}, err
	}
	defer rollbackCart(ctx, tx)
	var verifiedPhone *string
	if err := tx.QueryRow(ctx, "SELECT verified_phone FROM person WHERE id=$1 AND is_deleted IS NOT TRUE FOR UPDATE", id).Scan(&verifiedPhone); err != nil {
		return account{}, err
	}
	if input.Phone != nil && verifiedPhone != nil {
		phone, err := normalizeCheckoutPhone(*input.Phone)
		if err != nil || phone != *verifiedPhone {
			return account{}, quoteInputError("Verify the new phone before changing it")
		}
		input.Phone = verifiedPhone
	}
	user, err := scanAccount(tx.QueryRow(ctx, `UPDATE person SET
		first_name=COALESCE($2,first_name), second_name=COALESCE($3,second_name), surname=COALESCE($4,surname),
		email=COALESCE($5,email), phone=COALESCE($6,phone), password=COALESCE($7,password), image=COALESCE($8,image),
		is_email_verified=CASE WHEN $5::text IS NULL THEN is_email_verified ELSE false END, updated_at=now()
		WHERE id=$1`+returnAccount, id, input.FirstName, input.SecondName, input.Surname,
		input.Email, input.Phone, input.Password, input.Image))
	if err != nil {
		return account{}, err
	}
	return user, tx.Commit(ctx)
}

func (writes *accountWrites) deletePerson(hard bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !writes.admin(w, r) {
			return
		}
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		tx, err := writes.store.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
		if err != nil {
			writePersonError(w, err)
			return
		}
		defer rollbackCart(ctx, tx)
		query := "UPDATE person SET is_deleted=true WHERE id=$1"
		if hard {
			var lockedID pgtype.UUID
			if err := tx.QueryRow(ctx, "SELECT id FROM person WHERE id=$1 FOR UPDATE", id).Scan(&lockedID); err != nil {
				writePersonError(w, err)
				return
			}
			var referenced bool
			err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM "order" WHERE person_id=$1)
				OR EXISTS (SELECT 1 FROM promo_code_usage WHERE person_id=$1)`, lockedID).Scan(&referenced)
			if err != nil {
				writePersonError(w, err)
				return
			}
			if referenced {
				writeText(w, http.StatusConflict, "У клиента есть история заказов. Перенесите его в архив.")
				return
			}
			query = "DELETE FROM person WHERE id=$1"
		}
		user, err := scanAccount(tx.QueryRow(ctx, query+returnAccount, id))
		if err != nil {
			writePersonError(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writePersonError(w, err)
			return
		}
		writeJSON(w, singleResponse{Status: "success", Data: user})
	}
}

func (writes *accountWrites) saveCheckoutProfile(w http.ResponseWriter, r *http.Request) {
	claims, ok := writes.protectedSession(w, r)
	if !ok {
		return
	}
	var input struct {
		FirstName  string  `json:"firstName"`
		SecondName string  `json:"secondName"`
		Email      *string `json:"email"`
	}
	if !readAccountInput(w, r, &input) {
		return
	}
	first, second, email, err := validateCheckoutContact(input.FirstName, input.SecondName, input.Email)
	if err != nil {
		writePersonError(w, err)
		return
	}
	user, err := writes.updateContactProfile(r.Context(), claims.ID, first, second, email)
	if errors.Is(err, pgx.ErrNoRows) {
		err = quoteInputError("Сначала подтвердите телефон")
	}
	writePersonResult(w, user, err, false)
}

func (writes *accountWrites) updateContactProfile(ctx context.Context, id pgtype.UUID, first, second string, email *string) (account, error) {
	tx, err := writes.store.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return account{}, err
	}
	defer rollbackCart(ctx, tx)
	user, err := scanAccount(tx.QueryRow(ctx, `UPDATE person SET first_name=$2, second_name=$3, contact_email=$4, updated_at=now()
		WHERE id=$1 AND verified_phone IS NOT NULL AND is_deleted=false`+returnAccount, id, first, second, email))
	if err != nil {
		return account{}, err
	}
	return user, tx.Commit(ctx)
}

func (writes *accountWrites) claimGuestOrders(w http.ResponseWriter, r *http.Request) {
	account, ok := writes.protectedSession(w, r)
	if !ok {
		return
	}
	guest, ok := writes.guestSession(w, r, true)
	if !ok {
		return
	}
	if guest.ID == uuid.UUID(account.ID.Bytes) {
		writeCheckoutError(w, http.StatusUnauthorized, guestSessionExpired)
		return
	}
	count, err := writes.claimGuest(r.Context(), guest.ID, account.ID)
	if err != nil {
		writeCartError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]int{"claimed": count})
}

func (writes *accountWrites) claimGuest(ctx context.Context, guest uuid.UUID, target pgtype.UUID) (int, error) {
	tx, err := writes.store.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return 0, err
	}
	defer rollbackCart(ctx, tx)
	rows, err := tx.Query(ctx, `SELECT id, verified_phone, email IS NULL AND password IS NULL AND verified_phone IS NULL
		AND NOT is_admin AND NOT is_moderator FROM person WHERE id IN ($1,$2) AND NOT is_deleted ORDER BY id FOR UPDATE`, guest, target)
	if err != nil {
		return 0, err
	}
	type customerClaim struct {
		ID    uuid.UUID
		Phone *string
		Guest bool
	}
	customers, err := pgx.CollectRows(rows, pgx.RowToStructByPos[customerClaim])
	if err != nil {
		return 0, err
	}
	var phone *string
	var guestValid bool
	for _, customer := range customers {
		switch customer.ID {
		case guest:
			guestValid = customer.Guest
		case uuid.UUID(target.Bytes):
			phone = customer.Phone
		}
	}
	if !guestValid {
		return 0, cartRequestError{http.StatusUnauthorized, guestSessionExpired}
	}
	if phone == nil {
		return 0, cartRequestError{http.StatusUnauthorized, "Сначала подтвердите номер телефона"}
	}
	rows, err = tx.Query(ctx, `UPDATE "order" SET person_id=$1, updated_at=now() WHERE person_id=$2 AND status<>'NotChecked'
		AND NOT is_deleted AND contact_snapshot->>'phone'=$3 RETURNING id`, target, guest, *phone)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, "UPDATE promo_code_usage SET person_id=$1 WHERE order_id=ANY($2) AND person_id=$3", target, ids, guest); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

func writePersonResult(w http.ResponseWriter, user account, err error, envelope bool) {
	if err != nil {
		writePersonError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if envelope {
		writeJSON(w, singleResponse{Status: "success", Data: user})
		return
	}
	writeJSON(w, user)
}

func writePersonError(w http.ResponseWriter, err error) {
	var badInput quoteInputError
	var postgres *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		w.WriteHeader(http.StatusNotFound)
	case errors.As(err, &badInput):
		writeText(w, http.StatusBadRequest, badInput.Error())
	case errors.As(err, &postgres) && postgres.Code == "23505":
		writeText(w, http.StatusConflict, "Account already exists")
	default:
		slog.Error("write account failed")
		writeText(w, http.StatusInternalServerError, "Could not save account")
	}
}
