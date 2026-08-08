package mail

import (
	"fmt"
	"strings"
)

// IMAPFeatures represents the capabilities of an IMAP server
type IMAPFeatures struct {
	SupportsInternalDate bool
	SupportsUIDPlus      bool
	SupportsCondStore    bool
	SupportsQResync      bool
	SupportsMove         bool
	SupportsBinary       bool
	SupportsPreview      bool
	SupportsSnippet      bool
	ServerInfo           string
}

// ValidateModernFeatures checks if the server supports required modern features
func ValidateModernFeatures(features *IMAPFeatures) error {
	var missingFeatures []string

	if !features.SupportsInternalDate {
		missingFeatures = append(missingFeatures, "INTERNALDATE")
	}

	// Only require INTERNALDATE as it's essential for modern IMAP operations
	// UIDPLUS and CONDSTORE are nice-to-have but not strictly required
	if len(missingFeatures) > 0 {
		return fmt.Errorf("server does not support required modern IMAP features: %s", strings.Join(missingFeatures, ", "))
	}

	return nil
}

// PrintFeatures prints the server capabilities in a readable format
func (f *IMAPFeatures) PrintFeatures() {
	fmt.Printf("=== IMAP Server Features ===\n")
	fmt.Printf("Server Info: %s\n", f.ServerInfo)
	fmt.Printf("INTERNALDATE: %t\n", f.SupportsInternalDate)
	fmt.Printf("UIDPLUS: %t\n", f.SupportsUIDPlus)
	fmt.Printf("CONDSTORE: %t\n", f.SupportsCondStore)
	fmt.Printf("QRESYNC: %t\n", f.SupportsQResync)
	fmt.Printf("MOVE: %t\n", f.SupportsMove)
	fmt.Printf("BINARY: %t\n", f.SupportsBinary)
	fmt.Printf("PREVIEW: %t\n", f.SupportsPreview)
	fmt.Printf("SNIPPET: %t\n", f.SupportsSnippet)
	fmt.Printf("============================\n")
}
