package activation

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/servekit/go-common/ratelimit"

	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/pkg/xcodes"
)

// Limiter purposes. "activate" covers both Activate and Deactivate (same
// key-hash identity dimension); "trial" is fingerprint-dimension.
const (
	purposeActivate = "activate"
	purposeTrial    = "trial"
)

// defaultRateMax is the per-window quota applied when the config omits it:
// generous for legitimate flows (heartbeat is 24h; evict-retry costs one
// extra unit), still a hard stop for hammering.
const defaultRateMax = 10

// rateLimiter wraps go-common's fixed-window limiter. Semantics: at most Max
// requests per Window for each (identity, device_token) pair — an
// anti-abuse guardrail over hammering, deliberately generous for legitimate
// flows (heartbeat is 24h; the evict-retry flow consumes one extra unit).
//
// Quota consumption on 409 is accepted by design: with Max > 1 the
// pick-a-device-and-retry flow stays inside the quota, which replaced the
// old refund-on-409 single-shot limiter (design doc §7.5 as amended).
//
// Redis unavailability fails OPEN with a warning — the limiter is a
// guardrail, not an availability dependency.
type rateLimiter struct {
	lim    ratelimit.Limiter
	window time.Duration
}

// newRateLimiter applies defaults for empty/zero fields (direct-constructed
// configs and tests may omit them).
func newRateLimiter(rdb *redis.Client, prefix string, window time.Duration, windowMax int64) rateLimiter {
	if prefix == "" {
		prefix = "license:rate"
	}
	if window <= 0 {
		window = time.Minute
	}
	if windowMax <= 0 {
		windowMax = defaultRateMax
	}
	rule := &ratelimit.Rule{Window: window, Max: windowMax}
	return rateLimiter{
		lim: ratelimit.NewRedisLimiter(rdb, &ratelimit.Config{
			Prefix: prefix,
			Rules: map[string][]*ratelimit.Rule{
				purposeActivate: {rule},
				purposeTrial:    {rule},
			},
		}),
		window: window,
	}
}

// retryAfterSeconds is the client-facing hint, derived from the window.
func (l rateLimiter) retryAfterSeconds() int32 { return int32(l.window / time.Second) }

// allow consumes one unit for (purpose, target); target is
// "<id>:<device_token>" where id is a hex key hash or a fingerprint.
// Returns the detailed RATE_LIMITED error when the window quota is
// exhausted.
func (l rateLimiter) allow(ctx context.Context, purpose, target string) error {
	allowed, err := l.lim.Allow(ctx, purpose, target)
	if err != nil {
		slog.Warn("rate limiter unavailable, failing open", "error", err)
		return nil
	}
	if allowed {
		return nil
	}
	return xcodes.WithDetails(xcodes.ErrRateLimited.New(),
		&licensev1.RetryAfterInfo{Seconds: l.retryAfterSeconds()})
}
