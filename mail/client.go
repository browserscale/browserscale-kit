package mail

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
)

// IMAPClient represents a simple IMAP client
type IMAPClient struct {
	conn                  net.Conn
	reader                *bufio.Reader
	writer                *bufio.Writer
	username              string
	password              string
	server                string
	port                  int
	mutex                 sync.RWMutex // Protects concurrent access to the connection
	debug                 bool         // Enable debug output for raw send/receive
	insecureSkipTLSVerify bool         // Skip TLS certificate verification (for self-signed or internal CA certs)
}

// NewIMAPClient creates a new IMAP client for any server.
// TLS certificate verification is always skipped (for self-signed or corporate CA certificates).
func NewIMAPClient(server string, port int, username string, password string) *IMAPClient {
	return &IMAPClient{
		username:              username,
		password:              password,
		server:                server,
		port:                  port,
		debug:                 false,
		insecureSkipTLSVerify: true,
	}
}

// EnableDebug enables debug output for raw send/receive data
func (c *IMAPClient) EnableDebug() {
	c.debug = true
}

// IsDebug returns whether debug mode is enabled
func (c *IMAPClient) IsDebug() bool {
	return c.debug
}

// Connect establishes a connection to IMAP server
func (c *IMAPClient) Connect() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	// Connect to IMAP server
	addr := c.server + ":" + strconv.Itoa(c.port)
	tlsConfig := &tls.Config{ServerName: c.server}
	if c.insecureSkipTLSVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("failed to connect to IMAP server: %v", err)
	}

	c.conn = conn
	c.reader = bufio.NewReader(conn)
	c.writer = bufio.NewWriter(conn)

	// Read initial server response
	_, err = c.readResponseUnsafe()
	if err != nil {
		return fmt.Errorf("failed to read initial response: %v", err)
	}

	// Check server features by sending CAPABILITY command
	features, err := c.checkServerFeatures()
	if err != nil {
		return fmt.Errorf("failed to check server features: %v", err)
	}

	// Validate that server supports required modern features
	if err := ValidateModernFeatures(features); err != nil {
		return fmt.Errorf("server compatibility check failed: %v", err)
	}

	// Login
	if err := c.login(); err != nil {
		return fmt.Errorf("login failed: %v", err)
	}
	return nil
}

// login authenticates with the IMAP server
func (c *IMAPClient) login() error {
	// Send LOGIN command
	command := fmt.Sprintf("A001 LOGIN \"%s\" \"%s\"\r\n", c.username, c.password)
	if err := c.sendCommandUnsafe(command); err != nil {
		return err
	}

	// Read all responses until we get the final OK
	for {
		response, err := c.readResponseUnsafe()
		if err != nil {
			return err
		}

		if strings.Contains(response, "A001 OK") {
			return nil
		}
		if strings.Contains(response, "A001 NO") || strings.Contains(response, "A001 BAD") {
			return fmt.Errorf("login failed: %s", response)
		}
	}
}

// sendCommand sends a command to the IMAP server
func (c *IMAPClient) sendCommand(command string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.sendCommandUnsafe(command)
}

// sendCommandUnsafe sends a command without acquiring the mutex (for internal use)
func (c *IMAPClient) sendCommandUnsafe(command string) error {
	if c.debug {
		fmt.Printf("SEND: %s", command)
	}
	_, err := c.writer.WriteString(command)
	if err != nil {
		return fmt.Errorf("failed to send command: %v", err)
	}
	return c.writer.Flush()
}

// readResponse reads a response from the IMAP server
func (c *IMAPClient) readResponse() (string, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.readResponseUnsafe()
}

// readResponseUnsafe reads a response without acquiring the mutex (for internal use)
func (c *IMAPClient) readResponseUnsafe() (string, error) {
	var response strings.Builder

	chunk, err := c.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read response: %v", err)
	}
	response.WriteString(chunk)

	for {
		literalLength, hasLiteral := parseTrailingLiteralLength(chunk)
		if !hasLiteral {
			break
		}

		literalBytes, err := c.readBytesUnsafe(literalLength)
		if err != nil {
			return "", fmt.Errorf("failed to read response literal: %v", err)
		}
		response.Write(literalBytes)

		chunk, err = c.reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("failed to read response continuation: %v", err)
		}
		response.WriteString(chunk)
	}

	fullResponse := response.String()
	if c.debug {
		fmt.Printf("RECV: %s", fullResponse)
	}
	return fullResponse, nil
}

func parseTrailingLiteralLength(responseLine string) (int, bool) {
	trimmed := strings.TrimRight(responseLine, "\r\n")
	if !strings.HasSuffix(trimmed, "}") {
		return 0, false
	}

	start := strings.LastIndex(trimmed, "{")
	if start == -1 || start >= len(trimmed)-1 {
		return 0, false
	}

	literalLength := trimmed[start+1 : len(trimmed)-1]
	literalLength = strings.TrimSuffix(literalLength, "+")
	if literalLength == "" {
		return 0, false
	}

	length, err := strconv.Atoi(literalLength)
	if err != nil {
		return 0, false
	}

	return length, true
}

