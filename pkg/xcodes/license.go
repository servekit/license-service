package xcodes

import "github.com/servekit/go-common/xerr"

// License-domain error codes (design doc §6). The gRPC code column of the
// client contract is enforced here; the future HTTP gateway maps them 1:1
// (InvalidArgument→400, Unauthenticated→401, PermissionDenied→403,
// AlreadyExists→409, ResourceExhausted→429).
var (
	// ErrBadKeyFormat: normalized key does not match ^AV1D[0-9A-Z]{20}$,
	// both key/license_key empty, or a non-sellable module.
	ErrBadKeyFormat = xerr.New("BAD_KEY_FORMAT", xerr.CategoryBadRequest, 400, "invalid license key format")
	// ErrKeyNotFound: key_hash lookup returned nothing.
	ErrKeyNotFound = xerr.New("KEY_NOT_FOUND", xerr.CategoryUnauthorized, 401, "license key not found")
	// ErrKeyRevoked: key status is revoked (including heartbeats from
	// devices already in a slot).
	ErrKeyRevoked = xerr.New("KEY_REVOKED", xerr.CategoryForbidden, 403, "license key has been revoked")
	// ErrSlotLimit: all slots occupied and no valid evict token supplied.
	ErrSlotLimit = xerr.New("SLOT_LIMIT", xerr.CategoryConflict, 409, "device slot limit reached")
	// ErrAlreadyEntitled: trial/start for a module the key already holds a
	// valid entitlement for.
	ErrAlreadyEntitled = xerr.New("ALREADY_ENTITLED", xerr.CategoryBadRequest, 400, "key already entitled to this module")
	// ErrRateLimited: more than one request per 60s window for the same
	// (key_hash or fingerprint_id, device_token).
	ErrRateLimited = xerr.New("RATE_LIMITED", xerr.CategoryTooManyRequests, 429, "too many requests, retry later")
)

// App-registry error codes (calling-application credentials, messaging/
// storage platform app pattern).
var (
	// ErrAppNotFound: app_key lookup returned nothing.
	ErrAppNotFound = xerr.New("APP_NOT_FOUND", xerr.CategoryNotFound, 404, "app not found")
	// ErrAppExists: caller-supplied app_key collides with a live app.
	ErrAppExists = xerr.New("APP_EXISTS", xerr.CategoryConflict, 409, "app already exists")
	// ErrAppUnauthorized: missing app credentials, unknown/disabled app, or
	// bad secret on a data-plane call.
	ErrAppUnauthorized = xerr.New("APP_UNAUTHORIZED", xerr.CategoryUnauthorized, 401, "app missing, disabled, or bad credentials")
)
