package xray

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"xpanel/internal/ports"
)

func mapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	kind := ports.ErrorInternal
	retryable := true
	summary := "Xray operation failed"
	code := status.Code(err)
	switch {
	case code == codes.DeadlineExceeded || err == context.DeadlineExceeded:
		kind, retryable, summary = ports.ErrorDeadlineExceeded, true, "Xray operation timed out"
	case code == codes.Unavailable:
		kind, retryable, summary = ports.ErrorInstanceUnavailable, true, "Xray API is unavailable"
	case code == codes.InvalidArgument:
		kind, retryable, summary = ports.ErrorInvalidArgument, false, "Xray rejected invalid input"
	case code == codes.NotFound:
		kind, retryable, summary = ports.ErrorUserNotFound, false, "Xray target was not found"
		if operation == "validate_template" || operation == "list_users" {
			kind, summary = ports.ErrorProfileNotFound, "inbound was not found"
		} else if operation == "read_traffic" {
			kind, summary = ports.ErrorStatsNotFound, "Xray statistics were not found"
		} else if operation == "remove_inbound" || operation == "list_inbounds" {
			kind, summary = ports.ErrorInboundNotFound, "inbound was not found"
		}
	case code == codes.AlreadyExists:
		kind, retryable, summary = ports.ErrorUserAlreadyExists, false, "Xray user already exists"
	case code == codes.PermissionDenied || code == codes.Unauthenticated:
		kind, retryable, summary = ports.ErrorUpstreamRejected, false, "Xray rejected the operation"
	// 入站生命周期（contracts/xray-adapter.md 错误映射表；实测语句见 research.md R-002/R-004）。
	case strings.Contains(strings.ToLower(err.Error()), "address already in use"):
		kind, retryable, summary = ports.ErrorPortUnavailable, false, "port is already in use on the node"
	case strings.Contains(strings.ToLower(err.Error()), "existing tag found"):
		kind, retryable, summary = ports.ErrorInboundAlreadyExists, false, "inbound tag already exists"
	case strings.Contains(strings.ToLower(err.Error()), "handler not found"):
		kind, retryable, summary = ports.ErrorInboundNotFound, false, "inbound was not found"
	case strings.Contains(strings.ToLower(err.Error()), "not enough information for making a decision") && operation == "remove_inbound":
		kind, retryable, summary = ports.ErrorInboundNotFound, false, "inbound was not found"
	case strings.Contains(strings.ToLower(err.Error()), "not a usermanager"):
		kind, retryable, summary = ports.ErrorIncompatibleProfile, false, "inbound does not support dynamic users"
	case strings.Contains(strings.ToLower(err.Error()), "already exists"):
		kind, retryable, summary = ports.ErrorUserAlreadyExists, false, "Xray user already exists"
	case strings.Contains(strings.ToLower(err.Error()), "not found"):
		kind, retryable, summary = ports.ErrorUserNotFound, false, "Xray target was not found"
	}
	return &ports.AdapterError{Kind: kind, Operation: operation, Retryable: retryable, SafeSummary: summary}
}
