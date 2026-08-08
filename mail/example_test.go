package mail_test

import (
	"context"
	"time"

	"github.com/browserscale/browserscale-kit/mail"
)

// triggerSend stands in for the browser action that makes the site send its
// verification email, e.g. browser.Click(ctx, browserscale.CSS("#send-code")).
func triggerSend() error { return nil }

// ExampleCodeFetcher_FetchCode shows the mailTime idiom: capture the timestamp
// BEFORE the action that triggers the send, then pass it (minus a skew buffer)
// as sinceUnixMilli so only the fresh code matches — never a stale one left in
// a reused / catch-all mailbox.
func ExampleCodeFetcher_FetchCode() {
	f, err := mail.NewCodeFetcher("imap.example.com", 993, "catchall@example.com", "app-password")
	if err != nil {
		panic(err)
	}
	defer f.Stop()

	mailTime := time.Now().UnixMilli() // before the trigger
	if err := triggerSend(); err != nil {
		panic(err)
	}

	code, err := f.FetchCode(
		context.Background(),
		"noreply@site.com",     // fromEmail — the sender to match
		"user+123@example.com", // toEmail — this account's address on the catch-all
		"",                     // subjectKeyword — none
		`(\d{6})`,              // codeRegex — one capture group (6-digit OTP)
		mailTime-60_000,        // sinceUnixMilli — mailTime minus ~60s clock skew
		60_000,                 // maxSearchTimeMs — 60s budget
		true,                   // deleteAfterFetch — expunge once read
	)
	if err != nil {
		panic(err)
	}
	_ = code
}
