package node

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"paxos-banking/internal/types"
	pb "paxos-banking/proto"
)

// ============================================================================
// FULL TWO-PHASE COMMIT IMPLEMENTATION (Per Specification)
// ============================================================================
//
// CORRECTED PROTOCOL FLOW (WITH PARALLELISM):
// 1. Client → Coordinator: REQUEST with cross-shard transaction
// 2. Coordinator checks: no lock on sender, balance >= amount
// 3. Coordinator locks sender item
// 4. **PARALLEL EXECUTION**:
//    - Coordinator → Participant: PREPARE message (non-blocking)
//    - Coordinator: Run Paxos PREPARE ('P') in own cluster
// 5. Wait for BOTH:
//    - Own Paxos consensus achieved
//    - PREPARED message received from participant
// 6. Coordinator: Run Paxos COMMIT ('C') with SAME sequence number
// 7. Coordinator → Participant: COMMIT message
// 8. Both release locks, delete WAL
//
// KEY: Steps 4-5 happen IN PARALLEL for performance
// ============================================================================

// TwoPCState tracks active 2PC transactions
type TwoPCState struct {
	mu           sync.RWMutex
	transactions map[string]*TwoPCTransaction // txnID -> state
}

// TwoPCTransaction represents a 2PC transaction in progress
type TwoPCTransaction struct {
	TxnID       string
	Transaction *pb.Transaction
	ClientID    string
	Timestamp   int64
	Phase       string // "PREPARE", "COMMIT", "ABORT"
	PrepareSeq  int32  // Sequence number for prepare phase (REUSED for commit)
	CommitSeq   int32  // Sequence number for commit phase
	Prepared    bool
	Committed   bool
	LockedItems []int32
	WALEntries  map[int32]int32 // item_id -> old_balance (for rollback)
	CreatedAt   time.Time
	LastContact time.Time
}

// ============================================================================
// COORDINATOR ROLE
// ============================================================================

