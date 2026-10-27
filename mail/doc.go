// Package mail reads mail over IMAP, above all the one-time codes that
// account flows send by email.
//
// Most bots need only a [Fetcher]: create it once per mailbox at start and
// share it between all workers. Each worker arms a wait before the action
// that makes the site send its mail and collects the code afterwards:
//
//	f, err := mail.NewFetcher(ctx, mail.Config{
//		Host: "imap.example.com", Username: "catchall@example.com", Password: pw,
//	})
//	...
//	wait := f.Expect(mail.Query{
//		From: "noreply@site.com",
//		To:   account.Email,
//		Code: `(\d{6})`,
//	})
//	page.Click(ctx, browserscale.CSS("#send-code"))
//	code, err := wait.Code(ctx)
//
// Arming first is what keeps a stale code out: only mail that arrives after
// Expect counts (less [Config.ClockSkew], the allowance for the mail
// server's clock). When the mail may already be there, [Fetcher.FetchCode]
// takes an explicit Query.Since, or looks back [Config.Window] without one.
//
// The Fetcher keeps one connection alive, reconnects on its own and asks the
// server once per round for new mail, however many workers wait; matching
// sender, recipient, subject and code happens locally. Errors that last until
// a wait times out are reported with it, wrapped in [ErrTimeout].
//
// [Client] is the plain IMAP client underneath, for searching, reading and
// deleting mail directly. It speaks IMAP4rev1 over implicit TLS, quotes or
// sends as literal every string it gets, decodes MIME bodies and encoded
// headers in any common charset to UTF-8, and never marks mail as seen.
package mail
