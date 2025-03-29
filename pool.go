// Package smtppool creates a pool of reusable SMTP connections for high
// throughput e-mailing.
package smtppool

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"sync/atomic"
	"time"
)

// SSLType is the type of SSL connection to use.
type SSLType uint8

const (
	// SSLNone specifies a plain unencrypted connection.
	SSLNone SSLType = iota

	// SSLTLS specifies an SSL (TLS) connection without the STARTTLS extension.
	SSLTLS

	// SSLSTARTTLS specifies a non-TLS connection that then upgrades to STARTTLS.
	SSLSTARTTLS
)

// Opt represents SMTP pool options.
type Opt struct {
	// Host is the SMTP server's hostname.
	Host string `json:"host"`

	// Port is the SMTP server port.
	Port int `json:"port"`

	// HelloHostname is the optional hostname to pass with the HELO command.
	// Default is "localhost".
	HelloHostname string `json:"hello_hostname"`

	// MaxConns is the maximum allowed concurrent SMTP connections.
	MaxConns int `json:"max_conns"`

	// MaxMessageRetries is the number of times a message should be retried
	// if sending fails. Default is 2. Min is 1.
	MaxMessageRetries int `json:"max_msg_retries"`

	// IdleTimeout is the maximum time to wait for new activity on a connection
	// before closing it and removing it from the pool.
	IdleTimeout time.Duration `json:"idle_timeout"`

	// PoolWaitTimeout is the maximum time to wait to obtain a connection from
	// a pool before timing out. This may happen when all open connections are
	// busy sending e-mails and they're not returning to the pool fast enough.
	// This is also the timeout used when creating new SMTP connections.
	PoolWaitTimeout time.Duration `json:"wait_timeout"`

	// Given TLSConfig:
	// SSLNone (default); open a plain unencrypted connection.
	// SSLTLS; use an SSL (TLS) connection without the STARTTLS extension.
	// SSLSTARTTLS; open a non-TLS connection and then requests the STARTTLS extension.
	SSL SSLType `json:"ssl"`

	// Auth is the smtp.Auth authentication scheme.
	Auth smtp.Auth

	// TLSConfig is the optional TLS configuration.
	TLSConfig *tls.Config
}

// Pool represents an SMTP connection pool.
type Pool struct {
	opt          Opt
	conns        chan *conn
	createdConns atomic.Int32

	// stopBorrow signals all waiting borrowCon() calls on the pool to
	// immediately return an ErrPoolClosed.
	stopBorrow chan bool

	// closed marks the pool as closed.
	closed atomic.Bool
}

// conn represents an AMTP client connection in the pool.
type conn struct {
	conn *smtp.Client

	// lastActivity records the time when the last message on this client
	// was sent. Used for sweeping and disconnecting idle connections.
	lastActivity time.Time
}

// LoginAuth is the SMTP "LOGIN" type implementation for smtp.Auth.
type LoginAuth struct {
	Username string
	Password string
}

var (
	// ErrPoolClosed is thrown when a closed Pool is used.
	ErrPoolClosed = errors.New("pool closed")

	netErr net.Error
)

// New initializes and returns a new SMTP Pool.
func New(o Opt) (*Pool, error) {
	if o.MaxConns < 1 {
		return nil, errors.New("MaxConns should be >= 1")
	}
	if o.MaxMessageRetries == 0 {
		o.MaxMessageRetries = 1
	}
	if o.PoolWaitTimeout.Seconds() < 1 {
		o.PoolWaitTimeout = time.Second * 2
	}

	p := &Pool{
		opt:        o,
		conns:      make(chan *conn, o.MaxConns),
		stopBorrow: make(chan bool),
	}

	// Start the idle connection sweeper.
	if o.IdleTimeout.Seconds() >= 1 && o.MaxConns > 1 {
		go p.sweepConns(time.Second * 2)
	}
	return p, nil
}

// Send sends an e-mail using an available connection in the pool.
// On error, the message is retried on a new connection.
func (p *Pool) Send(e Email) error {
	var lastErr error
	for range p.opt.MaxMessageRetries {
		// Get a connection from the pool.
		c, err := p.borrowConn()
		if err != nil {
			if canRetry(err) {
				continue
			}

			return err
		}

		// Send the message.
		retry, err := c.send(e)
		if err == nil {
			_ = p.returnConn(c, nil)
			return nil
		}
		lastErr = err

		// Not a retriable error.
		_ = p.returnConn(c, err)
		if !retry {
			return err
		}

	}

	return lastErr
}

