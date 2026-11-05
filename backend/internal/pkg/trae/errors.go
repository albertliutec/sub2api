package trae

import "fmt"

// ErrorKind classifies Trae upstream failures so the gateway can map them to
// client-facing HTTP errors and account failover decisions.
type ErrorKind string

const (
	ErrorKindAuth          ErrorKind = "auth"
	ErrorKindTransient     ErrorKind = "transient"
	ErrorKindProtocol      ErrorKind = "protocol"
	ErrorKindFatal         ErrorKind = "fatal"
	ErrorKindAborted       ErrorKind = "aborted"
	ErrorKindModelNotFound ErrorKind = "model_not_found"
	ErrorKindBadRequest    ErrorKind = "bad_request"
)

// Error is returned by every Trae protocol operation.
type Error struct {
	Kind    ErrorKind
	Status  int
	Message string
	Detail  string
	Err     error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "trae upstream error"
}

func (e *Error) Unwrap() error { return e.Err }

func newError(kind ErrorKind, status int, message string) *Error {
	return &Error{Kind: kind, Status: status, Message: message}
}

func wrapError(kind ErrorKind, status int, message string, err error) *Error {
	return &Error{Kind: kind, Status: status, Message: message, Err: err}
}

func (e *Error) withDetail(detail string) *Error {
	if e != nil {
		e.Detail = detail
	}
	return e
}

func (e *Error) String() string {
	return fmt.Sprintf("trae %s: %s", e.Kind, e.Error())
}
