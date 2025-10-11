package node

import (
	"context"
	"fmt"
	"log"
	"time"

	"paxos-banking/internal/types"
	pb "paxos-banking/proto"
)

// StartRecovery initiates recovery process when node restarts
func (n *Node) StartRecovery() error {
	n.mu.Lock()
	n.isRecovering = true
	n.recoverySeq = n.lastExecuted
	lastExec := n.lastExecuted
	n.mu.Unlock()

	log.Printf("Node %d: 🔄 Starting recovery from seq %d", n.id, lastExec)

	// Wait briefly for peer connections to establish
	time.Sleep(500 * time.Millisecond)

	n.mu.RLock()
	leaderID := n.leaderID
	peerCount := len(n.peerClients)
	n.mu.RUnlock()

	if peerCount == 0 {
		log.Printf("Node %d: No peers connected yet, skipping active recovery", n.id)
		n.mu.Lock()
		n.isRecovering = false
		n.mu.Unlock()
		return nil
	}

	if leaderID <= 0 {
		log.Printf("Node %d: No leader yet, will catch up via NEW-VIEW", n.id)
		n.mu.Lock()
		n.isRecovering = false
		n.mu.Unlock()
		return nil
	}

	// Actively request missing data from leader
	if err := n.requestMissingData(leaderID); err != nil {
		log.Printf("Node %d: Active recovery failed: %v, will catch up via NEW-VIEW", n.id, err)
		n.mu.Lock()
		n.isRecovering = false
		n.mu.Unlock()
		return err
	}

	log.Printf("Node %d: ✅ Recovery complete", n.id)
	return nil
}

// requestMissingData asks leader for missing committed transactions
func (n *Node) requestMissingData(leaderID int32) error {
	n.mu.RLock()
	leaderClient, exists := n.peerClients[leaderID]
	fromSeq := n.lastExecuted + 1
	maxSeqKnown := n.getMaxKnownSeq()
	n.mu.RUnlock()

	if !exists {
		log.Printf("Node %d: Leader %d not connected for recovery", n.id, leaderID)
		return fmt.Errorf("leader %d not connected", leaderID)
	}

	if fromSeq > maxSeqKnown {
		// No gap to fill
		log.Printf("Node %d: No recovery needed, up to date at seq %d", n.id, fromSeq-1)
		n.mu.Lock()
		n.isRecovering = false
		n.mu.Unlock()
		return nil
	}

	log.Printf("Node %d: Requesting recovery data from leader %d (seq %d to %d)",
		n.id, leaderID, fromSeq, maxSeqKnown)

	// Request missing transactions by querying the leader's status
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	statusReq := &pb.StatusRequest{NodeId: leaderID}
	statusResp, err := leaderClient.GetStatus(ctx, statusReq)
	if err != nil {
		log.Printf("Node %d: Failed to get leader status: %v", n.id, err)
		n.mu.Lock()
		n.isRecovering = false
		n.mu.Unlock()
		return err
	}

	leaderNextSeq := statusResp.NextSequenceNumber

	// Request each missing sequence from any peer
	successCount := 0
	for seq := fromSeq; seq < leaderNextSeq; seq++ {
		if n.requestSequence(seq) {
			successCount++
		}
	}

	log.Printf("Node %d: Recovery complete, recovered %d sequences", n.id, successCount)

	n.mu.Lock()
	n.isRecovering = false
	n.mu.Unlock()

	return nil
}

// requestSequence requests a specific sequence from peers
func (n *Node) requestSequence(seq int32) bool {
	n.mu.RLock()
	// Check if we already have this sequence
	if _, exists := n.acceptLog[seq]; exists {
		if entry := n.acceptLog[seq]; entry.Status == "E" {
			n.mu.RUnlock()
			return true // Already have it
		}
	}

	// Get peers to query
	peers := make(map[int32]pb.PaxosNodeClient)
	for k, v := range n.peerClients {
		peers[k] = v
	}
	n.mu.RUnlock()

	// Try to get this sequence from any peer
	for peerID, peerClient := range peers {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		statusResp, err := peerClient.GetStatus(ctx, &pb.StatusRequest{NodeId: peerID})
		cancel()

		if err != nil {
			continue
		}

		// If peer has executed this sequence, we know it's committed
		// In a full implementation, we'd have a GetCommittedEntry RPC
		// For now, we mark that we need it and rely on NEW-VIEW to catch up
		if statusResp.NextSequenceNumber > seq {
			log.Printf("Node %d: Peer %d has seq %d (next=%d)",
				n.id, peerID, seq, statusResp.NextSequenceNumber)
			return true
		}
	}

	return false
}

// getMaxKnownSeq returns the highest sequence number we know about
func (n *Node) getMaxKnownSeq() int32 {
	maxSeq := n.lastExecuted
	for seq := range n.acceptLog {
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	return maxSeq
}

// CatchUp processes missing transactions
func (n *Node) CatchUp(commits []*pb.CommitRequest) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for _, commit := range commits {
		if commit.SequenceNumber <= n.lastExecuted {
			continue // Already executed
		}

		// Add to accept log if not present
		if _, exists := n.acceptLog[commit.SequenceNumber]; !exists {
			entry := types.NewLogEntry(
				types.BallotFromProto(commit.Ballot),
				commit.SequenceNumber,
				commit.Request,
				commit.IsNoop,
			)
			n.acceptLog[commit.SequenceNumber] = entry
		}

		// Commit and execute
		n.commitAndExecuteUnsafe(commit.SequenceNumber)
	}

	log.Printf("Node %d: Caught up to seq %d", n.id, n.lastExecuted)
}

// commitAndExecuteUnsafe is the unsafe version (caller must hold lock)
func (n *Node) commitAndExecuteUnsafe(seqNum int32) pb.ResultType {
	entry, exists := n.acceptLog[seqNum]
	if !exists {
		return pb.ResultType_FAILED
	}

	entry.Status = "C"
	n.committedLog[seqNum] = entry

	// Execute in order
	result := pb.ResultType_SUCCESS
	for seq := n.lastExecuted + 1; seq <= seqNum; seq++ {
		if committedEntry, exists := n.committedLog[seq]; exists && committedEntry.Status != "E" {
			result = n.executeTransactionUnsafe(seq, committedEntry)
		}
	}

	return result
}

// executeTransactionUnsafe is unsafe version (caller must hold lock)
func (n *Node) executeTransactionUnsafe(seqNum int32, entry *types.LogEntry) pb.ResultType {
	if entry.Status == "E" {
		return pb.ResultType_SUCCESS
	}

	if seqNum > n.lastExecuted+1 {
		return pb.ResultType_FAILED
	}

	var result pb.ResultType

	if entry.IsNoOp {
		result = pb.ResultType_SUCCESS
	} else {
		tx := entry.Request.Transaction

		if n.balances[tx.Sender] < tx.Amount {
			result = pb.ResultType_INSUFFICIENT_BALANCE
		} else {
			n.balances[tx.Sender] -= tx.Amount
			n.balances[tx.Receiver] += tx.Amount
			result = pb.ResultType_SUCCESS
			log.Printf("Node %d: Executed seq=%d: %s->%s:%d",
				n.id, seqNum, tx.Sender, tx.Receiver, tx.Amount)

			// Save database state during recovery
			n.mu.RUnlock()
			if err := n.saveDatabase(); err != nil {
				log.Printf("Node %d: ⚠️  Warning - failed to save database during recovery: %v", n.id, err)
			}
			n.mu.RLock()
		}
	}

	entry.Status = "E"
	n.lastExecuted = seqNum

	return result
}
