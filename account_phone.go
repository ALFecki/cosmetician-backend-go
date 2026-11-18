package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

const (
	phoneChallengeCookie = "phone_challenge"
	phoneChallengePrefix = "phone:challenge:"
)

const (
	phoneAuthUnavailable = "Вход по телефону пока недоступен"
	phoneCodeExpired     = "Код истёк. Запросите новый"
	phoneCodeUnavailable = "Код истёк или уже использован"
)

type phoneAuthentication struct {
	provider     smsCodeProvider
	dailyLimit   uint32
	trustedProxy netip.Addr
}

type phoneChallenge struct {
	Browser  string     `json:"browser"`
	CSRF     string     `json:"csrf"`
	Phone    string     `json:"phone"`
	Hash     string     `json:"hash"`
	PersonID *uuid.UUID `json:"person_id"`
	Attempts int        `json:"attempts"`
	Verified bool       `json:"verified"`
	Checking string     `json:"checking"`
}

func configuredPhoneAuthentication(checkoutEnabled bool) (phoneAuthentication, error) {
	settings := phoneAuthentication{dailyLimit: 200}
	enabled, err := optionalFlag("SMS_LOGIN_ENABLED")
	if err != nil {
		return settings, err
	}
	if value := os.Getenv("SMS_DAILY_LIMIT"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil || parsed == 0 {
			return settings, errors.New("invalid SMS_DAILY_LIMIT")
		}
		settings.dailyLimit = uint32(parsed)
	}
	if value := os.Getenv("SMS_TRUSTED_PROXY_IP"); value != "" {
		parsed, err := netip.ParseAddr(value)
		if err != nil {
			return settings, errors.New("invalid SMS_TRUSTED_PROXY_IP")
		}
		settings.trustedProxy = parsed.Unmap()
	}
	user, password, sender := os.Getenv("SMS_ASSISTANT_USER"), os.Getenv("SMS_ASSISTANT_PASSWORD"), os.Getenv("SMS_ASSISTANT_SENDER")
	if checkoutEnabled && enabled && strings.TrimSpace(user) != "" && strings.TrimSpace(password) != "" && strings.TrimSpace(sender) != "" {
		settings.provider = newSMSAssistant(user, password, sender)
	}
	return settings, nil
}

func (writes *accountWrites) phoneConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, struct {
		Enabled                bool `json:"enabled"`
		PhoneLoginAvailable    bool `json:"phoneLoginAvailable"`
		GuestCheckoutAvailable bool `json:"guestCheckoutAvailable"`
	}{writes.checkoutEnabled, writes.checkoutEnabled && writes.phone.provider != nil, writes.checkoutEnabled && writes.guestEnabled})
}

func (writes *accountWrites) allowPhone(w http.ResponseWriter, r *http.Request) bool {
	if !writes.checkoutEnabled || writes.phone.provider == nil {
		writeText(w, http.StatusServiceUnavailable, phoneAuthUnavailable)
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) != 0 && (len(origins) != 1 || origins[0] != writes.guestOrigin) {
		writeText(w, http.StatusUnauthorized, "Недопустимый источник запроса")
		return false
	}
	return true
}

