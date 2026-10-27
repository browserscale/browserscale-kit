package mail

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
)

// Address is one mailbox of an address header.
type Address struct {
	Name    string // display name, decoded
	Mailbox string // local part
	Host    string
}

// Email is mailbox@host.
func (a Address) Email() string {
	if a.Host == "" {
		return a.Mailbox
	}
	return a.Mailbox + "@" + a.Host
}

// String is "Name <mailbox@host>", or just the address without a name.
func (a Address) String() string {
	if a.Name == "" {
		return a.Email()
	}
	return a.Name + " <" + a.Email() + ">"
}

// Header is what the server tells about a message without its content.
type Header struct {
	UID uint32
	// InternalDate is when the server received the message, by its clock.
	InternalDate time.Time
	Date         string // the Date header as sent
	Subject      string // decoded
	From         []Address
	Sender       []Address
	ReplyTo      []Address
	To           []Address
	Cc           []Address
	Bcc          []Address
	InReplyTo    string
	MessageID    string
}

// Message is a complete message.
type Message struct {
	Header
	Text string // the text/plain parts, as UTF-8
	HTML string // the text/html parts, as UTF-8
	// Body is every text part in message order, as UTF-8, or the raw message
	// when it has no readable text part. Code patterns run over it.
	Body string
	Raw  []byte // the message as stored on the server
}

func headerFromItems(items map[string]value) (Header, bool) {
	uid, ok := items["UID"].number()
	if !ok || uid == 0 {
		return Header{}, false
	}
	h := Header{UID: uid}
	if v, ok := items["INTERNALDATE"]; ok {
		h.InternalDate, _ = parseInternalDate(v.nstring())
	}
	if v, ok := items["ENVELOPE"]; ok && v.kind == listValue {
		fillEnvelope(&h, v.list)
	}
	return h, true
}

// fillEnvelope reads the ENVELOPE fields in their fixed order: date,
// subject, from, sender, reply-to, to, cc, bcc, in-reply-to, message-id.
func fillEnvelope(h *Header, f []value) {
	at := func(i int) value {
		if i < len(f) {
			return f[i]
		}
		return value{kind: atomValue, text: "NIL"}
	}
	h.Date = at(0).nstring()
	h.Subject = decodeHeader(at(1).nstring())
	h.From = addresses(at(2))
	h.Sender = addresses(at(3))
	h.ReplyTo = addresses(at(4))
	h.To = addresses(at(5))
	h.Cc = addresses(at(6))
	h.Bcc = addresses(at(7))
	h.InReplyTo = at(8).nstring()
	h.MessageID = at(9).nstring()
}

// addresses reads an address list: (name adl mailbox host) per address.
// Group markers, which have no host, are skipped.
func addresses(v value) []Address {
	if v.kind != listValue {
		return nil
	}
	var out []Address
	for _, a := range v.list {
		if a.kind != listValue || len(a.list) < 4 || a.list[3].isNil() {
			continue
		}
		out = append(out, Address{
			Name:    decodeHeader(a.list[0].nstring()),
			Mailbox: a.list[2].nstring(),
			Host:    a.list[3].nstring(),
		})
	}
	return out
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

// decodeHeader decodes RFC 2047 encoded words in any charset the WHATWG
// encoding list knows, and leaves the text as it is otherwise.
func decodeHeader(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	d, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		return s
	}
	return d
}

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return nil, err
	}
	return enc.NewDecoder().Reader(input), nil
}

// toUTF8 converts text in the named charset; unknown charsets and
// undecodable text stay as they are.
func toUTF8(data []byte, charset string) string {
	cs := strings.ToLower(strings.TrimSpace(charset))
	if cs == "" || cs == "utf-8" || cs == "utf8" || cs == "us-ascii" {
		return string(data)
	}
	enc, err := htmlindex.Get(cs)
	if err != nil {
		return string(data)
	}
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return string(data)
	}
	return string(out)
}

type textParts struct {
	plain, html, all []string
}

// readBody pulls the text out of a raw message: every text part, transfer
// encoding undone and converted to UTF-8, through nested multiparts and
// attached messages.
func readBody(raw []byte) (text, html, body string) {
	var parts textParts
	if msg, err := netmail.ReadMessage(bytes.NewReader(raw)); err == nil {
		parts.walk(msg.Header, msg.Body, 0)
	}
	if len(parts.all) == 0 {
		return "", "", string(raw)
	}
	return strings.Join(parts.plain, "\n"), strings.Join(parts.html, "\n"), strings.Join(parts.all, "\n")
}

type headerGetter interface{ Get(string) string }

func (p *textParts) walk(h headerGetter, body io.Reader, depth int) {
	if depth > 20 {
		return
	}
	mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType, params = "text/plain", nil
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "":
		// A broken part ends the walk of this multipart, keeping what came
		// before it.
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			p.walk(part.Header, part, depth+1)
		}
	case mediaType == "message/rfc822" || mediaType == "message/global":
		if msg, err := netmail.ReadMessage(body); err == nil {
			p.walk(msg.Header, msg.Body, depth+1)
		}
	case strings.HasPrefix(mediaType, "text/"):
		data, err := io.ReadAll(body)
		if err != nil && len(data) == 0 {
			return
		}
		text := toUTF8(decodeTransfer(data, h.Get("Content-Transfer-Encoding")), params["charset"])
		if text == "" {
			return
		}
		p.all = append(p.all, text)
		switch mediaType {
		case "text/plain":
			p.plain = append(p.plain, text)
		case "text/html":
			p.html = append(p.html, text)
		}
	}
}

// decodeTransfer undoes a Content-Transfer-Encoding, keeping as much as it
// can of damaged input.
func decodeTransfer(data []byte, encoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		clean := make([]byte, 0, len(data))
		for _, c := range data {
			if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
				clean = append(clean, c)
			}
		}
		if out, err := base64.StdEncoding.DecodeString(string(clean)); err == nil {
			return out
		}
		if out, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(string(clean), "=")); err == nil {
			return out
		}
		return data
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
		if err != nil && len(out) == 0 {
			return data
		}
		return out
	}
	return data
}
