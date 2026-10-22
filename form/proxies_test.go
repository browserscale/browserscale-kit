package form

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/browserscale/browserscale-kit/proxy"
)

func TestCheckProxies(t *testing.T) {
	dir := t.TempDir()
	sources := filepath.Join(dir, "sources")
	t.Setenv(proxy.SourcesEnv, sources)
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(sources, "gw.json"), `{"kind":"gateway","name":"Residential","template":"gw.example:7000:u:p_country-{country}_session-{session}","params":{"country":"us"},"profiles":{"de":{"country":"de"}}}`)
	write(filepath.Join(sources, "acc.json"), `{"kind":"static","entries":[{"id":"ip1","line":"a.example:1:u:p"}]}`)
	write(filepath.Join(dir, "one.txt"), "a.example:1000:u:p\n")

	fresh := &ProxyRule{Kinds: []string{proxy.KindGateway, proxy.KindList, proxy.KindManaged}, Address: AddressFresh, Country: "de"}
	cases := []struct {
		value         string
		rule          *ProxyRule
		problem, note string
	}{
		{"proxy:gw/de", fresh, "", ""},
		{"proxy:gw", fresh, "", ""},
		{"proxy:gw?country=fr", fresh, "", "It asks for FR; the bot is made for DE."},
		{"proxy:gw?zone=x", fresh, "no parameter \"zone\"", ""},
		{"proxy:acc", fresh, "static IPs; this bot takes a gateway, a proxy list or browserscale's managed proxies", ""},
		{"one.txt", fresh, "", "Every browser gets the same address through it"},
		{"managed:de", fresh, "", ""},
		{"managed:fr", fresh, "", "This asks for FR; the bot is made for DE."},
		{"managed:germany", fresh, "isn't a country code", ""},
		{"", &ProxyRule{Kinds: []string{proxy.KindStatic}}, "pick its proxies: this bot takes static IPs", ""},
		{"managed", &ProxyRule{Kinds: []string{proxy.KindStatic}}, "not browserscale's managed proxies", ""},
		{"proxy:gw", &ProxyRule{Address: AddressKeep}, "", "Accounts keep their address only with static IPs"},
		{"proxy:acc", &ProxyRule{Address: AddressKeep}, "", ""},
		{"", nil, "", ""},
		{"proxy:nope", nil, "there's no saved proxy source nope", ""},
	}
	for _, c := range cases {
		problem, note := CheckProxies(dir, c.value, c.rule)
		if !strings.Contains(problem, c.problem) || (c.problem == "") != (problem == "") {
			t.Errorf("%q: problem %q, want %q", c.value, problem, c.problem)
		}
		if !strings.Contains(note, c.note) || (c.note == "") != (note == "") {
			t.Errorf("%q: note %q, want %q", c.value, note, c.note)
		}
	}
}

func TestProxiesField(t *testing.T) {
	var s struct{ Proxies string }
	f := New("m", "M", &s)
	f.Proxies("proxies", &s.Proxies).Kinds(proxy.KindGateway).Fresh().Country("DE").Sticky(15*time.Minute).Param("zone", "res").Required()
	field := f.Build().Field("proxies")
	if field.Kind != KindFile || field.FileTag != TagProxies || field.Proxies.Country != "de" || field.Proxies.Address != AddressFresh {
		t.Fatalf("field %+v, rule %+v", field, field.Proxies)
	}
	if w := field.Proxies.Wants(); w[proxy.Country] != "de" || w[proxy.Sticky] != "15m" || w["zone"] != "res" {
		t.Fatalf("wants %v", w)
	}
	if msg := field.Check(t.TempDir()); msg != "required" {
		t.Errorf("empty required: %q", msg)
	}
	s.Proxies = "managed"
	if msg := field.Check(t.TempDir()); !strings.Contains(msg, "takes a gateway") {
		t.Errorf("managed on a gateway-only field: %q", msg)
	}
}

func TestProxiesParamFrom(t *testing.T) {
	var s struct {
		Own     bool
		Country string
		Minutes int
		Proxies string
	}
	f := New("m", "M", &s)
	f.Bool("own", &s.Own)
	f.String("country", &s.Country).ShowWhenTruthy("own")
	f.Int("minutes", &s.Minutes)
	f.Proxies("proxies", &s.Proxies).Country("us").ParamFrom(proxy.Country, "country").ParamFrom(proxy.Sticky, "minutes")
	rule := f.Build().Field("proxies").Proxies

	s.Country, s.Minutes = "FR", 0
	if w := rule.Wants(); w[proxy.Country] != "us" || w[proxy.Sticky] != "" {
		t.Fatalf("hidden and empty fields: %v", w)
	}
	s.Own, s.Minutes = true, 20
	if w := rule.Wants(); w[proxy.Country] != "fr" || w[proxy.Sticky] != "20m" {
		t.Fatalf("wants %v", w)
	}
	if c := rule.Current(); c.Country != "fr" || c.Sticky != "20m" || c.From[proxy.Country] != "country" || rule.Country != "us" {
		t.Fatalf("current %+v, rule %+v", c, rule)
	}
}
