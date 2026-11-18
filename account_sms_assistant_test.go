package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type smsAssistantTestTransport func(*http.Request) (*http.Response, error)

func (transport smsAssistantTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func TestPortedAccountSMSAssistantContract(t *testing.T) {
	const number = "+375290004204"
	const hash = "synthetic-hash"
	client := newSMSAssistant("fixture-user", "fixture-key", "fixture-sender")
	httpReplies := []struct {
		operation string
		reply     string
	}{
		{"sms_code_generate", hash},
		{"sms_code_generate", "-3"},
		{"sms_code_check", number},
		{"sms_code_check", "-23"},
	}
	calls := 0
	client.client.Transport = smsAssistantTestTransport(func(request *http.Request) (*http.Response, error) {
		step := httpReplies[calls]
		calls++
		if request.Method != http.MethodPost || request.URL.String() != smsAssistantBaseURL+"/"+step.operation+"/plain" ||
			request.ParseForm() != nil || request.Form.Get("user") != "fixture-user" || request.Form.Get("password") != "fixture-key" {
			t.Fatalf("invalid SMS test request to %s", request.URL)
		}
		if step.operation == "sms_code_generate" {
			if request.Form.Get("recipient") != number || request.Form.Get("message") != smsAssistantMessage ||
				request.Form.Get("sender") != "fixture-sender" {
				t.Fatal("invalid SMS generation payload")
			}
		} else if request.Form.Get("check_hash") != hash || request.Form.Get("check_code") != "12345" {
			t.Fatal("invalid SMS check payload")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(step.reply))}, nil
	})
	if sent, err := client.sendCode(t.Context(), number); sent != hash || err != nil {
		t.Fatalf("SMS generate fixture = %q %v", sent, err)
	}
	if _, err := client.sendCode(t.Context(), number); err == nil {
		t.Fatal("provider failure accepted")
	}
	if recognized, err := client.checkCode(t.Context(), hash, "12345"); recognized != number || err != nil {
		t.Fatalf("SMS check fixture = %q %v", recognized, err)
	}
	if invalid, err := client.checkCode(t.Context(), hash, "12345"); invalid != "" || err != nil {
		t.Fatalf("invalid SMS fixture = %q %v", invalid, err)
	}
}
