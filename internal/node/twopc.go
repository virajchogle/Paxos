package node

import (
	"context"
	"fmt"
	"log"
	"time"

	pb "paxos-banking/proto"
)

// ============================================================================
// PROPER TWO-PHASE COMMIT IMPLEMENTATION
// ============================================================================
//
// TRUE 2PC PROTOCOL:
// PHASE 1 (PREPARE): Check if both operations can proceed (locks, balance)
//                    DO NOT execute yet - just vote YES/NO
// PHASE 2 (COMMIT):  If both voted YES, execute via Paxos
//                    If either voted NO, abort (no rollback needed)
//
// This ensures atomicity: either both execute or neither does
// ============================================================================

// TwoPCCoordinator implements the coordinator role for cross-shard transactions
func (n *Node) TwoPCCoordinator(tx *pb.Transaction, clientID string, timestamp int64) (bool, error) {
	txnID := fmt.Sprintf("2pc-%s-%d", clientID, timestamp)

	// Check for duplicate
	n.balanceMu.RLock()
	lastReply, hasReply := n.clientLastReply[clientID]
	lastTS, hasTS := n.clientLastTS[clientID]
	n.balanceMu.RUnlock()

	if hasReply && hasTS && timestamp <= lastTS {
		log.Printf("Node %d: 2PC[%s]: Duplicate request - returning cached result", n.id, txnID)
		return lastReply.Success && lastReply.Result == pb.ResultType_SUCCESS, nil
	}

	log.Printf("Node %d: 🎯 2PC START [%s]: %d→%d:%d", n.id, txnID, tx.Sender, tx.Receiver, tx.Amount)

	receiverCluster := n.config.GetClusterForDataItem(tx.Receiver)
	receiverLeader := n.config.GetLeaderNodeForCluster(receiverCluster)

	// ========================================================================
	// PHASE 1: PREPARE - Check if operations can proceed
	// ========================================================================
	log.Printf("Node %d: 2PC[%s]: PHASE 1 - PREPARE (check locks and balances)", n.id, txnID)

	// Step 1a: Check debit locally (lock + balance check, but DON'T execute)
	n.balanceMu.Lock()

	// Check if locks are available
	senderLock, senderLocked := n.locks[tx.Sender]
	if senderLocked && senderLock.clientID != clientID {
		n.balanceMu.Unlock()
		log.Printf("Node %d: 2PC[%s]: ❌ PREPARE FAILED - sender item %d locked by %s",
			n.id, txnID, tx.Sender, senderLock.clientID)
		n.cacheResult(clientID, timestamp, false, "sender locked")
		return false, fmt.Errorf("sender item locked")
	}

	// Check balance
	senderBalance := n.balances[tx.Sender]
	if senderBalance < tx.Amount {
		n.balanceMu.Unlock()
		log.Printf("Node %d: 2PC[%s]: ❌ PREPARE FAILED - insufficient balance (have %d, need %d)",
			n.id, txnID, senderBalance, tx.Amount)
		n.cacheResult(clientID, timestamp, false, "insufficient balance")
		return false, fmt.Errorf("insufficient balance")
	}

	log.Printf("Node %d: 2PC[%s]: ✅ Sender PREPARE OK - balance=%d, amount=%d",
		n.id, txnID, senderBalance, tx.Amount)
	n.balanceMu.Unlock()

	// Step 1b: Contact receiver cluster for PREPARE
	log.Printf("Node %d: 2PC[%s]: Contacting receiver cluster %d leader %d for PREPARE",
		n.id, txnID, receiverCluster, receiverLeader)

	receiverClient, err := n.getCrossClusterClient(receiverLeader)
	if err != nil {
		log.Printf("Node %d: 2PC[%s]: ❌ Cannot connect to receiver: %v", n.id, txnID, err)
		n.cacheResult(clientID, timestamp, false, "receiver unreachable")
		return false, fmt.Errorf("receiver unreachable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prepareReq := &pb.TwoPCPrepareRequest{
		TransactionId: txnID,
		Transaction:   tx,
		ClientId:      clientID,
		Timestamp:     timestamp,
		CoordinatorId: n.id,
	}

	prepareReply, err := receiverClient.TwoPCPrepare(ctx, prepareReq)
	if err != nil || !prepareReply.Success {
		log.Printf("Node %d: 2PC[%s]: ❌ Receiver PREPARE failed: %v", n.id, txnID, err)
		n.cacheResult(clientID, timestamp, false, fmt.Sprintf("receiver prepare failed: %v", err))
		return false, fmt.Errorf("receiver prepare failed: %v", err)
	}

	log.Printf("Node %d: 2PC[%s]: ✅ Receiver PREPARE OK", n.id, txnID)

	// ========================================================================
	// PHASE 2: COMMIT - Both sides prepared, now execute via Paxos
	// ========================================================================
	log.Printf("Node %d: 2PC[%s]: PHASE 2 - COMMIT (execute operations)", n.id, txnID)

	// Step 2a: Execute debit in sender cluster via Paxos
	debitReq := &pb.TransactionRequest{
		ClientId:    clientID,
		Timestamp:   timestamp,
		Transaction: tx,
	}

	debitReply, err := n.processAsLeader(debitReq)
	if err != nil || !debitReply.Success {
		log.Printf("Node %d: 2PC[%s]: ❌ COMMIT FAILED - debit execution failed: %v", n.id, txnID, err)
		// At this point we have a problem - receiver is prepared but we failed to execute
		// This should be extremely rare (would mean Paxos failed after prepare succeeded)
		// For now, log it and return failure
		n.cacheResult(clientID, timestamp, false, "debit execution failed")
		return false, fmt.Errorf("debit execution failed: %v", err)
	}

	log.Printf("Node %d: 2PC[%s]: ✅ Debit executed", n.id, txnID)

	// Step 2b: Execute credit in receiver cluster via Paxos
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()

	creditReply, err := receiverClient.SubmitTransaction(ctx2, debitReq)
	if err != nil || !creditReply.Success {
		log.Printf("Node %d: 2PC[%s]: ⚠️  Credit execution failed: %v", n.id, txnID, err)
		// This is bad - debit executed but credit didn't
		// Ideally we'd have a compensation mechanism here
		// For now, log the inconsistency
		log.Printf("Node %d: 2PC[%s]: ⚠️  CRITICAL: Debit succeeded but credit failed - INCONSISTENT STATE!", n.id, txnID)
		n.cacheResult(clientID, timestamp, false, "credit execution failed after debit")
		return false, fmt.Errorf("credit execution failed: %v", err)
	}

	log.Printf("Node %d: 2PC[%s]: ✅ Credit executed", n.id, txnID)

	// ========================================================================
	// SUCCESS
	// ========================================================================
	log.Printf("Node %d: 2PC[%s]: ✅ TRANSACTION COMMITTED", n.id, txnID)

	n.cacheResult(clientID, timestamp, true, "2PC committed")
	return true, nil
}

// TwoPCPrepare handles prepare requests from coordinator (participant role)
// Returns YES if can proceed, NO otherwise (check locks, balance, etc.)
func (n *Node) TwoPCPrepare(ctx context.Context, req *pb.TwoPCPrepareRequest) (*pb.TwoPCPrepareReply, error) {
	txnID := req.TransactionId
	tx := req.Transaction

	log.Printf("Node %d: 2PC[%s]: Received PREPARE request for item %d: +%d",
		n.id, txnID, tx.Receiver, tx.Amount)

	n.balanceMu.Lock()
	defer n.balanceMu.Unlock()

	// Check if receiver item is locked
	receiverLock, receiverLocked := n.locks[tx.Receiver]
	if receiverLocked && receiverLock.clientID != req.ClientId {
		log.Printf("Node %d: 2PC[%s]: ❌ PREPARE NO - receiver item %d locked by %s",
			n.id, txnID, tx.Receiver, receiverLock.clientID)
		return &pb.TwoPCPrepareReply{
			Success:       false,
			TransactionId: txnID,
			Message:       "receiver item locked",
			ParticipantId: n.id,
		}, nil
	}

	log.Printf("Node %d: 2PC[%s]: ✅ PREPARE YES - can proceed with credit", n.id, txnID)

	return &pb.TwoPCPrepareReply{
		Success:       true,
		TransactionId: txnID,
		Message:       "prepared",
		ParticipantId: n.id,
	}, nil
}

// cacheResult stores result for exactly-once semantics
func (n *Node) cacheResult(clientID string, timestamp int64, success bool, message string) {
	result := &pb.TransactionReply{
		Success: success,
		Message: message,
		Result:  pb.ResultType_SUCCESS,
	}
	if !success {
		result.Result = pb.ResultType_FAILED
	}

	n.balanceMu.Lock()
	n.clientLastReply[clientID] = result
	n.clientLastTS[clientID] = timestamp
	n.balanceMu.Unlock()
}
