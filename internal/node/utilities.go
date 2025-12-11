package node

// getClusterForDataItem returns the cluster that owns this data item (migration-aware)
func (n *Node) getClusterForDataItem(itemID int32) int32 {
	n.balanceMu.RLock()
	isMigratedIn := n.migratedInItems != nil && n.migratedInItems[itemID]
	isMigratedOut := n.migratedOutItems != nil && n.migratedOutItems[itemID]
	n.balanceMu.RUnlock()

	if isMigratedIn {
		return n.clusterID
	}

	if isMigratedOut {
		return -1 // Not owned by this cluster
	}

	return int32(n.config.GetClusterForDataItem(itemID))
}

// ownsDataItem returns true if this node's cluster owns the data item (migration-aware)
func (n *Node) ownsDataItem(itemID int32) bool {
	n.balanceMu.RLock()
	defer n.balanceMu.RUnlock()

	if n.migratedOutItems != nil && n.migratedOutItems[itemID] {
		return false
	}

	if n.migratedInItems != nil && n.migratedInItems[itemID] {
		return true
	}

	originalCluster := n.config.GetClusterForDataItem(itemID)
	return int32(originalCluster) == n.clusterID
}

// getBalance returns the balance for an item
func (n *Node) getBalance(itemID int32) int32 {
	n.balanceMu.RLock()
	balance, exists := n.balances[itemID]
	n.balanceMu.RUnlock()

	if exists {
		return balance
	}

	return n.config.Data.InitialBalance
}

// setBalance sets the balance and tracks it as modified
func (n *Node) setBalance(itemID int32, balance int32) {
	n.balanceMu.Lock()
	n.balances[itemID] = balance
	n.balanceMu.Unlock()

	n.trackModifiedItem(itemID)
}

// trackModifiedItem marks an item as modified for checkpointing
func (n *Node) trackModifiedItem(itemID int32) {
	n.checkpointMu.Lock()
	n.modifiedItems[itemID] = true
	n.checkpointMu.Unlock()
}
