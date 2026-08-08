package mail

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Constants for the main loop
const (
	FETCH_INTERVAL = 5 * time.Second  // Check for new fetch requests every 5 seconds
	NOOP_INTERVAL  = 15 * time.Second // Send NOOP command every 15 seconds to keep connection alive
)

// CodeFetcher is a long-lived, concurrency-safe IMAP poller purpose-built for
// pulling one-time verification codes out of a shared mailbox. Create one per
// mailbox at module start (it opens a persistent connection, keeps it alive
// with NOOP, and transparently reconnects), then call [CodeFetcher.FetchCode]
// from every worker — requests are multiplexed over the single connection, so
// hundreds of parallel accounts don't each open a socket.
//
// # The mailTime idiom (read this)
//
// IMAP's SINCE search only has day granularity, so it cannot express "mail that
// arrived in the last minute". FetchCode instead filters on the server's
// INTERNALDATE (millisecond precision) client-side. To match the *fresh* code
// and never a stale one from a previous attempt on the same (often catch-all)
// mailbox, capture a timestamp immediately BEFORE the browser action that
// triggers the send, then pass it as sinceUnixMilli:
//
//	mailTime := time.Now().UnixMilli()
//	browser.Click(ctx, browserscale.CSS("#send-code")) // triggers the email
//	code, err := f.FetchCode(ctx, from, toEmail, "", codeRegex, mailTime-60_000, 60_000, true)
//
// Subtract a skew buffer (~60s) because the mail server's clock and the local
// clock rarely agree to the millisecond; without it a valid mail whose
// INTERNALDATE is a few seconds "before" mailTime is wrongly skipped.
type CodeFetcher struct {
	IMAPClient    *IMAPClient
	fetchRequests chan FetchRequest
	stopChan      chan bool
	isRunning     bool
	mutex         sync.RWMutex
	debug         bool

	// Connection details for reconnection
	host     string
	port     int
	username string
	password string
}

// EnableDebug turns on verbose connection logging (reconnects, NOOP failures,
// non-matching mail bodies). Off by default so the library stays silent.
func (f *CodeFetcher) EnableDebug() {
	f.mutex.Lock()
	f.debug = true
	f.mutex.Unlock()
	if f.IMAPClient != nil {
		f.IMAPClient.EnableDebug()
	}
}

type FetchRequest struct {
	FromEmail        string
	ToEmail          string
	SubjectKeyword   string
	CodeRegex        string
	SinceUnixMilli   int64
	MaxSearchTime    int64 // Timeout in milliseconds for how long to keep looking for the code
	ResultChan       chan FetchResult
	DeleteAfterFetch bool
	deadline         time.Time
}

type FetchResult struct {
	Code string
	Err  error
}

func NewCodeFetcher(host string, port int, username string, password string) (*CodeFetcher, error) {
	fetcher := &CodeFetcher{
		fetchRequests: make(chan FetchRequest, 100),
		stopChan:      make(chan bool, 1),
		isRunning:     false,
		host:          host,
		port:          port,
		username:      username,
		password:      password,
	}

	// Create initial connection (inline like SolarRobot)
	client := NewIMAPClient(host, port, username, password)
	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect: %v", err)
	}
	if err := client.SelectInbox(); err != nil {
		client.Disconnect()
		return nil, fmt.Errorf("failed to select inbox: %v", err)
	}
	fetcher.IMAPClient = client

	// Start the main loop
	go fetcher.mainLoop()
	fetcher.isRunning = true

	return fetcher, nil
}

