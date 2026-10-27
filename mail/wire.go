package mail

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

type valueKind uint8

const (
	atomValue valueKind = iota
	stringValue
	listValue
)

// value is one item of server data: an atom (a number, NIL, a flag or a
// keyword), a string, quoted or literal, or a parenthesized list.
type value struct {
	kind valueKind
	text string
	list []value
}

func (v value) isNil() bool { return v.kind == atomValue && strings.EqualFold(v.text, "NIL") }

// nstring is the text of a string or atom; NIL and lists are "".
func (v value) nstring() string {
	if v.kind == listValue || v.isNil() {
		return ""
	}
	return v.text
}

func (v value) number() (uint32, bool) {
	if v.kind != atomValue {
		return 0, false
	}
	n, err := strconv.ParseUint(v.text, 10, 32)
	return uint32(n), err == nil
}

var errShort = errors.New("response ends early")

// scanner reads server data out of one response, whose literals the reader
// has already inlined after their {n} CRLF announcement.
type scanner struct {
	s string
	i int
}

func (sc *scanner) skipSpaces() {
	for sc.i < len(sc.s) && sc.s[sc.i] == ' ' {
		sc.i++
	}
}

func (sc *scanner) atEnd() bool {
	sc.skipSpaces()
	return sc.i >= len(sc.s) || sc.s[sc.i] == '\r' || sc.s[sc.i] == '\n'
}

// word reads up to the next space or the end of the line: tags, status
// words, message numbers.
func (sc *scanner) word() string {
	sc.skipSpaces()
	start := sc.i
	for sc.i < len(sc.s) && sc.s[sc.i] != ' ' && sc.s[sc.i] != '\r' && sc.s[sc.i] != '\n' {
		sc.i++
	}
	return sc.s[start:sc.i]
}

func (sc *scanner) rest() string {
	sc.skipSpaces()
	r := sc.s[sc.i:]
	sc.i = len(sc.s)
	return strings.TrimRight(r, "\r\n")
}

// itemName reads a FETCH item name such as UID or
// BODY[HEADER.FIELDS (SUBJECT)]<0>, whose section may hold spaces and
// parentheses.
func (sc *scanner) itemName() string {
	sc.skipSpaces()
	start, depth := sc.i, 0
	for ; sc.i < len(sc.s); sc.i++ {
		c := sc.s[sc.i]
		switch {
		case c == '[':
			depth++
		case c == ']':
			if depth > 0 {
				depth--
			}
		case depth > 0:
		case c == ' ' || c == '(' || c == ')' || c == '\r' || c == '\n':
			return sc.s[start:sc.i]
		}
	}
	return sc.s[start:]
}

func (sc *scanner) value() (value, error) {
	sc.skipSpaces()
	if sc.i >= len(sc.s) {
		return value{}, errShort
	}
	switch sc.s[sc.i] {
	case '(':
		sc.i++
		list := []value{}
		for {
			sc.skipSpaces()
			if sc.i >= len(sc.s) {
				return value{}, errShort
			}
			if sc.s[sc.i] == ')' {
				sc.i++
				return value{kind: listValue, list: list}, nil
			}
			v, err := sc.value()
			if err != nil {
				return value{}, err
			}
			list = append(list, v)
		}
	case '"':
		return sc.quoted()
	case '{':
		return sc.literal()
	}
	start, depth := sc.i, 0
	for ; sc.i < len(sc.s); sc.i++ {
		c := sc.s[sc.i]
		if c == '[' {
			depth++
		} else if c == ']' && depth > 0 {
			depth--
		} else if depth == 0 && (c == ' ' || c == '(' || c == ')' || c == '\r' || c == '\n') {
			break
		}
	}
	if sc.i == start {
		return value{}, fmt.Errorf("unexpected %q in response", sc.s[sc.i])
	}
	return value{kind: atomValue, text: sc.s[start:sc.i]}, nil
}

func (sc *scanner) quoted() (value, error) {
	var b strings.Builder
	for sc.i++; sc.i < len(sc.s); sc.i++ {
		switch c := sc.s[sc.i]; c {
		case '\\':
			sc.i++
			if sc.i < len(sc.s) {
				b.WriteByte(sc.s[sc.i])
			}
		case '"':
			sc.i++
			return value{kind: stringValue, text: b.String()}, nil
		case '\r', '\n':
			return value{}, errors.New("unterminated quoted string")
		default:
			b.WriteByte(c)
		}
	}
	return value{}, errShort
}

