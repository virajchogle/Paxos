package node

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
	n.checkpointMu.Lock()
	n.modifiedItems[itemID] = true
	n.checkpointMu.Unlock()
}
