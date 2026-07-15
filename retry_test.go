package smtppool

import (
	"errors"
	"io"
	"net"
	"net/textproto"
	"testing"
)

// TestCanRetry verifies the retriability classification: transient (4xx) SMTP
// replies and connection-level errors are retriable; permanent (5xx) replies
// and non-transient errors are not.
func TestCanRetry(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Transient SMTP replies (4xx) are safe to retry.
		{"ses 451 data timeout", &textproto.Error{Code: 451, Msg: "4.4.2 Timeout waiting for data from client."}, true},
		{"421 rate limit", &textproto.Error{Code: 421, Msg: "Too many connections"}, true},
		{"450 mailbox busy", &textproto.Error{Code: 450, Msg: "Requested mail action not taken"}, true},

		// Permanent SMTP replies (5xx) must NOT be retried.
		{"550 no such user", &textproto.Error{Code: 550, Msg: "No such user"}, false},
		{"552 message too large", &textproto.Error{Code: 552, Msg: "Message size exceeds limit"}, false},

		// Connection-level errors are retriable.
		{"io.EOF", io.EOF, true},
		{"net.OpError", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},

		// Everything else is not retriable.
		{"plain error", errors.New("some non-network error"), false},
		{"nil", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canRetry(tc.err); got != tc.want {
				t.Errorf("canRetry(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
