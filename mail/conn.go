package mail

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxLiteral caps a single literal, i.e. one message, so a broken or hostile
// server can't make the client allocate without bound.
const maxLiteral = 64 << 20

// conn speaks the protocol on one connection. It is not safe for concurrent
// use; Client serializes access.
type conn struct {
	nc        net.Conn
	r         *bufio.Reader
	w         *bufio.Writer
	timeout   time.Duration
	trace     io.Writer
	seq       uint64
	caps      map[string]bool
	capsKnown bool
	bye       string
	// broken is set once the connection's state is unknown, after an I/O
	// error or a timeout; every later command fails with it.
	broken error
}

func newConn(nc net.Conn, cfg Config) *conn {
	return &conn{
		nc:      nc,
		r:       bufio.NewReader(nc),
		w:       bufio.NewWriter(nc),
		timeout: cfg.Timeout,
		trace:   cfg.Trace,
		caps:    map[string]bool{},
	}
}

// open reads the greeting and logs in unless the server pre-authenticated.
func (c *conn) open(ctx context.Context, username, password string) error {
	stop := c.arm(ctx)
	greeting, err := c.readResponse()
	stop()
	if err != nil {
		return fmt.Errorf("mail: reading the greeting: %w", c.ioError(ctx, err))
	}
	c.observe(greeting)
	preauth := false
	switch greeting.status {
	case "OK":
	case "PREAUTH":
		preauth = true
	default:
		return fmt.Errorf("mail: server refused the connection: %s", strings.TrimSpace(greeting.raw))
	}
	if !c.capsKnown {
		if _, _, err := c.execute(ctx, newCommand("CAPABILITY")); err != nil {
			return err
		}
	}
	if preauth {
		return nil
	}
	if c.caps["LOGINDISABLED"] {
		return errors.New("mail: the server doesn't allow LOGIN on this connection")
	}
	c.capsKnown = false
	if _, _, err := c.execute(ctx, newCommand("LOGIN").str(username).str(password).hideArgs()); err != nil {
		return fmt.Errorf("mail: login: %w", err)
	}
	// Servers often extend their capabilities after login.
	if !c.capsKnown {
		if _, _, err := c.execute(ctx, newCommand("CAPABILITY")); err != nil {
			return err
		}
	}
	return nil
}

// arm sets the deadline for one exchange: the command timeout or ctx's
// deadline, whichever comes first, and cuts it short when ctx ends.
func (c *conn) arm(ctx context.Context) func() {
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.nc.SetDeadline(deadline)
	stopAfter := context.AfterFunc(ctx, func() { _ = c.nc.SetDeadline(time.Now()) })
	return func() {
		stopAfter()
		_ = c.nc.SetDeadline(time.Time{})
	}
}

func (c *conn) ioError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		err = fmt.Errorf("%w: the server didn't answer within %v", ErrTimeout, c.timeout)
	}
	if c.bye != "" {
		err = fmt.Errorf("%w (server said BYE: %s)", err, c.bye)
	}
	c.broken = err
	return err
}

// execute runs one command and returns its untagged responses and its
// tagged completion. NO and BAD come back as *CommandError.
func (c *conn) execute(ctx context.Context, cmd *command) ([]*response, *response, error) {
	if c.broken != nil {
		return nil, nil, c.broken
	}
	stop := c.arm(ctx)
	defer stop()
	untagged, done, err := c.run(cmd)
	if err != nil {
		var ce *CommandError
		if !errors.As(err, &ce) {
			return untagged, done, c.ioError(ctx, err)
		}
	}
	return untagged, done, err
}

func (c *conn) run(cmd *command) ([]*response, *response, error) {
	c.seq++
	tag := "a" + strconv.FormatUint(c.seq, 10)
	var untagged []*response
	var line strings.Builder
	line.WriteString(tag)
	traced := tag + " " + cmd.name
	if cmd.secret {
		traced += " <hidden>"
	}
	for i, p := range cmd.parts {
		line.WriteByte(' ')
		if !p.literal {
			line.WriteString(p.text)
			if !cmd.secret && i > 0 {
				traced += " " + p.text
			}
			continue
		}
		n := len(p.text)
		sync := !c.caps["LITERAL+"] && !(c.caps["LITERAL-"] && n <= 4096)
		if sync {
			fmt.Fprintf(&line, "{%d}\r\n", n)
		} else {
			fmt.Fprintf(&line, "{%d+}\r\n", n)
		}
		if !cmd.secret {
			traced += fmt.Sprintf(" {%d bytes}", n)
		}
		if _, err := c.w.WriteString(line.String()); err != nil {
			return untagged, nil, err
		}
		line.Reset()
		if sync {
			if err := c.w.Flush(); err != nil {
				return untagged, nil, err
			}
			for {
				r, err := c.readResponse()
				if err != nil {
					return untagged, nil, err
				}
				if r.tag == "+" {
					break
				}
				if r.tag == tag {
					return untagged, r, commandError(cmd, r)
				}
				c.observe(r)
				untagged = append(untagged, r)
			}
		}
		if _, err := c.w.WriteString(p.text); err != nil {
			return untagged, nil, err
		}
	}
	line.WriteString("\r\n")
	c.traceLine("C: ", traced+"\r\n")
	if _, err := c.w.WriteString(line.String()); err != nil {
		return untagged, nil, err
	}
	if err := c.w.Flush(); err != nil {
		return untagged, nil, err
	}
	for {
		r, err := c.readResponse()
		if err != nil {
			return untagged, nil, err
		}
		c.observe(r)
		switch {
		case r.tag == tag:
			if r.status != "OK" {
				return untagged, r, commandError(cmd, r)
			}
			return untagged, r, nil
		case r.tag == "+":
			return untagged, nil, errors.New("unexpected continuation request")
		case r.tag != "*":
			return untagged, nil, fmt.Errorf("response for unknown tag %q", r.tag)
		}
		if r.status == "BYE" {
			c.bye = r.text
		}
		untagged = append(untagged, r)
	}
}

func commandError(cmd *command, r *response) error {
	status := r.status
	if status == "" {
		status = "BAD"
	}
	return &CommandError{Command: cmd.name, Status: status, Code: r.code, Text: r.text}
}

// readResponse reads one response with its literals inlined.
func (c *conn) readResponse() (*response, error) {
	var b strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		b.WriteString(line)
		n, ok := trailingLiteral(line)
		if !ok {
			break
		}
		if n > maxLiteral {
			return nil, fmt.Errorf("server announced a %d byte literal, over the %d byte limit", n, maxLiteral)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		b.Write(buf)
	}
	raw := b.String()
	c.traceLine("S: ", raw)
	return parseResponse(raw), nil
}

// observe keeps the capability list current from CAPABILITY responses and
// [CAPABILITY ...] codes.
func (c *conn) observe(r *response) {
	var list string
	switch {
	case r.kind == "CAPABILITY":
		list = r.data.rest()
	case strings.HasPrefix(strings.ToUpper(r.code), "CAPABILITY "):
		list = r.code[len("CAPABILITY "):]
	default:
		return
	}
	c.caps = map[string]bool{}
	for _, name := range strings.Fields(list) {
		c.caps[strings.ToUpper(name)] = true
	}
	c.capsKnown = true
}

func (c *conn) traceLine(prefix, s string) {
	if c.trace == nil {
		return
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	_, _ = io.WriteString(c.trace, prefix+s)
}

// logout says goodbye briefly and closes the socket either way.
func (c *conn) logout() error {
	if c.broken == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, _ = c.execute(ctx, newCommand("LOGOUT"))
		cancel()
	}
	c.broken = ErrClosed
	return c.nc.Close()
}
