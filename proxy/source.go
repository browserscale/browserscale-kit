package proxy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/browserscale/browserscale-kit/input"
)

// The kinds of Source.
const (
	// KindList is a list of proxies, one per line, used in turn.
	KindList = "list"
	// KindGateway is one provider endpoint that hands out exit addresses
	// itself, written as a template, e.g. a new address per session id.
	KindGateway = "gateway"
	// KindStatic is fixed addresses, each with an id a bot can ask for.
	KindStatic = "static"
	// KindMobile is proxies whose exit address changes when their rotation
	// URL is called.
	KindMobile = "mobile"
	// KindManaged is browserscale's managed proxies, a proxies setting of
	// "managed" or "managed:<country>"; an empty setting means them too.
	KindManaged = "managed"
)

// RefPrefix starts a reference to a saved source instead of a file name:
// proxy:<id> or proxy:<id>/<profile>.
const RefPrefix = "proxy:"

// SourcesEnv names the directory saved sources are read from. Studio sets it
// for the bots it runs.
const SourcesEnv = "BROWSERSCALE_PROXIES"

const defaultRotateWait = 10 * time.Second

// Spec is a source as it's saved, a JSON file:
//
//	{"kind": "gateway", "name": "Residential",
//	 "template": "gw.example.com:7000:user:pass_country-{country}_session-{session}",
//	 "params": {"country": "de"}, "profiles": {"us": {"country": "us"}}}
//
// Lines, entries and the template may hold placeholders, filled for every
// browser: {session} (a new random id, {session:8} for 8 characters),
// {port:10000-10999} (a random port in the range) and {name} for a parameter.
// A line's scheme says how the browser talks to the proxy: http (also for
// none), socks5 or socks5h.
type Spec struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	// Template is a gateway's proxy line.
	Template string `json:"template,omitempty"`
	// Masque is a gateway's MASQUE proxy, "host:port": the browser's HTTP/3
	// goes through it with the template's credentials, which have to give
	// it the same exit address, so a session id rather than one that
	// rotates on every connection. Without it HTTP/3 falls back to TCP.
	Masque string `json:"masque,omitempty"`
	// Params are the values for the template's {name} placeholders;
	// Profiles are named sets that change some of them.
	Params   map[string]string            `json:"params,omitempty"`
	Profiles map[string]map[string]string `json:"profiles,omitempty"`
	// Lines and File hold a list's proxies; File is relative to the spec.
	Lines []string `json:"lines,omitempty"`
	File  string   `json:"file,omitempty"`
	// Entries are static and mobile proxies.
	Entries []SpecEntry `json:"entries,omitempty"`
	// RotateURL and RotateWaitMs apply to mobile entries without their own.
	RotateURL    string `json:"rotateUrl,omitempty"`
	RotateWaitMs int    `json:"rotateWaitMs,omitempty"`
}

// SpecEntry is one static or mobile proxy.
type SpecEntry struct {
	ID        string `json:"id"`
	Line      string `json:"line"`
	Label     string `json:"label,omitempty"`
	RotateURL string `json:"rotateUrl,omitempty"`
}

// Source is a spec read for a run, with its parameters fixed.
type Source struct {
	Kind    string
	Name    string
	Entries []Entry
	// Params are the template's parameters with the profile's applied.
	Params map[string]string
	// Skipped counts a list's lines that aren't proxies.
	Skipped int
}

// countryParams are the parameter names providers give the exit country.
var countryParams = []string{"country", "cc", "countrycode", "country_code", "region"}

// Country is the exit country the source's parameters ask for, lower case,
// or "" when they don't say.
func (s *Source) Country() string {
	for k, v := range s.Params {
		for _, n := range countryParams {
			if strings.EqualFold(k, n) && len(v) == 2 {
				return strings.ToLower(v)
			}
		}
	}
	return ""
}