func (sc *scanner) literal() (value, error) {
	end := strings.IndexByte(sc.s[sc.i:], '}')
	if end < 0 {
		return value{}, errShort
	}
	n, err := strconv.Atoi(strings.TrimSuffix(sc.s[sc.i+1:sc.i+end], "+"))
	if err != nil || n < 0 {
		return value{}, fmt.Errorf("bad literal %q", sc.s[sc.i:sc.i+end+1])
	}
	j := sc.i + end + 1
	switch {
	case strings.HasPrefix(sc.s[j:], "\r\n"):
		j += 2
	case strings.HasPrefix(sc.s[j:], "\n"):
		j++
	default:
		return value{}, errors.New("literal without line break")
	}
	if j+n > len(sc.s) {
		return value{}, errShort
	}
	sc.i = j + n
	return value{kind: stringValue, text: sc.s[j : j+n]}, nil
}

// trailingLiteral reports the size of the literal a response line announces
// at its end.
func trailingLiteral(line string) (int, bool) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	start := strings.LastIndexByte(line, '{')
	if start < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(line[start+1:len(line)-1], "+"))
	return n, err == nil && n >= 0
}

// response is one complete server response.
type response struct {
	tag    string // "*", "+" or the command's tag
	status string // OK, NO, BAD, BYE or PREAUTH for status responses
	code   string // response code without brackets, e.g. "UIDNEXT 42"
	text   string
	number uint32   // message number of "* 3 FETCH", "* 4 EXISTS"
	kind   string   // kind of untagged data: FETCH, EXISTS, SEARCH, LIST, ...
	data   *scanner // positioned after kind
	raw    string
}

func parseResponse(raw string) *response {
	sc := &scanner{s: raw}
	r := &response{raw: raw, tag: sc.word()}
	if r.tag == "+" {
		r.text = sc.rest()
		return r
	}
	w := sc.word()
	switch u := strings.ToUpper(w); u {
	case "OK", "NO", "BAD", "BYE", "PREAUTH":
		r.status = u
		sc.skipSpaces()
		if sc.i < len(sc.s) && sc.s[sc.i] == '[' {
			if end := strings.IndexByte(sc.s[sc.i:], ']'); end > 0 {
				r.code = sc.s[sc.i+1 : sc.i+end]
				sc.i += end + 1
			}
		}
		r.text = sc.rest()
		return r
	}
	if r.tag == "*" {
		if n, err := strconv.ParseUint(w, 10, 32); err == nil {
			r.number = uint32(n)
			w = sc.word()
		}
		r.kind = strings.ToUpper(w)
		r.data = sc
	}
	return r
}

// command is a command line under construction. Strings go out quoted when
// they can and as literals otherwise, so no value can change the command's
// structure.
type command struct {
	name   string
	parts  []cmdPart
	secret bool // arguments stay out of the trace
}

type cmdPart struct {
	text    string
	literal bool
}

func newCommand(name string) *command {
	return &command{name: name, parts: []cmdPart{{text: name}}}
}

// atom appends protocol text as it is: keywords, sequence sets, item lists.
func (c *command) atom(s string) *command {
	c.parts = append(c.parts, cmdPart{text: s})
	return c
}

// str appends a string argument.
func (c *command) str(s string) *command {
	if quotable(s) {
		return c.atom(quote(s))
	}
	c.parts = append(c.parts, cmdPart{text: s, literal: true})
	return c
}

func (c *command) hideArgs() *command {
	c.secret = true
	return c
}

// quotable reports whether s fits a quoted string: 7-bit, no NUL, CR or LF.
func quotable(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == 0 || c == '\r' || c == '\n' || c > 0x7f {
			return false
		}
	}
	return true
}

func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

// maxSetLen keeps sequence sets well under the 8000-odd bytes servers accept
// per command line.
const maxSetLen = 1000

// uidSets renders UIDs as sequence sets like 1:4,7,9:12, split so no command
// line grows past what servers accept.
func uidSets(uids []uint32) []string {
	sorted := slices.Clone(uids)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	for len(sorted) > 0 && sorted[0] == 0 {
		sorted = sorted[1:]
	}
	var sets []string
	var b strings.Builder
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		part := strconv.FormatUint(uint64(sorted[i]), 10)
		if j > i {
			part += ":" + strconv.FormatUint(uint64(sorted[j]), 10)
		}
		if b.Len() > 0 && b.Len()+1+len(part) > maxSetLen {
			sets = append(sets, b.String())
			b.Reset()
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(part)
		i = j + 1
	}
	if b.Len() > 0 {
		sets = append(sets, b.String())
	}
	return sets
}

// imapDate renders the date of t for SEARCH, e.g. 7-Oct-2026.
func imapDate(t time.Time) string { return t.Format("2-Jan-2006") }

// parseInternalDate reads an INTERNALDATE such as "17-Jul-1996 02:44:25 -0700".
// The day may be padded with a space instead of a zero.
func parseInternalDate(s string) (time.Time, bool) {
	t, err := time.Parse("2-Jan-2006 15:04:05 -0700", strings.TrimSpace(s))
	return t, err == nil
}
