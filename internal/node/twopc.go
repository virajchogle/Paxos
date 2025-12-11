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

type TwoPCState struct {
	mu           sync.RWMutex
	transactions map[string]*TwoPCTransaction // txnID -> state
}

type TwoPCTransaction struct {
	TxnID       string
	Transaction *pb.Transaction
	ClientID    string
	Timestamp   int64
	Phase       string
	PrepareSeq  int32
	CommitSeq   int32
	Prepared    bool
	Committed   bool
	LockedItems []int32
	WALEntries  map[int32]int32
	CreatedAt   time.Time
	LastContact time.Time
}

func (n *Node) TwoPCCoordinator(tx *pb.Transaction, clientID string, timestamp int64) (bool, error) {
	txnID := fmt.Sprintf("2pc-%s-%d", clientID, timestamp)

	// Check for duplicate
	n.clientMu.RLock()
	lastReply, hasReply := n.clientLastReply[clientID]
	lastTS, hasTS := n.clientLastTS[clientID]
	n.clientMu.RUnlock()

	if hasReply && hasTS && timestamp <= lastTS {
		return lastReply.Success && lastReply.Result == pb.ResultType_SUCCESS, nil
	}

	log.Printf("Node %d: 2PC[%s] %d→%d:%d", n.id, txnID, tx.Sender, tx.Receiver, tx.Amount)

	receiverCluster := n.getClusterForDataItem(tx.Receiver)
	receiverLeader := n.config.GetLeaderNodeForCluster(int(receiverCluster))

	// Use lockMu for lock operations (consistent with acquireLock/releaseLock)
	n.lockMu.Lock()
	senderLock, senderLocked := n.locks[tx.Sender]
	if senderLocked && senderLock.clientID != clientID {
		n.lockMu.Unlock()
		n.cacheResult(clientID, timestamp, false, pb.ResultType_FAILED)
		return false, fmt.Errorf("sender item locked")
	}
	n.locks[tx.Sender] = &DataItemLock{
		clientID:  clientID,
		timestamp: timestamp,
		lockedAt:  time.Now(),
	}
	n.lockMu.Unlock()

	// Use balanceMu for balance operations
	n.balanceMu.Lock()
	senderBalance := n.balances[tx.Sender]
	if senderBalance < tx.Amount {
		n.balanceMu.Unlock()
		// Release the lock we just acquired
		n.lockMu.Lock()
		delete(n.locks, tx.Sender)
		n.lockMu.Unlock()
		n.cacheResult(clientID, timestamp, false, pb.ResultType_INSUFFICIENT_BALANCE)
		return false, fmt.Errorf("insufficient balance")
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

	receiverClient, err := n.getCrossClusterClient(int32(receiverLeader))
	if err != nil {
		n.cleanup2PCCoordinator(txnID, false)
		n.cacheResult(clientID, timestamp, false, pb.ResultType_FAILED)
		return false, fmt.Errorf("participant unreachable: %v", err)
	}

	prepareReq := &pb.TransactionRequest{
		ClientId:    clientID,
		Timestamp:   timestamp,
		Transaction: tx,
	}

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

	go func() {
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

	go func() {
		prepareReply, prepareSeq, err := n.processAsLeaderWithPhaseAndSeq(prepareReq, "P", 0)
		coordChan <- coordinatorResult{reply: prepareReply, seq: prepareSeq, err: err}
	}()

	var coordResult coordinatorResult
	var partResult participantResult
	receivedCoord := false
	receivedPart := false

	for i := 0; i < 2; i++ {
		select {
		case coordResult = <-coordChan:
			receivedCoord = true
			if coordResult.err != nil || !coordResult.reply.Success {
				if !receivedPart {
					partResult = <-partChan
				}
				return n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, "coordinator prepare failed")
			}
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
				isLockConflict := partResult.reply != nil && len(partResult.reply.Message) >= 7 && partResult.reply.Message[:7] == "LOCKED:"
				if !receivedCoord {
					coordResult = <-coordChan
				}
				if isLockConflict {
					n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, fmt.Sprintf("lock conflict: %s", errorMsg))
					n.cacheResult(clientID, timestamp, false, pb.ResultType_FAILED)
					return false, fmt.Errorf("transaction permanently failed due to lock conflict: %s", errorMsg)
				}
				return n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, fmt.Sprintf("participant prepare failed: %s", errorMsg))
			}
		}
	}

	n.balanceMu.RLock()
	txState, exists := n.twoPCState.transactions[txnID]
	if !exists || txState == nil {
		n.balanceMu.RUnlock()
		return n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, "transaction state lost")
	}
	prepareSeq := txState.PrepareSeq
	n.balanceMu.RUnlock()

	commitReply, _, err := n.processAsLeaderWithPhaseAndSeq(prepareReq, "C", prepareSeq)
	if err != nil || commitReply == nil || !commitReply.Success {
		failureReason := "unknown"
		if commitReply != nil && commitReply.Message != "" {
			failureReason = commitReply.Message
		} else if err != nil {
			failureReason = err.Error()
		}
		return n.coordinatorAbort(txnID, clientID, timestamp, receiverClient, fmt.Sprintf("commit consensus failed: %s", failureReason))
	}

	commitMsg := &pb.TwoPCCommitRequest{
		TransactionId: txnID,
		CoordinatorId: n.id,
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		commitAck, err := receiverClient.TwoPCCommit(ctx, commitMsg)
		if err != nil || !commitAck.Success {
			for retry := 0; retry < 5; retry++ {
				time.Sleep(1 * time.Second)
				ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
				commitAck, err = receiverClient.TwoPCCommit(ctx2, commitMsg)
				cancel2()
				if err == nil && commitAck.Success {
					return
				}
			}
		}
	}()

	// cleanup coordinator state, dont wait for participant ACK
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
	txState, exists := n.twoPCState.transactions[txnID]
	if !exists {
		n.balanceMu.Unlock()
		return
	}

	// Copy values needed for lock release
	clientID := txState.ClientID
	lockedItems := make([]int32, len(txState.LockedItems))
	copy(lockedItems, txState.LockedItems)

	if !commit && len(txState.WALEntries) > 0 {
		// ROLLBACK: Restore old balances from WAL
		log.Printf("Node %d: 2PC[%s]: Rolling back using WAL", n.id, txnID)
		for itemID, oldBalance := range txState.WALEntries {
			log.Printf("Node %d: 2PC[%s]: Rollback item %d: %d → %d",
				n.id, txnID, itemID, n.balances[itemID], oldBalance)
			n.balances[itemID] = oldBalance
		}
	}

	// Delete transaction state (under balanceMu since twoPCState is protected by it)
	delete(n.twoPCState.transactions, txnID)
	n.balanceMu.Unlock()

	// Release locks (use lockMu for lock operations)
	n.lockMu.Lock()
	for _, itemID := range lockedItems {
		lock, exists := n.locks[itemID]
		if exists && lock.clientID == clientID {
			log.Printf("Node %d: 2PC[%s]: 🔓 Releasing lock on item %d", n.id, txnID, itemID)
			delete(n.locks, itemID)
		}
	}
	n.lockMu.Unlock()
}