// Fresh reports whether every browser through the source gets a new exit
// address: a gateway or entries with placeholders, mobile proxies, or a
// list of more than one.
func (s *Source) Fresh() bool {
	if s.Kind == KindMobile || (s.Kind == KindList && len(s.Entries) > 1) {
		return true
	}
	for _, e := range s.Entries {
		if e.Rotating {
			return true
		}
	}
	return false
}

// Managed reports whether a proxies setting's value means browserscale's
// managed proxies: empty, "managed" or "managed:<country>".
func Managed(value string) (country string, ok bool) {
	if value == "" || value == KindManaged {
		return "", true
	}
	cc, ok := strings.CutPrefix(value, KindManaged+":")
	return strings.ToLower(strings.TrimSpace(cc)), ok
}

// ValidCountry reports whether cc is a two-letter country code.
func ValidCountry(cc string) bool {
	if len(cc) != 2 {
		return false
	}
	for _, r := range cc {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// Entry is one proxy a worker can take.
type Entry struct {
	ID    string
	Label string
	// Line may hold placeholders; Render fills them.
	Line string
	// Rotating entries give every browser a new exit address, so a failure
	// says little about the next browser through them.
	Rotating   bool
	RotateURL  string
	RotateWait time.Duration
	// Masque is the MASQUE proxy for the browser's HTTP/3, "" for none.
	Masque string

	params map[string]string
}

// Name is how events and messages refer to e: its id, its label, or its
// address. It never holds credentials.
func (e Entry) Name() string {
	if e.ID != "" {
		return e.ID
	}
	if e.Label != "" {
		return e.Label
	}
	p, err := e.Render()
	if err != nil {
		return "a proxy"
	}
	return p.Addr()
}

// Render fills e's placeholders with fresh values and parses the result.
func (e Entry) Render() (Proxy, error) {
	line, err := Expand(e.Line, e.params, "")
	if err != nil {
		return Proxy{}, err
	}
	p, err := Parse(line)
	if err != nil {
		return Proxy{}, errors.New("not a proxy in a known format")
	}
	if _, ok := p.BrowserScheme(); !ok {
		return Proxy{}, fmt.Errorf("a %s proxy, which browsers can't use; http, socks5 and socks5h work", p.Scheme)
	}
	return p, nil
}

// ValidMasque reports whether s is a MASQUE proxy as "host:port".
func ValidMasque(s string) bool {
	host, port, err := net.SplitHostPort(s)
	n, perr := strconv.Atoi(port)
	return err == nil && host != "" && !strings.ContainsAny(host, "/@ ") && perr == nil && n > 0 && n <= 65535
}

// Rotate asks a mobile proxy for a new exit address and waits for it.
func (e Entry) Rotate(ctx context.Context) error {
	if e.RotateURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.RotateURL, nil)
	if err != nil {
		return errors.New("the rotation URL isn't valid")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("the rotation URL didn't answer: %w", unwrapURL(err))
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the rotation URL answered %d", resp.StatusCode)
	}
	t := time.NewTimer(e.RotateWait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// unwrapURL drops the URL from an http error; rotation URLs carry tokens.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

var placeholder = regexp.MustCompile(`\{([a-zA-Z][a-zA-Z0-9_]*)(?::([^{}]*))?\}`)

// Placeholders lists the names a line's placeholders use, each once:
// "session", "port" and parameter names.
func Placeholders(line string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range placeholder.FindAllStringSubmatch(line, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// Rotates reports whether line gives a new exit address each time it's
// expanded, through {session} or a {port:a-b} range.
func Rotates(line string) bool {
	for _, n := range Placeholders(line) {
		if n == "session" || n == "port" {
			return true
		}
	}
	return false
}

// Expand fills line's placeholders. session is used for {session}; empty
// draws a new one.
func Expand(line string, params map[string]string, session string) (string, error) {
	var bad error
	out := placeholder.ReplaceAllStringFunc(line, func(m string) string {
		sub := placeholder.FindStringSubmatch(m)
		name, arg := sub[1], sub[2]
		switch name {
		case "session":
			n := 10
			if arg != "" {
				v, err := strconv.Atoi(arg)
				if err != nil || v < 1 || v > 64 {
					bad = fmt.Errorf("{session:%s} needs a length from 1 to 64", arg)
					return m
				}
				n = v
			}
			if session != "" {
				if len(session) > n {
					return session[:n]
				}
				return session
			}
			return Token(n)
		case "port":
			lo, hi, ok := portRange(arg)
			if !ok {
				bad = fmt.Errorf("{port:%s} needs a range like {port:10000-10999}", arg)
				return m
			}
			return strconv.Itoa(lo + randInt(hi-lo+1))
		}
		v, ok := params[name]
		if !ok {
			bad = fmt.Errorf("{%s} has no value; set the parameter %s", name, name)
			return m
		}
		return v
	})
	return out, bad
}

func portRange(s string) (int, int, bool) {
	a, b, ok := strings.Cut(s, "-")
	lo, err1 := strconv.Atoi(strings.TrimSpace(a))
	hi, err2 := strconv.Atoi(strings.TrimSpace(b))
	return lo, hi, ok && err1 == nil && err2 == nil && lo > 0 && hi <= 65535 && lo <= hi
}

// Token is a random id of n lower-case letters and digits.
func Token(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[randInt(len(letters))]
	}
	return string(b)
}

func randInt(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

var refID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// IsRef reports whether a file setting's value refers to a saved source.
func IsRef(value string) bool { return strings.HasPrefix(value, RefPrefix) }

// ParseRef splits proxy:<id>[/<profile>].
func ParseRef(value string) (id, profile string, err error) {
	rest, ok := strings.CutPrefix(value, RefPrefix)
	if !ok {
		return "", "", fmt.Errorf("%q isn't a proxy source reference", value)
	}
	id, profile, _ = strings.Cut(rest, "/")
	if !refID.MatchString(id) {
		return "", "", fmt.Errorf("%q isn't a proxy source id", id)
	}
	return id, profile, nil
}

// SourcesDir is where saved sources are, from SourcesEnv.
func SourcesDir() string { return os.Getenv(SourcesEnv) }

// Path is the file a proxies setting reads: a file in dataDir (or an
// absolute path), or for proxy:<id> the saved source's spec. profile is the
// reference's profile.
func Path(dataDir, value string) (path, profile string, err error) {
	if !IsRef(value) {
		if filepath.IsAbs(value) || dataDir == "" {
			return value, "", nil
		}
		return filepath.Join(dataDir, value), "", nil
	}
	id, profile, err := ParseRef(value)
	if err != nil {
		return "", "", err
	}
	dir := SourcesDir()
	if dir == "" {
		return "", "", fmt.Errorf("%s names a saved proxy source, but %s doesn't say where they're kept; run the bot from Studio or set it", value, SourcesEnv)
	}
	return filepath.Join(dir, id+".json"), profile, nil
}

// Open reads the source a proxies setting names: a list file, a spec file
// (.json), or a saved source by reference.
func Open(dataDir, value string) (*Source, error) {
	path, profile, err := Path(dataDir, value)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(filepath.Ext(path), ".json") {
		lines, err := input.NonEmptyLines(path)
		if err != nil {
			return nil, err
		}
		return Spec{Kind: KindList, Lines: lines}.Source(profile, filepath.Dir(path))
	}
	spec, err := ReadSpec(path)
	if err != nil {
		if IsRef(value) && errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("there's no saved proxy source %s", strings.TrimPrefix(value, RefPrefix))
		}
		return nil, err
	}
	return spec.Source(profile, filepath.Dir(path))
}

// ReadSpec reads a spec file.
func ReadSpec(path string) (Spec, error) {
	var s Spec
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s isn't a proxy source: %v", filepath.Base(path), err)
	}
	return s, nil
}

// Source builds the run's entries with profile's parameters. dir is where a
// list's File is.
func (s Spec) Source(profile, dir string) (*Source, error) {
	params := map[string]string{}
	for k, v := range s.Params {
		params[k] = v
	}
	if profile != "" {
		p, ok := s.Profiles[profile]
		if !ok {
			return nil, fmt.Errorf("the proxy source has no profile %q%s", profile, names(s.Profiles))
		}
		for k, v := range p {
			params[k] = v
		}
	}
	src := &Source{Kind: s.Kind, Name: s.Name, Params: params}
	wait := defaultRotateWait
	if s.RotateWaitMs > 0 {
		wait = time.Duration(s.RotateWaitMs) * time.Millisecond
	}
	add := func(e Entry, where string) error {
		e.params = params
		e.Rotating = Rotates(e.Line)
		if _, err := e.Render(); err != nil {
			return fmt.Errorf("%s: %v", where, err)
		}
		src.Entries = append(src.Entries, e)
		return nil
	}
	switch s.Kind {
	case KindList, "":
		src.Kind = KindList
		lines := s.Lines
		if s.File != "" {
			fl, err := input.NonEmptyLines(filepath.Join(dir, filepath.Base(s.File)))
			if err != nil {
				return nil, err
			}
			lines = append(append([]string(nil), lines...), fl...)
		}
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			if add(Entry{Line: l}, "") != nil {
				src.Skipped++
			}
		}
	case KindGateway:
		if strings.TrimSpace(s.Template) == "" {
			return nil, errors.New("the gateway has no template")
		}
		masque := strings.TrimSpace(s.Masque)
		if masque != "" && !ValidMasque(masque) {
			return nil, fmt.Errorf("the MASQUE proxy %q isn't host:port", masque)
		}
		if err := add(Entry{Label: or(s.Name, "gateway"), Line: strings.TrimSpace(s.Template), Masque: masque}, "the template"); err != nil {
			return nil, err
		}
	case KindStatic, KindMobile:
		seen := map[string]bool{}
		for i, se := range s.Entries {
			where := fmt.Sprintf("entry %d", i+1)
			if se.ID != "" {
				where = "entry " + se.ID
			}
			if s.Kind == KindStatic && se.ID == "" {
				return nil, fmt.Errorf("%s has no id", where)
			}
			if se.ID != "" && seen[se.ID] {
				return nil, fmt.Errorf("the id %s is used twice", se.ID)
			}
			seen[se.ID] = true
			e := Entry{ID: se.ID, Label: se.Label, Line: strings.TrimSpace(se.Line)}
			if s.Kind == KindMobile {
				e.RotateURL, e.RotateWait = or(se.RotateURL, s.RotateURL), wait
			}
			if err := add(e, where); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unknown proxy source kind %q", s.Kind)
	}
	if len(src.Entries) == 0 {
		return nil, errors.New("the proxy source has no proxies")
	}
	return src, nil
}

// Entry finds an entry by id.
func (s *Source) Entry(id string) (Entry, bool) {
	for _, e := range s.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

func names(m map[string]map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return " (it has " + strings.Join(out, ", ") + ")"
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Saved is a saved source as a picker shows it.
type Saved struct {
	Ref      string
	Name     string
	Kind     string
	Profiles []string
}

// ListSaved lists the saved sources in SourcesDir.
func ListSaved() []Saved {
	dir := SourcesDir()
	if dir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []Saved
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		if !refID.MatchString(id) {
			continue
		}
		s, err := ReadSpec(f)
		if err != nil {
			continue
		}
		sv := Saved{Ref: RefPrefix + id, Name: or(s.Name, id), Kind: or(s.Kind, KindList)}
		for p := range s.Profiles {
			sv.Profiles = append(sv.Profiles, p)
		}
		sort.Strings(sv.Profiles)
		out = append(out, sv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
