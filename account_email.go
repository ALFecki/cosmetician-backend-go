package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

const (
	resetPasswordToken          = "reset_password"
	verifyEmailToken            = "verify_email"
	accountResetLifetime        = 10 * time.Minute
	accountVerificationLifetime = time.Hour
)

var (
	emailCodePattern   = regexp.MustCompile(`^[0-9]{6}$`)
	deleteAccountToken = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then
		return redis.call('DEL', KEYS[1])
	end
	return 0`)
)

func accountTokenKey(email, purpose string) string {
	return "person:" + email + ":" + purpose
}

func (writes *accountWrites) sendResetPassword(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	if !validMailRecipient(email) {
		writeText(w, http.StatusBadRequest, "Email is required")
		return
	}
	var exists bool
	err := writes.db.QueryRow(r.Context(), "SELECT EXISTS (SELECT 1 FROM person WHERE email=$1 AND is_deleted IS NOT TRUE)", email).Scan(&exists)
	if err != nil {
		writePersonError(w, err)
		return
	}
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		writePersonError(w, err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	parameters := url.Values{"email": {email}, "token": {token}}
	link := writes.guestOrigin + "/reset-password?" + parameters.Encode()
	writes.mailAccountToken(w, r, email, resetPasswordToken, token, accountResetLifetime, "Сброс пароля для SkViDerm",
		"Чтобы сменить пароль в SkViDerm, откройте ссылку: "+link+"\n\nСсылка действует 10 минут.",
		"Password reset email sent successfully.")
}

func (writes *accountWrites) sendEmailVerification(w http.ResponseWriter, r *http.Request) {
	claims, ok := writes.protectedSession(w, r)
	if !ok {
		return
	}
	user, err := scanAccount(writes.db.QueryRow(r.Context(), accountSelect+" WHERE id=$1 AND is_deleted IS NOT TRUE", claims.ID))
	if err != nil {
		writePersonError(w, err)
		return
	}
	if user.IsEmailVerified != nil && *user.IsEmailVerified {
		writeText(w, http.StatusBadRequest, "Email already verified")
		return
	}
	if user.Email == nil || !validMailRecipient(*user.Email) {
		writeText(w, http.StatusBadRequest, "No login email set")
		return
	}
	code, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		writePersonError(w, err)
		return
	}
	token := fmt.Sprintf("%06d", code.Int64())
	writes.mailAccountToken(w, r, *user.Email, verifyEmailToken, token, accountVerificationLifetime,
		"Подтверждение email для SkViDerm", "Код подтверждения электронной почты в SkViDerm: "+token+". Никому его не сообщайте.",
		"Verification email sent successfully.")
}

func (writes *accountWrites) mailAccountToken(w http.ResponseWriter, r *http.Request, email, purpose, token string,
	lifetime time.Duration, subject, message, result string,
) {
	if writes.mailer == nil {
		writeText(w, http.StatusServiceUnavailable, "Email is temporarily unavailable")
		return
	}
	ctx := r.Context()
	key := accountTokenKey(email, purpose)
	accepted, err := writes.redis.SetNX(ctx, key+":send", "1", time.Minute).Result()
	if err != nil {
		writePersonError(w, err)
		return
	}
	if !accepted {
		w.Header().Set("Retry-After", "60")
		writeText(w, http.StatusTooManyRequests, "Подождите перед повторной отправкой")
		return
	}
	if err := writes.redis.Set(ctx, key, token, lifetime).Err(); err != nil {
		writePersonError(w, err)
		return
	}
	if err := writes.mailer.send(ctx, email, subject, message); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = deleteAccountToken.Run(cleanup, writes.redis, []string{key}, token).Err()
		slog.Error("send account email failed")
		writeText(w, http.StatusServiceUnavailable, "Email is temporarily unavailable")
		return
	}
	if purpose == verifyEmailToken {
		_ = writes.redis.Del(ctx, key+":attempts").Err()
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]string{"result": result})
}

func (writes *accountWrites) matchingAccountToken(ctx context.Context, key, candidate string) (bool, error) {
	stored, err := writes.redis.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(candidate)) == 1, nil
}

func (writes *accountWrites) verifyResetPassword(w http.ResponseWriter, r *http.Request) {
	email, token := r.URL.Query().Get("email"), r.URL.Query().Get("token")
	switch {
	case !validMailRecipient(email):
		writeText(w, http.StatusBadRequest, "Email is required")
		return
	case token == "":
		writeText(w, http.StatusBadRequest, "Token is required")
		return
	}
	valid, err := writes.matchingAccountToken(r.Context(), accountTokenKey(email, resetPasswordToken), token)
	if err != nil {
		writePersonError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]bool{"valid": valid})
}

func (writes *accountWrites) resetAccountPassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Token    string `json:"token"`
		Password string `json:"newPassword"`
	}
	if !readAccountInput(w, r, &input) {
		return
	}
	switch {
	case !validMailRecipient(input.Email):
		writeText(w, http.StatusBadRequest, "Email is required")
		return
	case input.Token == "":
		writeText(w, http.StatusBadRequest, "Token is required")
		return
	case !validWirePassword(input.Password):
		writeText(w, http.StatusBadRequest, "New password is required")
		return
	}
	ctx := r.Context()
	key := accountTokenKey(input.Email, resetPasswordToken)
	valid, err := writes.matchingAccountToken(ctx, key, input.Token)
	if err != nil {
		writePersonError(w, err)
		return
	}
	if !valid {
		writePersonError(w, invalidAccountToken())
		return
	}
	hash, err := hashAccountPassword(input.Password)
	if err != nil {
		writePersonError(w, err)
		return
	}
	user, err := writes.applyAccountToken(ctx, key, input.Token, `UPDATE person SET password=$2, updated_at=now()
		WHERE email=$1 AND is_deleted IS NOT TRUE`+returnAccount, input.Email, hash)
	writePersonResult(w, user, err, false)
}

func (writes *accountWrites) verifyAccountEmail(w http.ResponseWriter, r *http.Request) {
	claims, ok := writes.protectedSession(w, r)
	if !ok {
		return
	}
	code := r.URL.Query().Get("code")
	if !emailCodePattern.MatchString(code) {
		writePersonError(w, invalidAccountToken())
		return
	}
	ctx := r.Context()
	person, err := scanAccount(writes.db.QueryRow(ctx, accountSelect+" WHERE id=$1 AND is_deleted IS NOT TRUE", claims.ID))
	if err != nil {
		writePersonError(w, err)
		return
	}
	if person.Email == nil || !validMailRecipient(*person.Email) {
		writePersonError(w, invalidAccountToken())
		return
	}
	key := accountTokenKey(*person.Email, verifyEmailToken)
	count, err := writes.redis.Eval(ctx, `local count=redis.call('INCR',KEYS[1]);
		if count==1 then redis.call('EXPIRE',KEYS[1],3600) end; return count`, []string{key + ":attempts"}).Int()
	if err != nil {
		writePersonError(w, err)
		return
	}
	if count > 12 {
		w.Header().Set("Retry-After", "60")
		writeText(w, http.StatusTooManyRequests, "Запросите новый код подтверждения")
		return
	}
	user, err := writes.applyAccountToken(ctx, key, code, `UPDATE person SET is_email_verified=true, updated_at=now()
		WHERE id=$1 AND email=$2 AND is_deleted IS NOT TRUE`+returnAccount, claims.ID, *person.Email)
	writePersonResult(w, user, err, false)
}

func (writes *accountWrites) applyAccountToken(ctx context.Context, key, token, query string, args ...any) (account, error) {
	valid, err := writes.matchingAccountToken(ctx, key, token)
	if err != nil {
		return account{}, err
	}
	if !valid {
		return account{}, invalidAccountToken()
	}
	tx, err := writes.store.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return account{}, err
	}
	defer rollbackCart(ctx, tx)
	user, err := scanAccount(tx.QueryRow(ctx, query, args...))
	if err != nil {
		return account{}, err
	}
	consumed, err := deleteAccountToken.Run(ctx, writes.redis, []string{key}, token).Int()
	if err != nil {
		return account{}, err
	}
	if consumed != 1 {
		return account{}, invalidAccountToken()
	}
	return user, tx.Commit(ctx)
}

func invalidAccountToken() error {
	return quoteInputError("Invalid or expired token")
}