// TwoPCCoordinator implements the coordinator role for cross-shard transactions
func (n *Node) TwoPCCoordinator(tx *pb.Transaction, clientID string, timestamp int64) (bool, error) {
	txnID := fmt.Sprintf("2pc-%s-%d", clientID, timestamp)

	// Check for duplicate
	n.clientMu.RLock()
	lastReply, hasReply := n.clientLastReply[clientID]
	lastTS, hasTS := n.clientLastTS[clientID]
	n.clientMu.RUnlock()

	if hasReply && hasTS && timestamp <= lastTS {
		log.Printf("Node %d: 2PC[%s]: Duplicate request - returning cached result", n.id, txnID)
		return lastReply.Success && lastReply.Result == pb.ResultType_SUCCESS, nil
	}

	log.Printf("Node %d: 🎯 2PC START [%s]: %d→%d:%d (COORDINATOR)", n.id, txnID, tx.Sender, tx.Receiver, tx.Amount)

	receiverCluster := n.config.GetClusterForDataItem(tx.Receiver)
	receiverLeader := n.config.GetLeaderNodeForCluster(receiverCluster)

	// ========================================================================
	// PHASE 1: PREPARE
	// ========================================================================
	log.Printf("Node %d: 2PC[%s]: PHASE 1 - PREPARE", n.id, txnID)

	// Step 1: Check and lock sender item
	n.balanceMu.Lock()

	// Check if sender is locked
	senderLock, senderLocked := n.locks[tx.Sender]
	if senderLocked && senderLock.clientID != clientID {
		n.balanceMu.Unlock()
		log.Printf("Node %d: 2PC[%s]: ❌ Sender item %d locked by %s", n.id, txnID, tx.Sender, senderLock.clientID)
		n.cacheResult(clientID, timestamp, false, pb.ResultType_FAILED)
		return false, fmt.Errorf("sender item locked")
	}

	// Check balance
	senderBalance := n.balances[tx.Sender]
	if senderBalance < tx.Amount {
		n.balanceMu.Unlock()
		log.Printf("Node %d: 2PC[%s]: ❌ Insufficient balance (have %d, need %d)", n.id, txnID, senderBalance, tx.Amount)
		n.cacheResult(clientID, timestamp, false, pb.ResultType_INSUFFICIENT_BALANCE)
		return false, fmt.Errorf("insufficient balance")
	}

	// Lock sender item and save old balance to WAL
	log.Printf("Node %d: 2PC[%s]: 🔒 Locking sender item %d (balance: %d)", n.id, txnID, tx.Sender, senderBalance)
	n.locks[tx.Sender] = &DataItemLock{
		clientID:  clientID,
		timestamp: timestamp,
		lockedAt:  time.Now(),
	}

	// Initialize 2PC state
	if n.twoPCState.transactions == nil {
		n.twoPCState.transactions = make(map[string]*TwoPCTransaction)
	}
	n.twoPCState.transactions[txnID] = &TwoPCTransaction{
		TxnID:       txnID,
		Transaction: tx,
		ClientID:    clientID,
		Timestamp:   timestamp,
		Phase:       "PREPARE",
		LockedItems: []int32{tx.Sender},
		WALEntries:  map[int32]int32{tx.Sender: senderBalance},
		CreatedAt:   time.Now(),
		LastContact: time.Now(),
	}

	n.balanceMu.Unlock()

	// ========================================================================
	// PARALLEL EXECUTION: Send PREPARE + Run own Paxos simultaneously
	// ========================================================================

	// Get receiver client before spawning goroutine
	receiverClient, err := n.getCrossClusterClient(int32(receiverLeader))
	if err != nil {
		log.Printf("Node %d: 2PC[%s]: ❌ Cannot connect to participant: %v", n.id, txnID, err)
		n.cleanup2PCCoordinator(txnID, false)
		n.cacheResult(clientID, timestamp, false, pb.ResultType_FAILED)
		return false, fmt.Errorf("participant unreachable: %v", err)
	}

	prepareReq := &pb.TransactionRequest{
		ClientId:    clientID,
		Timestamp:   timestamp,
		Transaction: tx,
	}

	// Channels for parallel execution
	type coordinatorResult struct {
		reply *pb.TransactionReply
		seq   int32
		err   error
	}
	type participantResult struct {
		reply *pb.TwoPCPrepareReply
		err   error
	}

	coordChan := make(chan coordinatorResult, 1)
	partChan := make(chan participantResult, 1)

	// Step 2: Send PREPARE to participant (parallel, non-blocking)
	go func() {
		log.Printf("Node %d: 2PC[%s]: Sending PREPARE to participant cluster %d leader %d",
			n.id, txnID, receiverCluster, receiverLeader)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		prepareMsg := &pb.TwoPCPrepareRequest{
			TransactionId: txnID,
			Transaction:   tx,
			ClientId:      clientID,
			Timestamp:     timestamp,
			CoordinatorId: n.id,
		}

		preparedReply, err := receiverClient.TwoPCPrepare(ctx, prepareMsg)
		partChan <- participantResult{reply: preparedReply, err: err}
	}()

	// Step 3: Run Paxos in coordinator cluster (parallel)
	go func() {
		log.Printf("Node %d: 2PC[%s]: Running Paxos for PREPARE phase (marker: 'P')", n.id, txnID)

		prepareReply, prepareSeq, err := n.processAsLeaderWithPhaseAndSeq(prepareReq, "P", 0)
		coordChan <- coordinatorResult{reply: prepareReply, seq: prepareSeq, err: err}
	}()

	// Step 4: Wait for BOTH to complete
	log.Printf("Node %d: 2PC[%s]: Waiting for both coordinator Paxos and participant PREPARED", n.id, txnID)

	var coordResult coordinatorResult
	var partResult participantResult
	receivedCoord := false
	receivedPart := false

	for i := 0; i < 2; i++ {
		select {
		case coordResult = <-coordChan:
			receivedCoord = true
			if coordResult.err != nil || !coordResult.reply.Success {
				log.Printf("Node %d: 2PC[%s]: ❌ Coordinator PREPARE consensus failed: %v", n.id, txnID, coordResult.err)
				// Wait for participant response before aborting
				if !receivedPart {
					partResult = <-partChan
				}
				return n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, "coordinator prepare failed")
			}
			log.Printf("Node %d: 2PC[%s]: ✅ Coordinator PREPARE complete (seq: %d)", n.id, txnID, coordResult.seq)

			// Save sequence number for reuse in COMMIT phase
			n.balanceMu.Lock()
			if txState, exists := n.twoPCState.transactions[txnID]; exists {
				txState.PrepareSeq = coordResult.seq
			}
			n.balanceMu.Unlock()

		case partResult = <-partChan:
			receivedPart = true
			if partResult.err != nil || !partResult.reply.Success {
				errorMsg := fmt.Sprintf("%v", partResult.err)
				if partResult.reply != nil {
					errorMsg = partResult.reply.Message
				}
				log.Printf("Node %d: 2PC[%s]: ❌ Participant PREPARE failed: %s", n.id, txnID, errorMsg)

				// Check if failure is due to lock conflict
				isLockConflict := partResult.reply != nil && len(partResult.reply.Message) >= 7 && partResult.reply.Message[:7] == "LOCKED:"

				// Wait for coordinator response before aborting
				if !receivedCoord {
					coordResult = <-coordChan
				}

				// If lock conflict, mark as permanently failed (not retryable)
				if isLockConflict {
					log.Printf("Node %d: 2PC[%s]: ❌ LOCK CONFLICT - transaction permanently FAILED (will NOT be retried)", n.id, txnID)
					// Abort and cache result as permanently FAILED
					n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, fmt.Sprintf("lock conflict: %s", errorMsg))
					// Cache result to prevent any retries
					n.cacheResult(clientID, timestamp, false, pb.ResultType_FAILED)
					return false, fmt.Errorf("transaction permanently failed due to lock conflict: %s", errorMsg)
				}

				return n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, fmt.Sprintf("participant prepare failed: %s", errorMsg))
			}
			log.Printf("Node %d: 2PC[%s]: ✅ Participant PREPARED", n.id, txnID)
		}
	}

	// Both PREPARE phases completed successfully!
	log.Printf("Node %d: 2PC[%s]: ✅ PREPARE phase complete on both clusters", n.id, txnID)

	// ========================================================================
	// PHASE 2: COMMIT
	// ========================================================================
	log.Printf("Node %d: 2PC[%s]: PHASE 2 - COMMIT", n.id, txnID)

	// Get the sequence number from PREPARE phase
	n.balanceMu.RLock()
	txState, exists := n.twoPCState.transactions[txnID]
	if !exists || txState == nil {
		n.balanceMu.RUnlock()
		log.Printf("Node %d: 2PC[%s]: ❌ CRITICAL ERROR - transaction state missing in COMMIT phase", n.id, txnID)
		return n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, "transaction state lost")
	}
	prepareSeq := txState.PrepareSeq
	n.balanceMu.RUnlock()

	// Step 2a: Run Paxos to replicate COMMIT entry with SAME sequence number
	log.Printf("Node %d: 2PC[%s]: Running Paxos for COMMIT phase (marker: 'C', reusing seq: %d)", n.id, txnID, prepareSeq)

	commitReply, _, err := n.processAsLeaderWithPhaseAndSeq(prepareReq, "C", prepareSeq)
	if err != nil || commitReply == nil || !commitReply.Success {
		// Determine failure reason from reply or error
		failureReason := "unknown"
		if commitReply != nil && commitReply.Message != "" {
			failureReason = commitReply.Message
		} else if err != nil {
			failureReason = err.Error()
		}

		log.Printf("Node %d: 2PC[%s]: ❌ COMMIT consensus failed: %s", n.id, txnID, failureReason)
		// This is bad - participant prepared but we can't commit
		return n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, fmt.Sprintf("commit consensus failed: %s", failureReason))
	}

	log.Printf("Node %d: 2PC[%s]: ✅ COMMIT replicated in coordinator cluster", n.id, txnID)

	// Step 2b: Send COMMIT to participant (non-blocking with background retry)
	log.Printf("Node %d: 2PC[%s]: Sending COMMIT to participant", n.id, txnID)

	commitMsg := &pb.TwoPCCommitRequest{
		TransactionId: txnID,
		CoordinatorId: n.id,
	}

	// Send initial COMMIT message (non-blocking - spawn goroutine for retries)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		commitAck, err := receiverClient.TwoPCCommit(ctx, commitMsg)
		if err != nil || !commitAck.Success {
			log.Printf("Node %d: 2PC[%s]: ⚠️  Participant COMMIT ACK not received, retrying in background...", n.id, txnID)
			// Retry in background (participant may be slow or message lost)
			for retry := 0; retry < 5; retry++ {
				time.Sleep(1 * time.Second)
				ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
				commitAck, err = receiverClient.TwoPCCommit(ctx2, commitMsg)
				cancel2()
				if err == nil && commitAck.Success {
					log.Printf("Node %d: 2PC[%s]: ✅ Participant ACK received after retry %d", n.id, txnID, retry+1)
					return
				}
			}
			log.Printf("Node %d: 2PC[%s]: ⚠️  Participant ACK not received after retries (participant will commit eventually)", n.id, txnID)
		} else {
			log.Printf("Node %d: 2PC[%s]: ✅ Participant ACK received", n.id, txnID)
		}
	}()

	// Step 2c: Cleanup coordinator state IMMEDIATELY (don't wait for participant ACK!)
	// CRITICAL: Coordinator's cluster has committed, transaction is durable!
	// Participant will commit on its replicas (execution already done in PREPARE)
	// Locks can be released now, client can be notified now!
	n.cleanup2PCCoordinator(txnID, true)

	log.Printf("Node %d: 2PC[%s]: ✅ TRANSACTION COMMITTED (locks released, participant will ACK in background)", n.id, txnID)
	n.cacheResult(clientID, timestamp, true, pb.ResultType_SUCCESS)
	return true, nil
}