// mainLoop is the central loop that handles NOOP, waiting, and fetch requests
func (f *CodeFetcher) mainLoop() {
	var active []FetchRequest

	for {
		select {
		case <-f.stopChan:
			return
		default:
			// Check if client is nil or not responsive (like SolarRobot)
			if f.IMAPClient == nil || f.IMAPClient.Noop() != nil {
				if f.IMAPClient != nil {
					if f.debug {
						fmt.Println("[mail] disposing unresponsive client")
					}
					f.IMAPClient.Disconnect()
					f.IMAPClient = nil
				}

				// Create a fresh client and reconnect.
				client := NewIMAPClient(f.host, f.port, f.username, f.password)
				if f.debug {
					client.EnableDebug()
				}

				if err := client.Connect(); err != nil {
					if f.debug {
						fmt.Println("[mail] connect failed:", err)
					}
					time.Sleep(5 * time.Second)
					continue
				}

				if err := client.SelectInbox(); err != nil {
					if f.debug {
						fmt.Println("[mail] select inbox failed:", err)
					}
					client.Disconnect()
					time.Sleep(5 * time.Second)
					continue
				}

				if f.debug {
					fmt.Println("[mail] inbox selected")
				}
				f.IMAPClient = client
			}

			// Drain all available new requests
			drainRequests := true
			for drainRequests {
				select {
				case request := <-f.fetchRequests:
					timeout := time.Duration(request.MaxSearchTime) * time.Millisecond
					if timeout == 0 {
						timeout = 5 * time.Minute
					}
					request.deadline = time.Now().Add(timeout)
					active = append(active, request)
				default:
					drainRequests = false
				}
			}

			// Process all active requests (like SolarRobot processes all SearchOperations)
			now := time.Now()
			writeIdx := 0
			processedAny := false

			for _, req := range active {
				// Check timeout
				if now.After(req.deadline) {
					req.ResultChan <- FetchResult{Code: "", Err: fmt.Errorf("code not found within timeout")}
					continue
				}

				// Try fetch
				code, err := f.fetchCodeInternal(req.FromEmail, req.ToEmail, req.SubjectKeyword, req.CodeRegex, req.SinceUnixMilli, req.DeleteAfterFetch)

				if code != "" {
					req.ResultChan <- FetchResult{Code: code, Err: nil}
					continue
				}

				if err != nil {
					// Dispose client and break loop (like SolarRobot)
					if f.IMAPClient != nil {
						f.IMAPClient.Disconnect()
						f.IMAPClient = nil
					}
					break // Break the loop on error like SolarRobot
				}

				// Keep for next iteration
				active[writeIdx] = req
				writeIdx++
				processedAny = true
			}
			active = active[:writeIdx]

			// Add delay between searches to avoid server-side rate limiting
			if processedAny {
				time.Sleep(5 * time.Second) // Same as SolarRobot
			} else {
				time.Sleep(FETCH_INTERVAL)
			}
		}
	}
}

// fetchCodeInternal is the internal implementation of code fetching
func (f *CodeFetcher) fetchCodeInternal(fromEmail string, toEmail string, subjectKeyword string, codeRegex string, sinceUnixMilli int64, deleteAfterFetch bool) (string, error) {
	// Build search criteria
	criteria := SearchCriteria{
		From: fromEmail,
		// Don't use IMAP date filter, we'll filter by INTERNALDATE after fetching headers
	}

	// Add to email filter if provided
	if toEmail != "" {
		criteria.To = toEmail
	}

	// Add subject filter if provided
	if subjectKeyword != "" {
		criteria.Subject = subjectKeyword
	}

	// Search for emails with retry logic
	var emailUIDs []string
	var err error

	// Single search attempt; reconnect is managed by main loop
	emailUIDs, err = f.IMAPClient.SearchEmails(criteria, 0) // 0 = no limit; avoid missing matches

	if err != nil {
		return "", fmt.Errorf("failed to search emails after retry: %v", err)
	}

	if len(emailUIDs) == 0 {
		return "", nil // No emails found, but that's not an error
	}

	// Fetch headers to sort by date (errors bubble up; reconnect handled by main loop)
	headers, err := f.IMAPClient.FetchHeaders(emailUIDs)
	if err != nil {
		return "", fmt.Errorf("failed to fetch headers: %v", err)
	}

	// Filter by date if sinceUnixMilli is provided
	if sinceUnixMilli > 0 {
		var filteredHeaders []EmailHeaders
		for _, header := range headers {
			if header.InternalDate >= sinceUnixMilli {
				filteredHeaders = append(filteredHeaders, header)
			}
		}
		headers = filteredHeaders

		if len(headers) == 0 {
			return "", nil // No emails found, but that's not an error
		}
	}

	// Sort by INTERNALDATE (newest first)
	sort.Slice(headers, func(i, j int) bool {
		return headers[i].InternalDate > headers[j].InternalDate
	})

	// Compile regex for code extraction
	regex, err := regexp.Compile(codeRegex)
	if err != nil {
		return "", fmt.Errorf("invalid regex pattern: %v", err)
	}

	// Try to find code in the newest emails
	for _, header := range headers {
		// Fetch complete email
		mail, err := f.IMAPClient.FetchMail(header.UID)
		if err != nil {
			continue // Skip this email if fetch fails
		}

		// Search for code in email body
		if matches := regex.FindStringSubmatch(mail.Body); len(matches) > 1 {
			if deleteAfterFetch {
				if err := f.IMAPClient.DeleteByUID(header.UID); err != nil {
					log.Println("DeleteByUID failed:", err)
				}
			}
			return matches[1], nil // Return first capture group
		} else {
			// Mail found but regex didn't match - print body in debug mode
			if f.IMAPClient != nil && f.IMAPClient.IsDebug() {
				fmt.Printf("[DEBUG] Mail found (UID: %s) but regex did not match. Body:\n%s\n", header.UID, mail.Body)
			}
		}
	}

	return "", nil // No code found, but that's not an error
}

