package activation

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/pkg/xcodes"
)

// rateLimiter is a fixed-window Redis limiter with the 409-refund semantics
// the client flow depends on: after a slot-limit rejection the user must be
// able to retry immediately once they picked a device to evict.
//
// Key prefix and window duration are operator-configured (rate_limit config
// section); the Retry-After hint derives from the window.
//
// Redis unavailability fails OPEN with a warning — the limiter is a
// guardrail, not an availability dependency.
type rateLimiter struct {
	rdb    *redis.Client
	prefix string
	window time.Duration
}

// newRateLimiter applies defaults for empty fields (direct-constructed
// configs and tests may omit them).
func newRateLimiter(rdb *redis.Client, prefix string, window time.Duration) rateLimiter {
	if prefix == "" {
		prefix = "license:rate:"
	}
	if window <= 0 {
		window = 60 * time.Second
	}
	return rateLimiter{rdb: rdb, prefix: prefix, window: window}
}

// retryAfterSeconds is the client-facing hint, derived from the window.
func (l rateLimiter) retryAfterSeconds() int32 { return int32(l.window / time.Second) }

// check consumes one unit for (id, deviceToken). It returns a refund
// closure on success (nil when the request is denied or the limiter is
// unavailable) and a detailed RATE_LIMITED error when the window is
// exhausted. The limiter key is <prefix>{id}:{device_token}; id is a hex
// hash or a fingerprint, both safe as key material.
func (l rateLimiter) check(ctx context.Context, id, deviceToken string) (func(), error) {
	// Defensive separator fold in case a future id ever carries ':'.
	key := l.prefix + strings.ReplaceAll(id, ":", "_") + ":" + deviceToken
	n, err := l.rdb.Incr(ctx, key).Result()
	if err != nil {
		slog.Warn("rate limiter unavailable, failing open", "error", err)
		return nil, nil
	}
	if n == 1 {
		if err := l.rdb.Expire(ctx, key, l.window).Err(); err != nil {
			slog.Warn("rate limiter expire failed", "error", err)
		}
	}
	if n > 1 {
		return nil, xcodes.WithDetails(xcodes.ErrRateLimited.New(),
			&licensev1.RetryAfterInfo{Seconds: l.retryAfterSeconds()})
	}
	return func() {
		if err := l.rdb.Decr(context.Background(), key).Err(); err != nil {
			slog.Warn("rate limit refund failed", "error", err)
		}
	}, nil
}
