package mail

import "strings"

// fetchItems returns the data items of an untagged FETCH response
// ("* 12 FETCH (UID 7 INTERNALDATE "..." ENVELOPE (...))"), keyed by the
// upper-cased item name ("UID", "INTERNALDATE", "ENVELOPE", "BODY[]"), each
// value exactly as it appears on the wire.
//
// RFC 3501 (7.4.2) fixes no order for the items: Dovecot puts UID first, Maddy
// puts it last. Values can be quoted strings, {n} literals or nested lists
// whose text may itself contain "UID " or parentheses, so the list is walked
// token by token rather than searched for substrings.
//
// Returns nil if response is not a FETCH response; stops at the first token it
// cannot read and returns the items read so far.
func fetchItems(response string) map[string]string {
	i := fetchListStart(response)
	if i < 0 {
		return nil
	}
	items := map[string]string{}
	for {
		i = skipSpaces(response, i)
		if i >= len(response) || response[i] == ')' {
			return items
		}
		nameEnd := scanItemName(response, i)
		if nameEnd == i {
			return items
		}
		name := strings.ToUpper(response[i:nameEnd])
		valueStart := skipSpaces(response, nameEnd)
		valueEnd, ok := scanValue(response, valueStart)
		if !ok {
			return items
		}
		items[name] = response[valueStart:valueEnd]
		i = valueEnd
	}
}

// fetchListStart returns the index just past "* <n> FETCH (", or -1.
func fetchListStart(response string) int {
	rest, ok := strings.CutPrefix(response, "* ")
	if !ok {
		return -1
	}
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	const marker = " FETCH ("
	if n == 0 || len(rest) < n+len(marker) || !strings.EqualFold(rest[n:n+len(marker)], marker) {
		return -1
	}
	return len(response) - len(rest) + n + len(marker)
}

func skipSpaces(s string, i int) int {
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return i
}

// scanItemName returns the end of the item name starting at i. Section
// specifiers may contain spaces and parentheses, as in
// "BODY[HEADER.FIELDS (SUBJECT)]", so those only end the name outside [].
func scanItemName(s string, i int) int {
	depth := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case ' ', '(', ')', '\r', '\n':
			if depth == 0 {
				return i
			}
		}
	}
	return i
}

// scanValue returns the end of the value starting at i: a quoted string, a
// {n} literal, a parenthesized list (recursively) or an atom such as a number
// or NIL.
func scanValue(s string, i int) (int, bool) {
	if i >= len(s) {
		return 0, false
	}
	switch s[i] {
	case '"':
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case '"':
				return j + 1, true
			}
		}
		return 0, false
	case '{':
		length, contentStart, ok := parseLiteralField(s, i)
		if !ok || contentStart+length > len(s) {
			return 0, false
		}
		return contentStart + length, true
	case '(':
		j := i + 1
		for {
			j = skipSpaces(s, j)
			if j >= len(s) {
				return 0, false
			}
			if s[j] == ')' {
				return j + 1, true
			}
			end, ok := scanValue(s, j)
			if !ok {
				return 0, false
			}
			j = end
		}
	default:
		j := i
		for j < len(s) && !strings.ContainsRune(" ()\"\r\n", rune(s[j])) {
			j++
		}
		if j == i {
			return 0, false
		}
		return j, true
	}
}

// imapString returns the content of a quoted-string or literal value. NIL and
// anything else report false.
func imapString(value string) (string, bool) {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		quoted := value[1 : len(value)-1]
		if !strings.Contains(quoted, `\`) {
			return quoted, true
		}
		var b strings.Builder
		for i := 0; i < len(quoted); i++ {
			if quoted[i] == '\\' && i+1 < len(quoted) {
				i++
			}
			b.WriteByte(quoted[i])
		}
		return b.String(), true
	}
	if length, start, ok := parseLiteralField(value, 0); ok && start+length <= len(value) {
		return value[start : start+length], true
	}
	return "", false
}

// isUID reports whether s is a UID: a non-empty run of digits.
func isUID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// fetchedMessage is what the client reads from one FETCH response.
type fetchedMessage struct {
	uid          string
	internalDate int64 // Unix milliseconds
	envelope     *EnvelopeData
	body         string // BODY[], if the response carried it
}

// parseFetchedMessage reads UID, INTERNALDATE, ENVELOPE and, if present,
// BODY[] from a FETCH response, wherever the server put them. It reports false
// for responses that lack any of the first three, e.g. an unsolicited FLAGS
// update.
func (c *IMAPClient) parseFetchedMessage(response string) (fetchedMessage, bool) {
	items := fetchItems(response)
	uid := items["UID"]
	if !isUID(uid) {
		return fetchedMessage{}, false
	}
	internalDate, ok := imapString(items["INTERNALDATE"])
	if !ok || internalDate == "" {
		return fetchedMessage{}, false
	}
	envelope := items["ENVELOPE"]
	if len(envelope) < 2 || envelope[0] != '(' {
		return fetchedMessage{}, false
	}
	body, _ := imapString(items["BODY[]"])
	return fetchedMessage{
		uid:          uid,
		internalDate: parseInternalDateToUnixMilli(internalDate),
		envelope:     c.parseEnvelope(envelope[1 : len(envelope)-1]),
		body:         body,
	}, true
}
