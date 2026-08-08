package mail

import (
	"fmt"
	"strings"
	"time"
)

// SearchCriteria defines search parameters for IMAP search
type SearchCriteria struct {
	Subject string
	From    string
	To      string
	Body    string
	Since   int64 // Unix milliseconds
	Before  int64 // Unix milliseconds
}

// SearchEmails searches for emails using IMAP search capabilities and returns only UIDs
func (c *IMAPClient) SearchEmails(criteria SearchCriteria, limit int) ([]string, error) {
	// Build IMAP search command
	searchCommand := c.buildSearchCommand(criteria)

	// Send UID SEARCH command
	command := fmt.Sprintf("A007 UID SEARCH %s\r\n", searchCommand)
	if err := c.sendCommand(command); err != nil {
		return nil, err
	}

	// Read all responses until we get the final OK for our command
	finalResponse, dataResponses, err := c.readCommandResponses("A007")
	if err != nil {
		return nil, err
	}

	// Check if command failed
	if strings.Contains(finalResponse, "A007 NO") || strings.Contains(finalResponse, "A007 BAD") {
		return nil, fmt.Errorf("search command failed: %s", finalResponse)
	}

	// Extract email UIDs from data responses
	var emailUIDs []string
	for _, response := range dataResponses {
		if strings.Contains(response, "* SEARCH") {
			// Extract email UIDs from response
			parts := strings.Fields(response)
			for i, part := range parts {
				if part == "SEARCH" && i+1 < len(parts) {
					for j := i + 1; j < len(parts); j++ {
						if parts[j] != "" {
							emailUIDs = append(emailUIDs, parts[j])
						}
					}
					break
				}
			}
		}
	}

	// Limit the number of emails to return
	if limit > 0 && len(emailUIDs) > limit {
		emailUIDs = emailUIDs[len(emailUIDs)-limit:]
	}

	return emailUIDs, nil
}

// unixMilliToIMAPDate converts Unix milliseconds to IMAP date format (DD-MMM-YYYY)
func unixMilliToIMAPDate(unixMilli int64) string {
	if unixMilli <= 0 {
		return ""
	}
	t := time.UnixMilli(unixMilli)
	return t.Format("02-Jan-2006")
}

// buildSearchCommand builds IMAP search command from criteria
func (c *IMAPClient) buildSearchCommand(criteria SearchCriteria) string {
	var conditions []string

	if criteria.To != "" {
		conditions = append(conditions, fmt.Sprintf("TO %s", criteria.To))
	}
	if criteria.From != "" {
		conditions = append(conditions, fmt.Sprintf("FROM %s", criteria.From))
	}
	if criteria.Subject != "" {
		conditions = append(conditions, fmt.Sprintf("SUBJECT %s", criteria.Subject))
	}
	if criteria.Body != "" {
		conditions = append(conditions, fmt.Sprintf("BODY \"%s\"", criteria.Body))
	}
	if criteria.Since > 0 {
		imapDate := unixMilliToIMAPDate(criteria.Since)
		if imapDate != "" {
			conditions = append(conditions, fmt.Sprintf("SINCE %s", imapDate))
		}
	}
	if criteria.Before > 0 {
		imapDate := unixMilliToIMAPDate(criteria.Before)
		if imapDate != "" {
			conditions = append(conditions, fmt.Sprintf("BEFORE %s", imapDate))
		}
	}

	// If no conditions, search all emails
	if len(conditions) == 0 {
		return "ALL"
	}

	return strings.Join(conditions, " ")
}