// Close closes the pool.
func (p *Pool) Close() {
	p.closed.Store(true)
	close(p.stopBorrow)

	// If the sweeper isn't already running, run it.
	if p.opt.IdleTimeout.Seconds() <= 1 {
		p.sweepConns(time.Second * 1)
	}
}

// newConn creates a new SMTP client connection that can be added to the pool.
func (p *Pool) newConn() (cn *conn, err error) {
	var (
		netCon net.Conn
		addr   = fmt.Sprintf("%s:%d", p.opt.Host, p.opt.Port)
	)

	switch p.opt.SSL {
	case SSLTLS:
		// TLS connection.
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: p.opt.PoolWaitTimeout}, "tcp", addr, p.opt.TLSConfig)
		if err != nil {
			return nil, err
		}
		netCon = c

	default:
		// SSLSTARTTLS, SSLNone
		// Non-TLS connection that may be upgraded later using STARTTLS.
		c, err := net.DialTimeout("tcp", addr, p.opt.PoolWaitTimeout)
		if err != nil {
			return nil, err
		}
		netCon = c
	}

	// Connect to the SMTP server
	sm, err := smtp.NewClient(netCon, p.opt.Host)
	if err != nil {
		return nil, err
	}

	// The return values are named so that the errors from multiple points
	// here on are captured and the connection closed.
	defer func() {
		if err != nil {
			sm.Close()
		}
	}()

	// Is there a custom hostname for doing a HELLO with the SMTP server?
	if p.opt.HelloHostname != "" {
		sm.Hello(p.opt.HelloHostname)
	}

	// Attempt to upgrade to STARTTLS.
	if p.opt.SSL == SSLSTARTTLS {
		if ok, _ := sm.Extension("STARTTLS"); !ok {
			return nil, errors.New("SMTP STARTTLS extension not found")
		}
		if err := sm.StartTLS(p.opt.TLSConfig); err != nil {
			return nil, err
		}
	}

	// Optional auth.
	if p.opt.Auth != nil {
		if ok, _ := sm.Extension("AUTH"); !ok {
			return nil, errors.New("SMTP AUTH extension not found")
		}
		if err := sm.Auth(p.opt.Auth); err != nil {
			return nil, err
		}
	}

	return &conn{
		conn: sm,
	}, nil
}

// borrowConn borrows a connection from the pool.
func (p *Pool) borrowConn() (*conn, error) {
	// Check pool status and connection counts first.
	switch {
	case p.closed.Load():
		// If the pool is closed, return an error immediately.
		return nil, ErrPoolClosed
	case int(p.createdConns.Load()) < p.opt.MaxConns && len(p.conns) == 0:
		// If there are no connections in the pool and if there is room for new
		// connections, create a new connection. Locks are used ad-hoc to avoid
		// locking when IO bound newConn() is happening.
		p.createdConns.Add(1)
		cn, err := p.newConn()
		if err != nil {
			// Decrement counter on failed connection creation.
			p.createdConns.Add(-1)
			return nil, err
		}
		return cn, nil
	}

	// Try to get a connection from the pool or handle timeouts and pool closure.
	select {
	case c := <-p.conns:
		// Return the connection if one is available.
		return c, nil
	case <-p.stopBorrow:
		// Return error if the pool is closing down.
		return nil, ErrPoolClosed
	case <-time.After(p.opt.PoolWaitTimeout):
		// Return timeout error if no connection becomes available.
		return nil, errors.New("timed out waiting for free conn in pool")
	}
}

// returnConn returns connection to the pool based on the error from the last
// transaction on it.
func (p *Pool) returnConn(c *conn, lastErr error) (err error) {
	// If the function returns an error, that it means it's a bad connection
	// and should be closed and not added back to the pool.
	defer func() {
		if err != nil {
			p.createdConns.Add(-1)
			c.conn.Close()
		}
	}()

	if lastErr != nil {
		// Any error, except for textproto.Error (according to jordan-wright/email),
		// is a bad connection that should be killed.
		if _, ok := lastErr.(*textproto.Error); !ok {
			return lastErr
		}
	}

	// Always RSET (SMTP) the connection bfeore reusing it as some servers
	// throw "sender already specified", or "commands out of sequence" errors.
	if err := c.conn.Reset(); err != nil {
		return err
	}

	select {
	case p.conns <- c:
		return nil
	case <-time.After(p.opt.PoolWaitTimeout):
		return errors.New("timed out returning connection to pool")
	case <-p.stopBorrow:
		return ErrPoolClosed
	}
}