func (writes *accountWrites) startPhone(w http.ResponseWriter, r *http.Request) {
	if !writes.allowPhone(w, r) {
		return
	}
	var input struct {
		Phone   string `json:"phone"`
		Purpose string `json:"purpose"`
	}
	if !readAccountInput(w, r, &input) {
		return
	}
	var personID *uuid.UUID
	switch input.Purpose {
	case "login":
	case "link":
		claims, ok := writes.protectedSession(w, r)
		if !ok {
			return
		}
		if claims.PhoneAuth {
			writeText(w, http.StatusUnauthorized, "Сначала войдите по email и паролю")
			return
		}
		id := uuid.UUID(claims.ID.Bytes)
		personID = &id
	default:
		writeText(w, http.StatusBadRequest, "Проверьте назначение кода")
		return
	}
	phone, err := normalizeCheckoutPhone(input.Phone)
	if err != nil {
		writePersonError(w, err)
		return
	}
	ctx := r.Context()
	ip := guestCheckoutIP(r, writes.phone.trustedProxy)
	accepted, err := reservePhoneSend.Run(ctx, writes.redis, []string{
		"phone:cooldown:" + phone, "phone:hour:" + phone, "phone:day:" + phone, "phone:ip:" + ip, "phone:global:day",
	}, 5, 10, 20, writes.phone.dailyLimit).Int()
	if err != nil {
		writePersonError(w, err)
		return
	}
	if accepted != 1 {
		writePhoneError(w, cartRequestError{http.StatusTooManyRequests, "Подождите перед повторной отправкой кода"})
		return
	}
	hash, err := writes.phone.provider.sendCode(ctx, phone)
	if err != nil {
		writePhoneError(w, err)
		return
	}
	id := uuid.New()
	challenge := phoneChallenge{Browser: uuid.NewString(), CSRF: uuid.NewString(), Phone: phone, Hash: hash, PersonID: personID}
	body, err := json.Marshal(challenge)
	if err != nil {
		writePersonError(w, err)
		return
	}
	if err := writes.redis.Set(ctx, phoneChallengePrefix+id.String(), body, 5*time.Minute).Err(); err != nil {
		writePersonError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: phoneChallengeCookie, Value: challenge.Browser, HttpOnly: true,
		Secure: writes.secureCookies, SameSite: http.SameSiteStrictMode, Path: "/api/auth/phone", MaxAge: 1200})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, struct {
		ChallengeID uuid.UUID `json:"challengeId"`
		CSRFToken   string    `json:"csrfToken"`
		RetryAfter  int       `json:"retryAfter"`
		ExpiresIn   int       `json:"expiresIn"`
	}{id, challenge.CSRF, 60, 300})
}

var reservePhoneSend = redis.NewScript(`
	if redis.call('EXISTS', KEYS[1])==1 then return 0 end
	for i=2,5 do
		if tonumber(redis.call('GET',KEYS[i]) or '0')>=tonumber(ARGV[i-1]) then return 0 end
	end
	redis.call('SET',KEYS[1],'1','EX',60)
	for i=2,5 do
		local n=redis.call('INCR',KEYS[i])
		if n==1 then redis.call('EXPIRE',KEYS[i],(i==2 or i==4) and 3600 or 86400) end
	end
	return 1
`)

func (writes *accountWrites) loadPhoneChallenge(r *http.Request, id uuid.UUID) (phoneChallenge, error) {
	var challenge phoneChallenge
	if id == uuid.Nil {
		return challenge, quoteInputError(phoneCodeExpired)
	}
	raw, err := writes.redis.Get(r.Context(), phoneChallengePrefix+id.String()).Result()
	if errors.Is(err, redis.Nil) {
		return challenge, quoteInputError(phoneCodeExpired)
	}
	if err != nil {
		return challenge, err
	}
	if json.Unmarshal([]byte(raw), &challenge) != nil {
		return challenge, quoteInputError("Запросите новый код")
	}
	cookie, err := r.Cookie(phoneChallengeCookie)
	csrf := r.Header.Values("X-Phone-CSRF")
	if challenge.Browser == "" || challenge.CSRF == "" || err != nil || len(csrf) != 1 ||
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(challenge.Browser)) != 1 ||
		subtle.ConstantTimeCompare([]byte(csrf[0]), []byte(challenge.CSRF)) != 1 {
		return challenge, cartRequestError{http.StatusUnauthorized, "Подтвердите номер в том же браузере"}
	}
	return challenge, nil
}

var claimPhoneChallenge = redis.NewScript(`
	local raw=redis.call('GET',KEYS[1]); if not raw then return 0 end
	local c=cjson.decode(raw)
	if c.checking~='' then return 2 end
	if ARGV[1]=='1' then
		if not c.verified then return 0 end
	else
		if c.verified then return 0 end
		if c.attempts>=5 then return 3 end
		c.attempts=c.attempts+1
	end
	c.checking=ARGV[2]
	redis.call('SET',KEYS[1],cjson.encode(c),'KEEPTTL')
	return 1
`)

