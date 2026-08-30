// Package xcodes defines license-service error codes. Domain errors live in
// per-domain files (one per major capability area); generic codes mirror
// go-common's xerr so grpcx-style interception keeps working with
// service-specific reasons.
//
// Usage:
//
//	if err != nil {
//	    return nil, xcodes.ErrKeyNotFound.Wrapf(err, "key hash=%s", hash)
//	}
//
// The gRPC status message is always "REASON: message" — the future HTTP
// gateway splits on the first ": " to recover the lowercase error code.
package xcodes

import "github.com/servekit/go-common/xerr"

// Generic codes mirrored from go-common.
var (
	ErrBadRequest         = xerr.New("BAD_REQUEST", xerr.CategoryBadRequest, 400, "bad request")
	ErrUnauthorized       = xerr.New("UNAUTHORIZED", xerr.CategoryUnauthorized, 401, "unauthorized")
	ErrForbidden          = xerr.New("FORBIDDEN", xerr.CategoryForbidden, 403, "forbidden")
	ErrNotFound           = xerr.New("NOT_FOUND", xerr.CategoryNotFound, 404, "not found")
	ErrConflict           = xerr.New("CONFLICT", xerr.CategoryConflict, 409, "conflict")
	ErrTooManyRequests    = xerr.New("TOO_MANY_REQUESTS", xerr.CategoryTooManyRequests, 429, "too many requests")
	ErrInternal           = xerr.New("INTERNAL_ERROR", xerr.CategoryInternal, 500, "internal server error")
	ErrServiceUnavailable = xerr.New("SERVICE_UNAVAILABLE", xerr.CategoryServiceUnavailable, 503, "service unavailable")
)
