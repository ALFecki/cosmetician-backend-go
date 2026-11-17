package main

import (
	"context"
	"errors"
	"net/http"
	"net/netip"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const guestOrderLimitTTL = 3600

var guestOrderCounter = redis.NewScript(
	"local n=redis.call('INCR',KEYS[1]); if n==1 then redis.call('EXPIRE',KEYS[1],ARGV[1]) end; return n",
)

type guestOrderLimiter struct {
	client       *redis.Client
	trustedProxy netip.Addr
}

func newGuestOrderLimiter(redisURL, trustedProxy string) (*guestOrderLimiter, error) {
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, errors.New("invalid REDIS_URL for guest checkout")
	}
	var proxy netip.Addr
	if trustedProxy != "" {
		proxy, err = netip.ParseAddr(trustedProxy)
		if err != nil || proxy.Zone() != "" {
			return nil, errors.New("invalid SMS_TRUSTED_PROXY_IP")
		}
	}
	// Retrying a counter with an unknown outcome could increment it twice.
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	options.Protocol = 2
	options.DisableIdentity = true
	return &guestOrderLimiter{client: redis.NewClient(options), trustedProxy: proxy}, nil
}

func (limiter *guestOrderLimiter) Close() error {
	return limiter.client.Close()
}

func (limiter *guestOrderLimiter) allow(ctx context.Context, guestID uuid.UUID, request *http.Request) error {
	sessionKey := "guest-checkout:orders:session:" + guestID.String()
	if err := limiter.reserve(ctx, sessionKey, 5); err != nil {
		return err
	}
	addressKey := "guest-checkout:orders:ip:" + guestCheckoutIP(request, limiter.trustedProxy)
	return limiter.reserve(ctx, addressKey, 20)
}

func (limiter *guestOrderLimiter) reserve(ctx context.Context, key string, maximum int64) error {
	count, err := guestOrderCounter.Run(ctx, limiter.client, []string{key}, guestOrderLimitTTL).Int64()
	switch {
	case err != nil:
		return cartRequestError{http.StatusServiceUnavailable, "Оформление временно недоступно. Попробуйте позже"}
	case count > maximum:
		return cartRequestError{http.StatusTooManyRequests, "Слишком много попыток. Попробуйте позже"}
	default:
		return nil
	}
}

func guestCheckoutIP(request *http.Request, trustedProxy netip.Addr) string {
	remote, err := netip.ParseAddrPort(request.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	peer := remote.Addr().WithZone("")
	if peer == trustedProxy {
		if address, err := netip.ParseAddr(request.Header.Get("X-Real-IP")); err == nil && address.Zone() == "" {
			return address.String()
		}
	}
	return peer.String()
}
