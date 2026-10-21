package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/quotacore/quotacore/internal/observability"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

// serverErr is a reply error as go-redis hands one back.
type serverErr string

func (e serverErr) Error() string { return string(e) }
func (serverErr) RedisError()     {}

// TestTheStoreErrorClasses (IP-13 DoD 1, S3): each failure lands in exactly
// one class of datastore_errors_total, and an unknown one in other.
func TestTheStoreErrorClasses(t *testing.T) {
	cases := map[string]error{
		observability.ClassTimeout:  context.DeadlineExceeded,
		observability.ClassNoScript: serverErr("NOSCRIPT No matching script. Please use EVAL."),
		observability.ClassOOM:      fmt.Errorf("wrapped: %w", serverErr("OOM command not allowed when used memory > 'maxmemory'")),
		observability.ClassRefused:  &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
		observability.ClassOther:    errors.New("ERR something else"),
	}
	for want, err := range cases {
		if got := Class(err); got != want {
			t.Errorf("Class(%v) = %s, want %s", err, got, want)
		}
	}
	for _, err := range []error{timeoutErr{}, &net.OpError{Op: "read", Err: timeoutErr{}}} {
		if got := Class(err); got != observability.ClassTimeout {
			t.Errorf("Class(%v) = %s, want timeout", err, got)
		}
	}
	for _, err := range []error{io.EOF, syscall.ECONNRESET, redis.ErrClosed} {
		if got := Class(err); got != observability.ClassRefused {
			t.Errorf("Class(%v) = %s, want refused", err, got)
		}
	}
}
