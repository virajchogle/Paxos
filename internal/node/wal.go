package node

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"paxos-banking/internal/types"
)

// ============================================================================
// PHASE 5: WRITE-AHEAD LOG (WAL) FOR 2PC ROLLBACK
// ============================================================================

// createWALEntry creates a new WAL entry for a transaction
// This is called before executing the transaction
func (n *Node) createWALEntry(txnID string, seqNum int32) *types.WALEntry {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	entry := types.NewWALEntry(txnID, seqNum)
	n.wal[txnID] = entry

	log.Printf("Node %d: 📝 Created WAL entry for txn %s (seq %d)", n.id, txnID, seqNum)
	return entry
}

// writeToWAL logs an operation to the WAL before executing it
// This allows us to undo the operation if needed (2PC rollback)
func (n *Node) writeToWAL(txnID string, opType types.OperationType, dataItem, oldValue, newValue int32) error {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	entry, exists := n.wal[txnID]
	if !exists {
		return fmt.Errorf("WAL entry not found for transaction %s", txnID)
	}

	entry.AddOperation(opType, dataItem, oldValue, newValue)

	log.Printf("Node %d: 📝 WAL[%s]: %s item %d: %d -> %d",
		n.id, txnID, opType, dataItem, oldValue, newValue)

	// Persist WAL to disk
	return n.persistWAL()
}

// commitWAL marks a WAL entry as committed
// This is called after a transaction successfully commits
func (n *Node) commitWAL(txnID string) error {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	entry, exists := n.wal[txnID]
	if !exists {
		return fmt.Errorf("WAL entry not found for transaction %s", txnID)
	}

	entry.Status = types.WALStatusCommitted
	log.Printf("Node %d: ✅ WAL[%s]: Committed", n.id, txnID)

	// Persist updated status
	if err := n.persistWAL(); err != nil {
		return err
	}

	// Clean up committed entries (can be done async)
	go n.cleanupCommittedWAL(txnID)

	return nil
}

// abortWAL rolls back a transaction by undoing all its operations
// This is the core rollback mechanism for 2PC
func (n *Node) abortWAL(txnID string) error {
	n.walMu.Lock()
	entry, exists := n.wal[txnID]
	if !exists {
		n.walMu.Unlock()
		return fmt.Errorf("WAL entry not found for transaction %s", txnID)
	}

	entry.Status = types.WALStatusAborting
	n.walMu.Unlock()

	log.Printf("Node %d: 🔄 WAL[%s]: Aborting - rolling back %d operations",
		n.id, txnID, len(entry.Operations))

	// Undo operations in REVERSE order (use balanceMu)
	n.balanceMu.Lock()
	for i := len(entry.Operations) - 1; i >= 0; i-- {
		op := entry.Operations[i]

		// Restore old value
		n.balances[op.DataItem] = op.OldValue

		log.Printf("Node %d: ↩️  WAL[%s]: Undo %s item %d: %d -> %d (restored)",
			n.id, txnID, op.Type, op.DataItem, op.NewValue, op.OldValue)
	}
	n.balanceMu.Unlock()

	// Save rolled-back database state
	if err := n.saveDatabase(); err != nil {
		log.Printf("Node %d: ⚠️  Warning - failed to save database after rollback: %v", n.id, err)
	}

	// Mark as aborted
	n.walMu.Lock()
	entry.Status = types.WALStatusAborted
	n.walMu.Unlock()

	log.Printf("Node %d: ✅ WAL[%s]: Aborted successfully", n.id, txnID)

	// Persist updated status
	if err := n.persistWAL(); err != nil {
		return err
	}

	// Clean up aborted entry
	go n.cleanupAbortedWAL(txnID)

	return nil
}

// persistWAL writes the current WAL state to disk
// This ensures WAL survives node crashes
func (n *Node) persistWAL() error {
	// Called with walMu already locked

	data, err := json.MarshalIndent(n.wal, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal WAL: %w", err)
	}

	if err := os.WriteFile(n.walFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write WAL file: %w", err)
	}

	return nil
}

// loadWAL loads the WAL from disk (for crash recovery)
func (n *Node) loadWAL() error {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	// Check if WAL file exists
	if _, err := os.Stat(n.walFile); os.IsNotExist(err) {
		log.Printf("Node %d: No existing WAL file found (fresh start)", n.id)
		return nil
	}

	data, err := os.ReadFile(n.walFile)
	if err != nil {
		return fmt.Errorf("failed to read WAL file: %w", err)
	}

	if len(data) == 0 {
		log.Printf("Node %d: WAL file is empty", n.id)
		return nil
	}

	if err := json.Unmarshal(data, &n.wal); err != nil {
		return fmt.Errorf("failed to unmarshal WAL: %w", err)
	}

	log.Printf("Node %d: 📖 Loaded %d WAL entries from disk", n.id, len(n.wal))

	// Check for any uncommitted transactions that need recovery
	for txnID, entry := range n.wal {
		if entry.Status == types.WALStatusPreparing || entry.Status == types.WALStatusPrepared {
			log.Printf("Node %d: ⚠️  Found uncommitted transaction %s in WAL (status: %s) - may need recovery",
				n.id, txnID, entry.Status)
		}
	}

	return nil
}

// cleanupCommittedWAL removes committed WAL entries after a delay
func (n *Node) cleanupCommittedWAL(txnID string) {
	// Wait a bit before cleanup (in case we need to replay)
	// time.Sleep(1 * time.Second)

	n.walMu.Lock()
	defer n.walMu.Unlock()

	if entry, exists := n.wal[txnID]; exists && entry.Status == types.WALStatusCommitted {
		delete(n.wal, txnID)
		log.Printf("Node %d: 🗑️  WAL[%s]: Cleaned up committed entry", n.id, txnID)

		// Persist cleanup
		if err := n.persistWAL(); err != nil {
			log.Printf("Node %d: ⚠️  Warning - failed to persist WAL cleanup: %v", n.id, err)
		}
	}
}

// cleanupAbortedWAL removes aborted WAL entries after a delay
func (n *Node) cleanupAbortedWAL(txnID string) {
	// Wait a bit before cleanup
	// time.Sleep(1 * time.Second)

	n.walMu.Lock()
	defer n.walMu.Unlock()

	if entry, exists := n.wal[txnID]; exists && entry.Status == types.WALStatusAborted {
		delete(n.wal, txnID)
		log.Printf("Node %d: 🗑️  WAL[%s]: Cleaned up aborted entry", n.id, txnID)

		// Persist cleanup
		if err := n.persistWAL(); err != nil {
			log.Printf("Node %d: ⚠️  Warning - failed to persist WAL cleanup: %v", n.id, err)
		}
	}
}

// getWALEntry retrieves a WAL entry by transaction ID
func (n *Node) getWALEntry(txnID string) (*types.WALEntry, bool) {
	n.walMu.RLock()
	defer n.walMu.RUnlock()

	entry, exists := n.wal[txnID]
	return entry, exists
}

// getWALStatus returns the status of a WAL entry
func (n *Node) getWALStatus(txnID string) (types.WALStatus, bool) {
	n.walMu.RLock()
	defer n.walMu.RUnlock()

	if entry, exists := n.wal[txnID]; exists {
		return entry.Status, true
	}
	return "", false
}
