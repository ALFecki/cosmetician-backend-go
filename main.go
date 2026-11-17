package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		slog.Error("backend stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	address := os.Getenv("HTTP_ADDR")
	if address == "" {
		address = ":8000"
	}

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid DATABASE_URL")
	}
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return errors.New("cannot initialize PostgreSQL pool")
	}
	defer pool.Close()

	pingCtx, done := context.WithTimeout(ctx, 5*time.Second)
	err = pool.Ping(pingCtx)
	done()
	if err != nil {
		return errors.New("cannot connect to PostgreSQL")
	}

	images, err := newImageStore(os.Getenv("MINIO_ENDPOINT"), os.Getenv("MINIO_ACCESS_KEY"),
		os.Getenv("MINIO_SECRET_KEY"), os.Getenv("MINIO_BUCKET"))
	if err != nil {
		return err
	}

	var accounts *accountAuth
	if key := os.Getenv("JWT_SECRET"); key != "" {
		secureCookies := true
		if raw := os.Getenv("SECURE_MODE"); raw != "" {
			secureCookies, err = strconv.ParseBool(raw)
			if err != nil {
				return errors.New("SECURE_MODE must be a boolean")
			}
		}
		accounts = &accountAuth{db: pool, key: []byte(key), secureCookies: secureCookies}
	}

	server := &http.Server{
		Addr: address, Handler: newHandler(pool, images, accounts),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	slog.Info("backend listening", "address", address)
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		err := server.Shutdown(shutdown)
		if err != nil {
			_ = server.Close()
		}
		return err
	}
}