// coordinatorAbort handles abort from coordinator side
func (n *Node) coordinatorAbort(txnID, clientID string, timestamp int64, participantClient pb.PaxosNodeClient, reason string) (bool, error) {
	log.Printf("Node %d: 2PC[%s]: ❌ ABORT - %s", n.id, txnID, reason)

	// Run Paxos to replicate ABORT entry (marker: 'A')
	n.balanceMu.RLock()
	txState, exists := n.twoPCState.transactions[txnID]
	n.balanceMu.RUnlock()

	if exists && txState != nil {
		abortReq := &pb.TransactionRequest{
			ClientId:    clientID,
			Timestamp:   timestamp,
			Transaction: txState.Transaction,
		}

		log.Printf("Node %d: 2PC[%s]: Running Paxos for ABORT phase (marker: 'A')", n.id, txnID)

		// Use same sequence if we have one from PREPARE, otherwise get new
		seq := txState.PrepareSeq
		_, _, err := n.processAsLeaderWithPhaseAndSeq(abortReq, "A", seq)
		if err != nil {
			log.Printf("Node %d: 2PC[%s]: ⚠️  ABORT consensus failed: %v", n.id, txnID, err)
		}
	}

	// Send ABORT to participant
	if participantClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		abortMsg := &pb.TwoPCAbortRequest{
			TransactionId: txnID,
			CoordinatorId: n.id,
			Reason:        reason,
		}

		_, err := participantClient.TwoPCAbort(ctx, abortMsg)
		if err != nil {
			log.Printf("Node %d: 2PC[%s]: ⚠️  Failed to send ABORT to participant: %v", n.id, txnID, err)
		}
	}

	// Cleanup (rollback using WAL)
	n.cleanup2PCCoordinator(txnID, false)

	n.cacheResult(clientID, timestamp, false, pb.ResultType_FAILED)
	return false, fmt.Errorf("transaction aborted: %s", reason)
}