func (writes *accountWrites) claimPhoneCheck(ctx context.Context, id uuid.UUID, completion bool) (string, error) {
	nonce := uuid.NewString()
	action := 0
	if completion {
		action = 1
	}
	status, err := claimPhoneChallenge.Run(ctx, writes.redis, []string{phoneChallengePrefix + id.String()}, action, nonce).Int()
	if err != nil {
		return "", err
	}
	switch status {
	case 1:
		return nonce, nil
	case 2:
		return "", cartRequestError{http.StatusConflict, "Проверка уже выполняется"}
	case 3:
		return "", cartRequestError{http.StatusTooManyRequests, "Слишком много попыток. Запросите новый код"}
	default:
		return "", quoteInputError(phoneCodeUnavailable)
	}
}

var finishPhoneChallenge = redis.NewScript(`
	local raw=redis.call('GET',KEYS[1]); if not raw then return 0 end
	local c=cjson.decode(raw); if c.checking~=ARGV[1] then return 0 end
	if ARGV[2]=='1' then
		c.verified=true
		c.hash=''
		redis.call('SET',KEYS[1],cjson.encode(c),'EX',900)
	elseif ARGV[2]=='2' then
		redis.call('DEL',KEYS[1])
	else
		c.checking=''
		redis.call('SET',KEYS[1],cjson.encode(c),'KEEPTTL')
	end
	return 1
`)

func (writes *accountWrites) finishPhoneCheck(ctx context.Context, id uuid.UUID, nonce string, status int) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	updated, err := finishPhoneChallenge.Run(cleanup, writes.redis, []string{phoneChallengePrefix + id.String()}, nonce, status).Int()
	if err != nil {
		return err
	}
	if updated != 1 {
		return quoteInputError(phoneCodeExpired)
	}
	return nil
}

func (writes *accountWrites) verifyPhone(w http.ResponseWriter, r *http.Request) {
	if !writes.allowPhone(w, r) {
		return
	}
	var input struct {
		ChallengeID uuid.UUID `json:"challengeId"`
		Code        string    `json:"code"`
	}
	if !readAccountInput(w, r, &input) {
		return
	}
	if !validPhoneCode(input.Code) {
		writeText(w, http.StatusBadRequest, "Введите 5 цифр из SMS")
		return
	}
	challenge, err := writes.loadPhoneChallenge(r, input.ChallengeID)
	if err != nil {
		writePhoneError(w, err)
		return
	}
	if challenge.PersonID != nil {
		claims, ok := writes.protectedSession(w, r)
		if !ok {
			return
		}
		if claims.PhoneAuth || uuid.UUID(claims.ID.Bytes) != *challenge.PersonID {
			writeText(w, http.StatusUnauthorized, "Сначала войдите в прежний аккаунт")
			return
		}
	}
	ctx := r.Context()
	nonce, err := writes.claimPhoneCheck(ctx, input.ChallengeID, false)
	if err != nil {
		writePhoneError(w, err)
		return
	}
	number, providerErr := writes.phone.provider.checkCode(ctx, challenge.Hash, input.Code)
	verified, normalizeErr := normalizeCheckoutPhone(number)
	if providerErr != nil || normalizeErr != nil || verified != challenge.Phone {
		if err := writes.finishPhoneCheck(ctx, input.ChallengeID, nonce, 0); err != nil {
			writePhoneError(w, err)
			return
		}
		if providerErr != nil {
			writePhoneError(w, providerErr)
			return
		}
		writeText(w, http.StatusBadRequest, "Неверный код. Проверьте SMS")
		return
	}
	if err := writes.finishPhoneCheck(ctx, input.ChallengeID, nonce, 1); err != nil {
		writePhoneError(w, err)
		return
	}
	if challenge.PersonID != nil {
		writes.linkVerifiedPhone(w, ctx, input.ChallengeID, nonce, challenge)
		return
	}
	user, err := scanAccount(writes.db.QueryRow(ctx, accountSelect+" WHERE verified_phone=$1 AND is_deleted=false", challenge.Phone))
	if errors.Is(err, pgx.ErrNoRows) {
		if err := writes.finishPhoneCheck(ctx, input.ChallengeID, nonce, 0); err != nil {
			writePhoneError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]bool{"registrationRequired": true})
		return
	}
	if err != nil {
		_ = writes.finishPhoneCheck(ctx, input.ChallengeID, nonce, 0)
		writePersonError(w, err)
		return
	}
	writes.consumePhoneCheck(w, ctx, input.ChallengeID, nonce, user, true)
}