// readCommandResponses reads all responses until the final response for a command
// Returns the final response and any data responses (responses starting with *)
func (c *IMAPClient) readCommandResponses(commandTag string) (string, []string, error) {
	var dataResponses []string
	var finalResponse string

	for {
		response, err := c.readResponseUnsafe()
		if err != nil {
			return "", dataResponses, err
		}

		// Check if this is the final response for our command
		if strings.Contains(response, commandTag+" OK") ||
			strings.Contains(response, commandTag+" NO") ||
			strings.Contains(response, commandTag+" BAD") {
			finalResponse = response
			break
		}

		// This is a data response (starts with *)
		dataResponses = append(dataResponses, response)
	}

	return finalResponse, dataResponses, nil
}

// readBytes reads a specific number of bytes from the IMAP server
func (c *IMAPClient) readBytes(length int) ([]byte, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.readBytesUnsafe(length)
}

// readBytesUnsafe reads bytes without acquiring the mutex (for internal use)
func (c *IMAPClient) readBytesUnsafe(length int) ([]byte, error) {
	buffer := make([]byte, length)
	totalRead := 0

	for totalRead < length {
		n, err := c.reader.Read(buffer[totalRead:])
		if err != nil {
			return nil, fmt.Errorf("failed to read %d bytes (read %d): %v", length, totalRead, err)
		}
		totalRead += n
	}

	return buffer, nil
}

// ListMailboxes lists available mailboxes
func (c *IMAPClient) ListMailboxes() error {
	command := "A002 LIST \"\" \"*\"\r\n"
	if err := c.sendCommand(command); err != nil {
		return err
	}

	// Read all responses until we get the final OK
	for {
		response, err := c.readResponse()
		if err != nil {
			return err
		}
		fmt.Printf("Mailbox: %s", response)

		if strings.Contains(response, "A002 OK") {
			break
		}
	}
	return nil
}

// SelectInbox selects the INBOX
func (c *IMAPClient) SelectInbox() error {
	command := "A003 SELECT INBOX\r\n"
	if err := c.sendCommand(command); err != nil {
		return err
	}

	// Read all responses until we get the final OK
	for {
		response, err := c.readResponse()
		if err != nil {
			return err
		}

		if strings.Contains(response, "A003 OK") {
			break
		}
	}
	return nil
}

// Disconnect closes the connection (aggressive like Chilkat DisposeImap)
func (c *IMAPClient) Disconnect() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.conn != nil {
		// Just kill the socket immediately (like Chilkat DisposeImap)
		err := c.conn.Close()
		c.conn = nil
		c.reader = nil
		c.writer = nil
		return err
	}
	return nil
}

// checkServerFeatures sends a CAPABILITY command to check server features
func (c *IMAPClient) checkServerFeatures() (*IMAPFeatures, error) {
	// Send CAPABILITY command
	command := "A000 CAPABILITY\r\n"
	if err := c.sendCommandUnsafe(command); err != nil {
		return nil, err
	}

	// Read capability responses
	features := &IMAPFeatures{
		ServerInfo: "Connected to " + c.server,
	}

	// Read all capability responses
	for {
		response, err := c.readResponseUnsafe()
		if err != nil {
			return nil, err
		}

		// Check for specific capabilities in the response
		capabilities := strings.ToUpper(response)

		// INTERNALDATE is a standard IMAP4rev1 feature, so it's always supported
		features.SupportsInternalDate = true

		if strings.Contains(capabilities, "UIDPLUS") {
			features.SupportsUIDPlus = true
		}
		if strings.Contains(capabilities, "CONDSTORE") {
			features.SupportsCondStore = true
		}
		if strings.Contains(capabilities, "QRESYNC") {
			features.SupportsQResync = true
		}
		if strings.Contains(capabilities, "MOVE") {
			features.SupportsMove = true
		}
		if strings.Contains(capabilities, "BINARY") {
			features.SupportsBinary = true
		}
		if strings.Contains(capabilities, "PREVIEW") {
			features.SupportsPreview = true
		}
		if strings.Contains(capabilities, "SNIPPET") {
			features.SupportsSnippet = true
		}

		// Check if this is the final response
		if strings.HasPrefix(response, "A000 OK") || strings.HasPrefix(response, "A000 ") {
			break
		}
	}

	return features, nil
}

// Noop sends a NOOP command to keep the connection alive
func (c *IMAPClient) Noop() error {
	command := "A004 NOOP\r\n"
	if err := c.sendCommand(command); err != nil {
		return err
	}

	// Read all responses until we get the final OK for our command
	finalResponse, _, err := c.readCommandResponses("A004")
	if err != nil {
		return err
	}

	// Check if NOOP was successful
	if strings.Contains(finalResponse, "A004 OK") {
		return nil
	}
	return fmt.Errorf("NOOP command failed: %s", finalResponse)
}