// cleanup2PCCoordinator cleans up coordinator state (rollback if needed)
func (n *Node) cleanup2PCCoordinator(txnID string, commit bool) {
	n.balanceMu.Lock()
	defer n.balanceMu.Unlock()

	txState, exists := n.twoPCState.transactions[txnID]
	if !exists {
		return
	}

	if !commit && len(txState.WALEntries) > 0 {
		// ROLLBACK: Restore old balances from WAL
		log.Printf("Node %d: 2PC[%s]: Rolling back using WAL", n.id, txnID)
		for itemID, oldBalance := range txState.WALEntries {
			log.Printf("Node %d: 2PC[%s]: Rollback item %d: %d → %d",
				n.id, txnID, itemID, n.balances[itemID], oldBalance)
			n.balances[itemID] = oldBalance
		}
	}

	// Release locks
	for _, itemID := range txState.LockedItems {
		lock, exists := n.locks[itemID]
		if exists && lock.clientID == txState.ClientID {
			log.Printf("Node %d: 2PC[%s]: 🔓 Releasing lock on item %d", n.id, txnID, itemID)
			delete(n.locks, itemID)
		}
	}

	// Delete transaction state
	delete(n.twoPCState.transactions, txnID)
}

// ============================================================================
// PARTICIPANT ROLE
// ============================================================================

