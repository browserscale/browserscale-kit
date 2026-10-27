package mail

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func dialFake(t *testing.T, s *fakeServer) *Client {
	t.Helper()
	ctx := context.Background()
	c, err := Dial(ctx, s.config())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Select(ctx, "INBOX"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	return c
}

func TestLoginSendsAnyPasswordIntact(t *testing.T) {
	for _, caps := range []string{"IMAP4rev1", "IMAP4rev1 LITERAL+"} {
		for _, pw := range []string{`pa"ss\word`, "pässwört", "with space", `{5}`} {
			s := newFakeServer(t)
			s.caps = caps
			s.username = `odd"user`
			s.password = pw
			c, err := Dial(context.Background(), s.config())
			if err != nil {
				t.Fatalf("%s, password %q: %v", caps, pw, err)
			}
			_ = c.Close()
		}
	}
}

func TestLoginFailureIsACommandError(t *testing.T) {
	s := newFakeServer(t)
	s.refuseLogin = true
	_, err := Dial(context.Background(), s.config())
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Status != "NO" || ce.Code != "AUTHENTICATIONFAILED" {
		t.Fatalf("got %v, want a NO [AUTHENTICATIONFAILED] CommandError", err)
	}
}

func TestSearchKeepsStringsWhole(t *testing.T) {
	s := newFakeServer(t)
	now := time.Now()
	want := s.add(now, mailText("a@x.com", "me@x.com", "Dein Code NEW", "hi"))
	s.add(now, mailText("a@x.com", "me@x.com", "Dein alter Code", "hi"))
	umlaut := s.add(now, mailText("a@x.com", "me@x.com", "Prüfcode", "hi"))
	c := dialFake(t, s)
	ctx := context.Background()

	got, err := c.Search(ctx, Criteria{Subject: "Dein Code NEW"})
	if err != nil || !slices.Equal(got, []uint32{want}) {
		t.Fatalf("subject with spaces: got %v, %v; want [%d]", got, err, want)
	}
	got, err = c.Search(ctx, Criteria{Subject: `Code "NEW`})
	if err != nil || len(got) != 0 {
		t.Fatalf("subject with a quote: got %v, %v; want none", got, err)
	}
	got, err = c.Search(ctx, Criteria{Subject: "prüf"})
	if err != nil || !slices.Equal(got, []uint32{umlaut}) {
		t.Fatalf("non-ASCII subject: got %v, %v; want [%d]", got, err, umlaut)
	}
	got, err = c.Search(ctx, Criteria{Limit: 2})
	if err != nil || !slices.Equal(got, []uint32{2, 3}) {
		t.Fatalf("limit: got %v, %v; want the newest two", got, err)
	}
}

func TestHeadersReadsWhatServersSend(t *testing.T) {
	s := newFakeServer(t)
	s.uidLast = true
	s.padDay = true
	s.tz = time.FixedZone("", -7*3600)
	arrived := time.Date(2026, 10, 7, 9, 5, 1, 0, time.UTC)
	uid := s.add(arrived, "From: PayPal <service.intl@paypal.com>\r\n"+
		"To: \"Doe, Jane\" <jane@x.com>, bob@x.com\r\n"+
		"Subject: =?windows-1252?Q?Ihr_Pr=FCfcode_=28neu=29?=\r\n"+
		"Message-ID: <abc@paypal.com>\r\n\r\nbody\r\n")
	c := dialFake(t, s)

	hs, err := c.Headers(context.Background(), []uint32{uid})
	if err != nil || len(hs) != 1 {
		t.Fatalf("Headers: %v, %v", hs, err)
	}
	h := hs[0]
	if !h.InternalDate.Equal(arrived) {
		t.Errorf("InternalDate = %v, want %v", h.InternalDate, arrived)
	}
	if h.Subject != "Ihr Prüfcode (neu)" {
		t.Errorf("Subject = %q", h.Subject)
	}
	if len(h.From) != 1 || h.From[0].Email() != "service.intl@paypal.com" || h.From[0].Name != "PayPal" {
		t.Errorf("From = %+v", h.From)
	}
	if len(h.To) != 2 || h.To[0].Name != "Doe, Jane" || h.To[1].Email() != "bob@x.com" {
		t.Errorf("To = %+v", h.To)
	}
	if h.MessageID != "<abc@paypal.com>" {
		t.Errorf("MessageID = %q", h.MessageID)
	}
}

func TestHeadersSplitsLongUIDLists(t *testing.T) {
	s := newFakeServer(t)
	s.maxLine = 8192
	now := time.Now()
	var want []uint32
	for i := range 6000 {
		uid := s.add(now, mailText("a@x.com", "me@x.com", "s", "b"))
		if i%2 == 0 {
			want = append(want, uid)
		}
	}
	c := dialFake(t, s)
	hs, err := c.Headers(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != len(want) {
		t.Fatalf("got %d headers, want %d", len(hs), len(want))
	}
}

func TestFetchDecodesMIME(t *testing.T) {
	s := newFakeServer(t)
	html := base64.StdEncoding.EncodeToString([]byte("<p>Code <b>482913</b> für dich</p>"))
	raw := "From: Shop <noreply@shop.com>\r\nTo: me@x.com\r\nSubject: Code\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=\"inner\"\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Dein Code f=FCr heute: 482913. Lange Zeile, umbrochen mit Soft-Break am En=\r\nde.\r\n" +
		"--inner\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		html[:20] + "\r\n" + html[20:] + "\r\n" +
		"--inner--\r\n" +
		"--outer\r\nContent-Type: message/rfc822\r\n\r\n" +
		"From: x@y.z\r\nSubject: fwd\r\nContent-Type: text/plain\r\n\r\nWeitergeleitet: 777\r\n" +
		"--outer\r\nContent-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0=\r\n" +
		"--outer--\r\n"
	uid := s.add(time.Now(), raw)
	c := dialFake(t, s)

	m, err := c.Fetch(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Text, "Dein Code für heute: 482913") || !strings.Contains(m.Text, "am Ende.") {
		t.Errorf("Text = %q", m.Text)
	}
	if !strings.Contains(m.Text, "Weitergeleitet: 777") {
		t.Errorf("Text misses the forwarded mail: %q", m.Text)
	}
	if !strings.Contains(m.HTML, "<b>482913</b> für dich") {
		t.Errorf("HTML = %q", m.HTML)
	}
	if !strings.Contains(m.Body, "für heute") || !strings.Contains(m.Body, "<b>482913</b>") {
		t.Errorf("Body = %q", m.Body)
	}
	if strings.Contains(m.Body, "JVBERi0") {
		t.Errorf("Body holds the attachment: %q", m.Body)
	}
	if string(m.Raw) != raw {
		t.Errorf("Raw differs from the stored message")
	}
	if _, err := c.Fetch(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing UID: got %v, want ErrNotFound", err)
	}
}

func TestFetchSurvivesProtocolLookalikesInBody(t *testing.T) {
	s := newFakeServer(t)
	body := "a5 OK fake done\r\n* 3 EXPUNGE\r\n)\r\n{12}\r\nCode: 123456"
	uid := s.add(time.Now(), mailText("a@x.com", "me@x.com", "s", body))
	c := dialFake(t, s)
	m, err := c.Fetch(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Body, "Code: 123456") {
		t.Errorf("Body = %q", m.Body)
	}
	if err := c.Noop(context.Background()); err != nil {
		t.Errorf("connection out of step after the fetch: %v", err)
	}
}

func TestDelete(t *testing.T) {
	for _, caps := range []string{"IMAP4rev1 UIDPLUS", "IMAP4rev1"} {
		s := newFakeServer(t)
		s.caps = caps
		now := time.Now()
		a := s.add(now, mailText("a@x.com", "me@x.com", "a", "a"))
		b := s.add(now, mailText("a@x.com", "me@x.com", "b", "b"))
		c := dialFake(t, s)
		if err := c.Delete(context.Background(), a); err != nil {
			t.Fatalf("%s: %v", caps, err)
		}
		if s.has(a) || !s.has(b) {
			t.Errorf("%s: mail a kept %v, mail b kept %v", caps, s.has(a), s.has(b))
		}
		if strings.Contains(caps, "UIDPLUS") != (s.count("UID EXPUNGE") == 1) {
			t.Errorf("%s: UID EXPUNGE sent %d times", caps, s.count("UID EXPUNGE"))
		}
	}
}

func TestSelectFailureIsAnError(t *testing.T) {
	s := newFakeServer(t)
	s.failSelect = true
	c, err := Dial(context.Background(), s.config())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Select(context.Background(), "INBOX")
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Status != "NO" {
		t.Fatalf("got %v, want a NO CommandError", err)
	}
}

func TestList(t *testing.T) {
	c := dialFake(t, newFakeServer(t))
	names, err := c.List(context.Background())
	if err != nil || !slices.Equal(names, []string{"INBOX", "Junk Mail"}) {
		t.Fatalf("got %v, %v", names, err)
	}
}

func TestSilentServerTimesOut(t *testing.T) {
	s := newFakeServer(t)
	s.hangOn = "NOOP"
	cfg := s.config()
	cfg.Timeout = 200 * time.Millisecond
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if err := c.Noop(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	if err := c.Noop(context.Background()); err == nil {
		t.Fatal("a broken connection accepted another command")
	}
}

func TestContextCancelsCommand(t *testing.T) {
	s := newFakeServer(t)
	s.hangOn = "NOOP"
	c, err := Dial(context.Background(), s.config())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Noop(ctx); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestUIDSets(t *testing.T) {
	got := uidSets([]uint32{9, 1, 2, 3, 0, 5, 3, 10, 11})
	if !slices.Equal(got, []string{"1:3,5,9:11"}) {
		t.Fatalf("got %v", got)
	}
	var many []uint32
	for i := uint32(1); i < 20000; i += 2 {
		many = append(many, i)
	}
	for _, set := range uidSets(many) {
		if len(set) > maxSetLen {
			t.Fatalf("set of %d bytes", len(set))
		}
	}
}
