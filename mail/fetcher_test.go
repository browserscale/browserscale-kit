package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const codePattern = `Code: (\d{6})`

func newFakeFetcher(t *testing.T, s *fakeServer) *Fetcher {
	t.Helper()
	f, err := NewFetcher(context.Background(), s.config())
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func codeMail(to, code string) string {
	return mailText("Shop <noreply@shop.com>", to, "Dein Code", "Hallo,\r\nCode: "+code+"\r\nGruss")
}

func TestExpectReceivesMailArrivingLater(t *testing.T) {
	s := newFakeServer(t)
	f := newFakeFetcher(t, s)
	w := f.Expect(Query{From: "noreply@shop.com", To: "me@x.com", Code: codePattern, Timeout: 5 * time.Second})
	time.AfterFunc(100*time.Millisecond, func() {
		s.add(time.Now(), codeMail("other@x.com", "999999"))
		s.add(time.Now(), codeMail("me@x.com", "123456"))
	})
	res, err := w.Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != "123456" || res.Message.Subject != "Dein Code" {
		t.Fatalf("got code %q, subject %q", res.Code, res.Message.Subject)
	}
}

func TestExpectIgnoresStaleMail(t *testing.T) {
	s := newFakeServer(t)
	s.add(time.Now().Add(-5*time.Minute), codeMail("me@x.com", "111111"))
	f := newFakeFetcher(t, s)
	w := f.Expect(Query{To: "me@x.com", Code: codePattern, Timeout: 5 * time.Second})
	time.AfterFunc(100*time.Millisecond, func() { s.add(time.Now(), codeMail("me@x.com", "222222")) })
	if code, err := w.Code(context.Background()); err != nil || code != "222222" {
		t.Fatalf("got %q, %v; want the fresh code", code, err)
	}
}

func TestExpectAllowsServerClockBehind(t *testing.T) {
	s := newFakeServer(t)
	f := newFakeFetcher(t, s)
	w := f.Expect(Query{To: "me@x.com", Code: codePattern, Timeout: 5 * time.Second})
	s.add(time.Now().Add(-30*time.Second), codeMail("me@x.com", "333333"))
	if code, err := w.Code(context.Background()); err != nil || code != "333333" {
		t.Fatalf("got %q, %v", code, err)
	}
}

func TestFetchCodeLooksBackOverWindow(t *testing.T) {
	s := newFakeServer(t)
	now := time.Now()
	s.add(now.Add(-48*time.Hour), codeMail("a@x.com", "111111"))
	s.add(now.Add(-96*time.Hour), codeMail("b@x.com", "222222"))
	f := newFakeFetcher(t, s)
	ctx := context.Background()

	if code, err := f.FetchCode(ctx, Query{To: "a@x.com", Code: codePattern, Timeout: 5 * time.Second}); err != nil || code != "111111" {
		t.Fatalf("within the window: got %q, %v", code, err)
	}
	_, err := f.FetchCode(ctx, Query{To: "b@x.com", Code: codePattern, Timeout: 300 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("outside the window: got %v, want ErrTimeout", err)
	}
	if code, err := f.FetchCode(ctx, Query{To: "b@x.com", Code: codePattern, AllMail: true, Timeout: 5 * time.Second}); err != nil || code != "222222" {
		t.Fatalf("AllMail: got %q, %v", code, err)
	}
}

// SINCE compares dates in the server's time zone. A mail from just after
// the requested time can fall on the day before there and must not be lost.
func TestFetchCodeAcrossServerTimeZone(t *testing.T) {
	s := newFakeServer(t)
	s.tz = time.FixedZone("HST", -10*3600)
	since := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	s.add(since.Add(-2*time.Hour), codeMail("me@x.com", "111111"))
	s.add(since.Add(10*time.Second), codeMail("me@x.com", "222222"))
	f := newFakeFetcher(t, s)
	code, err := f.FetchCode(context.Background(), Query{To: "me@x.com", Code: codePattern, Since: since, Timeout: 5 * time.Second})
	if err != nil || code != "222222" {
		t.Fatalf("got %q, %v", code, err)
	}
}

func TestManyWaitersShareOnePollPerRound(t *testing.T) {
	s := newFakeServer(t)
	cfg := s.config()
	cfg.PollInterval = 50 * time.Millisecond
	f, err := NewFetcher(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const n = 50
	waits := make([]*Wait, n)
	for i := range waits {
		waits[i] = f.Expect(Query{To: fmt.Sprintf("user%d@x.com", i), Code: codePattern, Timeout: 10 * time.Second})
	}
	time.Sleep(100 * time.Millisecond)
	before := s.count("UID SEARCH")
	time.Sleep(500 * time.Millisecond)
	if polls := s.count("UID SEARCH") - before; polls > 15 {
		t.Fatalf("%d searches in 500ms with %d waiters", polls, n)
	}

	for i := range n {
		s.add(time.Now(), codeMail(fmt.Sprintf("user%d@x.com", i), fmt.Sprintf("%06d", i)))
	}
	var wg sync.WaitGroup
	for i, w := range waits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, err := w.Code(context.Background())
			if want := fmt.Sprintf("%06d", i); err != nil || code != want {
				t.Errorf("waiter %d: got %q, %v; want %s", i, code, err, want)
			}
		}()
	}
	wg.Wait()
}

func TestWaitSurvivesReconnect(t *testing.T) {
	s := newFakeServer(t)
	f := newFakeFetcher(t, s)
	w := f.Expect(Query{To: "me@x.com", Code: codePattern, Timeout: 5 * time.Second})
	time.Sleep(50 * time.Millisecond)
	s.dropConnections()
	s.add(time.Now(), codeMail("me@x.com", "444444"))
	if code, err := w.Code(context.Background()); err != nil || code != "444444" {
		t.Fatalf("got %q, %v", code, err)
	}
}

func TestTimeoutReportsConnectionError(t *testing.T) {
	s := newFakeServer(t)
	f := newFakeFetcher(t, s)
	s.mu.Lock()
	s.failSelect = true
	s.mu.Unlock()
	s.dropConnections()
	_, err := f.FetchCode(context.Background(), Query{To: "me@x.com", Timeout: 500 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "SELECT") {
		t.Fatalf("got %v, want ErrTimeout naming the failed SELECT", err)
	}
}

func TestInvalidCodePatternFailsAtOnce(t *testing.T) {
	f := newFakeFetcher(t, newFakeServer(t))
	start := time.Now()
	_, err := f.Expect(Query{Code: "(", Timeout: 5 * time.Second}).Code(context.Background())
	if err == nil || errors.Is(err, ErrTimeout) || time.Since(start) > time.Second {
		t.Fatalf("got %v after %v", err, time.Since(start))
	}
}

func TestCodeWithoutGroupAndDelete(t *testing.T) {
	s := newFakeServer(t)
	f := newFakeFetcher(t, s)
	uid := s.add(time.Now(), codeMail("me@x.com", "555555"))
	code, err := f.FetchCode(context.Background(), Query{To: "me@x.com", Code: `\d{6}`, Delete: true, Timeout: 5 * time.Second})
	if err != nil || code != "555555" {
		t.Fatalf("got %q, %v", code, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.has(uid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.has(uid) {
		t.Fatal("matched mail not deleted")
	}
}

func TestMatchesDecodedHeaders(t *testing.T) {
	s := newFakeServer(t)
	f := newFakeFetcher(t, s)
	s.add(time.Now(), "From: PayPal <service.intl@paypal.com>\r\nTo: me@x.com\r\n"+
		"Subject: =?windows-1252?Q?Ihr_Pr=FCfcode?=\r\n\r\nCode: 666666\r\n")
	code, err := f.FetchCode(context.Background(), Query{From: "paypal", Subject: "PRÜFCODE", Code: codePattern, Timeout: 5 * time.Second})
	if err != nil || code != "666666" {
		t.Fatalf("got %q, %v", code, err)
	}
}

func TestMailWithoutCodeIsPassedOver(t *testing.T) {
	s := newFakeServer(t)
	f := newFakeFetcher(t, s)
	w := f.Expect(Query{To: "me@x.com", Code: codePattern, Timeout: 5 * time.Second})
	s.add(time.Now(), codeMail("me@x.com", "777777"))
	s.add(time.Now(), mailText("noreply@shop.com", "me@x.com", "Willkommen", "Schön, dass du da bist."))
	if code, err := w.Code(context.Background()); err != nil || code != "777777" {
		t.Fatalf("got %q, %v", code, err)
	}
}

func TestEmptyCodeTakesFirstMatchingMail(t *testing.T) {
	s := newFakeServer(t)
	f := newFakeFetcher(t, s)
	w := f.Expect(Query{Subject: "Willkommen", Timeout: 5 * time.Second})
	s.add(time.Now(), mailText("noreply@shop.com", "me@x.com", "Willkommen", "Link: https://shop.com/verify?t=abc"))
	res, err := w.Result(context.Background())
	if err != nil || res.Code != "" || !strings.Contains(res.Message.Body, "verify?t=abc") {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestCloseEndsWaits(t *testing.T) {
	s := newFakeServer(t)
	f, err := NewFetcher(context.Background(), s.config())
	if err != nil {
		t.Fatal(err)
	}
	w := f.Expect(Query{To: "me@x.com", Timeout: 5 * time.Second})
	time.AfterFunc(50*time.Millisecond, func() { _ = f.Close() })
	if _, err := w.Code(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
	if _, err := f.Expect(Query{}).Code(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: got %v, want ErrClosed", err)
	}
}

func TestNewFetcherFailsFast(t *testing.T) {
	s := newFakeServer(t)
	s.refuseLogin = true
	if _, err := NewFetcher(context.Background(), s.config()); err == nil {
		t.Fatal("NewFetcher accepted a refused login")
	}
}
