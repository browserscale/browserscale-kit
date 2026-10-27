package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"time"
)

// Config describes a mailbox and how the Client and the Fetcher talk to it.
// Only Host, Username and Password are needed; everything else has a default.
type Config struct {
	Host     string
	Port     int // 993 when zero; the connection is implicit TLS
	Username string
	Password string
	// Mailbox is the folder the Fetcher watches, INBOX when empty.
	Mailbox string
	// VerifyTLS checks the server's certificate. It is off by default so
	// servers with self-signed or private-CA certificates connect.
	VerifyTLS bool
	// Timeout bounds connecting and every single command, 30s when zero.
	Timeout time.Duration
	// Trace receives the raw protocol. The login's credentials are left out.
	Trace io.Writer

	// The fields below only concern the Fetcher.

	// PollInterval is how often the mailbox is checked for new mail while
	// someone waits, 3s when zero.
	PollInterval time.Duration
	// ClockSkew is taken off every Since, because the mail server's clock
	// and this machine's rarely agree to the second; 60s when zero.
	ClockSkew time.Duration
	// Window is how far back FetchCode looks when its query has no Since,
	// 72h when zero.
	Window time.Duration
	// Logger receives reconnects and errors, and at debug level every mail
	// that matched a query but not its Code pattern, body included.
	Logger *slog.Logger

	dial func(ctx context.Context) (net.Conn, error) // tests
}

func (c Config) withDefaults() Config {
	if c.Port == 0 {
		c.Port = 993
	}
	if c.Mailbox == "" {
		c.Mailbox = "INBOX"
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 3 * time.Second
	}
	if c.ClockSkew <= 0 {
		c.ClockSkew = 60 * time.Second
	}
	if c.Window <= 0 {
		c.Window = 72 * time.Hour
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return c
}

func (c Config) connect(ctx context.Context) (net.Conn, error) {
	if c.dial != nil {
		return c.dial(ctx)
	}
	if c.Host == "" {
		return nil, errors.New("mail: Config.Host is empty")
	}
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: c.Timeout, KeepAlive: 30 * time.Second},
		Config:    &tls.Config{ServerName: c.Host, InsecureSkipVerify: !c.VerifyTLS},
	}
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	if err != nil {
		return nil, fmt.Errorf("mail: connecting to %s:%d: %w", c.Host, c.Port, err)
	}
	return nc, nil
}

var (
	// ErrTimeout is wrapped by the error of a wait no matching mail arrived
	// for in time, which also carries the last connection error when the
	// mailbox was unreachable meanwhile, and by the error of a command the
	// server didn't answer within Config.Timeout.
	ErrTimeout = errors.New("mail: timed out")
	// ErrClosed is returned once the Client or Fetcher has been closed.
	ErrClosed = errors.New("mail: closed")
	// ErrNotFound is returned for a UID the mailbox doesn't hold (anymore).
	ErrNotFound = errors.New("mail: message not found")
)

// CommandError is a command the server refused with NO or BAD. The
// connection stays usable after it.
type CommandError struct {
	Command string
	Status  string // NO or BAD
	Code    string // response code such as BADCHARSET, empty when none
	Text    string
}

func (e *CommandError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("mail: %s: %s [%s] %s", e.Command, e.Status, e.Code, e.Text)
	}
	return fmt.Sprintf("mail: %s: %s %s", e.Command, e.Status, e.Text)
}
