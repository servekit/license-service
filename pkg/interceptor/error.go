// Package interceptor provides license-service gRPC unary interceptors: the
// xerr→status error mapping with proto details support, and admin Bearer
// authentication.
package interceptor

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"

	"github.com/servekit/go-common/xerr"

	"github.com/servekit/license-service/pkg/xcodes"
)

// categoryToGRPC mirrors grpcx.ErrorInterceptor's mapping; kept local so the
// license interceptor can additionally promote xcodes.Detailed details into
// the status.
var categoryToGRPC = map[xerr.Category]codes.Code{
	xerr.CategoryBadRequest:         codes.InvalidArgument,
	xerr.CategoryUnauthorized:       codes.Unauthenticated,
	xerr.CategoryForbidden:          codes.PermissionDenied,
	xerr.CategoryNotFound:           codes.NotFound,
	xerr.CategoryConflict:           codes.AlreadyExists,
	xerr.CategoryTooManyRequests:    codes.ResourceExhausted,
	xerr.CategoryInternal:           codes.Internal,
	xerr.CategoryServiceUnavailable: codes.Unavailable,
}

// Error maps xerr-wrapped service errors to gRPC status codes and attaches
// proto details carried by xcodes.Detailed. It replaces
// grpcx.ErrorInterceptor for this service:
//
//   - the status message keeps xerr's "REASON: message" text verbatim — the
//     future HTTP gateway splits on the first ": " to recover the error code;
//   - SlotLimitInfo / RetryAfterInfo details ride along via
//     status.WithDetails (downgraded to a bare status when WithDetails
//     fails, e.g. unregistered detail types).
//
// Non-xerr errors pass through unchanged.
func Error(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err == nil {
		return resp, nil
	}

	var det *xcodes.Detailed
	if errors.As(err, &det) {
		return nil, withDetails(det.Err, det.Details).Err()
	}

	var xe *xerr.Error
	if errors.As(err, &xe) {
		return nil, status.Error(categoryToGRPC[xe.Category()], xe.Error())
	}

	return nil, err
}

// withDetails builds a status with details attached, falling back to the
// plain status when WithDetails cannot marshal the details.
func withDetails(xe *xerr.Error, details []proto.Message) *status.Status {
	st := status.New(categoryToGRPC[xe.Category()], xe.Error())
	if len(details) == 0 {
		return st
	}
	v1 := make([]protoadapt.MessageV1, len(details))
	for i, d := range details {
		v1[i] = protoadapt.MessageV1Of(d)
	}
	enriched, err := st.WithDetails(v1...)
	if err != nil {
		return st
	}
	return enriched
}
