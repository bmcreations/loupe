package emugrpc

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Code is a gRPC status code.
type Code uint32

const (
	OK                 Code = 0
	Canceled           Code = 1
	Unknown            Code = 2
	InvalidArgument    Code = 3
	DeadlineExceeded   Code = 4
	NotFound           Code = 5
	AlreadyExists      Code = 6
	PermissionDenied   Code = 7
	ResourceExhausted  Code = 8
	FailedPrecondition Code = 9
	Aborted            Code = 10
	OutOfRange         Code = 11
	Unimplemented      Code = 12
	Internal           Code = 13
	Unavailable        Code = 14
	DataLoss           Code = 15
	Unauthenticated    Code = 16
)

var codeNames = [...]string{
	"OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED",
	"NOT_FOUND", "ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED",
	"FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED",
	"INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED",
}

func (c Code) String() string {
	if int(c) < len(codeNames) {
		return codeNames[c]
	}
	return "CODE(" + strconv.FormatUint(uint64(c), 10) + ")"
}

// Status is the grpc-status and grpc-message a call ended with. A call that
// ended OK has a Status too, so a caller can tell "the emulator closed the
// call" from "the connection dropped".
type Status struct {
	Code    Code
	Message string
}

func (s *Status) Error() string {
	if s.Message == "" {
		return "emulator gRPC: " + s.Code.String()
	}
	return fmt.Sprintf("emulator gRPC: %s: %s", s.Code, s.Message)
}

// statusFrom reads grpc-status and grpc-message from h, the response
// headers of a Trailers-Only reply or the trailers of any other. ok is false
// when h carries no grpc-status.
func statusFrom(h http.Header) (st *Status, ok bool) {
	raw := h.Get("Grpc-Status")
	if raw == "" {
		return nil, false
	}
	code, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return &Status{Code: Unknown, Message: "unparseable grpc-status " + strconv.Quote(raw)}, true
	}
	msg := h.Get("Grpc-Message")
	// grpc-message is percent-encoded. A server that sent it raw still
	// says something worth showing.
	if dec, err := url.PathUnescape(msg); err == nil {
		msg = dec
	}
	return &Status{Code: Code(code), Message: msg}, true
}

// httpStatus is gRPC's mapping for a reply that is not HTTP 200 and has no
// grpc-status: something in front of the service answered, not the service.
func httpStatus(code int) *Status {
	c := Unknown
	switch code {
	case http.StatusBadRequest:
		c = Internal
	case http.StatusUnauthorized:
		c = Unauthenticated
	case http.StatusForbidden:
		c = PermissionDenied
	case http.StatusNotFound:
		c = Unimplemented
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		c = Unavailable
	}
	return &Status{Code: c, Message: fmt.Sprintf("HTTP %d with no grpc-status", code)}
}
