// Package mail fetches one-time verification codes from an IMAP mailbox.
//
// It is the kit home for the "wait for the OTP email, pull the code out of it"
// chore that almost every account-registration flow needs. The one type you
// use is [CodeFetcher]: open it once per mailbox at module start, then call
// [CodeFetcher.FetchCode] from your workers — requests are multiplexed over a
// single, kept-alive, auto-reconnecting connection, so hundreds of parallel
// accounts don't each open a socket.
//
// Read the [CodeFetcher] doc for the *mailTime idiom* — capturing a timestamp
// before the action that triggers the send — which is what keeps you from
// matching a stale code on a reused or catch-all mailbox.
//
// The client speaks IMAP over implicit TLS directly with no external
// dependency, and needs only the standard INTERNALDATE capability.
package mail
