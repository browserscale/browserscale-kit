package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Pool hands proxies out to workers. Unlike input.Queue it never
// empties: Next round-robins forever and Random samples with
// replacement, which matches how proxies are actually used (rotate
// across many attempts). All methods are safe for concurrent use.
type Pool struct {
	mu      sync.Mutex
	proxies []Proxy
	rr      int
}

// NewPool builds a Pool from an already-parsed slice.
func NewPool(proxies []Proxy) *Pool {
	cp := make([]Proxy, len(proxies))
	copy(cp, proxies)
	return &Pool{proxies: cp}
}

// LoadPool parses a proxy list file into a Pool.
func LoadPool(path string) (*Pool, error) {
	proxies, err := ParseFile(path)
	if err != nil {
		return nil, err
	}
	return NewPool(proxies), nil
}

// Next returns the next proxy round-robin, wrapping around at the end.
// The bool is false only when the pool is empty.
func (p *Pool) Next() (Proxy, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.proxies) == 0 {
		return Proxy{}, false
	}
	pr := p.proxies[p.rr%len(p.proxies)]
	p.rr++
	return pr, true
}

// Random returns a uniformly random proxy (with replacement). The bool
// is false only when the pool is empty.
func (p *Pool) Random() (Proxy, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.proxies) == 0 {
		return Proxy{}, false
	}
	return p.proxies[rand.IntN(len(p.proxies))], true
}

// Len returns the number of proxies in the pool.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.proxies)
}

// ipInfo is the subset of ip-api.com/json we care about.
type ipInfo struct {
	Status      string `json:"status"`
	CountryCode string `json:"countryCode"`
	Query       string `json:"query"`
}

// Check verifies the proxy can reach the internet by fetching
// ip-api.com through it. Returns nil on success. This makes an outbound
// request and is intentionally kept out of Parse/Pool so parsing stays
// pure; call it explicitly when you want a liveness gate.
func Check(p Proxy, timeout time.Duration) error {
	proxyURL, err := url.Parse(p.URL())
	if err != nil {
		return fmt.Errorf("invalid proxy URL: %w", err)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   timeout,
	}
	resp, err := client.Get("http://ip-api.com/json/")
	if err != nil {
		return fmt.Errorf("request through proxy failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	var info ipInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if info.Status != "success" {
		return fmt.Errorf("ip lookup failed with status %q", info.Status)
	}
	return nil
}
