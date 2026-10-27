package mail_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/browserscale/browserscale-kit/mail"
)

func ExampleFetcher_Expect() {
	ctx := context.Background()
	f, err := mail.NewFetcher(ctx, mail.Config{
		Host:     "imap.example.com",
		Username: "catchall@example.com",
		Password: "app-password",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	// Arm the wait first, then make the site send the mail.
	wait := f.Expect(mail.Query{
		From:    "noreply@site.com",
		To:      "jane.doe@example.com",
		Code:    `(\d{6})`,
		Timeout: 2 * time.Minute,
		Delete:  true,
	})
	// page.Click(ctx, browserscale.CSS("#send-code"))
	code, err := wait.Code(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(code)
}

func ExampleFetcher_FetchCode() {
	ctx := context.Background()
	f, err := mail.NewFetcher(ctx, mail.Config{
		Host:     "imap.example.com",
		Username: "catchall@example.com",
		Password: "app-password",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	// The mail was requested earlier; accept anything since then.
	requestedAt := time.Now().Add(-3 * time.Minute)
	link, err := f.FetchCode(ctx, mail.Query{
		To:    "jane.doe@example.com",
		Code:  `https://site\.com/verify\?token=[\w-]+`,
		Since: requestedAt,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(link)
}

func ExampleClient() {
	ctx := context.Background()
	c, err := mail.Dial(ctx, mail.Config{
		Host:     "imap.example.com",
		Username: "catchall@example.com",
		Password: "app-password",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Select(ctx, "INBOX"); err != nil {
		log.Fatal(err)
	}
	uids, err := c.Search(ctx, mail.Criteria{Subject: "Your order", Limit: 10})
	if err != nil {
		log.Fatal(err)
	}
	headers, err := c.Headers(ctx, uids)
	if err != nil {
		log.Fatal(err)
	}
	for _, h := range headers {
		fmt.Println(h.InternalDate.Format(time.DateTime), h.From, h.Subject)
	}
}