// TwoPCPrepare handles PREPARE requests from coordinator (participant role)
func (n *Node) TwoPCPrepare(ctx context.Context, req *pb.TwoPCPrepareRequest) (*pb.TwoPCPrepareReply, error) {
	txnID := req.TransactionId
	tx := req.Transaction

	log.Printf("Node %d: 2PC[%s]: Received PREPARE request for item %d (PARTICIPANT)", n.id, txnID, tx.Receiver)

	// Wait for leader election if needed
	// IMPORTANT: Only expected leaders should start elections
	n.paxosMu.RLock()
	hasLeader := n.isLeader || n.leaderID > 0
	n.paxosMu.RUnlock()

	if !hasLeader && n.isExpectedLeader() {
		go n.StartLeaderElection()
		time.Sleep(200 * time.Millisecond) // Brief wait for election
	} else if !hasLeader {
		// Not expected leader - wait for election to happen
		time.Sleep(300 * time.Millisecond)
	}

	n.balanceMu.Lock()

	// Check if receiver item is locked
	receiverLock, receiverLocked := n.locks[tx.Receiver]
	if receiverLocked && receiverLock.clientID != req.ClientId {
		n.balanceMu.Unlock()
		log.Printf("Node %d: 2PC[%s]: ❌ PREPARE REJECTED - receiver item %d locked by %s (sending ABORT to coordinator)",
			n.id, txnID, tx.Receiver, receiverLock.clientID)

		// DON'T run Paxos for ABORT - coordinator will handle it
		// Just return failure immediately so coordinator knows to abort

		return &pb.TwoPCPrepareReply{
			Success:       false,
			TransactionId: txnID,
			Message:       "LOCKED:" + receiverLock.clientID, // Special prefix to indicate lock conflict
			ParticipantId: n.id,
		}, nil
	}

	// Lock receiver item and save old balance to WAL
	receiverBalance := n.balances[tx.Receiver]
	log.Printf("Node %d: 2PC[%s]: 🔒 Locking receiver item %d (balance: %d)", n.id, txnID, tx.Receiver, receiverBalance)

	n.locks[tx.Receiver] = &DataItemLock{
		clientID:  req.ClientId,
		timestamp: req.Timestamp,
		lockedAt:  time.Now(),
	}

	// Initialize 2PC state for participant
	if n.twoPCState.transactions == nil {
		n.twoPCState.transactions = make(map[string]*TwoPCTransaction)
	}
	n.twoPCState.transactions[txnID] = &TwoPCTransaction{
		TxnID:       txnID,
		Transaction: tx,
		ClientID:    req.ClientId,
		Timestamp:   req.Timestamp,
		Phase:       "PREPARE",
		LockedItems: []int32{tx.Receiver},
		WALEntries:  map[int32]int32{tx.Receiver: receiverBalance},
		CreatedAt:   time.Now(),
		LastContact: time.Now(),
	}

	n.balanceMu.Unlock()

	// Run Paxos to replicate PREPARE entry in participant cluster (marker: 'P')
	log.Printf("Node %d: 2PC[%s]: Running Paxos for PREPARE phase (marker: 'P')", n.id, txnID)

	prepareReq := &pb.TransactionRequest{
		ClientId:    req.ClientId,
		Timestamp:   req.Timestamp,
		Transaction: tx,
	}

	prepareReply, prepareSeq, err := n.processAsLeaderWithPhaseAndSeq(prepareReq, "P", 0)
	if err != nil || prepareReply == nil || !prepareReply.Success {
		// Determine failure reason from reply or error
		failureReason := "unknown"
		if prepareReply != nil && prepareReply.Message != "" {
			failureReason = prepareReply.Message
		} else if err != nil {
			failureReason = err.Error()
		}

		log.Printf("Node %d: 2PC[%s]: ❌ PREPARE consensus failed: %s", n.id, txnID, failureReason)
		n.cleanup2PCParticipant(txnID, false)
		return &pb.TwoPCPrepareReply{
			Success:       false,
			TransactionId: txnID,
			Message:       fmt.Sprintf("prepare consensus failed: %s", failureReason),
			ParticipantId: n.id,
		}, nil
	}

	// Save sequence number for reuse in COMMIT phase
	n.balanceMu.Lock()
	if txState, exists := n.twoPCState.transactions[txnID]; exists {
		txState.PrepareSeq = prepareSeq
	}
	n.balanceMu.Unlock()

	log.Printf("Node %d: 2PC[%s]: ✅ PREPARE replicated (seq: %d), transaction executed, WAL updated", n.id, txnID, prepareSeq)

	return &pb.TwoPCPrepareReply{
		Success:       true,
		TransactionId: txnID,
		Message:       "prepared",
		ParticipantId: n.id,
	}, nil
}

// TwoPCCommit handles COMMIT requests from coordinator
func (n *Node) TwoPCCommit(ctx context.Context, req *pb.TwoPCCommitRequest) (*pb.TwoPCCommitReply, error) {
	txnID := req.TransactionId

	log.Printf("Node %d: 2PC[%s]: Received COMMIT request (PARTICIPANT)", n.id, txnID)

	n.balanceMu.RLock()
	txState, exists := n.twoPCState.transactions[txnID]
	n.balanceMu.RUnlock()

	if !exists {
		log.Printf("Node %d: 2PC[%s]: ⚠️  Transaction state not found", n.id, txnID)
		return &pb.TwoPCCommitReply{
			Success:       true, // Assume already committed
			TransactionId: txnID,
			Message:       "transaction not found (possibly already committed)",
			ParticipantId: n.id,
		}, nil
	}

	// Run Paxos to replicate COMMIT entry with SAME sequence number (marker: 'C')
	prepareSeq := txState.PrepareSeq
	log.Printf("Node %d: 2PC[%s]: Running Paxos for COMMIT phase (marker: 'C', reusing seq: %d)", n.id, txnID, prepareSeq)

	commitReq := &pb.TransactionRequest{
		ClientId:    txState.ClientID,
		Timestamp:   txState.Timestamp,
		Transaction: txState.Transaction,
	}

	commitReply, _, err := n.processAsLeaderWithPhaseAndSeq(commitReq, "C", prepareSeq)
	if err != nil || commitReply == nil || !commitReply.Success {
		// Determine failure reason from reply or error
		failureReason := "unknown"
		if commitReply != nil && commitReply.Message != "" {
			failureReason = commitReply.Message
		} else if err != nil {
			failureReason = err.Error()
		}

		log.Printf("Node %d: 2PC[%s]: ⚠️  COMMIT consensus failed: %s", n.id, txnID, failureReason)
		return &pb.TwoPCCommitReply{
			Success:       false,
			TransactionId: txnID,
			Message:       fmt.Sprintf("commit consensus failed: %s", failureReason),
			ParticipantId: n.id,
		}, nil
	}

	log.Printf("Node %d: 2PC[%s]: ✅ COMMIT replicated", n.id, txnID)

	// Cleanup (commit - just release locks, keep WAL changes)
	n.cleanup2PCParticipant(txnID, true)

	return &pb.TwoPCCommitReply{
		Success:       true,
		TransactionId: txnID,
		Message:       "committed",
		ParticipantId: n.id,
	}, nil
}

