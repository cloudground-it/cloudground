package agentrpc

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

// urlPasswordRe finds the password of a URL's userinfo (rest:https://u:pw@h,
// s3:https://key:secret@endpoint), which a tool's output can carry.
var urlPasswordRe = regexp.MustCompile(`(://[^/@:\s]*:)[^/@\s]+@`)

// RedactCredentials hides every URL password in s. An error's detail is
// written to the agent's and the panel's journals, which read-only accounts
// can read (GET /api/logs), so it goes through this first.
func RedactCredentials(s string) string {
	return urlPasswordRe.ReplaceAllString(s, "${1}***@")
}

// Code classifies an agent error (SPEC.md §4).
type Code string

const (
	CodeInvalid      Code = "invalid"
	CodeNotFound     Code = "not_found"
	CodeConflict     Code = "conflict"
	CodeUnauthorized Code = "unauthorized"
	CodeTimeout      Code = "timeout"
	CodeCanceled     Code = "canceled"
	CodeUnavailable  Code = "unavailable"
	CodeInternal     Code = "internal"
)

// Error is the one error type that crosses the socket. Message is safe to
// show a user; Detail is for logs only.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	// Key, when set, names Message in the API's catalogue (internal/i18n)
	// so it can be shown translated; Args are its values, in order. Both
	// are as safe to show as Message (SPEC.md §4).
	Key  string   `json:"key,omitempty"`
	Args []string `json:"args,omitempty"`
}

// MessageKey is the translation key of the message and its values ("" when
// it has none): a task stores them to show the error in each reader's
// language (state.KeyedError).
func (e *Error) MessageKey() (string, []string) { return e.Key, e.Args }

// Error returns the user-safe message only, so wrapping an agent error in a
// task error or a response never carries the detail along.
func (e *Error) Error() string { return e.Message }

// Invalid is a request the agent refuses; the message says why and must be
// safe to show a user.
func Invalid(format string, args ...any) error {
	return &Error{Code: CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

// NotFound is a request naming something that does not exist.
func NotFound(format string, args ...any) error {
	return &Error{Code: CodeNotFound, Message: fmt.Sprintf(format, args...)}
}

// Conflict is a request the current state forbids.
func Conflict(format string, args ...any) error {
	return &Error{Code: CodeConflict, Message: fmt.Sprintf(format, args...)}
}

// Wrap gives err a user-safe message and code; err's text becomes the detail.
func Wrap(code Code, message string, err error) error {
	e := &Error{Code: code, Message: message}
	if err != nil {
		e.Detail = err.Error()
	}
	return e
}

// AsError returns err's first *Error, or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// CodeOf returns err's code: CodeInternal for an error that carries none.
func CodeOf(err error) Code {
	if e := AsError(err); e != nil {
		return e.Code
	}
	return CodeInternal
}

// toError is what the server sends for a handler's error: the first *Error
// in the chain decides code and message, the whole chain's text is the
// detail. Anything else is internal with the method's Fail message.
func toError(spec Spec, err error) *Error {
	if e := AsError(err); e != nil {
		out := *e
		if full := err.Error(); full != e.Message {
			if out.Detail == "" {
				out.Detail = full
			} else {
				out.Detail = full + ": " + out.Detail
			}
		}
		out.Detail = RedactCredentials(out.Detail)
		return &out
	}
	msg := spec.Fail
	if msg == "" {
		msg = "The agent could not complete the operation"
	}
	return &Error{Code: CodeInternal, Message: msg, Detail: RedactCredentials(err.Error())}
}

// socketStatus is the status the agent's server answers with.
func socketStatus(c Code) int {
	switch c {
	case CodeInvalid:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeTimeout:
		return http.StatusGatewayTimeout
	case CodeCanceled:
		return 499
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// HTTPStatus is the status the API answers a browser with for an agent
// error. def replaces 502 Bad Gateway for internal errors (0 keeps 502).
func HTTPStatus(err error, def int) int {
	if def == 0 {
		def = http.StatusBadGateway
	}
	switch CodeOf(err) {
	case CodeInvalid:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeTimeout:
		return http.StatusGatewayTimeout
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		// unauthorized and canceled are the API's own trouble with the
		// agent, not the browser's request.
		return def
	}
}

// Message is err's user-safe text: an agent error's message, or the given
// fallback for any other error.
func Message(err error, fallback string) string {
	if e := AsError(err); e != nil {
		return e.Message
	}
	return fallback
}
