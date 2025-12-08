package node

import (
	"context"
	"fmt"
	"log"
	"time"

	pb "paxos-banking/proto"

	"github.com/cockroachdb/pebble"
)

var timeSecond = time.Second

// Checkpoint creates a checkpoint at a specific sequence number
func (n *Node) Checkpoint(ctx context.Context, req *pb.CheckpointRequest) (*pb.CheckpointReply, error) {
	log.Printf("Node %d: 📸 Received checkpoint request for seq %d from leader %d",
		n.id, req.SequenceNumber, req.LeaderId)

	// Verify we're not too far behind
	n.logMu.RLock()
	lastExec := n.lastExecuted
	n.logMu.RUnlock()

	if req.SequenceNumber > lastExec {
		log.Printf("Node %d: ⚠️  Checkpoint seq %d > lastExecuted %d, waiting to catch up",
			n.id, req.SequenceNumber, lastExec)
		return &pb.CheckpointReply{Success: false, NodeId: n.id}, nil
	}

	// Store checkpoint
	n.checkpointMu.Lock()
	n.lastCheckpointSeq = req.SequenceNumber

	// Deep copy the checkpoint state AND track modified items
	n.checkpointedBalance = make(map[int32]int32)
	n.modifiedItems = make(map[int32]bool)
	for k, v := range req.State {
		// Convert string keys back to int32
		var itemID int32
		fmt.Sscanf(k, "%d", &itemID)
		n.checkpointedBalance[itemID] = v
		n.modifiedItems[itemID] = true
	}
	n.checkpointMu.Unlock()

	// Persist checkpoint to disk
	if err := n.saveCheckpoint(req.SequenceNumber, req.State); err != nil {
		log.Printf("Node %d: ⚠️  Failed to persist checkpoint: %v", n.id, err)
		return &pb.CheckpointReply{Success: false, NodeId: n.id}, nil
	}

	// Truncate log entries before checkpoint
	n.truncateLogBeforeSeq(req.SequenceNumber)

	log.Printf("Node %d: ✅ Checkpoint created at seq %d, truncated log", n.id, req.SequenceNumber)
	return &pb.CheckpointReply{Success: true, NodeId: n.id}, nil
}

// GetCheckpoint retrieves the latest checkpoint
func (n *Node) GetCheckpoint(ctx context.Context, req *pb.GetCheckpointRequest) (*pb.GetCheckpointReply, error) {
	log.Printf("Node %d: 📸 GetCheckpoint request from node %d", n.id, req.NodeId)

	n.checkpointMu.RLock()
	checkpointSeq := n.lastCheckpointSeq

	// Convert checkpoint state to string map for protobuf
	state := make(map[string]int32)
	for k, v := range n.checkpointedBalance {
		state[fmt.Sprintf("%d", k)] = v
	}
	n.checkpointMu.RUnlock()

	if checkpointSeq == 0 {
		log.Printf("Node %d: No checkpoint available yet", n.id)
		return &pb.GetCheckpointReply{Success: false}, nil
	}

	return &pb.GetCheckpointReply{
		Success:        true,
		SequenceNumber: checkpointSeq,
		State:          state,
	}, nil
}

// createCheckpointIfNeeded checks if we should create a new checkpoint and does so
func (n *Node) createCheckpointIfNeeded() {
	n.paxosMu.RLock()
	isLeader := n.isLeader
	n.paxosMu.RUnlock()

	if !isLeader {
		return // Only leader creates checkpoints
	}

	n.logMu.RLock()
	lastExec := n.lastExecuted
	n.logMu.RUnlock()

	n.checkpointMu.RLock()
	lastCheckpoint := n.lastCheckpointSeq
	interval := n.checkpointInterval
	n.checkpointMu.RUnlock()

	// Check if we've executed enough transactions since last checkpoint
	if lastExec-lastCheckpoint < interval {
		return // Not enough progress yet
	}

	// Time to create a checkpoint!
	log.Printf("Node %d: 📸 Creating checkpoint at seq %d (last checkpoint: %d)",
		n.id, lastExec, lastCheckpoint)

	// Get ONLY modified items for checkpoint (massive optimization!)
	n.balanceMu.RLock()
	n.checkpointMu.RLock()
	state := make(map[string]int32)
	for itemID := range n.modifiedItems {
		if balance, exists := n.balances[itemID]; exists {
			state[fmt.Sprintf("%d", itemID)] = balance
		}
	}
	modifiedCount := len(n.modifiedItems)
	n.checkpointMu.RUnlock()
	n.balanceMu.RUnlock()

	log.Printf("Node %d: 📊 Checkpoint: %d modified items (out of 9000 total)",
		n.id, modifiedCount)

	// Create checkpoint request
	checkpointReq := &pb.CheckpointRequest{
		SequenceNumber: lastExec,
		State:          state,
		LeaderId:       n.id,
	}

	// Apply checkpoint locally first
	n.Checkpoint(context.Background(), checkpointReq)

	// Broadcast checkpoint to all peers in cluster
	n.paxosMu.RLock()
	peers := make(map[int32]pb.PaxosNodeClient)
	for k, v := range n.peerClients {
		peers[k] = v
	}
	n.paxosMu.RUnlock()

	for peerID, client := range peers {
		go func(pid int32, c pb.PaxosNodeClient) {
			// Checkpoint is fast now (~50ms), but allow buffer for network/processing
			ctx, cancel := context.WithTimeout(context.Background(), 1*timeSecond)
			defer cancel()

			reply, err := c.Checkpoint(ctx, checkpointReq)
			if err != nil {
				log.Printf("Node %d: ⚠️  Failed to send checkpoint to node %d: %v", n.id, pid, err)
			} else if reply.Success {
				log.Printf("Node %d: ✅ Node %d accepted checkpoint", n.id, pid)
			}
		}(peerID, client)
	}
}

