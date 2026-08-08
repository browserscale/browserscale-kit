// Package proxy parses proxy list files and hands proxies out to workers.
//
// It is the modernized replacement for solar2-core's proxies package:
// Parse now returns descriptive errors (the old one returned errors.New(""))
// and accepts every common list format instead of just two.
package proxy

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/browserscale/browserscale-kit/input"
)

// Proxy is a single parsed proxy endpoint. Username/Password are empty
// for unauthenticated proxies. Scheme is "" when the source line carried
// no scheme (host:port[:user:pass] shorthand).
type Proxy struct {
	Scheme   string
	Host     string
	Port     int
	Username string
	Password string
}

// Parse accepts the common proxy list formats and returns a descriptive
// error on failure:
//
//	host:port
//	host:port:user:pass
//	user:pass@host:port
//	host:port@user:pass
//	scheme://host:port
//	scheme://user:pass@host:port
func Parse(s string) (Proxy, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Proxy{}, fmt.Errorf("empty proxy")
	}

	// scheme://... — delegate to net/url which handles userinfo cleanly.
	if strings.Contains(s, "://") {
		return parseURL(s)
	}

	// user:pass@host:port  or  host:port@user:pass
	if at := strings.Index(s, "@"); at >= 0 {
		left, right := s[:at], s[at+1:]
		// Decide which side is host:port by which one parses as such.
		if h, err := parseHostPort(right); err == nil {
			u, p := splitUserPass(left)
			h.Username, h.Password = u, p
			return h, nil
		}
		if h, err := parseHostPort(left); err == nil {
			u, p := splitUserPass(right)
			h.Username, h.Password = u, p
			return h, nil
		}
		return Proxy{}, fmt.Errorf("proxy %q: neither side of @ is host:port", s)
	}

	// Colon-delimited shorthand: host:port or host:port:user:pass.
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 2:
		return parseHostPort(s)
	case 4:
		p, err := parseHostPort(parts[0] + ":" + parts[1])
		if err != nil {
			return Proxy{}, err
		}
		p.Username, p.Password = parts[2], parts[3]
		return p, nil
	default:
		return Proxy{}, fmt.Errorf("proxy %q: want host:port or host:port:user:pass", s)
	}
}

func parseURL(s string) (Proxy, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Proxy{}, fmt.Errorf("proxy %q: %w", s, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return Proxy{}, fmt.Errorf("proxy %q: bad port %q", s, u.Port())
	}
	p := Proxy{Scheme: u.Scheme, Host: u.Hostname(), Port: port}
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
	}
	if p.Host == "" {
		return Proxy{}, fmt.Errorf("proxy %q: missing host", s)
	}
	return p, nil
}

func parseHostPort(s string) (Proxy, error) {
	host, portStr, ok := strings.Cut(s, ":")
	if !ok || host == "" {
		return Proxy{}, fmt.Errorf("proxy %q: want host:port", s)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return Proxy{}, fmt.Errorf("proxy %q: bad port %q", s, portStr)
	}
	return Proxy{Host: host, Port: port}, nil
}

func splitUserPass(s string) (user, pass string) {
	user, pass, _ = strings.Cut(s, ":")
	return user, pass
}

// ParseLines parses each line, silently skipping ones that don't parse.
// Use Parse directly when you want to know why a line was rejected.
func ParseLines(lines []string) []Proxy {
	out := make([]Proxy, 0, len(lines))
	for _, l := range lines {
		if p, err := Parse(l); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// ParseFile reads a proxy list file and parses its non-empty lines.
func ParseFile(path string) ([]Proxy, error) {
	lines, err := input.NonEmptyLines(path)
	if err != nil {
		return nil, err
	}
	return ParseLines(lines), nil
}

// HasAuth reports whether the proxy carries credentials.
func (p Proxy) HasAuth() bool { return p.Username != "" || p.Password != "" }

// Addr returns "host:port".
func (p Proxy) Addr() string { return p.Host + ":" + strconv.Itoa(p.Port) }

// URL renders the proxy as a scheme://[user:pass@]host:port URL. When the
// source carried no scheme, http is assumed.
func (p Proxy) URL() string {
	scheme := p.Scheme
	if scheme == "" {
		scheme = "http"
	}
	u := &url.URL{Scheme: scheme, Host: p.Addr()}
	if p.HasAuth() {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String()
}
