package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGuestCheckoutIP(t *testing.T) {
	proxy := netip.MustParseAddr("192.0.2.10")
	for _, tc := range []struct {
		name, peer, forwarded, want string
		proxy                       netip.Addr
	}{
		{"direct", "192.0.2.20:5000", "192.0.2.99", "192.0.2.20", proxy},
		{"trusted", "192.0.2.10:5000", "2001:0DB8:0000:0000::1", "2001:db8::1", proxy},
		{"missing forwarded value", "192.0.2.10:5000", "", "192.0.2.10", proxy},
		{"malformed forwarded value", "192.0.2.10:5000", "192.0.2.20, 192.0.2.21", "192.0.2.10", proxy},
		{"scoped forwarded address", "192.0.2.10:5000", "fe80::1%en0", "192.0.2.10", proxy},
		{"canonical peer", "[2001:0DB8::2]:5000", "", "2001:db8::2", proxy},
		{"no proxy configured", "192.0.2.10:5000", "192.0.2.20", "192.0.2.10", netip.Addr{}},
		{"unknown peer", "", "192.0.2.20", "unknown", netip.Addr{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/checkout/guest", nil)
			request.RemoteAddr = tc.peer
			request.Header.Set("X-Real-IP", tc.forwarded)
			request.Header.Set("X-Forwarded-For", "192.0.2.30")
			if got := guestCheckoutIP(request, tc.proxy); got != tc.want {
				t.Errorf("client address = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGuestOrderLimiterConfiguration(t *testing.T) {
	const password = "invalid-fixture-password"
	for _, tc := range []struct{ url, proxy string }{
		{"wrong://redis-user:" + password + "@localhost:6379", ""},
		{"redis://redis-user:" + password + "@localhost:6379", "192.0.2.0/24"},
	} {
		limiter, err := newGuestOrderLimiter(tc.url, tc.proxy)
		if err == nil || limiter != nil {
			t.Fatal("invalid limiter configuration was accepted")
		}
		if strings.Contains(err.Error(), password) {
			t.Fatal("limiter error included Redis credentials")
		}
	}
}

func TestGuestOrderLimiterUnavailable(t *testing.T) {
	limiter, err := newGuestOrderLimiter("redis://example.invalid:6379", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/checkout/guest", nil)
	assertRateLimitError(t, limiter.allow(t.Context(), uuid.New(), request), http.StatusServiceUnavailable)
}

func TestGuestOrderLimiterRedis(t *testing.T) {
	redisURL := os.Getenv("SKVIDERM_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set SKVIDERM_TEST_REDIS_URL to an isolated Redis instance")
	}
	limiter, err := newGuestOrderLimiter(redisURL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	guestID, newGuestID, burstGuestID := uuid.New(), uuid.New(), uuid.New()
	const ip, burstIP = "192.0.2.93", "192.0.2.94"
	sessionKey := "guest-checkout:orders:session:" + guestID.String()
	ipKey := "guest-checkout:orders:ip:" + ip
	newSessionKey := "guest-checkout:orders:session:" + newGuestID.String()
	burstSessionKey := "guest-checkout:orders:session:" + burstGuestID.String()
	burstIPKey := "guest-checkout:orders:ip:" + burstIP
	defer limiter.client.Del(ctx, sessionKey, ipKey, newSessionKey, burstSessionKey, burstIPKey)

	const remaining = 19 * time.Minute
	for key, count := range map[string]int{sessionKey: 4, ipKey: 19} {
		if err := limiter.client.Set(ctx, key, count, remaining).Err(); err != nil {
			t.Fatal(err)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/api/checkout/guest", nil)
	request.RemoteAddr = ip + ":5000"
	if err := limiter.allow(ctx, guestID, request); err != nil {
		t.Fatalf("last available request: %v", err)
	}
	assertRateLimitError(t, limiter.allow(ctx, guestID, request), http.StatusTooManyRequests)
	assertRedisCounter(t, ctx, limiter, sessionKey, 6, remaining)
	assertRedisCounter(t, ctx, limiter, ipKey, 20, remaining)

	assertRateLimitError(t, limiter.allow(ctx, newGuestID, request), http.StatusTooManyRequests)
	assertRedisCounter(t, ctx, limiter, newSessionKey, 1, time.Hour)
	assertRedisCounter(t, ctx, limiter, ipKey, 21, remaining)

	burstRequest := httptest.NewRequest(http.MethodPost, "/api/checkout/guest", nil)
	burstRequest.RemoteAddr = burstIP + ":5000"
	const requests = 16
	results := make(chan error, requests)
	var pending sync.WaitGroup
	for range requests {
		pending.Go(func() { results <- limiter.allow(ctx, burstGuestID, burstRequest) })
	}
	pending.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			assertRateLimitError(t, err, http.StatusTooManyRequests)
		}
	}
	if accepted != 5 {
		t.Fatalf("accepted %d simultaneous guest requests, want 5", accepted)
	}
	assertRedisCounter(t, ctx, limiter, burstSessionKey, requests, time.Hour)
	assertRedisCounter(t, ctx, limiter, burstIPKey, 5, time.Hour)
}

func assertRedisCounter(t *testing.T, ctx context.Context, limiter *guestOrderLimiter, key string, count int, expiry time.Duration) {
	t.Helper()
	if got, err := limiter.client.Get(ctx, key).Int(); err != nil || got != count {
		t.Fatalf("counter = %d, want %d: %v", got, count, err)
	}
	if ttl, err := limiter.client.TTL(ctx, key).Result(); err != nil || ttl <= expiry-time.Minute || ttl > expiry {
		t.Fatalf("counter TTL = %s, want close to %s: %v", ttl, expiry, err)
	}
}

func assertRateLimitError(t *testing.T, err error, status int) {
	t.Helper()
	var problem cartRequestError
	if !errors.As(err, &problem) || problem.status != status {
		t.Fatalf("rate limit returned %v, want HTTP %d", err, status)
	}
}