// truncateLogBeforeSeq removes all log entries with sequence number < seq
func (n *Node) truncateLogBeforeSeq(seq int32) {
	n.logMu.Lock()
	defer n.logMu.Unlock()

	removedCount := 0
	for s := range n.log {
		if s < seq {
			delete(n.log, s)
			removedCount++
		}
	}

	if removedCount > 0 {
		log.Printf("Node %d: 🗑️  Truncated %d log entries before seq %d", n.id, removedCount, seq)
	}
}

// saveCheckpoint persists checkpoint to PebbleDB using BATCHED writes (fast!)
func (n *Node) saveCheckpoint(seq int32, state map[string]int32) error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	// Use batch for atomic, fast writes (100x faster than individual Sets)
	batch := n.pebbleDB.NewBatch()
	defer batch.Close()

	// Save checkpoint sequence number
	seqKey := []byte("checkpoint_seq")
	seqValue := []byte(fmt.Sprintf("%d", seq))
	if err := batch.Set(seqKey, seqValue, nil); err != nil {
		return fmt.Errorf("failed to add checkpoint seq to batch: %w", err)
	}

	// Add ONLY modified items to batch (no disk I/O yet)
	for itemIDStr, balance := range state {
		key := []byte(fmt.Sprintf("checkpoint_%s", itemIDStr))
		value := []byte(fmt.Sprintf("%d", balance))
		if err := batch.Set(key, value, nil); err != nil {
			return fmt.Errorf("failed to add item to batch: %w", err)
		}
	}

	// Single atomic write to disk - fast!
	if err := batch.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("failed to commit checkpoint batch: %w", err)
	}

	log.Printf("Node %d: 💾 Checkpoint persisted to disk (seq=%d, %d items)",
		n.id, seq, len(state))
	return nil
}

// loadCheckpoint loads the latest checkpoint from disk
func (n *Node) loadCheckpoint() error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	// Load checkpoint sequence number
	seqKey := []byte("checkpoint_seq")
	seqValue, closer, err := n.pebbleDB.Get(seqKey)
	if err != nil {
		// No checkpoint exists yet
		return nil
	}
	defer closer.Close()

	var checkpointSeq int32
	fmt.Sscanf(string(seqValue), "%d", &checkpointSeq)

	if checkpointSeq == 0 {
		return nil // No checkpoint
	}

	// Load checkpoint balances
	checkpointState := make(map[int32]int32)
	iter, err := n.pebbleDB.NewIter(&pebble.IterOptions{
		LowerBound: []byte("checkpoint_"),
		UpperBound: []byte("checkpoint`"), // '`' is one char after '_' in ASCII
	})
	if err != nil {
		return fmt.Errorf("failed to create iterator: %w", err)
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		key := string(iter.Key())
		if key == "checkpoint_seq" {
			continue // Skip the seq marker
		}

		// Extract item ID from key "checkpoint_12345"
		var itemID int32
		fmt.Sscanf(key, "checkpoint_%d", &itemID)

		// Parse balance value
		var balance int32
		fmt.Sscanf(string(iter.Value()), "%d", &balance)

		checkpointState[itemID] = balance
	}

	// Apply checkpoint
	n.checkpointMu.Lock()
	n.lastCheckpointSeq = checkpointSeq
	n.checkpointedBalance = checkpointState
	n.checkpointMu.Unlock()

	// Update lastExecuted to checkpoint seq
	n.logMu.Lock()
	if n.lastExecuted < checkpointSeq {
		n.lastExecuted = checkpointSeq
	}
	n.logMu.Unlock()

	log.Printf("Node %d: 📸 Loaded checkpoint from disk (seq=%d, %d items)",
		n.id, checkpointSeq, len(checkpointState))
	return nil
}
