package node

import (
	"encoding/json"
	"fmt"
	"log"

	"paxos-banking/internal/types"

	"github.com/cockroachdb/pebble"
)

func (n *Node) createWALEntry(txnID string, seqNum int32) *types.WALEntry {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	entry := types.NewWALEntry(txnID, seqNum)
	n.wal[txnID] = entry
	return entry
}

func (n *Node) writeToWAL(txnID string, opType types.OperationType, dataItem, oldValue, newValue int32) error {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	entry, exists := n.wal[txnID]
	if !exists {
		return fmt.Errorf("WAL entry not found for transaction %s", txnID)
	}

	entry.AddOperation(opType, dataItem, oldValue, newValue)
	return n.persistWALEntry(txnID, entry)
}

func (n *Node) commitWAL(txnID string) error {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	entry, exists := n.wal[txnID]
	if !exists {
		return fmt.Errorf("WAL entry not found for transaction %s", txnID)
	}

	entry.Status = types.WALStatusCommitted
	if err := n.persistWALEntry(txnID, entry); err != nil {
		return err
	}

	go n.cleanupCommittedWAL(txnID)
	return nil
}

func (n *Node) abortWAL(txnID string) error {
	n.walMu.Lock()
	entry, exists := n.wal[txnID]
	if !exists {
		n.walMu.Unlock()
		return fmt.Errorf("WAL entry not found for transaction %s", txnID)
	}
	entry.Status = types.WALStatusAborting
	n.walMu.Unlock()

	n.balanceMu.Lock()
	for i := len(entry.Operations) - 1; i >= 0; i-- {
		op := entry.Operations[i]
		n.balances[op.DataItem] = op.OldValue
	}
	n.balanceMu.Unlock()

	if err := n.saveDatabase(); err != nil {
		log.Printf("Node %d: Failed to save database after rollback: %v", n.id, err)
	}

	n.walMu.Lock()
	entry.Status = types.WALStatusAborted
	n.walMu.Unlock()

	if err := n.persistWALEntry(txnID, entry); err != nil {
		return err
	}

	go n.cleanupAbortedWAL(txnID)
	return nil
}

func (n *Node) persistWAL() error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	batch := n.pebbleDB.NewBatch()
	for txnID, entry := range n.wal {
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("failed to marshal WAL entry %s: %w", txnID, err)
		}

		key := []byte("wal_" + txnID)
		if err := batch.Set(key, data, pebble.NoSync); err != nil {
			return fmt.Errorf("failed to write WAL entry %s: %w", txnID, err)
		}
	}

	if err := batch.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("failed to commit WAL batch: %w", err)
	}
	return nil
}

func (n *Node) persistWALEntry(txnID string, entry *types.WALEntry) error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to marshal WAL entry: %w", err)
	}

	key := []byte("wal_" + txnID)
	if err := n.pebbleDB.Set(key, data, pebble.NoSync); err != nil {
		return fmt.Errorf("failed to write WAL entry: %w", err)
	}
	return nil
}

func (n *Node) loadWAL() error {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	if n.pebbleDB == nil {
		return nil
	}

	iter, err := n.pebbleDB.NewIter(&pebble.IterOptions{
		LowerBound: []byte("wal_"),
		UpperBound: []byte("wal`"),
	})
	if err != nil {
		return fmt.Errorf("failed to create WAL iterator: %w", err)
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		key := string(iter.Key())
		txnID := key[4:]

		var entry types.WALEntry
		if err := json.Unmarshal(iter.Value(), &entry); err != nil {
			log.Printf("Node %d: Failed to unmarshal WAL entry %s: %v", n.id, txnID, err)
			continue
		}

		n.wal[txnID] = &entry

		if entry.Status == types.WALStatusPreparing || entry.Status == types.WALStatusPrepared {
			log.Printf("Node %d: Uncommitted transaction %s in WAL (status: %s)", n.id, txnID, entry.Status)
		}
	}

	if err := iter.Error(); err != nil {
		return fmt.Errorf("WAL iteration error: %w", err)
	}
	return nil
}

func (n *Node) cleanupCommittedWAL(txnID string) {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	if entry, exists := n.wal[txnID]; exists && entry.Status == types.WALStatusCommitted {
		delete(n.wal, txnID)
		if n.pebbleDB != nil {
			key := []byte("wal_" + txnID)
			if err := n.pebbleDB.Delete(key, pebble.NoSync); err != nil {
				log.Printf("Node %d: Failed to delete WAL entry: %v", n.id, err)
			}
		}
	}
}

func (n *Node) cleanupAbortedWAL(txnID string) {
	n.walMu.Lock()
	defer n.walMu.Unlock()

	if entry, exists := n.wal[txnID]; exists && entry.Status == types.WALStatusAborted {
		delete(n.wal, txnID)
		if n.pebbleDB != nil {
			key := []byte("wal_" + txnID)
			if err := n.pebbleDB.Delete(key, pebble.NoSync); err != nil {
				log.Printf("Node %d: Failed to delete WAL entry: %v", n.id, err)
			}
		}
	}
}

func (n *Node) getWALEntry(txnID string) (*types.WALEntry, bool) {
	n.walMu.RLock()
	defer n.walMu.RUnlock()
	entry, exists := n.wal[txnID]
	return entry, exists
}

func (n *Node) getWALStatus(txnID string) (types.WALStatus, bool) {
	n.walMu.RLock()
	defer n.walMu.RUnlock()
	if entry, exists := n.wal[txnID]; exists {
		return entry.Status, true
	}
	return "", false
}
