package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"strconv"
	"strings"
	"time"
)

// EmailHeaders represents email header information including INTERNALDATE
type EmailHeaders struct {
	UID          string
	Subject      string
	From         string
	To           string
	Date         string
	InternalDate int64 // Unix milliseconds
}

// Mail represents a complete email with headers and body
type Mail struct {
	UID          string
	Subject      string
	From         string
	To           string
	Date         string
	InternalDate int64 // Unix milliseconds
	Body         string
}

// FetchHeaders fetches headers (including INTERNALDATE) for the given UIDs
func (c *IMAPClient) FetchHeaders(emailUIDs []string) ([]EmailHeaders, error) {
	if len(emailUIDs) == 0 {
		return []EmailHeaders{}, nil
	}

	// Build the message set string
	messageSet := strings.Join(emailUIDs, ",")

	// Send UID FETCH command to get headers and INTERNALDATE
	command := fmt.Sprintf("A012 UID FETCH %s (ENVELOPE INTERNALDATE)\r\n", messageSet)
	if err := c.sendCommand(command); err != nil {
		return nil, err
	}

	// Read all responses until we get the final OK for our command
	finalResponse, dataResponses, err := c.readCommandResponses("A012")
	if err != nil {
		return nil, err
	}

	// Check if command failed
	if strings.Contains(finalResponse, "A012 NO") || strings.Contains(finalResponse, "A012 BAD") {
		return nil, fmt.Errorf("fetch headers command failed: %s", finalResponse)
	}

	// Parse email headers from data responses
	var headers []EmailHeaders
	for _, response := range dataResponses {
		c.parseSingleEmailResponse(response, &headers)
	}

	return headers, nil
}

// FetchMail fetches a complete email (headers and body) for a single UID
func (c *IMAPClient) FetchMail(uid string) (*Mail, error) {
	if uid == "" {
		return nil, fmt.Errorf("UID cannot be empty")
	}

	// Fetch the full raw message so MIME parts can be parsed with the standard library.
	command := fmt.Sprintf("A013 UID FETCH %s (ENVELOPE INTERNALDATE BODY[])\r\n", uid)
	if err := c.sendCommand(command); err != nil {
		return nil, err
	}

	// Read responses manually to handle BODY[] literal data
	var mail *Mail
	var rawMessage strings.Builder

	for {
		response, err := c.readResponse()
		if err != nil {
			return nil, err
		}

		// Check if this is the end of the fetch response
		if strings.Contains(response, "A013 OK") {
			break
		}
		if strings.Contains(response, "A013 NO") || strings.Contains(response, "A013 BAD") {
			return nil, fmt.Errorf("fetch mail command failed: %s", response)
		}

		// Parse headers and body from the assembled FETCH response.
		if strings.Contains(response, "UID") && strings.Contains(response, "INTERNALDATE") && strings.Contains(response, "ENVELOPE") {
			mail = c.parseSingleMailResponse(response)
		}

		bodyChunk, hasBody, err := extractFetchLiteral(response, "BODY[] {")
		if err != nil {
			return nil, fmt.Errorf("failed to parse body content: %v", err)
		}
		if hasBody {
			rawMessage.WriteString(bodyChunk)
		}
	}

	if mail == nil {
		return nil, fmt.Errorf("failed to parse mail for UID %s", uid)
	}

	rawBody := rawMessage.String()
	decodedBody, err := extractSearchableBody(rawBody)
	if err != nil || decodedBody == "" {
		// Fall back to the raw message so callers can still attempt a regex match.
		mail.Body = rawBody
	} else {
		mail.Body = decodedBody
	}

	return mail, nil
}

func extractFetchLiteral(response string, bodyMarker string) (string, bool, error) {
	bodyStart := strings.Index(response, bodyMarker)
	if bodyStart == -1 {
		return "", false, nil
	}

	lengthStart := bodyStart + len(bodyMarker)
	lengthEnd := strings.Index(response[lengthStart:], "}")
	if lengthEnd == -1 {
		return "", false, fmt.Errorf("missing fetch literal terminator")
	}
	lengthEnd += lengthStart

	bodyLength, err := strconv.Atoi(response[lengthStart:lengthEnd])
	if err != nil {
		return "", false, fmt.Errorf("invalid fetch literal length: %w", err)
	}

	bodyOffset := lengthEnd + 1
	switch {
	case strings.HasPrefix(response[bodyOffset:], "\r\n"):
		bodyOffset += 2
	case strings.HasPrefix(response[bodyOffset:], "\n"):
		bodyOffset++
	default:
		return "", false, fmt.Errorf("missing fetch literal separator")
	}

	if len(response[bodyOffset:]) < bodyLength {
		return "", false, fmt.Errorf("fetch literal truncated: expected %d bytes, got %d", bodyLength, len(response[bodyOffset:]))
	}

	return response[bodyOffset : bodyOffset+bodyLength], true, nil
}

