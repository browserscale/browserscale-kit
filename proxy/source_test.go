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
	list, err := Spec{Kind: KindList, File: "res.txt"}.Source("", dir)
	if err != nil || len(list.Entries) != 2 || list.Skipped != 1 {
		t.Fatalf("list %+v, %v", list, err)
	}

	gw := Spec{Kind: KindGateway, Name: "Res", Template: "gw.example:7000:u:p_country-{country}_session-{session}",
		Params: map[string]string{"country": "de"}, Profiles: map[string]map[string]string{"us": {"country": "us"}}}
	src, err := gw.Source("us", "")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := src.Entries[0].Render()
	if !strings.HasPrefix(p.Password, "p_country-us_session-") || !src.Entries[0].Rotating || src.Entries[0].Name() != "Res" {
		t.Fatalf("gateway %+v", p)
	}
	if _, err := gw.Source("fr", ""); err == nil || !strings.Contains(err.Error(), "us") {
		t.Fatalf("unknown profile: %v", err)
	}

	st := Spec{Kind: KindStatic, Entries: []SpecEntry{{ID: "a", Line: "a.example:1"}, {ID: "a", Line: "b.example:2"}}}
	if _, err := st.Source("", ""); err == nil {
		t.Fatal("duplicate id passed")
	}
	st.Entries[1].ID = "b"
	src, err = st.Source("", "")
	if e, ok := src.Entry("b"); err != nil || !ok || e.Name() != "b" {
		t.Fatalf("static %+v, %v", src, err)
	}

	mob := Spec{Kind: KindMobile, RotateURL: "http://rotate.example/x", Entries: []SpecEntry{{Line: "m.example:1:u:p"}}}
	src, err = mob.Source("", "")
	if err != nil || src.Entries[0].RotateURL == "" || src.Entries[0].RotateWait != defaultRotateWait {
		t.Fatalf("mobile %+v, %v", src, err)
	}
}

func TestSchemesAndMasque(t *testing.T) {
	list, err := Spec{Kind: KindList, Lines: []string{
		"socks5h://u:p@a.example:1080", "socks5://b.example:1080", "http://c.example:8080", "d.example:3128",
		"socks4://e.example:1080", "https://u:p@f.example:443",
	}}.Source("", "")
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
	src, err := gw.Source("", "")
	if err != nil || src.Entries[0].Masque != "masque.example:50000" {
		t.Fatalf("gateway %+v, %v", src, err)
	}
	for _, bad := range []string{"masque.example", "masque.example:0", "u:p@masque.example:443", "https://masque.example:443"} {
		gw.Masque = bad
		if _, err := gw.Source("", ""); err == nil {
			t.Fatalf("MASQUE %q passed", bad)
		}
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
