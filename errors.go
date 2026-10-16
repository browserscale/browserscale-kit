package kit

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// retryError asks for another try of the job with a fresh browser.
type retryError struct {
	reason   string
	newProxy bool
}

func (e *retryError) Error() string { return e.reason }

// fatalError stops the whole run, such as a wrong API key.
type fatalError struct{ msg string }

func (e *fatalError) Error() string { return e.msg }

// watchHit is what Wait returns when a watched condition matched instead of
// one the step waited for. Flow catches it and goes on with then.
type watchHit struct {
	then Step
	to   string
}

func (e *watchHit) Error() string { return "a watched condition matched: " + e.to }

type panicError struct {
	value any
	stack []byte
}

func (e *panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

const sdkPkg = "github.com/browserscale/browserscale-go"

// sdkCode finds the browserscale error in err's chain and its code, such as
// "timeout" for a wait or "net_error" for a navigation.
func sdkCode(err error) (command, code string, ok bool) {
	if err == nil {
		return "", "", false
	}
	if v := reflect.ValueOf(err); v.Kind() == reflect.Pointer && !v.IsNil() && v.Elem().Kind() == reflect.Struct && v.Elem().Type().PkgPath() == sdkPkg {
		s := v.Elem()
		if f := s.FieldByName("Code"); f.IsValid() && f.Kind() == reflect.String {
			code = f.String()
		}
		if f := s.FieldByName("Command"); f.IsValid() && f.Kind() == reflect.String {
			command = f.String()
		}
		return command, code, true
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return sdkCode(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if command, code, ok := sdkCode(e); ok {
				return command, code, true
			}
		}
	}
	return "", "", false
}

// proxyFault reports whether a navigation never got through: the page
// didn't load at all, which with a known target is the proxy's doing far
// more often than the site's.
func proxyFault(err error) bool {
	command, code, ok := sdkCode(err)
	return ok && command == "navigate" && (code == "net_error" || code == "timeout")
}

// transportFault reports whether the connection to the browser broke, as
// opposed to the page answering with an error.
func transportFault(err error) bool {
	if _, _, ok := sdkCode(err); ok {
		return false
	}
	s, ok := status.FromError(err)
	if !ok {
		var gs interface{ GRPCStatus() *status.Status }
		if !errors.As(err, &gs) {
			return false
		}
		s = gs.GRPCStatus()
	}
	switch s.Code() {
	case codes.Unavailable, codes.Aborted, codes.Internal, codes.Unknown, codes.ResourceExhausted, codes.DeadlineExceeded:
		return true
	}
	return false
}

// rentCode picks the rent API's failure code out of a RentBrowser error.
func rentCode(err error) string {
	msg := err.Error()
	for _, c := range rentCodes {
		if strings.Contains(msg, c) {
			return c
		}
	}
	return ""
}

var rentCodes = []string{
	"INVALID_API_KEY", "INSUFFICIENT_CREDITS", "INSUFFICIENT_PROXY_QUOTA",
	"PROXY_CONNECTION_FAILED", "PROXY_SERVER_UNAVAILABLE", "PROXY_GENERATION_FAILED",
	"NO_CAPACITY_AVAILABLE", "SESSION_LIMIT_REACHED", "BROWSER_CREATION_FAILED",
}
