package errors

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Externally visible error codes
const (
	CodeUnauthorized   = "ERR_UNAUTHORIZED"
	CodeBadRequest     = "ERR_BAD_REQUEST"
	CodeForbidden      = "ERR_FORBIDDEN"
	CodeNotFound       = "ERR_NOT_FOUND"
	CodeNotImplemented = "ERR_NOT_IMPLEMENTED"
	CodeTimeout        = "ERR_TIMEOUT"
	CodeInternal       = "ERR_INTERNAL"
)

// type kubeTaskerError interface {
// 	Error() string
// 	Code() string
// 	HTTPCode() int
// 	JSON() []byte
// }

type kubetaskererr struct {
	code    string
	message string
	err     error
}

// New returns an error with the supplied message.
func New(code string, message string) error {
	err := errors.New(message)
	return kubetaskererr{code, message, err}
}

// Errorf returns an error and formats according to a format specifier
func Errorf(code string, format string, args ...interface{}) error {
	return New(code, fmt.Sprintf(format, args...))
}

// InternalError is a convenience function to create a Internal error with a message
func InternalError(message string) error {
	return New(CodeInternal, message)
}

// InternalErrorf is a convenience function to format an Internal error
func InternalErrorf(format string, args ...interface{}) error {
	return Errorf(CodeInternal, format, args...)
}

// InternalWrapError annotates the error with the ERR_INTERNAL code and a stack trace, optional message
func InternalWrapError(err error, message ...string) error {
	if len(message) == 0 {
		return Wrap(err, CodeInternal, err.Error())
	}
	return Wrap(err, CodeInternal, message[0])
}

// InternalWrapErrorf annotates the error with the ERR_INTERNAL code and a stack trace, optional message
func InternalWrapErrorf(err error, format string, args ...interface{}) error {
	return Wrap(err, CodeInternal, fmt.Sprintf(format, args...))
}

// Wrap returns an error annotating err with a stack trace at the point Wrap is called,
// and a new supplied message. The previous original is preserved and accessible via Cause().
// If err is nil, Wrap returns nil.
func Wrap(err error, code string, message string) error {
	if err == nil {
		return nil
	}
	err = fmt.Errorf(message+": %w", err)
	return kubetaskererr{code, message, err}
}

// Cause returns the underlying cause of the error.
func Cause(err error) error {
	if kubeTaskerErr, ok := err.(kubetaskererr); ok {
		return unwrapCausekubeTaskerErr(kubeTaskerErr.err)
	}
	return unwrapCause(err)
}

func unwrapCausekubeTaskerErr(err error) error {
	innerErr := errors.Unwrap(err)
	for innerErr != nil {
		err = innerErr
		innerErr = errors.Unwrap(err)
	}
	return err
}

func unwrapCause(err error) error {
	type causer interface {
		Cause() error
	}

	for err != nil {
		cause, ok := err.(causer)
		if !ok {
			break
		}
		err = cause.Cause()
	}
	return err
}

func (e kubetaskererr) Error() string {
	return e.message
}

func (e kubetaskererr) Code() string {
	return e.code
}

func (e kubetaskererr) JSON() []byte {
	type errBean struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	eb := errBean{e.code, e.message}
	j, _ := json.Marshal(eb)
	return j
}

func (e kubetaskererr) HTTPCode() int {
	switch e.Code() {
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeNotImplemented:
		return http.StatusNotImplemented
	case CodeTimeout, CodeInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// IsCode is a helper to determine if the error is of a specific code
func IsCode(code string, err error) bool {
	if kubeTaskerErr, ok := err.(kubetaskererr); ok {
		return kubeTaskerErr.code == code
	}
	return false
}