// FetchCode polls the mailbox until a matching message arrives, extracts a code
// from its body and returns it — or errors on timeout. It blocks up to
// maxSearchTimeMs (default 5 min when 0) or until ctx is cancelled.
//
// Parameters:
//   - fromEmail:        sender to match (IMAP FROM). "" matches any sender.
//   - toEmail:          recipient to match (IMAP TO) — the account's address on
//     a catch-all mailbox. "" matches any recipient.
//   - subjectKeyword:   optional IMAP SUBJECT filter. "" to skip.
//   - codeRegex:        Go regexp with ONE capture group; the first submatch is
//     returned (e.g. `(\d{6})` for a 6-digit OTP).
//   - sinceUnixMilli:   only consider mail whose INTERNALDATE >= this. Pass the
//     mailTime captured before the trigger, minus a ~60s skew (see the type doc);
//     0 disables the filter (risking a stale code on a reused mailbox).
//   - maxSearchTimeMs:  overall budget for this fetch.
//   - deleteAfterFetch: expunge the matched mail once the code is read (keeps a
//     shared mailbox clean so the next attempt can't re-match it).
func (f *CodeFetcher) FetchCode(ctx context.Context, fromEmail string, toEmail string, subjectKeyword string, codeRegex string, sinceUnixMilli int64, maxSearchTimeMs int64, deleteAfterFetch bool) (string, error) {
	timeout := time.Duration(maxSearchTimeMs) * time.Millisecond
	if timeout == 0 {
		timeout = 5 * time.Minute // Default 5 minutes
	}

	resultChan := make(chan FetchResult, 1)

	request := FetchRequest{
		FromEmail:        fromEmail,
		ToEmail:          toEmail,
		SubjectKeyword:   subjectKeyword,
		CodeRegex:        codeRegex,
		SinceUnixMilli:   sinceUnixMilli,
		MaxSearchTime:    maxSearchTimeMs,
		ResultChan:       resultChan,
		DeleteAfterFetch: deleteAfterFetch,
	}

	// Send exactly one request to the main loop
	select {
	case f.fetchRequests <- request:
		// sent
	case <-time.After(1 * time.Second):
		return "", fmt.Errorf("fetch request queue is full")
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Wait for result until timeout or context cancellation
	select {
	case result := <-resultChan:
		if result.Err != nil {
			return "", result.Err
		}
		return result.Code, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("code not found within %v timeout", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Stop stops the CodeFetcher and disconnects
func (f *CodeFetcher) Stop() {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if f.isRunning {
		f.stopChan <- true
		f.isRunning = false
		if f.IMAPClient != nil {
			f.IMAPClient.Disconnect()
		}
	}
}
