package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Code is a stable, machine-readable category for tool failures. Agents key
// retry / fallback logic off this rather than parsing error text.
type Code string

const (
	CodeNotFound         Code = "not_found"
	CodeStale            Code = "stale"
	CodeTimeout          Code = "timeout"
	CodeNavigationFailed Code = "navigation_failed"
	CodeEvalThrew        Code = "eval_threw"
	CodeInvalidParam     Code = "invalid_param"
	CodeInternal         Code = "internal"
)

// Error is the structured tool error. JSON-RPC layer extracts Code into the
// error.data field so callers can branch on category.
type Error struct {
	Code    Code
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func newErr(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// wrap turns a third-party error (chromedp / CDP) into a typed Error using
// best-effort heuristics on the error text.
func wrap(cause error) *Error {
	if cause == nil {
		return nil
	}
	var e *Error
	if errors.As(cause, &e) {
		return e
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: "deadline exceeded", Cause: cause}
	}
	msg := cause.Error()
	switch {
	case strings.Contains(msg, "could not find node"),
		strings.Contains(msg, "No node with given id"),
		strings.Contains(msg, "Node was not found"),
		strings.Contains(msg, "Node is detached from document"):
		return &Error{Code: CodeStale, Message: "element is no longer attached", Cause: cause}
	case strings.Contains(msg, "net::ERR_"):
		return &Error{Code: CodeNavigationFailed, Message: extractNetErr(msg), Cause: cause}
	case strings.Contains(msg, "Cannot find context with specified id"):
		return &Error{Code: CodeStale, Message: "execution context gone (page navigated?)", Cause: cause}
	default:
		return &Error{Code: CodeInternal, Message: msg, Cause: cause}
	}
}

func extractNetErr(msg string) string {
	if i := strings.Index(msg, "net::ERR_"); i >= 0 {
		tail := msg[i:]
		if j := strings.IndexAny(tail, " \t\n"); j > 0 {
			return tail[:j]
		}
		return tail
	}
	return msg
}
