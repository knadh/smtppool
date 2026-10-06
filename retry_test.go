package smtppool

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestCanRetry(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"ses 451 data timeout", &textproto.Error{Code: 451, Msg: "4.4.2 Timeout waiting for data from client."}, true},
		{"421 rate limit", &textproto.Error{Code: 421, Msg: "Too many connections"}, true},
		{"450 mailbox busy", &textproto.Error{Code: 450, Msg: "Requested mail action not taken"}, true},

		{"550 no such user", &textproto.Error{Code: 550, Msg: "No such user"}, false},
		{"552 message too large", &textproto.Error{Code: 552, Msg: "Message size exceeds limit"}, false},

		{"io.EOF", io.EOF, true},
		{"net.OpError", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},

		{"plain error", errors.New("some non-network error"), false},
		{"nil", nil, false},
		{"wrapped 451", fmt.Errorf("send: %w", &textproto.Error{Code: 451}), true},
		{"wrapped EOF", fmt.Errorf("send: %w", io.EOF), true},
		{"399", &textproto.Error{Code: 399}, false},
		{"499", &textproto.Error{Code: 499}, true},
		{"500", &textproto.Error{Code: 500}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canRetry(tc.err); got != tc.want {
				t.Errorf("canRetry(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDataReply(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reply     string
		wantRetry bool
		wantError bool
	}{
		{"accepted", "250 OK", false, false},
		{"temporary", "451 Try later", true, true},
		{"permanent", "550 Rejected", false, true},
		{"lost reply", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			client.SetDeadline(time.Now().Add(3 * time.Second))
			server.SetDeadline(time.Now().Add(3 * time.Second))
			done := make(chan error, 1)
			go func() { done <- serveRetrySMTP(server, tc.reply) }()
			sm, err := smtp.NewClient(client, "localhost")
			if err != nil {
				t.Fatal(err)
			}
			c := &conn{conn: sm}
			retry, err := c.send(Email{From: "a@example.com", To: []string{"b@example.com"}, Text: []byte("hello")})
			if retry != tc.wantRetry || (err != nil) != tc.wantError {
				t.Errorf("send() = (%v, %v), want retry=%v, error=%v", retry, err, tc.wantRetry, tc.wantError)
			}
			client.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSendRetriesFreshConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() {
		for _, reply := range []string{"451 Try later", "250 OK"} {
			c, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			c.SetDeadline(time.Now().Add(3 * time.Second))
			if err := serveRetrySMTP(c, reply); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	pool, err := New(Opt{
		Host:              "127.0.0.1",
		Port:              listener.Addr().(*net.TCPAddr).Port,
		MaxConns:          1,
		MaxMessageRetries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Send(Email{From: "a@example.com", To: []string{"b@example.com"}, Text: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	// Release the successful connection so the server can finish.
	c := <-pool.conns
	c.conn.Close()
	pool.createdConns.Add(-1)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// serveRetrySMTP handles one message and rejects unexpected commands.
func serveRetrySMTP(c net.Conn, reply string) error {
	defer c.Close()
	tp := textproto.NewConn(c)
	if err := tp.PrintfLine("220 localhost SMTP"); err != nil {
		return err
	}
	dataSeen := false
	for {
		line, err := tp.ReadLine()
		if err == io.EOF && dataSeen {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case !dataSeen && (strings.HasPrefix(line, "EHLO ") || strings.HasPrefix(line, "MAIL FROM:") || strings.HasPrefix(line, "RCPT TO:")):
			err = tp.PrintfLine("250 OK")
		case !dataSeen && line == "DATA":
			if err := tp.PrintfLine("354 Send data"); err != nil {
				return err
			}
			if _, err := tp.ReadDotBytes(); err != nil {
				return err
			}
			dataSeen = true
			if reply == "" {
				return nil
			}
			err = tp.PrintfLine("%s", reply)
		case dataSeen && strings.HasPrefix(reply, "250") && line == "RSET":
			err = tp.PrintfLine("250 OK")
		default:
			return fmt.Errorf("unexpected SMTP command: %q", line)
		}
		if err != nil {
			return err
		}
	}
}