// sweepConns periodically sweeps through connections and closes that have not
// any activity in Opt.IdleTimeout time. This is a blocking function and should
// be run as a goroutine.
func (p *Pool) sweepConns(interval time.Duration) {
	activeConns := make([]*conn, cap(p.conns))
	for {
		<-time.After(interval)
		activeConns = activeConns[:0]

		// The number of conns in the channel are the ones that are potentially
		// idling. Iterate through them and examine their activity timestamp.
		var (
			num          = len(p.conns)
			createdConns = p.createdConns.Load()
			closed       = p.closed.Load()
		)
		if closed && createdConns == 0 {
			// If the pool is closed and there are no more connections, exit
			// the sweeper.
			return
		}

		for i := 0; i < num; i++ {
			var c *conn

			// Pick a connection to check from the pool.
			select {
			case c = <-p.conns:
			default:
				continue
			}

			if closed || time.Since(c.lastActivity) > p.opt.IdleTimeout {
				// If the pool is closed or the the connection is idling,
				// close the conn.
				p.createdConns.Add(-1)

				// Unlock mutex before blockong on IO.
				if closed {
					_ = c.conn.Quit()
				} else {
					_ = c.conn.Close()
				}

				continue
			}

			activeConns = append(activeConns, c)
		}

		// Put the active conns back in the pool.
		for _, c := range activeConns {
			select {
			case p.conns <- c:
			default:
				_ = c.conn.Close()
				p.createdConns.Add(-1)
			}
		}
	}
}

// send sends a message using the connection. The bool in the return indicates
// if the message can be retried in case of an SMTP related error.
func (c *conn) send(e Email) (bool, error) {
	c.lastActivity = time.Now()

	// Combine e-mail addresses from multiple lists.
	emails, err := combineEmails(e.To, e.Cc, e.Bcc)
	if err != nil {
		return true, err
	}

	// Extract SMTP envelope sender from the email struct.
	from, err := e.parseSender()
	if err != nil {
		return true, err
	}

	// Send the Mail command.
	if err = c.conn.Mail(from); err != nil {
		return canRetry(err), err
	}

	// Send RCPT for all receipients.
	for _, recip := range emails {
		if err = c.conn.Rcpt(recip); err != nil {
			return canRetry(err), err
		}
	}

	// Write the message.
	w, err := c.conn.Data()
	if err != nil {
		return canRetry(err), err
	}

	isClosed := false
	defer func() {
		if !isClosed {
			w.Close()
		}
	}()

	// Get raw message payload.
	msg, err := e.Bytes()
	if err != nil {
		return false, err
	}

	if _, err = w.Write(msg); err != nil {
		return canRetry(err), err
	}

	if err := w.Close(); err != nil {
		return false, err
	}
	isClosed = true

	return false, nil
}

// Start starts the SMTP LOGIN auth type.
// https://gist.github.com/andelf/5118732
func (a *LoginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", []byte{}, nil
}

// Next passes the credentials for SMTP LOGIN auth type.
func (a *LoginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch string(fromServer) {
	case "Username:":
		return []byte(a.Username), nil
	case "Password:":
		return []byte(a.Password), nil
	default:
		return nil, errors.New("unkown SMTP fromServer")
	}
}

// combineEmails takes multiple lists of e-mails, parses them, and combines
// them into a single list.
func combineEmails(lists ...[]string) ([]string, error) {
	ln := 0
	for _, l := range lists {
		ln += len(l)
	}

	out := make([]string, 0, ln)
	for _, l := range lists {
		for _, email := range l {
			// Parse the e-mail out of the address string.
			// Eg: a@a.com out of John Doe <a@a.com>.
			addr, err := mail.ParseAddress(email)
			if err != nil {
				return nil, err
			}
			out = append(out, addr.Address)
		}
	}
	return out, nil
}

// canRetry returns true if the given SMTP err is network
// related and hence, can be retried.
// eg: TCP/DNS/timeout/broken pipe etc.
func canRetry(err error) bool {
	if errors.As(err, &netErr) {
		return true
	} else if _, ok := err.(*net.OpError); ok {
		return true
	} else if err == io.EOF {
		return true
	}

	return false
}