// ============================================================================
// PARTICIPANT ROLE
// ============================================================================

// TwoPCPrepare handles PREPARE requests from coordinator (participant role)
func (n *Node) TwoPCPrepare(ctx context.Context, req *pb.TwoPCPrepareRequest) (*pb.TwoPCPrepareReply, error) {
	txnID := req.TransactionId
	tx := req.Transaction

	log.Printf("Node %d: 2PC[%s]: Received PREPARE request for item %d (PARTICIPANT)", n.id, txnID, tx.Receiver)

	// wait for leader election if needed
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

	// Use lockMu for lock operations (consistent with acquireLock/releaseLock)
	n.lockMu.Lock()
	receiverLock, receiverLocked := n.locks[tx.Receiver]
	if receiverLocked && receiverLock.clientID != req.ClientId {
		n.lockMu.Unlock()
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

	// Lock receiver item
	n.locks[tx.Receiver] = &DataItemLock{
		clientID:  req.ClientId,
		timestamp: req.Timestamp,
		lockedAt:  time.Now(),
	}
	n.lockMu.Unlock()

	// Use balanceMu for balance operations
	n.balanceMu.Lock()
	receiverBalance := n.balances[tx.Receiver]

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

	prepareReq := &pb.TransactionRequest{
		ClientId:    req.ClientId,
		Timestamp:   req.Timestamp,
		Transaction: tx,
	}

	prepareReply, prepareSeq, err := n.processAsLeaderWithPhaseAndSeq(prepareReq, "P", 0)
	if err != nil || prepareReply == nil || !prepareReply.Success {
		failureReason := "unknown"
		if prepareReply != nil && prepareReply.Message != "" {
			failureReason = prepareReply.Message
		} else if err != nil {
			failureReason = err.Error()
		}
		n.cleanup2PCParticipant(txnID, false)
		return &pb.TwoPCPrepareReply{
			Success:       false,
			TransactionId: txnID,
			Message:       fmt.Sprintf("prepare consensus failed: %s", failureReason),
			ParticipantId: n.id,
		}, nil
	}

	n.balanceMu.Lock()
	if txState, exists := n.twoPCState.transactions[txnID]; exists {
		txState.PrepareSeq = prepareSeq
	}
	n.balanceMu.Unlock()

	return &pb.TwoPCPrepareReply{
		Success:       true,
		TransactionId: txnID,
		Message:       "prepared",
		ParticipantId: n.id,
	}, nil
}

func (n *Node) TwoPCCommit(ctx context.Context, req *pb.TwoPCCommitRequest) (*pb.TwoPCCommitReply, error) {
	txnID := req.TransactionId

	n.balanceMu.RLock()
	txState, exists := n.twoPCState.transactions[txnID]
	n.balanceMu.RUnlock()

	if !exists {
		return &pb.TwoPCCommitReply{
			Success:       true,
			TransactionId: txnID,
			Message:       "already committed",
			ParticipantId: n.id,
		}, nil
	}

	prepareSeq := txState.PrepareSeq

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
	txState, exists := n.twoPCState.transactions[txnID]
	if !exists {
		n.balanceMu.Unlock()
		return
	}

	// Copy values needed for lock release
	clientID := txState.ClientID
	lockedItems := make([]int32, len(txState.LockedItems))
	copy(lockedItems, txState.LockedItems)

	if !commit && len(txState.WALEntries) > 0 {
		// ROLLBACK: Restore old balances from WAL
		log.Printf("Node %d: 2PC[%s]: Rolling back using WAL", n.id, txnID)
		for itemID, oldBalance := range txState.WALEntries {
			log.Printf("Node %d: 2PC[%s]: Rollback item %d: %d → %d",
				n.id, txnID, itemID, n.balances[itemID], oldBalance)
			n.balances[itemID] = oldBalance
		}
	}

	// Delete transaction state and WAL entries (under balanceMu since twoPCState is protected by it)
	delete(n.twoPCState.transactions, txnID)
	n.balanceMu.Unlock()

	// Release locks (use lockMu for lock operations)
	n.lockMu.Lock()
	for _, itemID := range lockedItems {
		lock, exists := n.locks[itemID]
		if exists && lock.clientID == clientID {
			log.Printf("Node %d: 2PC[%s]: 🔓 Releasing lock on item %d", n.id, txnID, itemID)
			delete(n.locks, itemID)
		}
	}
	n.lockMu.Unlock()
}

// ============================================================================
// ALL NODES HANDLE 2PC PHASES (Not just leader!)
// ============================================================================

// handle2PCPhase handles 2PC phases for all nodes
func (n *Node) handle2PCPhase(entry *types.LogEntry, phase string) {
	if entry.Request == nil || entry.Request.Transaction == nil {
		return
	}

	tx := entry.Request.Transaction
	txnID := fmt.Sprintf("2pc-%s-%d", entry.Request.ClientId, entry.Request.Timestamp)
	clientID := entry.Request.ClientId
	timestamp := entry.Request.Timestamp

	senderCluster := n.getClusterForDataItem(tx.Sender)
	receiverCluster := n.getClusterForDataItem(tx.Receiver)

	switch phase {
	case "P": // PREPARE phase - save to WAL and acquire locks (for all replicas including followers)
		// First, save balance to WAL under balanceMu
		n.balanceMu.Lock()
		if n.twoPCWAL[txnID] == nil {
			n.twoPCWAL[txnID] = make(map[int32]int32)
		}

		if n.clusterID == senderCluster {
			oldBalance := n.balances[tx.Sender]
			n.twoPCWAL[txnID][tx.Sender] = oldBalance
			log.Printf("Node %d: 2PC[%s] PREPARE: Saved sender WAL[%d]=%d",
				n.id, txnID, tx.Sender, oldBalance)
		}

		if n.clusterID == receiverCluster {
			oldBalance := n.balances[tx.Receiver]
			n.twoPCWAL[txnID][tx.Receiver] = oldBalance
			log.Printf("Node %d: 2PC[%s] PREPARE: Saved receiver WAL[%d]=%d",
				n.id, txnID, tx.Receiver, oldBalance)
		}
		n.balanceMu.Unlock()

		// Then, acquire locks under lockMu (LOCK REPLICATION for followers)
		n.lockMu.Lock()
		if n.clusterID == senderCluster {
			if _, locked := n.locks[tx.Sender]; !locked {
				n.locks[tx.Sender] = &DataItemLock{
					clientID:  clientID,
					timestamp: timestamp,
					lockedAt:  time.Now(),
				}
				log.Printf("Node %d: 2PC[%s] PREPARE: Acquired lock on sender %d (replica)",
					n.id, txnID, tx.Sender)
			}
		}

		if n.clusterID == receiverCluster {
			if _, locked := n.locks[tx.Receiver]; !locked {
				n.locks[tx.Receiver] = &DataItemLock{
					clientID:  clientID,
					timestamp: timestamp,
					lockedAt:  time.Now(),
				}
				log.Printf("Node %d: 2PC[%s] PREPARE: Acquired lock on receiver %d (replica)",
					n.id, txnID, tx.Receiver)
			}
		}
		n.lockMu.Unlock()

	case "C": // COMMIT phase - delete WAL and release locks
		// Delete WAL under balanceMu
		n.balanceMu.Lock()
		if _, exists := n.twoPCWAL[txnID]; exists {
			delete(n.twoPCWAL, txnID)
			log.Printf("Node %d: 2PC[%s] COMMIT: Deleted WAL (changes committed)", n.id, txnID)
		}
		n.balanceMu.Unlock()

		// Release locks under lockMu
		n.lockMu.Lock()
		if n.clusterID == senderCluster {
			if lock, exists := n.locks[tx.Sender]; exists && lock.clientID == clientID {
				delete(n.locks, tx.Sender)
				log.Printf("Node %d: 2PC[%s] COMMIT: Released lock on sender %d (replica)",
					n.id, txnID, tx.Sender)
			}
		}
		if n.clusterID == receiverCluster {
			if lock, exists := n.locks[tx.Receiver]; exists && lock.clientID == clientID {
				delete(n.locks, tx.Receiver)
				log.Printf("Node %d: 2PC[%s] COMMIT: Released lock on receiver %d (replica)",
					n.id, txnID, tx.Receiver)
			}
		}
		n.lockMu.Unlock()

	case "A": // ABORT phase - rollback using WAL and release locks
		// Rollback balances under balanceMu
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

		// Release locks under lockMu
		n.lockMu.Lock()
		if n.clusterID == senderCluster {
			if lock, exists := n.locks[tx.Sender]; exists && lock.clientID == clientID {
				delete(n.locks, tx.Sender)
				log.Printf("Node %d: 2PC[%s] ABORT: Released lock on sender %d (replica)",
					n.id, txnID, tx.Sender)
			}
		}
		if n.clusterID == receiverCluster {
			if lock, exists := n.locks[tx.Receiver]; exists && lock.clientID == clientID {
				delete(n.locks, tx.Receiver)
				log.Printf("Node %d: 2PC[%s] ABORT: Released lock on receiver %d (replica)",
					n.id, txnID, tx.Receiver)
			}
		}
		n.lockMu.Unlock()
	}
}

// ============================================================================
// 2PC CRASH RECOVERY - New Leader Continuation
// ============================================================================

// Recover2PCTransactions scans for incomplete 2PC transactions after NEW-VIEW
// and resumes them. Called when a new leader is elected.
func (n *Node) Recover2PCTransactions() {
	// Only coordinator cluster (sender cluster) should recover 2PC
	n.paxosMu.RLock()
	isLeader := n.isLeader
	n.paxosMu.RUnlock()

	if !isLeader {
		return
	}

	log.Printf("Node %d: 🔄 Scanning for incomplete 2PC transactions...", n.id)

	// Find all PREPARE entries without corresponding COMMIT/ABORT
	incomplete := n.findIncomplete2PCTransactions()

	if len(incomplete) == 0 {
		log.Printf("Node %d: ✅ No incomplete 2PC transactions found", n.id)
		return
	}

	log.Printf("Node %d: Found %d incomplete 2PC transactions to recover", n.id, len(incomplete))

	// Resume each incomplete 2PC transaction
	for _, entry := range incomplete {
		go n.resume2PCTransaction(entry)
	}
}

// findIncomplete2PCTransactions scans the log for PREPARE entries without COMMIT/ABORT
func (n *Node) findIncomplete2PCTransactions() []*types.LogEntry {
	n.logMu.RLock()
	defer n.logMu.RUnlock()

	// Build map of txnID -> phases seen
	txnPhases := make(map[string]map[string]int32) // txnID -> phase -> seq

	for seq, entry := range n.log {
		if entry == nil || entry.Request == nil || entry.Request.Transaction == nil {
			continue
		}
		if entry.Phase == "" {
			continue // Not a 2PC entry
		}

		tx := entry.Request.Transaction
		// Only process cross-shard transactions where we are the sender cluster (coordinator)
		senderCluster := n.getClusterForDataItem(tx.Sender)
		receiverCluster := n.getClusterForDataItem(tx.Receiver)

		if senderCluster == receiverCluster {
			continue // Intra-shard, not 2PC
		}
		if n.clusterID != senderCluster {
			continue // We're not the coordinator
		}

		txnID := fmt.Sprintf("2pc-%s-%d", entry.Request.ClientId, entry.Request.Timestamp)

		if txnPhases[txnID] == nil {
			txnPhases[txnID] = make(map[string]int32)
		}
		txnPhases[txnID][entry.Phase] = seq
	}

	// Find transactions with PREPARE but no COMMIT or ABORT
	var incomplete []*types.LogEntry
	for txnID, phases := range txnPhases {
		prepareSeq, hasPrepare := phases["P"]
		_, hasCommit := phases["C"]
		_, hasAbort := phases["A"]

		if hasPrepare && !hasCommit && !hasAbort {
			entry := n.log[prepareSeq]
			if entry != nil {
				log.Printf("Node %d: Found incomplete 2PC: %s (seq=%d, phase=P only)",
					n.id, txnID, prepareSeq)
				incomplete = append(incomplete, entry)
			}
		}
	}

	return incomplete
}

// resume2PCTransaction resumes an incomplete 2PC transaction
func (n *Node) resume2PCTransaction(entry *types.LogEntry) {
	if entry == nil || entry.Request == nil || entry.Request.Transaction == nil {
		return
	}

	tx := entry.Request.Transaction
	clientID := entry.Request.ClientId
	timestamp := entry.Request.Timestamp
	txnID := fmt.Sprintf("2pc-%s-%d", clientID, timestamp)

	log.Printf("Node %d: 🔄 Resuming 2PC[%s]: %d → %d", n.id, txnID, tx.Sender, tx.Receiver)

	receiverCluster := n.getClusterForDataItem(tx.Receiver)
	receiverLeader := n.config.GetLeaderNodeForCluster(int(receiverCluster))

	receiverClient, err := n.getCrossClusterClient(int32(receiverLeader))
	if err != nil {
		log.Printf("Node %d: 2PC[%s] Recovery: ❌ Cannot connect to participant: %v", n.id, txnID, err)
		n.abortRecovered2PC(entry, "participant unreachable")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prepareMsg := &pb.TwoPCPrepareRequest{
		TransactionId: txnID,
		Transaction:   tx,
		ClientId:      clientID,
		Timestamp:     timestamp,
		CoordinatorId: n.id,
	}

	log.Printf("Node %d: 2PC[%s] Recovery: Checking participant status...", n.id, txnID)
	prepareReply, err := receiverClient.TwoPCPrepare(ctx, prepareMsg)

	if err != nil {
		log.Printf("Node %d: 2PC[%s] Recovery: ❌ Participant error: %v - aborting", n.id, txnID, err)
		n.abortRecovered2PC(entry, fmt.Sprintf("participant error: %v", err))
		return
	}

	if !prepareReply.Success {
		log.Printf("Node %d: 2PC[%s] Recovery: ❌ Participant not prepared: %s - aborting",
			n.id, txnID, prepareReply.Message)
		n.abortRecovered2PC(entry, fmt.Sprintf("participant not prepared: %s", prepareReply.Message))
		return
	}

	// Participant is prepared! Complete the COMMIT
	log.Printf("Node %d: 2PC[%s] Recovery: ✅ Participant is PREPARED - completing COMMIT", n.id, txnID)

	// Run Paxos for COMMIT phase
	prepareReq := &pb.TransactionRequest{
		ClientId:    clientID,
		Timestamp:   timestamp,
		Transaction: tx,
	}

	// Get the sequence number from the PREPARE entry
	prepareSeq := entry.SeqNum

	commitReply, _, err := n.processAsLeaderWithPhaseAndSeq(prepareReq, "C", prepareSeq)
	if err != nil || commitReply == nil || !commitReply.Success {
		log.Printf("Node %d: 2PC[%s] Recovery: ❌ COMMIT consensus failed - participant will timeout",
			n.id, txnID)
		return
	}

	// Send COMMIT to participant
	commitMsg := &pb.TwoPCCommitRequest{
		TransactionId: txnID,
		CoordinatorId: n.id,
	}

	commitCtx, commitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer commitCancel()

	commitReply2, err := receiverClient.TwoPCCommit(commitCtx, commitMsg)
	if err != nil || !commitReply2.Success {
		log.Printf("Node %d: 2PC[%s] Recovery: ⚠️ Participant COMMIT ACK failed, will retry in background",
			n.id, txnID)
		// Background retry for commit
		go func() {
			for retry := 0; retry < 5; retry++ {
				time.Sleep(1 * time.Second)
				ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
				ack, err := receiverClient.TwoPCCommit(ctx2, commitMsg)
				cancel2()
				if err == nil && ack.Success {
					log.Printf("Node %d: 2PC[%s] Recovery: ✅ Participant ACK after retry", n.id, txnID)
					return
				}
			}
		}()
	} else {
		log.Printf("Node %d: 2PC[%s] Recovery: ✅ Participant COMMITTED", n.id, txnID)
	}

	// Cleanup coordinator state
	n.cleanup2PCCoordinator(txnID, true)
	log.Printf("Node %d: 2PC[%s] Recovery: ✅ Transaction RECOVERED and COMMITTED", n.id, txnID)
}

// abortRecovered2PC aborts a recovered 2PC transaction that can't be completed
func (n *Node) abortRecovered2PC(entry *types.LogEntry, reason string) {
	if entry == nil || entry.Request == nil {
		return
	}

	tx := entry.Request.Transaction
	clientID := entry.Request.ClientId
	timestamp := entry.Request.Timestamp
	txnID := fmt.Sprintf("2pc-%s-%d", clientID, timestamp)
	prepareSeq := entry.SeqNum

	log.Printf("Node %d: 2PC[%s] Recovery: Aborting - %s", n.id, txnID, reason)

	// Run Paxos for ABORT phase
	abortReq := &pb.TransactionRequest{
		ClientId:    clientID,
		Timestamp:   timestamp,
		Transaction: tx,
	}

	_, _, err := n.processAsLeaderWithPhaseAndSeq(abortReq, "A", prepareSeq)
	if err != nil {
		log.Printf("Node %d: 2PC[%s] Recovery: ⚠️ ABORT consensus failed: %v", n.id, txnID, err)
	}

	// Send ABORT to participant (best effort)
	receiverCluster := n.getClusterForDataItem(tx.Receiver)
	receiverLeader := n.config.GetLeaderNodeForCluster(int(receiverCluster))

	if receiverClient, err := n.getCrossClusterClient(int32(receiverLeader)); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		abortMsg := &pb.TwoPCAbortRequest{
			TransactionId: txnID,
			CoordinatorId: n.id,
			Reason:        reason,
		}
		receiverClient.TwoPCAbort(ctx, abortMsg)
	}

	// Cleanup
	n.cleanup2PCCoordinator(txnID, false)
	log.Printf("Node %d: 2PC[%s] Recovery: ✅ Transaction ABORTED", n.id, txnID)
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
