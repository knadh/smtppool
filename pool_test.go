package smtppool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	neturl "net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	smtpAddr = "localhost:1025"
	apiURL   = "http://localhost:8025"
)

var (
	reqTimeout = 3 * time.Second
)

func TestMain(m *testing.M) {
	// Start MailHog server.
	cmdPath := os.Getenv("MAILHOG")
	if cmdPath == "" {
		cmdPath = "mailhog"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := exec.CommandContext(ctx, cmdPath)
	if err := srv.Start(); err != nil {
		fmt.Printf("error starting mailhog: %v\n", err)
		os.Exit(1)
	}

	// Wait for MailHog to be ready
	if !waitForServer(reqTimeout) {
		fmt.Println("mailhog start timed out")
		os.Exit(1)
	}

	code := m.Run()

	// Stop MailHog
	srv.Process.Kill()
	os.Exit(code)
}

func waitForServer(timeout time.Duration) bool {
	start := time.Now()
	for {
		conn, err := net.DialTimeout("tcp", smtpAddr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		if time.Since(start) > timeout {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func clearServer() {
	http.DefaultClient.Do(&http.Request{
		Method: "DELETE",
		URL:    mustParse(apiURL + "/api/v1/messages"),
	})
}

func mustParse(url string) *neturl.URL {
	u, _ := neturl.Parse(url)
	return u
}

func getMessageCount(t *testing.T) int {
	resp, err := http.Get(apiURL + "/api/v2/messages")
	if err != nil {
		t.Fatalf("error getting messages: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Count int
		Items []interface{}
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("error decoding response: %v", err)
	}
	return result.Count
}

func TestSendEmail(t *testing.T) {
	clearServer()

	pool, err := New(Opt{
		Host:            "localhost",
		Port:            1025,
		MaxConns:        3,
		PoolWaitTimeout: 2 * time.Second,
		SSL:             SSLNone,
	})
	if err != nil {
		t.Fatalf("error creating pool: %v", err)
	}
	defer pool.Close()

	email := Email{
		From:    "sender@example.com",
		To:      []string{"recipient@example.com"},
		Subject: "Test Subject",
		Text:    []byte("Test Body"),
	}

	if err := pool.Send(email); err != nil {
		t.Fatalf("error sending email: %v", err)
	}

	// Verify email arrival.
	deadline := time.Now().Add(reqTimeout)
	for time.Now().Before(deadline) {
		if getMessageCount(t) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("email not received by server")
}

func TestConnectionPooling(t *testing.T) {
	clearServer()

	pool, err := New(Opt{
		Host:            "localhost",
		Port:            1025,
		MaxConns:        2,
		PoolWaitTimeout: 2 * time.Second,
		SSL:             SSLNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Send more emails than pool size
	for i := range 5 {
		go func() {
			email := Email{
				From:    fmt.Sprintf("sender%d@example.com", i),
				To:      []string{"recipient@example.com"},
				Subject: "Concurrent Test",
				Text:    []byte("Concurrent Body"),
			}
			pool.Send(email)
		}()
	}

	time.Sleep(2 * time.Second)
	if count := getMessageCount(t); count != 5 {
		t.Errorf("expected 5 messages, got %d", count)
	}
}

func TestPoolClose(t *testing.T) {
	pool, err := New(Opt{
		Host:            "localhost",
		Port:            1025,
		MaxConns:        1,
		PoolWaitTimeout: 2 * time.Second,
		SSL:             SSLNone,
	})
	if err != nil {
		t.Fatal(err)
	}

	pool.Close()

	err = pool.Send(Email{
		From: "test@example.com",
		To:   []string{"recipient@example.com"},
	})
	if err == nil {
		t.Error("expected error when sending after pool closed")
	}
}

func TestPoolCloseReleasesConnsWithoutSweeper(t *testing.T) {
	// With MaxConns == 1 the background idle sweeper is never started, even
	// when IdleTimeout is set. Close must still quit the pooled connection.
	clearServer()

	pool, err := New(Opt{
		Host:            "localhost",
		Port:            1025,
		MaxConns:        1,
		IdleTimeout:     10 * time.Second,
		PoolWaitTimeout: 2 * time.Second,
		SSL:             SSLNone,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := pool.Send(Email{
		From:    "sender@example.com",
		To:      []string{"recipient@example.com"},
		Subject: "Test Subject",
		Text:    []byte("Test Body"),
	}); err != nil {
		t.Fatalf("error sending email: %v", err)
	}
	if n := pool.createdConns.Load(); n != 1 {
		t.Fatalf("expected 1 open connection before Close, got %d", n)
	}

	done := make(chan struct{})
	go func() {
		pool.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}

	if n := pool.createdConns.Load(); n != 0 {
		t.Errorf("expected all connections closed after Close, got %d open", n)
	}
}

func TestSendInvalidEmail(t *testing.T) {
	clearServer()

	pool, err := New(Opt{
		Host:            "localhost",
		Port:            1025,
		MaxConns:        1,
		PoolWaitTimeout: 2 * time.Second,
		SSL:             SSLNone,
	})
	if err != nil {
		t.Fatalf("error creating pool: %v", err)
	}
	defer pool.Close()

	// Test with invalid From address
	invalidFromEmail := Email{
		From:    "invalid-email-address",
		To:      []string{"recipient@example.com"},
		Subject: "Test Invalid From",
		Text:    []byte("Test Body"),
	}

	if err := pool.Send(invalidFromEmail); err == nil {
		t.Error("expected error when sending email with invalid From address")
	}

	// Test with invalid To address
	invalidToEmail := Email{
		From:    "sender@example.com",
		To:      []string{"invalid-recipient"},
		Subject: "Test Invalid To",
		Text:    []byte("Test Body"),
	}

	if err = pool.Send(invalidToEmail); err == nil {
		t.Error("expected error when sending email with invalid To address")
	}

	// Verify no emails were actually sent
	if count := getMessageCount(t); count != 0 {
		t.Errorf("expected 0 messages, got %d", count)
	}
}

func TestCanRetryConcurrent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "timeout",
			err:  &net.DNSError{Err: "timeout", IsTimeout: true},
			want: true,
		},
		{
			name: "wrapped_network_error",
			err:  fmt.Errorf("send: %w", &net.OpError{Op: "write", Net: "tcp", Err: io.ErrClosedPipe}),
			want: true,
		},
		{name: "eof", err: io.EOF, want: true},
		{name: "other_error", err: errors.New("invalid message")},
		{name: "nil"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					for range 100 {
						if got := canRetry(tc.err); got != tc.want {
							t.Errorf("canRetry(%v) = %v, want %v", tc.err, got, tc.want)
							return
						}
					}
				}()
			}
			close(start)
			wg.Wait()
		})
	}
}

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
