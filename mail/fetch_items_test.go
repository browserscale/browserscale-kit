package mail

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"testing"
)

const testEnvelope = `("Mon, 26 Oct 2026 20:30:02 +0000" "Your code" (("Site" NIL "noreply" "site.com")) (("Site" NIL "noreply" "site.com")) (("Site" NIL "noreply" "site.com")) ((NIL NIL "user+123" "example.com")) NIL NIL NIL "<id@site.com>")`

// fakeServer returns a client that reads the given server output and discards
// what it sends.
func fakeServer(output string) *IMAPClient {
	return &IMAPClient{
		reader: bufio.NewReader(strings.NewReader(output)),
		writer: bufio.NewWriter(io.Discard),
	}
}

func TestFetchHeadersAnyItemOrder(t *testing.T) {
	const date = `"26-Oct-2026 20:30:03 +0000"`
	tests := []struct {
		name     string
		response string
	}{
		{"uid first (Dovecot)", "* 1 FETCH (UID 2977 ENVELOPE " + testEnvelope + " INTERNALDATE " + date + ")\r\n"},
		{"uid last (Maddy)", "* 2974 FETCH (ENVELOPE " + testEnvelope + " INTERNALDATE " + date + " UID 2977)\r\n"},
		{"uid between", "* 3 FETCH (INTERNALDATE " + date + " UID 2977 ENVELOPE " + testEnvelope + ")\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fakeServer(tt.response + "A012 OK done\r\n")
			headers, err := c.FetchHeaders([]string{"2977"})
			if err != nil {
				t.Fatal(err)
			}
			if len(headers) != 1 {
				t.Fatalf("got %d headers, want 1", len(headers))
			}
			h := headers[0]
			if h.UID != "2977" || h.Subject != "Your code" || h.From != "noreply@site.com" ||
				h.To != "user+123@example.com" || h.InternalDate != 1793046603000 {
				t.Errorf("got %+v", h)
			}
		})
	}
}

// A subject can carry text that looks like FETCH items or unbalances
// parentheses; neither may leak into UID, INTERNALDATE or the address fields.
func TestFetchHeadersMisleadingSubject(t *testing.T) {
	envelope := strings.Replace(testEnvelope, `"Your code"`, `"UID 4711 (code) :) INTERNALDATE \"x\""`, 1)
	c := fakeServer("* 1 FETCH (ENVELOPE " + envelope + ` INTERNALDATE "26-Oct-2026 20:30:03 +0000" UID 2977)` + "\r\nA012 OK done\r\n")
	headers, err := c.FetchHeaders([]string{"2977"})
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 {
		t.Fatalf("got %d headers, want 1", len(headers))
	}
	h := headers[0]
	if h.UID != "2977" || h.InternalDate != 1793046603000 || h.From != "noreply@site.com" ||
		h.Subject != `UID 4711 (code) :) INTERNALDATE "x"` {
		t.Errorf("got %+v", h)
	}
}

func TestFetchHeadersSkipsUnsolicitedFetch(t *testing.T) {
	c := fakeServer("* 5 FETCH (FLAGS (\\Seen))\r\n" +
		"* 1 FETCH (ENVELOPE " + testEnvelope + ` INTERNALDATE "26-Oct-2026 20:30:03 +0000" UID 2977)` + "\r\n" +
		"A012 OK done\r\n")
	headers, err := c.FetchHeaders([]string{"2977"})
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 || headers[0].UID != "2977" {
		t.Errorf("got %+v", headers)
	}
}

// The body is a literal that sits before the UID in Maddy's order and can
// contain both "UID " and the command's own completion text.
func TestFetchMailUIDAfterBody(t *testing.T) {
	body := "Subject: Your code\r\nContent-Type: text/plain\r\n\r\nUID 1 A013 OK your code is 482913\r\n"
	c := fakeServer(fmt.Sprintf("* 2974 FETCH (ENVELOPE %s INTERNALDATE \"26-Oct-2026 20:30:03 +0000\" BODY[] {%d}\r\n%s UID 2977)\r\nA013 OK done\r\n",
		testEnvelope, len(body), body))
	mail, err := c.FetchMail("2977")
	if err != nil {
		t.Fatal(err)
	}
	if mail.UID != "2977" || mail.From != "noreply@site.com" || !strings.Contains(mail.Body, "482913") {
		t.Errorf("got %+v", mail)
	}
}

func TestFetchItemsLiteralAndSection(t *testing.T) {
	items := fetchItems("* 9 FETCH (BODY[HEADER.FIELDS (SUBJECT)] {5}\r\nhello UID 12 FLAGS (\\Seen \\Answered))\r\n")
	if items["BODY[HEADER.FIELDS (SUBJECT)]"] != "{5}\r\nhello" || items["UID"] != "12" || items["FLAGS"] != `(\Seen \Answered)` {
		t.Errorf("got %q", items)
	}
	if fetchItems("* OK still here\r\n") != nil {
		t.Error("non-FETCH response parsed as FETCH")
	}
}
