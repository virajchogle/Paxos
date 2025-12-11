package node

// getClusterForDataItem returns the cluster that owns this data item
// This is migration-aware: checks if item was migrated INTO or OUT OF this cluster
func (n *Node) getClusterForDataItem(itemID int32) int32 {
	n.balanceMu.RLock()
	isMigratedIn := n.migratedInItems != nil && n.migratedInItems[itemID]
	isMigratedOut := n.migratedOutItems != nil && n.migratedOutItems[itemID]
	n.balanceMu.RUnlock()

	if isMigratedIn {
		// Item was migrated INTO this cluster - it belongs here now
		return n.clusterID
	}

	if isMigratedOut {
		// Item was migrated OUT of this cluster
		// Return a different cluster to indicate we don't own it
		// We use -1 as a sentinel to indicate "not this cluster"
		// The caller should check ownsDataItem() for accurate ownership
		return -1
	}

	// Use original config-based assignment
	return int32(n.config.GetClusterForDataItem(itemID))
}

// ownsDataItem returns true if this node's cluster owns the given data item
// This is migration-aware and the definitive check for ownership
func (n *Node) ownsDataItem(itemID int32) bool {
	n.balanceMu.RLock()
	defer n.balanceMu.RUnlock()

	// If migrated OUT, we definitely don't own it
	if n.migratedOutItems != nil && n.migratedOutItems[itemID] {
		return false
	}

	// If migrated IN, we definitely own it
	if n.migratedInItems != nil && n.migratedInItems[itemID] {
		return true
	}

	// Check original shard assignment
	originalCluster := n.config.GetClusterForDataItem(itemID)
	return int32(originalCluster) == n.clusterID
}

// getBalance returns the balance for an item, with default value for unmodified items
// This is a key optimization: we don't store all 9000 items, only modified ones
func (n *Node) getBalance(itemID int32) int32 {
	n.balanceMu.RLock()
	balance, exists := n.balances[itemID]
	n.balanceMu.RUnlock()

	if exists {
		return balance
	}

	// Return default initial balance for unmodified items
	return n.config.Data.InitialBalance
}

// setBalance sets the balance and tracks it as modified
func (n *Node) setBalance(itemID int32, balance int32) {
	n.balanceMu.Lock()
	n.balances[itemID] = balance
	n.balanceMu.Unlock()

	// Track as modified for checkpointing
	n.trackModifiedItem(itemID)
}

// trackModifiedItem marks an item as modified for checkpointing
// Call this after modifying a balance (can be called without holding balanceMu)
func (n *Node) trackModifiedItem(itemID int32) {
	n.checkpointMu.Lock()
	n.modifiedItems[itemID] = true
	n.checkpointMu.Unlock()
}
