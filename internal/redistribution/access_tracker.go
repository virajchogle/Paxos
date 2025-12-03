package redistribution

import (
	"sync"
	"time"
)

// AccessPattern tracks how data items are accessed together
type AccessPattern struct {
	mu sync.RWMutex

	// Co-access frequency: items that are accessed together
	// key = pair of items (smaller, larger), value = count
	coAccessCount map[ItemPair]int64

	// Item access frequency
	itemAccessCount map[int32]int64

	// Transaction history (rolling window)
	transactionHistory []*TransactionRecord
	historyWindowSize  int

	// Time tracking
	startTime time.Time
	lastReset time.Time
}

// ItemPair represents a pair of data items (always ordered: First < Second)
type ItemPair struct {
	First  int32
	Second int32
}

// NewItemPair creates a properly ordered item pair
func NewItemPair(a, b int32) ItemPair {
	if a < b {
		return ItemPair{First: a, Second: b}
	}
	return ItemPair{First: b, Second: a}
}

// TransactionRecord records a transaction for analysis
type TransactionRecord struct {
	Sender    int32
	Receiver  int32
	Timestamp time.Time
	IsCross   bool // Was it a cross-shard transaction?
}

// NewAccessTracker creates a new access pattern tracker
func NewAccessTracker(historyWindowSize int) *AccessTracker {
	if historyWindowSize <= 0 {
		historyWindowSize = 100000 // Default: keep last 100K transactions
	}

	return &AccessTracker{
		coAccessCount:      make(map[ItemPair]int64),
		itemAccessCount:    make(map[int32]int64),
		transactionHistory: make([]*TransactionRecord, 0, historyWindowSize),
		historyWindowSize:  historyWindowSize,
		startTime:          time.Now(),
		lastReset:          time.Now(),
	}
}

// AccessTracker is the main struct (renamed from AccessPattern for clarity)
type AccessTracker struct {
	mu sync.RWMutex

	coAccessCount      map[ItemPair]int64
	itemAccessCount    map[int32]int64
	transactionHistory []*TransactionRecord
	historyWindowSize  int
	startTime          time.Time
	lastReset          time.Time
}

// RecordTransaction records a transaction for pattern analysis
func (at *AccessTracker) RecordTransaction(sender, receiver int32, isCross bool) {
	at.mu.Lock()
	defer at.mu.Unlock()

	// Record individual item access
	at.itemAccessCount[sender]++
	at.itemAccessCount[receiver]++

	// Record co-access pattern
	pair := NewItemPair(sender, receiver)
	at.coAccessCount[pair]++

	// Add to history
	record := &TransactionRecord{
		Sender:    sender,
		Receiver:  receiver,
		Timestamp: time.Now(),
		IsCross:   isCross,
	}

	at.transactionHistory = append(at.transactionHistory, record)

	// Trim history if needed
	if len(at.transactionHistory) > at.historyWindowSize {
		// Remove oldest entries
		trimCount := len(at.transactionHistory) - at.historyWindowSize
		at.transactionHistory = at.transactionHistory[trimCount:]
	}
}

// GetCoAccessCount returns co-access count for an item pair
func (at *AccessTracker) GetCoAccessCount(a, b int32) int64 {
	at.mu.RLock()
	defer at.mu.RUnlock()

	pair := NewItemPair(a, b)
	return at.coAccessCount[pair]
}

// GetItemAccessCount returns access count for a single item
func (at *AccessTracker) GetItemAccessCount(item int32) int64 {
	at.mu.RLock()
	defer at.mu.RUnlock()

	return at.itemAccessCount[item]
}

// GetTopCoAccessPairs returns the top N most frequently co-accessed pairs
func (at *AccessTracker) GetTopCoAccessPairs(n int) []CoAccessEntry {
	at.mu.RLock()
	defer at.mu.RUnlock()

	// Convert to slice
	entries := make([]CoAccessEntry, 0, len(at.coAccessCount))
	for pair, count := range at.coAccessCount {
		entries = append(entries, CoAccessEntry{
			Pair:  pair,
			Count: count,
		})
	}

	// Sort by count (descending)
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Count > entries[i].Count {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	// Return top N
	if n > len(entries) {
		n = len(entries)
	}
	return entries[:n]
}

// CoAccessEntry represents a co-access pair with its count
type CoAccessEntry struct {
	Pair  ItemPair
	Count int64
}

// GetCrossShardTransactionCount returns count of cross-shard transactions
func (at *AccessTracker) GetCrossShardTransactionCount() int64 {
	at.mu.RLock()
	defer at.mu.RUnlock()

	var count int64
	for _, record := range at.transactionHistory {
		if record.IsCross {
			count++
		}
	}
	return count
}

// GetTotalTransactionCount returns total transaction count
func (at *AccessTracker) GetTotalTransactionCount() int64 {
	at.mu.RLock()
	defer at.mu.RUnlock()

	return int64(len(at.transactionHistory))
}

// GetCrossShardRatio returns the ratio of cross-shard to total transactions
func (at *AccessTracker) GetCrossShardRatio() float64 {
	total := at.GetTotalTransactionCount()
	if total == 0 {
		return 0
	}
	cross := at.GetCrossShardTransactionCount()
	return float64(cross) / float64(total)
}

// GetCoAccessMatrix returns the full co-access matrix for analysis
func (at *AccessTracker) GetCoAccessMatrix() map[ItemPair]int64 {
	at.mu.RLock()
	defer at.mu.RUnlock()

	// Return a copy
	result := make(map[ItemPair]int64, len(at.coAccessCount))
	for k, v := range at.coAccessCount {
		result[k] = v
	}
	return result
}

// GetItemAccessMap returns the item access frequency map
func (at *AccessTracker) GetItemAccessMap() map[int32]int64 {
	at.mu.RLock()
	defer at.mu.RUnlock()

	result := make(map[int32]int64, len(at.itemAccessCount))
	for k, v := range at.itemAccessCount {
		result[k] = v
	}
	return result
}

// Reset clears all tracking data
func (at *AccessTracker) Reset() {
	at.mu.Lock()
	defer at.mu.Unlock()

	at.coAccessCount = make(map[ItemPair]int64)
	at.itemAccessCount = make(map[int32]int64)
	at.transactionHistory = make([]*TransactionRecord, 0, at.historyWindowSize)
	at.lastReset = time.Now()
}

// GetStats returns summary statistics
func (at *AccessTracker) GetStats() AccessStats {
	at.mu.RLock()
	defer at.mu.RUnlock()

	var crossCount int64
	for _, r := range at.transactionHistory {
		if r.IsCross {
			crossCount++
		}
	}

	return AccessStats{
		TotalTransactions:      int64(len(at.transactionHistory)),
		CrossShardTransactions: crossCount,
		UniqueCoAccessPairs:    int64(len(at.coAccessCount)),
		UniqueItemsAccessed:    int64(len(at.itemAccessCount)),
		TrackingSince:          at.startTime,
		LastReset:              at.lastReset,
	}
}

// AccessStats contains summary statistics
type AccessStats struct {
	TotalTransactions      int64
	CrossShardTransactions int64
	UniqueCoAccessPairs    int64
	UniqueItemsAccessed    int64
	TrackingSince          time.Time
	LastReset              time.Time
}