// TwoPCAbort handles ABORT requests from coordinator
func (n *Node) TwoPCAbort(ctx context.Context, req *pb.TwoPCAbortRequest) (*pb.TwoPCAbortReply, error) {
	txnID := req.TransactionId

	log.Printf("Node %d: 2PC[%s]: Received ABORT request - reason: %s (PARTICIPANT)", n.id, txnID, req.Reason)

	n.balanceMu.RLock()
	txState, exists := n.twoPCState.transactions[txnID]
	n.balanceMu.RUnlock()

	if !exists {
		log.Printf("Node %d: 2PC[%s]: ⚠️  Transaction state not found", n.id, txnID)
		return &pb.TwoPCAbortReply{
			Success:       true, // Assume already aborted
			TransactionId: txnID,
			Message:       "transaction not found (possibly already aborted)",
			ParticipantId: n.id,
		}, nil
	}

	// Run Paxos to replicate ABORT entry with same sequence (marker: 'A')
	prepareSeq := txState.PrepareSeq
	log.Printf("Node %d: 2PC[%s]: Running Paxos for ABORT phase (marker: 'A', seq: %d)", n.id, txnID, prepareSeq)

	abortReq := &pb.TransactionRequest{
		ClientId:    txState.ClientID,
		Timestamp:   txState.Timestamp,
		Transaction: txState.Transaction,
	}

	_, _, err := n.processAsLeaderWithPhaseAndSeq(abortReq, "A", prepareSeq)
	if err != nil {
		log.Printf("Node %d: 2PC[%s]: ⚠️  ABORT consensus failed: %v", n.id, txnID, err)
	}

	// Cleanup (abort - rollback using WAL)
	n.cleanup2PCParticipant(txnID, false)

	return &pb.TwoPCAbortReply{
		Success:       true,
		TransactionId: txnID,
		Message:       "aborted",
		ParticipantId: n.id,
	}, nil
}

// cleanup2PCParticipant cleans up participant state (rollback if needed)
func (n *Node) cleanup2PCParticipant(txnID string, commit bool) {
	n.balanceMu.Lock()
	defer n.balanceMu.Unlock()

	txState, exists := n.twoPCState.transactions[txnID]
	if !exists {
		return
	}

	if !commit && len(txState.WALEntries) > 0 {
		// ROLLBACK: Restore old balances from WAL
		log.Printf("Node %d: 2PC[%s]: Rolling back using WAL", n.id, txnID)
		for itemID, oldBalance := range txState.WALEntries {
			log.Printf("Node %d: 2PC[%s]: Rollback item %d: %d → %d",
				n.id, txnID, itemID, n.balances[itemID], oldBalance)
			n.balances[itemID] = oldBalance
		}
	}

	// Release locks
	for _, itemID := range txState.LockedItems {
		lock, exists := n.locks[itemID]
		if exists && lock.clientID == txState.ClientID {
			log.Printf("Node %d: 2PC[%s]: 🔓 Releasing lock on item %d", n.id, txnID, itemID)
			delete(n.locks, itemID)
		}
	}

	// Delete transaction state and WAL entries
	delete(n.twoPCState.transactions, txnID)
}

// ============================================================================
// ALL NODES HANDLE 2PC PHASES (Not just leader!)
// ============================================================================

