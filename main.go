package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
		accounts = &accountAuth{db: pool, key: []byte(key), secureCookies: secureCookies,
			guestOrigin: strings.TrimRight(os.Getenv("CLIENT_URL"), "/")}
	}

	checkoutEnabled := false
	if raw := os.Getenv("PHONE_CHECKOUT_ENABLED"); raw != "" {
		checkoutEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			return errors.New("PHONE_CHECKOUT_ENABLED must be a boolean")
		}
	}

	if raw := os.Getenv("CART_STORAGE_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("CART_STORAGE_ENABLED must be a boolean")
		}
		if enabled {
			if !checkoutEnabled || accounts == nil {
				return errors.New("CART_STORAGE_ENABLED requires PHONE_CHECKOUT_ENABLED and JWT_SECRET")
			}
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := pool.Exec(checkCtx, "SELECT 1 FROM cart_add_operation LIMIT 0")
			cancel()
			if err != nil {
				return errors.New("cannot access cart_add_operation")
			}
			accounts.cartDB = pool
		}
	}

	if raw := os.Getenv("ORDER_CONFIRMATION_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("ORDER_CONFIRMATION_ENABLED must be a boolean")
		}
		if enabled {
			if accounts == nil || accounts.cartDB == nil {
				return errors.New("ORDER_CONFIRMATION_ENABLED requires CART_STORAGE_ENABLED")
			}
			settings := &checkoutConfirmation{}
			if rawGuest := os.Getenv("GUEST_CHECKOUT_ENABLED"); rawGuest != "" {
				settings.guestEnabled, err = strconv.ParseBool(rawGuest)
				if err != nil {
					return errors.New("GUEST_CHECKOUT_ENABLED must be a boolean")
				}
			}
			if settings.guestEnabled {
				if accounts.guestOrigin == "" {
					return errors.New("GUEST_CHECKOUT_ENABLED requires CLIENT_URL")
				}
				limiter, err := newGuestOrderLimiter(os.Getenv("REDIS_URL"), os.Getenv("SMS_TRUSTED_PROXY_IP"))
				if err != nil {
					return err
				}
				defer limiter.Close()
				settings.guestLimiter = limiter
			}
			accounts.confirmation = settings
		}
	}

	server := &http.Server{
		Addr: address, Handler: newHandler(pool, images, accounts, checkoutEnabled),
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