func extractSearchableBody(rawMessage string) (string, error) {
	msg, err := netmail.ReadMessage(strings.NewReader(rawMessage))
	if err != nil {
		return "", err
	}

	var parts []string
	if err := collectTextParts(msg.Header, msg.Body, &parts); err != nil {
		return "", err
	}

	return strings.Join(parts, "\n"), nil
}

type mimeHeader interface {
	Get(string) string
}

func collectTextParts(headers mimeHeader, body io.Reader, parts *[]string) error {
	mediaType, params, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}

	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("multipart message missing boundary")
		}

		reader := multipart.NewReader(body, boundary)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if err := collectTextParts(part.Header, part, parts); err != nil {
				return err
			}
		}
		return nil
	}

	if strings.EqualFold(mediaType, "message/rfc822") {
		nestedMessage, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		nestedBody, err := extractSearchableBody(string(nestedMessage))
		if err != nil {
			return err
		}
		if nestedBody != "" {
			*parts = append(*parts, nestedBody)
		}
		return nil
	}

	if !strings.HasPrefix(strings.ToLower(mediaType), "text/") {
		_, err := io.Copy(io.Discard, body)
		return err
	}

	decodedPart, err := decodePartBody(body, headers.Get("Content-Transfer-Encoding"))
	if err != nil {
		return err
	}
	if len(decodedPart) > 0 {
		*parts = append(*parts, string(decodedPart))
	}
	return nil
}

func decodePartBody(body io.Reader, transferEncoding string) ([]byte, error) {
	rawBody, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(strings.TrimSpace(transferEncoding)) {
	case "", "7bit", "8bit", "binary":
		return rawBody, nil
	case "quoted-printable":
		reader := quotedprintable.NewReader(bytes.NewReader(rawBody))
		return io.ReadAll(reader)
	case "base64":
		reader := base64.NewDecoder(base64.StdEncoding, bytes.NewReader(rawBody))
		return io.ReadAll(reader)
	default:
		return rawBody, nil
	}
}