// handle2PCPhase is called by ALL nodes when they accept/commit a 2PC transaction
// This ensures followers also maintain WAL and can rollback if needed
func (n *Node) handle2PCPhase(entry *types.LogEntry, phase string) {
	if entry.Request == nil || entry.Request.Transaction == nil {
		return
	}

	tx := entry.Request.Transaction
	txnID := fmt.Sprintf("2pc-%s-%d", entry.Request.ClientId, entry.Request.Timestamp)

	switch phase {
	case "P": // PREPARE phase - save to WAL before execution
		n.balanceMu.Lock()

		// Initialize WAL for this transaction if needed
		if n.twoPCWAL[txnID] == nil {
			n.twoPCWAL[txnID] = make(map[int32]int32)
		}

		// Determine which item(s) this node is responsible for
		senderCluster := n.config.GetClusterForDataItem(tx.Sender)
		receiverCluster := n.config.GetClusterForDataItem(tx.Receiver)

		if int(n.clusterID) == senderCluster {
			// This node handles sender - save sender's old balance
			oldBalance := n.balances[tx.Sender]
			n.twoPCWAL[txnID][tx.Sender] = oldBalance
			log.Printf("Node %d: 2PC[%s] PREPARE: Saved sender WAL[%d]=%d",
				n.id, txnID, tx.Sender, oldBalance)
		}

		if int(n.clusterID) == receiverCluster {
			// This node handles receiver - save receiver's old balance
			oldBalance := n.balances[tx.Receiver]
			n.twoPCWAL[txnID][tx.Receiver] = oldBalance
			log.Printf("Node %d: 2PC[%s] PREPARE: Saved receiver WAL[%d]=%d",
				n.id, txnID, tx.Receiver, oldBalance)
		}

		n.balanceMu.Unlock()

	case "C": // COMMIT phase - delete WAL, keep changes
		n.balanceMu.Lock()
		if _, exists := n.twoPCWAL[txnID]; exists {
			delete(n.twoPCWAL, txnID)
			log.Printf("Node %d: 2PC[%s] COMMIT: Deleted WAL (changes committed)", n.id, txnID)
		}
		n.balanceMu.Unlock()

	case "A": // ABORT phase - rollback using WAL
		n.balanceMu.Lock()
		if walEntries, exists := n.twoPCWAL[txnID]; exists {
			log.Printf("Node %d: 2PC[%s] ABORT: Rolling back %d items", n.id, txnID, len(walEntries))

			for itemID, oldBalance := range walEntries {
				currentBalance := n.balances[itemID]
				n.balances[itemID] = oldBalance
				log.Printf("Node %d: 2PC[%s] ABORT: Rolled back item %d: %d → %d",
					n.id, txnID, itemID, currentBalance, oldBalance)
			}

			delete(n.twoPCWAL, txnID)
		}
		n.balanceMu.Unlock()
	}
}

// ============================================================================
// HELPER FUNCTIONS
// ============================================================================

// cacheResult stores result for exactly-once semantics
func (n *Node) cacheResult(clientID string, timestamp int64, success bool, result pb.ResultType) {
	reply := &pb.TransactionReply{
		Success: success,
		Message: result.String(),
		Result:  result,
	}

	n.clientMu.Lock()
	n.clientLastReply[clientID] = reply
	n.clientLastTS[clientID] = timestamp
	n.clientMu.Unlock()
}

