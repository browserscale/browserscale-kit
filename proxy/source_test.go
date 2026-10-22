package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	line := "gw.example:{port:10000-10002}:user:pw_country-{country}_session-{session:6}"
	out, err := Expand(line, map[string]string{"country": "de"}, "abcdefghij")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, ":user:pw_country-de_session-abcdef") {
		t.Fatalf("out = %q", out)
	}
	p, err := Parse(out)
	if err != nil || p.Port < 10000 || p.Port > 10002 {
		t.Fatalf("parsed %+v, %v", p, err)
	}
	if _, err := Expand("h:1:u:{city}", nil, ""); err == nil || !strings.Contains(err.Error(), "city") {
		t.Fatalf("missing param: %v", err)
	}
	if _, err := Expand("h:{port:9-1}", nil, ""); err == nil {
		t.Fatal("bad range passed")
	}
	if !Rotates(line) || Rotates("h:1:u:{country}") {
		t.Fatal("Rotates")
	}
}

func TestSpecKinds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "res.txt"), []byte("a.example:1\n# note\nnot a proxy\nb.example:2:u:p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := Spec{Kind: KindList, File: "res.txt"}.Source(dir, Ref{}, nil)
	if err != nil || len(list.Entries) != 2 || list.Skipped != 1 {
		t.Fatalf("list %+v, %v", list, err)
	}

	gw := Spec{Kind: KindGateway, Name: "Res", Template: "gw.example:7000:u:p_country-{country}_session-{session}",
		Params: map[string]string{"country": "de"}, Profiles: map[string]map[string]string{"us": {"country": "us"}}}
	src, err := gw.Source("", Ref{Profile: "us"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := src.Entries[0].Render()
	if !strings.HasPrefix(p.Password, "p_country-us_session-") || !src.Entries[0].Rotating || src.Entries[0].Name() != "Res" {
		t.Fatalf("gateway %+v", p)
	}
	if _, err := gw.Source("", Ref{Profile: "fr"}, nil); err == nil || !strings.Contains(err.Error(), "us") {
		t.Fatalf("unknown profile: %v", err)
	}

	st := Spec{Kind: KindStatic, Entries: []SpecEntry{{ID: "a", Line: "a.example:1"}, {ID: "a", Line: "b.example:2"}}}
	if _, err := st.Source("", Ref{}, nil); err == nil {
		t.Fatal("duplicate id passed")
	}
	st.Entries[1].ID = "b"
	src, err = st.Source("", Ref{}, nil)
	if e, ok := src.Entry("b"); err != nil || !ok || e.Name() != "b" {
		t.Fatalf("static %+v, %v", src, err)
	}

	mob := Spec{Kind: KindMobile, RotateURL: "http://rotate.example/x", Entries: []SpecEntry{{Line: "m.example:1:u:p"}}}
	src, err = mob.Source("", Ref{}, nil)
	if err != nil || src.Entries[0].RotateURL == "" || src.Entries[0].RotateWait != defaultRotateWait {
		t.Fatalf("mobile %+v, %v", src, err)
	}
}

func TestSchemesAndMasque(t *testing.T) {
	list, err := Spec{Kind: KindList, Lines: []string{
		"socks5h://u:p@a.example:1080", "socks5://b.example:1080", "http://c.example:8080", "d.example:3128",
		"socks4://e.example:1080", "https://u:p@f.example:443",
	}}.Source("", Ref{}, nil)
	if err != nil || len(list.Entries) != 4 || list.Skipped != 2 {
		t.Fatalf("list %+v, %v", list, err)
	}
	want := []string{"socks5", "socks5", "http", "http"}
	for i, e := range list.Entries {
		p, err := e.Render()
		if s, ok := p.BrowserScheme(); err != nil || !ok || s != want[i] {
			t.Fatalf("entry %d: %q %v %v", i, s, ok, err)
		}
	}
	if _, err := (Entry{Line: "socks4://e.example:1080"}).Render(); err == nil || !strings.Contains(err.Error(), "socks4") {
		t.Fatalf("socks4: %v", err)
	}

	gw := Spec{Kind: KindGateway, Template: "socks5h://u-session-{session}:p@gw.example:7000", Masque: " masque.example:50000 "}
	src, err := gw.Source("", Ref{}, nil)
	if err != nil || src.Entries[0].Masque != "masque.example:50000" {
		t.Fatalf("gateway %+v, %v", src, err)
	}
	for _, bad := range []string{"masque.example", "masque.example:0", "u:p@masque.example:443", "https://masque.example:443"} {
		gw.Masque = bad
		if _, err := gw.Source("", Ref{}, nil); err == nil {
			t.Fatalf("MASQUE %q passed", bad)
		}
	}
}

func TestParamLayers(t *testing.T) {
	gw := Spec{Kind: KindGateway, Template: "gw.example:7000:u:p_cc-{cc}_session-{session}_sesstime-{sesstime}_zone-{zone}",
		Params: map[string]string{"cc": "US", "sesstime": "3", "zone": "res"}}
	password := func(src *Source, e Entry) string {
		t.Helper()
		p, err := e.Render()
		if err != nil {
			t.Fatal(err)
		}
		return p.Password
	}

	src, err := gw.Source("", Ref{}, nil)
	if err != nil || src.ParamName(Country) != "cc" || src.ParamName(Sticky) != "sesstime" || src.Country() != "us" {
		t.Fatalf("roles %+v, %v", src, err)
	}

	wants := map[string]string{Country: "de", Sticky: "15m", "zone": "dc", "city": "berlin"}
	src, err = gw.Source("", Ref{}, wants)
	if got := password(src, src.Entries[0]); err != nil || !strings.Contains(got, "cc-DE_") || !strings.Contains(got, "sesstime-15_zone-dc") {
		t.Fatalf("bot's values: %q, %v", got, err)
	}

	ref, err := ParseRef("proxy:gw?cc=fr&sticky=1h")
	if err != nil || ref.ID != "gw" || ref.String() != "proxy:gw?cc=fr&sticky=1h" {
		t.Fatalf("ref %+v, %v", ref, err)
	}
	src, err = gw.Source("", ref, wants)
	if got := password(src, src.Entries[0]); err != nil || !strings.Contains(got, "cc-fr_") || !strings.Contains(got, "sesstime-60_") {
		t.Fatalf("setting's values: %q, %v", got, err)
	}

	e, err := src.With(src.Entries[0], map[string]string{Country: "nl", Sticky: "90s"})
	if got := password(src, e); err != nil || !strings.Contains(got, "cc-NL_") || !strings.Contains(got, "sesstime-2_") {
		t.Fatalf("job's values: %q, %v", got, err)
	}
	if got := password(src, src.Entries[0]); !strings.Contains(got, "cc-fr_") {
		t.Fatalf("With changed the source: %q", got)
	}
	if _, err := src.With(src.Entries[0], map[string]string{"city": "x"}); err == nil || !strings.Contains(err.Error(), "city") {
		t.Fatalf("unknown name: %v", err)
	}
	if _, err := gw.Source("", Ref{Params: map[string]string{"city": "x"}}, nil); err == nil {
		t.Fatal("unknown name in the setting passed")
	}

	iproyal := Spec{Kind: KindGateway, Template: "gw.example:12321:u:p_country-{country}_session-{session}_lifetime-{lifetime}",
		Params: map[string]string{"country": "de", "lifetime": "3m"}}
	src, err = iproyal.Source("", Ref{}, map[string]string{Sticky: "15m"})
	if got := password(src, src.Entries[0]); err != nil || !strings.HasSuffix(got, "_lifetime-15m") {
		t.Fatalf("lifetime: %q, %v", got, err)
	}

	odd := Spec{Kind: KindGateway, Template: "gw.example:1:u:p-st-{st}-s-{session}", Params: map[string]string{"st": "300"}, Roles: map[string]string{"st": "sticky:s"}}
	src, err = odd.Source("", Ref{}, map[string]string{Sticky: "10m", Country: "de"})
	if got := password(src, src.Entries[0]); err != nil || !strings.HasPrefix(got, "p-st-600-") {
		t.Fatalf("role by spec: %q, %v", got, err)
	}

	list, err := Spec{Kind: KindList, Lines: []string{"a.example:1"}}.Source("", Ref{}, map[string]string{Country: "de"})
	if err != nil || len(list.Params) != 0 {
		t.Fatalf("a list without parameters: %+v, %v", list, err)
	}
}

func TestOpenRef(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(SourcesEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "res.json"), []byte(`{"kind":"gateway","template":"gw.example:7000:u:{session}"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := Open("", "proxy:res")
	if err != nil || src.Kind != KindGateway {
		t.Fatalf("%+v, %v", src, err)
	}
	if _, err := Open("", "proxy:../x"); err == nil {
		t.Fatal("path in a ref passed")
	}
	if _, err := Open("", "proxy:gone"); err == nil || !strings.Contains(err.Error(), "no saved proxy source") {
		t.Fatalf("missing: %v", err)
	}
	if saved := ListSaved(); len(saved) != 1 || saved[0].Ref != "proxy:res" {
		t.Fatalf("saved %+v", saved)
	}
}