// decodeQuotedPrintable decodes Quoted-Printable encoded content
func decodeQuotedPrintable(content string) (string, error) {
	reader := quotedprintable.NewReader(strings.NewReader(content))
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// parseInternalDateToUnixMilli converts IMAP INTERNALDATE string to Unix milliseconds
func parseInternalDateToUnixMilli(internalDateStr string) int64 {
	// IMAP INTERNALDATE format: "17-Jul-1996 02:44:25 -0700"
	parsedTime, err := time.Parse("02-Jan-2006 15:04:05 -0700", internalDateStr)
	if err != nil {
		return 0 // Return 0 if parsing fails
	}
	return parsedTime.UnixMilli()
}

// parseSingleMailResponse parses a single mail response line (similar to parseSingleEmailResponse but returns Mail)
func (c *IMAPClient) parseSingleMailResponse(response string) *Mail {
	// Parse UID, ENVELOPE, and INTERNALDATE from response manually with robust error handling
	// Format: * 1 FETCH (UID 1 INTERNALDATE "..." ENVELOPE (...))
	if strings.Contains(response, "UID") && strings.Contains(response, "INTERNALDATE") && strings.Contains(response, "ENVELOPE") {
		// Find UID with bounds checking
		uidStart := strings.Index(response, "UID ")
		if uidStart == -1 {
			return nil
		}
		uidStart += 4
		uidEnd := strings.Index(response[uidStart:], " ")
		if uidEnd == -1 {
			return nil
		}
		uid := response[uidStart : uidStart+uidEnd]

		// Find INTERNALDATE with bounds checking
		internalDateStart := strings.Index(response, "INTERNALDATE \"")
		if internalDateStart == -1 {
			return nil
		}
		internalDateStart += 14
		internalDateEnd := strings.Index(response[internalDateStart:], "\"")
		if internalDateEnd == -1 {
			return nil
		}
		internalDate := response[internalDateStart : internalDateStart+internalDateEnd]

		// Find ENVELOPE data with robust parenthesis matching
		envelopeStart := strings.Index(response, "ENVELOPE (")
		if envelopeStart == -1 {
			return nil
		}
		envelopeStart += 10
		envelopeEnd := envelopeStart
		parenCount := 1
		for i := envelopeStart; i < len(response); i++ {
			if response[i] == '(' {
				parenCount++
			} else if response[i] == ')' {
				parenCount--
				if parenCount == 0 {
					envelopeEnd = i
					break
				}
			}
		}

		// Validate envelope data was found
		if envelopeEnd <= envelopeStart {
			return nil
		}

		envelopeData := response[envelopeStart:envelopeEnd]

		// Parse envelope data
		envelope := c.parseEnvelope(envelopeData)

		// Only return if we have valid data
		if uid != "" && internalDate != "" {
			return &Mail{
				UID:          uid,
				Subject:      envelope.Subject,
				From:         envelope.From,
				To:           envelope.To,
				Date:         envelope.Date,
				InternalDate: parseInternalDateToUnixMilli(internalDate),
				Body:         "", // Will be set later
			}
		}
	}

	return nil
}

// parseSingleEmailResponse parses a single email response line
func (c *IMAPClient) parseSingleEmailResponse(response string, headers *[]EmailHeaders) {
	// Parse UID, ENVELOPE, and INTERNALDATE from response manually with robust error handling
	// Format: * 1 FETCH (UID 1 INTERNALDATE "..." ENVELOPE (...))
	if strings.Contains(response, "UID") && strings.Contains(response, "INTERNALDATE") && strings.Contains(response, "ENVELOPE") {
		// Find UID with bounds checking
		uidStart := strings.Index(response, "UID ")
		if uidStart == -1 {
			return
		}
		uidStart += 4
		uidEnd := strings.Index(response[uidStart:], " ")
		if uidEnd == -1 {
			return
		}
		uid := response[uidStart : uidStart+uidEnd]

		// Find INTERNALDATE with bounds checking
		internalDateStart := strings.Index(response, "INTERNALDATE \"")
		if internalDateStart == -1 {
			return
		}
		internalDateStart += 14
		internalDateEnd := strings.Index(response[internalDateStart:], "\"")
		if internalDateEnd == -1 {
			return
		}
		internalDate := response[internalDateStart : internalDateStart+internalDateEnd]

		// Find ENVELOPE data with robust parenthesis matching
		envelopeStart := strings.Index(response, "ENVELOPE (")
		if envelopeStart == -1 {
			return
		}
		envelopeStart += 10
		envelopeEnd := envelopeStart
		parenCount := 1
		for i := envelopeStart; i < len(response); i++ {
			if response[i] == '(' {
				parenCount++
			} else if response[i] == ')' {
				parenCount--
				if parenCount == 0 {
					envelopeEnd = i
					break
				}
			}
		}

		// Validate envelope data was found
		if envelopeEnd <= envelopeStart {
			return
		}

		envelopeData := response[envelopeStart:envelopeEnd]

		// Parse envelope data
		envelope := c.parseEnvelope(envelopeData)

		// Only add if we have valid data
		if uid != "" && internalDate != "" {
			*headers = append(*headers, EmailHeaders{
				UID:          uid,
				Subject:      envelope.Subject,
				From:         envelope.From,
				To:           envelope.To,
				Date:         envelope.Date,
				InternalDate: parseInternalDateToUnixMilli(internalDate),
			})
		}
	}
}

// EnvelopeData represents parsed envelope information
type EnvelopeData struct {
	Subject string
	From    string
	To      string
	Date    string
}

// parseEnvelope parses IMAP envelope response with robust error handling
func (c *IMAPClient) parseEnvelope(envelopeData string) *EnvelopeData {
	envelope := &EnvelopeData{}

	// Handle empty or invalid envelope data
	if envelopeData == "" {
		return envelope
	}

	// Parse the envelope data - it's a space-separated list of fields
	fields := c.parseEnvelopeFields(envelopeData)

	// Safely extract fields with bounds checking
	if len(fields) >= 1 {
		envelope.Date = c.cleanEnvelopeField(fields[0])
	}

	if len(fields) >= 2 {
		envelope.Subject = c.cleanEnvelopeField(fields[1])
	}

	if len(fields) >= 3 {
		envelope.From = c.parseAddressList(fields[2])
	}

	// TO field is at index 5 in the envelope structure
	if len(fields) >= 6 {
		envelope.To = c.parseAddressList(fields[5])
	}

	return envelope
}

// parseEnvelopeFields splits envelope data into fields
func (c *IMAPClient) parseEnvelopeFields(data string) []string {
	var fields []string
	var current strings.Builder
	parenCount := 0
	inQuotes := false

	for i := 0; i < len(data); i++ {
		if !inQuotes {
			if literalLength, literalStart, ok := parseLiteralField(data, i); ok {
				literalEnd := literalStart + literalLength
				if literalEnd > len(data) {
					literalEnd = len(data)
				}
				current.WriteString(data[literalStart:literalEnd])
				i = literalEnd - 1
				continue
			}
		}

		char := data[i]
		switch char {
		case '"':
			inQuotes = !inQuotes
			current.WriteByte(char)
		case '(':
			parenCount++
			current.WriteByte(char)
		case ')':
			parenCount--
			current.WriteByte(char)
		case ' ':
			if parenCount == 0 && !inQuotes {
				if current.Len() > 0 {
					fields = append(fields, current.String())
					current.Reset()
				}
			} else {
				current.WriteByte(char)
			}
		default:
			current.WriteByte(char)
		}
	}

	if current.Len() > 0 {
		fields = append(fields, current.String())
	}

	return fields
}

func parseLiteralField(data string, start int) (int, int, bool) {
	if start >= len(data) || data[start] != '{' {
		return 0, 0, false
	}

	end := strings.IndexByte(data[start:], '}')
	if end == -1 {
		return 0, 0, false
	}
	end += start

	literalLength := data[start+1 : end]
	literalLength = strings.TrimSuffix(literalLength, "+")
	if literalLength == "" {
		return 0, 0, false
	}

	length, err := strconv.Atoi(literalLength)
	if err != nil {
		return 0, 0, false
	}

	contentStart := end + 1
	switch {
	case strings.HasPrefix(data[contentStart:], "\r\n"):
		contentStart += 2
	case strings.HasPrefix(data[contentStart:], "\n"):
		contentStart++
	default:
		return 0, 0, false
	}

	return length, contentStart, true
}

// cleanEnvelopeField removes quotes and NIL values
func (c *IMAPClient) cleanEnvelopeField(field string) string {
	field = strings.TrimSpace(field)
	if field == "NIL" {
		return ""
	}
	if strings.HasPrefix(field, "\"") && strings.HasSuffix(field, "\"") {
		field = field[1 : len(field)-1]
	}

	// Decode MIME-encoded headers (RFC 2047) using Go's built-in mime package
	decoder := &mime.WordDecoder{}
	decoded, err := decoder.DecodeHeader(field)
	if err == nil && decoded != field {
		return decoded
	}

	return field
}

// parseAddressList parses an IMAP address list with robust error handling
func (c *IMAPClient) parseAddressList(addressData string) string {
	// Handle NIL case
	addressData = strings.TrimSpace(addressData)
	if addressData == "NIL" || addressData == "" {
		return ""
	}

	// Look for email pattern in the format: (("Name" NIL "user" "domain.com"))
	// The format is: (("FIFA" NIL "noreply" "fifa.com"))
	// We need to find the 3rd and 4th quoted strings (username and domain)

	// Find all quoted strings in the address data
	var quotedStrings []string
	start := 0
	for {
		startQuote := strings.Index(addressData[start:], "\"")
		if startQuote == -1 {
			break
		}
		startQuote += start

		endQuote := strings.Index(addressData[startQuote+1:], "\"")
		if endQuote == -1 {
			break
		}
		endQuote += startQuote + 1

		quotedStrings = append(quotedStrings, addressData[startQuote+1:endQuote])
		start = endQuote + 1
	}

	// Need at least 2 quoted strings for a valid email address (username and domain)
	if len(quotedStrings) < 2 {
		return ""
	}

	// Look for a pattern where we have a username and domain
	// The format is: (("Name" NIL "user" "domain.com")) or ((NIL NIL "user" "domain.com"))
	// We need to find the username and domain from the quoted strings
	if len(quotedStrings) >= 2 {
		// Try different combinations
		for i := 0; i < len(quotedStrings)-1; i++ {
			username := quotedStrings[i]
			domain := quotedStrings[i+1]

			// Validate username and domain
			if c.isValidEmailPart(username) && c.isValidDomain(domain) {
				return username + "@" + domain
			}
		}
	}

	return ""
}

// isValidEmailPart checks if a string is a valid email username part
func (c *IMAPClient) isValidEmailPart(part string) bool {
	if part == "" || part == "NIL" {
		return false
	}
	// Username should not contain spaces and should be reasonable length
	return !strings.Contains(part, " ") && len(part) > 0 && len(part) < 100
}

// isValidDomain checks if a string is a valid domain
func (c *IMAPClient) isValidDomain(domain string) bool {
	if domain == "" || domain == "NIL" {
		return false
	}
	// Domain should contain a dot and be reasonable length
	return strings.Contains(domain, ".") && len(domain) > 2 && len(domain) < 255
}
