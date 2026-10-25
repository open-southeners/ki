package rpc

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Server-defined error codes (the -32000 to -32099 range is reserved by the
// JSON-RPC 2.0 specification for implementations).
const (
	CodeUnauthorized         = -32001
	CodeHandshakeRequired    = -32002
	CodeIncompatibleVersion  = -32003
	CodeSubscriptionNotFound = -32004
	CodeOverloaded           = -32005
	CodeNotFound             = -32006
	CodePolicyViolation      = -32007
	CodeRuntimeUnavailable   = -32008
	CodeAlreadyExists        = -32009
	CodeRepositoryError      = -32010
	CodeEnrollmentError      = -32011
	CodeDeployError          = -32012
)

// ErrClosed is returned by operations on a Peer that has been closed or whose
// connection has ended.
var ErrClosed = errors.New("rpc: peer closed")

// Error is a JSON-RPC 2.0 error object. It implements the error interface so
// handlers can return it directly; the peer maps it onto the wire verbatim.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// NewError returns an Error with the given code and message.
func NewError(code int, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Errorf returns an Error with the given code and a formatted message.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithData returns a copy of e carrying data (marshalled to JSON) in the
// error's data member. If data cannot be marshalled it is omitted.
func (e *Error) WithData(data any) *Error {
	c := *e
	if b, err := json.Marshal(data); err == nil {
		c.Data = b
	}
	return &c
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// DecodeParams unmarshals raw request params into v. Absent or null params
// leave v untouched. A decoding failure is returned as an InvalidParams
// *Error, so handlers may return it unchanged.
func DecodeParams(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return Errorf(CodeInvalidParams, "invalid params: %v", err)
	}
	return nil
}