// processAsLeaderWithPhaseAndSeq runs Paxos with a 2PC phase marker and optional sequence number
// If seq == 0, gets a new sequence number. Otherwise, reuses the provided sequence.
// Returns (reply, sequence_number, error)
func (n *Node) processAsLeaderWithPhaseAndSeq(req *pb.TransactionRequest, phase string, seq int32) (*pb.TransactionReply, int32, error) {
	// Quick duplicate check
	n.clientMu.RLock()
	if lastReply, exists := n.clientLastReply[req.ClientId]; exists {
		if req.Timestamp <= n.clientLastTS[req.ClientId] {
			n.clientMu.RUnlock()
			// Return cached sequence if available
			if seq == 0 {
				n.logMu.RLock()
				seq = n.nextSeqNum - 1
				n.logMu.RUnlock()
			}
			return lastReply, seq, nil
		}
	}
	n.clientMu.RUnlock()

	// Allocate or reuse sequence number
	if seq == 0 {
		// Get new sequence
		n.logMu.Lock()
		seq = n.nextSeqNum
		n.nextSeqNum++
		n.logMu.Unlock()
		log.Printf("Node %d: 2PC phase '%s': Allocated NEW seq=%d", n.id, phase, seq)
	} else {
		// Reuse existing sequence (for COMMIT/ABORT after PREPARE)
		log.Printf("Node %d: 2PC phase '%s': REUSING seq=%d", n.id, phase, seq)
	}

	// Get current ballot
	n.paxosMu.RLock()
	ballot := &types.Ballot{
		Number: n.currentBallot.Number,
		NodeID: n.currentBallot.NodeID,
	}
	n.paxosMu.RUnlock()

	// Create log entry WITH phase marker
	entry := types.NewLogEntryWithPhase(ballot, seq, req, false, phase)
	entry.AcceptedBy = make(map[int32]bool)
	entry.AcceptedBy[n.id] = true
	entry.Status = "A"

	n.logMu.Lock()
	if existingEntry, exists := n.log[seq]; exists && phase != "P" {
		// COMMIT or ABORT phase - update existing entry's phase
		log.Printf("Node %d: Updating existing entry at seq=%d from phase '%s' to '%s'",
			n.id, seq, existingEntry.Phase, phase)
		existingEntry.Phase = phase
		existingEntry.Status = "A"
		entry = existingEntry
	} else {
		n.log[seq] = entry
	}
	n.logMu.Unlock()

	// Handle 2PC phase on leader (saves WAL for PREPARE)
	n.handle2PCPhase(entry, phase)

	// Send ACCEPT with phase marker to all peers
	n.paxosMu.RLock()
	peers := make(map[int32]pb.PaxosNodeClient)
	for pid, cli := range n.peerClients {
		peers[pid] = cli
	}
	n.paxosMu.RUnlock()

	acceptReq := &pb.AcceptRequest{
		Ballot:         ballot.ToProto(),
		SequenceNumber: seq,
		Request:        req,
		IsNoop:         false,
		Phase:          phase, // NEW: Include phase marker
	}

	// Collect accepts (parallel)
	acceptedCount := 1 // Leader accepts
	var wg sync.WaitGroup
	var mu sync.Mutex

	for pid, client := range peers {
		wg.Add(1)
		go func(peerID int32, c pb.PaxosNodeClient) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			resp, err := c.Accept(ctx, acceptReq)
			if err == nil && resp.Success {
				mu.Lock()
				acceptedCount++
				mu.Unlock()
			}
		}(pid, client)
	}
	wg.Wait()

	quorum := (len(peers) + 1 + 1) / 2

	if acceptedCount < quorum {
		log.Printf("Node %d: ❌ No quorum for seq %d phase '%s' (accepted=%d, need=%d)",
			n.id, seq, phase, acceptedCount, quorum)
		return &pb.TransactionReply{
			Success: false,
			Message: "No quorum",
			Result:  pb.ResultType_FAILED,
		}, seq, nil
	}

	log.Printf("Node %d: ✅ Quorum achieved for seq %d phase '%s'", n.id, seq, phase)

	// Mark entry as committed and broadcast COMMIT
	n.logMu.Lock()
	entry.Status = "C"
	n.logMu.Unlock()

	commitReq := &pb.CommitRequest{
		Ballot:         ballot.ToProto(),
		SequenceNumber: seq,
		Request:        req,
		IsNoop:         false,
		Phase:          phase, // NEW: Include phase marker
	}

	// Send COMMIT to all peers (don't wait for replies - fire and forget for performance)
	for pid, client := range peers {
		go func(peerID int32, c pb.PaxosNodeClient) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			c.Commit(ctx, commitReq)
		}(pid, client)
	}

	// Execute ONLY in PREPARE phase, not in COMMIT/ABORT
	var result pb.ResultType
	var reply *pb.TransactionReply

	if phase == "P" {
		// PREPARE: Execute the transaction
		result = n.commitAndExecute(seq)

		// Build reply based on execution result
		if result == pb.ResultType_SUCCESS {
			reply = &pb.TransactionReply{
				Success: true,
				Message: fmt.Sprintf("Transaction committed (seq=%d, phase='%s')", seq, phase),
				Ballot:  ballot.ToProto(),
				Result:  result,
			}
		} else if result == pb.ResultType_INSUFFICIENT_BALANCE {
			reply = &pb.TransactionReply{
				Success: false,
				Message: "Insufficient balance",
				Ballot:  ballot.ToProto(),
				Result:  result,
			}
		} else {
			reply = &pb.TransactionReply{
				Success: false,
				Message: "Transaction failed",
				Ballot:  ballot.ToProto(),
				Result:  result,
			}
		}
	} else {
		// COMMIT or ABORT: No execution, just confirmation
		log.Printf("Node %d: 2PC phase '%s' complete for seq=%d (no execution)", n.id, phase, seq)
		reply = &pb.TransactionReply{
			Success: true,
			Message: fmt.Sprintf("Transaction phase '%s' complete (seq=%d)", phase, seq),
			Ballot:  ballot.ToProto(),
			Result:  pb.ResultType_SUCCESS,
		}
	}

	// Cache reply
	n.clientMu.Lock()
	n.clientLastReply[req.ClientId] = reply
	n.clientLastTS[req.ClientId] = req.Timestamp
	n.clientMu.Unlock()

	return reply, seq, nil
}