func validPhoneCode(code string) bool {
	if len(code) != 5 {
		return false
	}
	for i := range len(code) {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

func (writes *accountWrites) linkVerifiedPhone(w http.ResponseWriter, ctx context.Context, challengeID uuid.UUID, nonce string, challenge phoneChallenge) {
	user, err := writes.saveVerifiedPhone(ctx, `UPDATE person SET
		contact_email=CASE WHEN verified_phone IS NULL THEN COALESCE(contact_email,email) ELSE contact_email END,
		verified_phone=$1,phone=$1,phone_verified_at=now(),updated_at=now()
		WHERE id=$2 AND is_deleted=false`+returnAccount, challenge.Phone, *challenge.PersonID)
	if err != nil {
		_ = writes.finishPhoneCheck(ctx, challengeID, nonce, 2)
		writeText(w, http.StatusConflict, "Номер уже привязан к другому аккаунту или аккаунт недоступен")
		return
	}
	writes.consumePhoneCheck(w, ctx, challengeID, nonce, user, false)
}

func (writes *accountWrites) completePhone(w http.ResponseWriter, r *http.Request) {
	if !writes.allowPhone(w, r) {
		return
	}
	var input struct {
		ChallengeID uuid.UUID `json:"challengeId"`
		FirstName   string    `json:"firstName"`
		SecondName  string    `json:"secondName"`
		Email       *string   `json:"email"`
	}
	if !readAccountInput(w, r, &input) {
		return
	}
	challenge, err := writes.loadPhoneChallenge(r, input.ChallengeID)
	if err != nil {
		writePhoneError(w, err)
		return
	}
	if !challenge.Verified || challenge.PersonID != nil {
		writeText(w, http.StatusBadRequest, "Сначала подтвердите номер")
		return
	}
	first, second, email, err := validateCheckoutContact(input.FirstName, input.SecondName, input.Email)
	if err != nil {
		writePersonError(w, err)
		return
	}
	ctx := r.Context()
	nonce, err := writes.claimPhoneCheck(ctx, input.ChallengeID, true)
	if err != nil {
		writePhoneError(w, err)
		return
	}
	user, err := writes.saveVerifiedPhone(ctx, `INSERT INTO person
		(first_name,second_name,email,password,phone,verified_phone,phone_verified_at,contact_email)
		VALUES ($1,$2,NULL,NULL,$3,$3,now(),$4)
		ON CONFLICT (verified_phone) DO UPDATE SET verified_phone=EXCLUDED.verified_phone WHERE person.is_deleted=false`+returnAccount,
		first, second, challenge.Phone, email)
	if err != nil {
		_ = writes.finishPhoneCheck(ctx, input.ChallengeID, nonce, 0)
		writeText(w, http.StatusConflict, "Не удалось сохранить профиль. Попробуйте ещё раз")
		return
	}
	writes.consumePhoneCheck(w, ctx, input.ChallengeID, nonce, user, true)
}

func (writes *accountWrites) saveVerifiedPhone(ctx context.Context, query string, args ...any) (account, error) {
	tx, err := writes.store.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return account{}, err
	}
	defer rollbackCart(ctx, tx)
	user, err := scanAccount(tx.QueryRow(ctx, query, args...))
	if err != nil {
		return account{}, err
	}
	return user, tx.Commit(ctx)
}

func (writes *accountWrites) consumePhoneCheck(w http.ResponseWriter, ctx context.Context, id uuid.UUID, nonce string, user account, phoneAuth bool) {
	if err := writes.finishPhoneCheck(ctx, id, nonce, 2); err != nil {
		writePhoneError(w, err)
		return
	}
	writes.writeSession(w, user, phoneAuth)
}

func writePhoneError(w http.ResponseWriter, err error) {
	var problem cartRequestError
	if errors.As(err, &problem) {
		if problem.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "60")
		}
		writeText(w, problem.status, problem.message)
		return
	}
	writePersonError(w, err)
}
