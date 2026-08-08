package mail

import (
	"fmt"
	"strings"
)

// DeleteByUID permanently removes a message by UID from the currently selected mailbox.
// Flow: mark \Deleted with UID STORE → try UID EXPUNGE → fallback to EXPUNGE.
func (c *IMAPClient) DeleteByUID(uid string) error {
	if uid == "" {
		return fmt.Errorf("UID cannot be empty")
	}

	// Ensure mailbox is selected in READ-WRITE and supports \\Deleted
	if err := c.sendCommand("A017 SELECT INBOX\r\n"); err != nil {
		return err
	}
	finalSel, dataSel, err := c.readCommandResponses("A017")
	if err != nil {
		return err
	}
	if strings.Contains(finalSel, "A017 NO") || strings.Contains(finalSel, "A017 BAD") {
		return fmt.Errorf("SELECT INBOX failed: %s", finalSel)
	}
	// Determine READ-WRITE and PERMANENTFLAGS
	isReadWrite := false
	allowsDeleted := false
	for _, line := range dataSel {
		u := strings.ToUpper(line)
		if strings.Contains(u, "[READ-WRITE]") {
			isReadWrite = true
		}
		if strings.Contains(u, "PERMANENTFLAGS") {
			// If server advertises \* or \DELETED, we can set it
			if strings.Contains(u, "\\DELETED") || strings.Contains(u, "\\*") {
				allowsDeleted = true
			}
		}
	}
	// Some servers only announce READ-WRITE in final tagged OK
	if strings.Contains(strings.ToUpper(finalSel), "[READ-WRITE]") {
		isReadWrite = true
	}
	if !isReadWrite {
		return fmt.Errorf("mailbox is not READ-WRITE; cannot delete")
	}
	if !allowsDeleted {
		// Try anyway; some servers omit PERMANENTFLAGS details
	}

	// Attempt a direct UID EXPUNGE first (no-op on some servers, but cheap)
	uidExpungeDirect := fmt.Sprintf("A019 UID EXPUNGE %s\r\n", uid)
	if err := c.sendCommand(uidExpungeDirect); err == nil {
		finalUidExpungeDirect, _, err := c.readCommandResponses("A019")
		if err == nil && strings.Contains(finalUidExpungeDirect, "A019 OK") {
			// Continue to normal flow; some servers require \Deleted set for actual removal
		}
	}

	// Step 1: mark the message as \\Deleted
	// First attempt with UID STORE
	storeCmd := fmt.Sprintf("A020 UID STORE %s +FLAGS.SILENT (\\Deleted)\r\n", uid)
	if err := c.sendCommand(storeCmd); err != nil {
		return err
	}
	finalStore, _, err := c.readCommandResponses("A020")
	if err != nil {
		return err
	}
	if strings.Contains(finalStore, "A020 NO") || strings.Contains(finalStore, "A020 BAD") {
		// Some servers are picky about flag case or UID STORE; try alternatives
		// Try UID STORE with uppercase flag
		storeCmdAlt := fmt.Sprintf("A020 UID STORE %s +FLAGS.SILENT (\\DELETED)\r\n", uid)
		if err := c.sendCommand(storeCmdAlt); err != nil {
			return err
		}
		finalStoreAlt, _, err := c.readCommandResponses("A020")
		if err != nil {
			return err
		}
		if strings.Contains(finalStoreAlt, "A020 NO") || strings.Contains(finalStoreAlt, "A020 BAD") {
			// Fallback: fetch sequence number and use non-UID STORE
			fetchSeq := fmt.Sprintf("A018 UID FETCH %s (FLAGS)\r\n", uid)
			if err := c.sendCommand(fetchSeq); err != nil {
				return err
			}
			_, data, err := c.readCommandResponses("A018")
			if err != nil {
				return err
			}
			// Parse sequence number from response like: "* 10976 FETCH (UID 3223603 ..."
			seqNum := ""
			for _, line := range data {
				if strings.HasPrefix(line, "* ") && strings.Contains(line, " FETCH (") && strings.Contains(line, "UID "+uid) {
					// After "* ", sequence until space
					afterStar := line[2:]
					space := strings.Index(afterStar, " ")
					if space > 0 {
						seqNum = afterStar[:space]
						break
					}
				}
			}
			if seqNum == "" {
				return fmt.Errorf("failed to determine sequence number for UID %s", uid)
			}
			// Try STORE with sequence number and both flag casings
			storeSeq := fmt.Sprintf("A020 STORE %s +FLAGS.SILENT (\\Deleted)\r\n", seqNum)
			if err := c.sendCommand(storeSeq); err != nil {
				return err
			}
			finalStoreSeq, _, err := c.readCommandResponses("A020")
			if err != nil {
				return err
			}
			if strings.Contains(finalStoreSeq, "A020 NO") || strings.Contains(finalStoreSeq, "A020 BAD") {
				storeSeqAlt := fmt.Sprintf("A020 STORE %s +FLAGS.SILENT (\\DELETED)\r\n", seqNum)
				if err := c.sendCommand(storeSeqAlt); err != nil {
					return err
				}
				finalStoreSeqAlt, _, err := c.readCommandResponses("A020")
				if err != nil {
					return err
				}
				if strings.Contains(finalStoreSeqAlt, "A020 NO") || strings.Contains(finalStoreSeqAlt, "A020 BAD") {
					return fmt.Errorf("STORE failed: server rejected \\Deleted flag")
				}
			}
		}
	}

	// Step 2: try to remove only that UID using UID EXPUNGE (UIDPLUS)
	uidExpunge := fmt.Sprintf("A021 UID EXPUNGE %s\r\n", uid)
	if err := c.sendCommand(uidExpunge); err != nil {
		return err
	}
	finalUidExpunge, _, err := c.readCommandResponses("A021")
	if err == nil && strings.Contains(finalUidExpunge, "A021 OK") {
		return nil
	}

	// Step 3: fallback to EXPUNGE (will expunge all \Deleted messages)
	expungeCmd := "A022 EXPUNGE\r\n"
	if err := c.sendCommand(expungeCmd); err != nil {
		return err
	}
	finalExpunge, _, err := c.readCommandResponses("A022")
	if err != nil {
		return err
	}
	if strings.Contains(finalExpunge, "A022 OK") {
		return nil
	}
	return fmt.Errorf("EXPUNGE failed: %s", finalExpunge)
}
