package activation

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/pkg/xcodes"
)

// rateWindow is the fixed window: one request per (identity, device_token)
// per 60s. This is a factual guardrail over the 24h heartbeat rhythm, not an
// anti-DDoS boundary (design doc §7.5).
const (
	rateWindow   = 60 * time.Second
	retrySeconds = int32(60)
)

// rateLimiter is a fixed-window Redis limiter with the 409-refund semantics
// the client flow depends on: after a slot-limit rejection the user must be
// able to retry immediately once they picked a device to evict.
//
// Redis unavailability fails OPEN with a warning — the limiter is a
// guardrail, not an availability dependency.
type rateLimiter struct {
	rdb *redis.Client
}

// check consumes one unit for (id, deviceToken). It returns a refund
// closure on success (nil when the request is denied or the limiter is
// unavailable) and a detailed RATE_LIMITED error when the window is
// exhausted.
func (l *rateLimiter) check(ctx context.Context, id, deviceToken string) (func(), error) {
	key := "license:rate:" + id + ":" + deviceToken
	n, err := l.rdb.Incr(ctx, key).Result()
	if err != nil {
		slog.Warn("rate limiter unavailable, failing open", "error", err)
		return nil, nil
	}
	if n == 1 {
		if err := l.rdb.Expire(ctx, key, rateWindow).Err(); err != nil {
			slog.Warn("rate limiter expire failed", "error", err)
		}
	}
	if n > 1 {
		return nil, xcodes.WithDetails(xcodes.ErrRateLimited.New(),
			&licensev1.RetryAfterInfo{Seconds: retrySeconds})
	}
	return func() {
		if err := l.rdb.Decr(context.Background(), key).Err(); err != nil {
			slog.Warn("rate limit refund failed", "error", err)
		}
	}, nil
}
