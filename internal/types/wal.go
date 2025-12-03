package types

import (
	"time"
)

// WALEntry represents a Write-Ahead Log entry for transaction rollback
// Used in 2PC to undo operations if transaction fails
type WALEntry struct {
	TransactionID string                 // Unique transaction ID (for 2PC tracking)
	SequenceNum   int32                  // Paxos sequence number
	Timestamp     int64                  // When the operation was logged
	Operations    []WALOperation         // List of operations in this transaction
	Status        WALStatus              // Current status of the WAL entry
	Metadata      map[string]interface{} // Additional metadata (coordinator, participants, etc.)
}

// WALOperation represents a single operation within a transaction
type WALOperation struct {
	Type      OperationType // Type of operation
	DataItem  int32         // Which data item is affected
	OldValue  int32         // Value before operation (for undo)
	NewValue  int32         // Value after operation
	Timestamp int64         // When this operation occurred
}

// OperationType defines the type of operation in WAL
type OperationType string

const (
	OpTypeDebit  OperationType = "DEBIT"  // Subtract from balance
	OpTypeCredit OperationType = "CREDIT" // Add to balance
)

// WALStatus defines the current state of a WAL entry
type WALStatus string

const (
	WALStatusPreparing  WALStatus = "PREPARING"  // Transaction being prepared (2PC phase 1)
	WALStatusPrepared   WALStatus = "PREPARED"   // Ready to commit (2PC phase 1 complete)
	WALStatusCommitting WALStatus = "COMMITTING" // Being committed (2PC phase 2)
	WALStatusCommitted  WALStatus = "COMMITTED"  // Successfully committed
	WALStatusAborting   WALStatus = "ABORTING"   // Being rolled back
	WALStatusAborted    WALStatus = "ABORTED"    // Rolled back successfully
)

// NewWALEntry creates a new WAL entry for a transaction
func NewWALEntry(txnID string, seqNum int32) *WALEntry {
	return &WALEntry{
		TransactionID: txnID,
		SequenceNum:   seqNum,
		Timestamp:     time.Now().UnixNano(),
		Operations:    make([]WALOperation, 0),
		Status:        WALStatusPreparing,
		Metadata:      make(map[string]interface{}),
	}
}

// AddOperation adds an operation to the WAL entry
func (w *WALEntry) AddOperation(opType OperationType, dataItem, oldValue, newValue int32) {
	op := WALOperation{
		Type:      opType,
		DataItem:  dataItem,
		OldValue:  oldValue,
		NewValue:  newValue,
		Timestamp: time.Now().UnixNano(),
	}
	w.Operations = append(w.Operations, op)
}
