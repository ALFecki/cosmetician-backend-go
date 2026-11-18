package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

type accountMailer interface {
	send(ctx context.Context, to, subject, body string) error
}

type accountSMTP struct {
	address  string
	host     string
	login    string
	password string
}

func newAccountMailer(login, password string) (accountMailer, error) {
	if login == "" && password == "" {
		return nil, nil
	}
	if !validMailRecipient(login) || password == "" {
		return nil, errors.New("EMAIL_LOGIN and EMAIL_PASSWORD must contain valid SMTP credentials")
	}
	return accountSMTP{address: "smtp.gmail.com:587", host: "smtp.gmail.com", login: login, password: password}, nil
}

func validMailRecipient(recipient string) bool {
	parsed, err := mail.ParseAddress(recipient)
	return err == nil && parsed.Address == recipient
}

func (mailer accountSMTP) send(ctx context.Context, to, subject, body string) error {
	if !validMailRecipient(to) || strings.ContainsAny(subject, "\r\n") {
		return errors.New("invalid email header")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", mailer.address)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, mailer.host)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.StartTLS(&tls.Config{ServerName: mailer.host, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if err := client.Auth(smtp.PlainAuth("", mailer.login, mailer.password, mailer.host)); err != nil {
		return err
	}
	if err := client.Mail(mailer.login); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	data, err := client.Data()
	if err != nil {
		return err
	}
	from := (&mail.Address{Name: "SkViDerm", Address: mailer.login}).String()
	if _, err := fmt.Fprintf(data, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n",
		from, (&mail.Address{Address: to}).String(), mime.BEncoding.Encode("UTF-8", subject)); err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for start := 0; start < len(encoded); start += 76 {
		if _, err := fmt.Fprint(data, encoded[start:min(start+76, len(encoded))], "\r\n"); err != nil {
			return err
		}
	}
	return data.Close()
}
