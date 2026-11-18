package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	smsAssistantBaseURL = "https://userarea.sms-assistent.by/api/v1"
	smsAssistantMessage = "Код входа в SkViDerm: {KOD}. Никому его не сообщайте."
)

type smsCodeProvider interface {
	sendCode(context.Context, string) (string, error)
	checkCode(context.Context, string, string) (string, error)
}

type smsAssistant struct {
	client   *http.Client
	baseURL  string
	user     string
	password string
	sender   string
}

func newSMSAssistant(user, password, sender string) smsAssistant {
	return smsAssistant{
		client:  &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectSMSRedirect},
		baseURL: smsAssistantBaseURL, user: user, password: password, sender: sender,
	}
}

func rejectSMSRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func (client smsAssistant) post(ctx context.Context, operation string, data url.Values) (string, error) {
	data.Set("user", client.user)
	data.Set("password", client.password)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/"+operation+"/plain", strings.NewReader(data.Encode()))
	if err != nil {
		return "", smsUnavailable()
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.client.Do(request)
	if err != nil {
		return "", smsUnavailable()
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", smsUnavailable()
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 513))
	if err != nil || len(body) > 512 {
		return "", smsUnavailable()
	}
	return strings.TrimSpace(string(body)), nil
}

func (client smsAssistant) sendCode(ctx context.Context, phone string) (string, error) {
	hash, err := client.post(ctx, "sms_code_generate", url.Values{"recipient": {phone}, "message": {smsAssistantMessage}, "sender": {client.sender}})
	if err != nil {
		return "", err
	}
	if hash == "" || strings.HasPrefix(hash, "-") || strings.ContainsFunc(hash, invalidSMSHashCharacter) {
		return "", smsUnavailable()
	}
	return hash, nil
}

func invalidSMSHashCharacter(char rune) bool {
	return !(char >= '0' && char <= '9' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char == '_' || char == '-')
}

func (client smsAssistant) checkCode(ctx context.Context, hash, code string) (string, error) {
	reply, err := client.post(ctx, "sms_code_check", url.Values{"check_hash": {hash}, "check_code": {code}})
	if reply == "-23" {
		return "", nil
	}
	if strings.HasPrefix(reply, "-") {
		return "", smsUnavailable()
	}
	return reply, err
}

func smsUnavailable() error {
	return cartRequestError{http.StatusServiceUnavailable, "Не удалось связаться с SMS-сервисом. Попробуйте позже."}
}
