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

// SubmitTransaction handles client RPCs. This is the client entrypoint.
func (n *Node) SubmitTransaction(ctx context.Context, req *pb.TransactionRequest) (*pb.TransactionReply, error) {
	// 🔒 FINE-GRAINED: Use specific locks for different state
	n.paxosMu.RLock()
	isLeader := n.isLeader
	leaderID := n.leaderID
	active := n.isActive
	systemInit := n.systemInitialized
	n.paxosMu.RUnlock()

	nodeID := n.id // Read-only, no lock needed

	// Lookup last reply (use clientMu)
	n.clientMu.RLock()
	lastReply, hasReply := n.clientLastReply[req.ClientId]
	lastTS, hasTS := n.clientLastTS[req.ClientId]
	n.clientMu.RUnlock()

	if !active {
		return nil, fmt.Errorf("node inactive")
	}

	// 🎯 Bootstrap: If system not initialized, handle first transaction
	bootstrapElectionStarted := false
	if !systemInit {
		n.paxosMu.Lock()
		if !n.systemInitialized { // Double-check under lock
			n.systemInitialized = true
			n.paxosMu.Unlock()

			if nodeID == 1 || nodeID == 4 || nodeID == 7 {
				// Expected leader nodes (n1, n4, n7): Start leader election immediately
				log.Printf("Node %d: 🚀 First transaction received - bootstrapping system and starting leader election", nodeID)
				// Start election directly - no need for timer since we're the expected leader
				go n.StartLeaderElection()
				bootstrapElectionStarted = true
			} else {
				// Other nodes: Forward to expected leader (n1, n4, or n7) to trigger bootstrap
				log.Printf("Node %d: 🔄 First transaction received - forwarding to expected leader to bootstrap system", nodeID)

				// Determine which expected leader to forward to based on cluster
				expectedLeader := int32(1) // Default to n1
				if n.clusterID == 2 {
					expectedLeader = 4
				} else if n.clusterID == 3 {
					expectedLeader = 7
				}

				leaderClient, hasLeader := n.peerClients[expectedLeader] // Read-only after init
				if hasLeader {
					// Forward to expected leader
					ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					resp, err := leaderClient.SubmitTransaction(ctx2, req)
					if err == nil {
						return resp, nil
					}
					log.Printf("Node %d: Failed to forward to node %d: %v - will wait for election", nodeID, expectedLeader, err)
				} else {
					log.Printf("Node %d: Not connected to node %d - will wait for election", nodeID, expectedLeader)
				}
			}
		} else {
			n.paxosMu.Unlock()
		}

		// Update local variables after initialization
		n.paxosMu.RLock()
		isLeader = n.isLeader
		leaderID = n.leaderID
		n.paxosMu.RUnlock()
	}

	// exactly-once: if request timestamp <= last processed timestamp, resend last reply
	if hasReply && hasTS && req.Timestamp <= lastTS {
		log.Printf("Node %d: Duplicate request from %s (ts=%d, last=%d) -> resending", n.id, req.ClientId, req.Timestamp, lastTS)
		return lastReply, nil
	}

	// if not leader, forward to leader if known
	if !isLeader {
		// leaderID > 0 means we know a leader (leaderID = -1 means uninitialized, 0 means no leader)
		if leaderID > 0 && leaderID != n.id {
			client, ok := n.peerClients[leaderID]
			if ok {
				// forward to leader RPC and return its reply
				ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				resp, err := client.SubmitTransaction(ctx2, req)
				if err == nil {
					return resp, nil
				}
				log.Printf("Node %d: Forward to leader %d failed: %v", n.id, leaderID, err)

				// Forward failed - clear leader and start election
				n.paxosMu.Lock()
				n.leaderID = 0 // Clear failed leader
				n.paxosMu.Unlock()

				// Fall through to wait for election below
				log.Printf("Node %d: Leader failed, will wait for new election", n.id)
			}
		}

		// No leader known - start election and wait for it to complete
		// (unless we already started one during bootstrap)
		if !bootstrapElectionStarted {
			go n.StartLeaderElection()
		}

		// Wait up to 3 seconds for a leader to be elected
		maxWait := 1000 * time.Millisecond
		pollInterval := 100 * time.Millisecond
		deadline := time.Now().Add(maxWait)

		log.Printf("Node %d: No leader, waiting for election to complete...", n.id)

		for time.Now().Before(deadline) {
			time.Sleep(pollInterval)

			n.paxosMu.RLock()
			currentLeaderID := n.leaderID
			nowLeader := n.isLeader
			n.paxosMu.RUnlock()

			if nowLeader {
				// We became leader during the wait - check if cross-shard
				log.Printf("Node %d: Became leader, processing transaction", n.id)

				// Check if this is a cross-shard transaction (Phase 6: 2PC)
				tx := req.Transaction
				senderCluster := n.config.GetClusterForDataItem(tx.Sender)
				receiverCluster := n.config.GetClusterForDataItem(tx.Receiver)

				log.Printf("Node %d: 🔍 DEBUG - Transaction %d→%d: senderCluster=%d, receiverCluster=%d, crossShard=%v",
					n.id, tx.Sender, tx.Receiver, senderCluster, receiverCluster, senderCluster != receiverCluster)

				// ONLY the sender cluster initiates 2PC!
				if senderCluster != receiverCluster && int(n.clusterID) == senderCluster {
					// Cross-shard transaction AND we're the sender cluster - use 2PC
					log.Printf("Node %d: 🌐 Cross-shard transaction detected (%d in cluster %d → %d in cluster %d) - initiating 2PC as COORDINATOR",
						n.id, tx.Sender, senderCluster, tx.Receiver, receiverCluster)

					success, err := n.TwoPCCoordinator(tx, req.ClientId, req.Timestamp)
					if err != nil {
						return &pb.TransactionReply{
							Success: false,
							Message: fmt.Sprintf("2PC failed: %v", err),
							Result:  pb.ResultType_FAILED,
						}, nil
					}

					if success {
						return &pb.TransactionReply{
							Success: true,
							Message: "Cross-shard transaction committed via 2PC",
							Result:  pb.ResultType_SUCCESS,
						}, nil
					} else {
						return &pb.TransactionReply{
							Success: false,
							Message: "Cross-shard transaction aborted",
							Result:  pb.ResultType_FAILED,
						}, nil
					}
				}

				// Intra-shard: use normal Paxos
				return n.processAsLeader(req)
			} else if currentLeaderID > 0 && currentLeaderID != n.id {
				// A leader was elected, forward to them
				log.Printf("Node %d: Leader %d elected, forwarding transaction", n.id, currentLeaderID)
				client, exists := n.peerClients[currentLeaderID] // Read-only, no lock needed

				if exists {
					ctx3, cancel3 := context.WithTimeout(context.Background(), 1*time.Second)
					defer cancel3()
					resp2, err2 := client.SubmitTransaction(ctx3, req)
					if err2 == nil {
						return resp2, nil
					}
					log.Printf("Node %d: Forward to new leader %d failed: %v", n.id, currentLeaderID, err2)
				}
			}
		}

		// Timeout - no leader elected
		log.Printf("Node %d: Election timeout, no leader available", n.id)
		return nil, fmt.Errorf("no leader available yet")
	}

	// leader: check if this is a cross-shard transaction (Phase 6: 2PC)
	tx := req.Transaction
	senderCluster := n.config.GetClusterForDataItem(tx.Sender)
	receiverCluster := n.config.GetClusterForDataItem(tx.Receiver)

	// ONLY the sender cluster initiates 2PC!
	// The receiver cluster processes it as a normal transaction via Paxos
	if senderCluster != receiverCluster && int(n.clusterID) == senderCluster {
		// Cross-shard transaction AND we're the sender cluster - use 2PC
		log.Printf("Node %d: 🌐 Cross-shard transaction detected (%d in cluster %d → %d in cluster %d) - initiating 2PC as SENDER",
			n.id, tx.Sender, senderCluster, tx.Receiver, receiverCluster)

		success, err := n.TwoPCCoordinator(tx, req.ClientId, req.Timestamp)
		if err != nil {
			return &pb.TransactionReply{
				Success: false,
				Message: fmt.Sprintf("2PC failed: %v", err),
				Result:  pb.ResultType_FAILED,
			}, nil
		}

		if success {
			return &pb.TransactionReply{
				Success: true,
				Message: "Cross-shard transaction committed via 2PC",
				Result:  pb.ResultType_SUCCESS,
			}, nil
		} else {
			return &pb.TransactionReply{
				Success: false,
				Message: "Cross-shard transaction aborted",
				Result:  pb.ResultType_FAILED,
			}, nil
		}
	}

	// Process with pipelined Paxos
	return n.processAsLeaderAsync(req)
}

// ============================================================================
// PHASE 3: READ-ONLY TRANSACTIONS (Balance Queries)
// ============================================================================

// QueryBalance handles read-only balance queries
// No Paxos consensus needed - just read from local replica
// 🔒 FINE-GRAINED: Use balanceMu for read-only balance access
func (n *Node) QueryBalance(ctx context.Context, req *pb.BalanceQueryRequest) (*pb.BalanceQueryReply, error) {
	dataItemID := req.DataItemId

	// Check if node is active
	n.paxosMu.RLock()
	active := n.isActive
	n.paxosMu.RUnlock()

	if !active {
		return &pb.BalanceQueryReply{
			Success:   false,
			Balance:   0,
			Message:   "Node is inactive",
			NodeId:    n.id,
			ClusterId: n.clusterID,
		}, nil
	}

	// Check if this node's cluster owns this data item (no lock needed - config is read-only)
	cluster := int32(n.config.GetClusterForDataItem(dataItemID))
	if cluster != n.clusterID {
		return &pb.BalanceQueryReply{
			Success:   false,
			Balance:   0,
			Message:   fmt.Sprintf("Data item %d not in this cluster (cluster %d owns items in different range)", dataItemID, n.clusterID),
			NodeId:    n.id,
			ClusterId: n.clusterID,
		}, nil
	}

	// Trigger election if no leader (for bootstrap)
	n.paxosMu.RLock()
	hasLeader := n.isLeader || n.leaderID > 0
	n.paxosMu.RUnlock()

	if !hasLeader {
		go n.StartLeaderElection()
	}

	// Read balance from local replica (use balanceMu)
	n.balanceMu.RLock()
	balance, exists := n.balances[dataItemID]
	n.balanceMu.RUnlock()

	if !exists {
		// Item exists in this cluster's range but hasn't been modified yet
		balance = n.config.Data.InitialBalance
	}

	log.Printf("Node %d: 📖 Balance query for item %d = %d", n.id, dataItemID, balance)

	return &pb.BalanceQueryReply{
		Success:   true,
		Balance:   balance,
		Message:   "Balance query successful",
		NodeId:    n.id,
		ClusterId: n.clusterID,
	}, nil
}

// 🚀 processAsLeaderAsync: PIPELINED version that overlaps consensus phases
// Leader can start Accept for seq=2 while seq=1 is still executing
func (n *Node) processAsLeaderAsync(req *pb.TransactionRequest) (*pb.TransactionReply, error) {
	// Start timing for performance measurement
	txnStart := time.Now()

	// Quick check for duplicate (use clientMu)
	n.clientMu.RLock()
	if lastReply, exists := n.clientLastReply[req.ClientId]; exists {
		if req.Timestamp <= n.clientLastTS[req.ClientId] {
			log.Printf("Node %d: 🔄 Returning cached reply for client %s (ts %d)", n.id, req.ClientId, req.Timestamp)
			n.clientMu.RUnlock()
			return lastReply, nil
		}
	}
	n.clientMu.RUnlock()

	// Allocate sequence number (use logMu)
	n.logMu.Lock()
	seq := n.nextSeqNum
	n.nextSeqNum++
	n.logMu.Unlock()

	// Get current ballot (use paxosMu)
	n.paxosMu.RLock()
	ballot := &types.Ballot{
		Number: n.currentBallot.Number,
		NodeID: n.currentBallot.NodeID,
	}
	n.paxosMu.RUnlock()

	// Create log entry
	entry := &types.LogEntry{
		Ballot:  ballot,
		Request: req,
		Status:  "A",
		IsNoOp:  false,
	}

	n.logMu.Lock()
	n.log[seq] = entry
	n.logMu.Unlock()

	// Run consensus
	reply, _ := n.runPipelinedConsensus(seq, ballot, req, entry)

	// Record performance metrics (use microseconds for sub-ms precision)
	txnDurationUs := time.Since(txnStart).Microseconds()
	n.perfMu.Lock()
	n.totalTransactions++
	n.totalTransactionTimeMs += txnDurationUs // Store in microseconds
	n.totalTransactionCount++
	if reply.Success {
		n.successfulTransactions++
	} else {
		n.failedTransactions++
	}
	n.perfMu.Unlock()

	return reply, nil
}

// runPipelinedConsensus runs the Accept→Commit→Execute phases for a sequence
func (n *Node) runPipelinedConsensus(seq int32, ballot *types.Ballot, req *pb.TransactionRequest, entry *types.LogEntry) (*pb.TransactionReply, error) {
	// Send Accept to all peers
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
	}

	// Collect accepts (parallel)
	acceptedCount := 1 // Leader accepts
	var wg sync.WaitGroup
	var mu sync.Mutex

	for pid, client := range peers {
		wg.Add(1)
		go func(peerID int32, c pb.PaxosNodeClient) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
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
		n.logMu.Lock()
		delete(n.log, seq)
		n.logMu.Unlock()

		return &pb.TransactionReply{
			Success: false,
			Message: "No quorum - transaction aborted",
			Result:  pb.ResultType_FAILED,
		}, nil
	}

	// Send Commit to peers (async, fire-and-forget)
	commitReq := &pb.CommitRequest{
		Ballot:         ballot.ToProto(),
		SequenceNumber: seq,
		Request:        req,
		IsNoop:         false,
	}
	for pid, cli := range peers {
		go func(peerID int32, c pb.PaxosNodeClient) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, _ = c.Commit(ctx, commitReq)
		}(pid, cli)
	}

	// Commit and execute locally
	result := n.commitAndExecute(seq)

	// Prepare reply
	reply := &pb.TransactionReply{
		Success: result == pb.ResultType_SUCCESS,
		Message: n.getResultMessage(result),
		Ballot:  ballot.ToProto(),
		Result:  result,
	}

	// Store reply for exactly-once (use clientMu)
	n.clientMu.Lock()
	n.clientLastReply[req.ClientId] = reply
	n.clientLastTS[req.ClientId] = req.Timestamp
	n.clientMu.Unlock()

	return reply, nil
}

// processAsLeader runs Phase 2 (send accept; collect accepted; commit; execute)
// NOTE: This is the old synchronous version, kept for compatibility
func (n *Node) processAsLeader(req *pb.TransactionRequest) (*pb.TransactionReply, error) {
	// Check for duplicate in log (use logMu and clientMu)
	n.logMu.RLock()
	for seq, entry := range n.log {
		if entry.Request != nil &&
			entry.Request.ClientId == req.ClientId &&
			entry.Request.Timestamp == req.Timestamp {
			status := entry.Status
			n.logMu.RUnlock()

			log.Printf("Node %d: Duplicate request %s:%d already in log at seq %d (status: %s)",
				n.id, req.ClientId, req.Timestamp, seq, status)

			// If already executed or committed, return success
			if status == "E" || status == "C" {
				// Look for cached reply (use clientMu)
				n.clientMu.RLock()
				lastReply, hasReply := n.clientLastReply[req.ClientId]
				n.clientMu.RUnlock()

				if hasReply {
					log.Printf("Node %d: Returning cached reply for %s:%d", n.id, req.ClientId, req.Timestamp)
					return lastReply, nil
				}
				// Return success even without cached reply
				return &pb.TransactionReply{
					Success: true,
					Message: "Transaction already committed",
				}, nil
			}

			// Still being processed
			return nil, fmt.Errorf("duplicate request already being processed at seq %d", seq)
		}
	}
	n.logMu.RUnlock()

	// Allocate sequence (use logMu)
	n.logMu.Lock()
	seq := n.nextSeqNum
	n.nextSeqNum++
	n.logMu.Unlock()

	// Copy ballot (use paxosMu)
	n.paxosMu.RLock()
	ballot := types.NewBallot(n.currentBallot.Number, n.currentBallot.NodeID)
	n.paxosMu.RUnlock()

	log.Printf("Node %d: Processing as LEADER - seq=%d", n.id, seq)

	// Create log entry and mark accepted by self
	entry := types.NewLogEntry(ballot, seq, req, false)
	entry.AcceptedBy = make(map[int32]bool)
	entry.AcceptedBy[n.id] = true
	entry.Status = "A" // accepted by leader

	n.logMu.Lock()
	n.log[seq] = entry
	n.logMu.Unlock()

	acceptReq := &pb.AcceptRequest{
		Ballot:         ballot.ToProto(),
		SequenceNumber: seq,
		Request:        req,
		IsNoop:         false,
	}

	// Send Accept requests concurrently
	type respT struct {
		nodeID int32
		ok     bool
		err    error
	}

	// Get peers (read-only after init, no lock needed)
	peers := make(map[int32]pb.PaxosNodeClient, len(n.peerClients))
	for k, v := range n.peerClients {
		peers[k] = v
	}

	respCh := make(chan respT, len(peers))
	for pid, client := range peers {
		go func(pid int32, cli pb.PaxosNodeClient) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			r, err := cli.Accept(ctx, acceptReq)
			if err != nil {
				respCh <- respT{nodeID: pid, ok: false, err: err}
				return
			}
			respCh <- respT{nodeID: r.NodeId, ok: r.Success, err: nil}
		}(pid, client)
	}

	acceptedCount := 1 // leader already accepted
	timeout := time.After(1 * time.Second)
	got := 0
	for got < len(peers) {
		select {
		case r := <-respCh:
			got++
			if r.ok {
				acceptedCount++
				n.logMu.Lock()
				if entry2, exists := n.log[seq]; exists {
					if entry2.AcceptedBy == nil {
						entry2.AcceptedBy = make(map[int32]bool)
					}
					entry2.AcceptedBy[r.nodeID] = true
				}
				n.logMu.Unlock()
			}
			if n.hasQuorum(acceptedCount) {
				// we have quorum early; drain others optionally
				goto QUORUM
			}
		case <-timeout:
			goto QUORUM
		}
	}
QUORUM:

	if !n.hasQuorum(acceptedCount) {
		// DON'T cleanup log[seq] - keep it for NEW-VIEW recovery
		// When more nodes come back, this will be included in NEW-VIEW and can achieve quorum then
		return nil, fmt.Errorf("no quorum for seq %d (have %d, need %d)", seq, acceptedCount, n.quorumSize())
	}

	log.Printf("Node %d: Quorum achieved for seq %d (accepted=%d)", n.id, seq, acceptedCount)

	// send commit to peers (best-effort)
	commitReq := &pb.CommitRequest{
		Ballot:         ballot.ToProto(),
		SequenceNumber: seq,
		Request:        req,
		IsNoop:         false,
	}
	for pid, cli := range peers {
		go func(peerID int32, c pb.PaxosNodeClient) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, _ = c.Commit(ctx, commitReq)
			_ = peerID
		}(pid, cli)
	}

	// leader commits locally and executes (commitAndExecute will execute any pending commit up to seq)
	result := n.commitAndExecute(seq)

	// prepare reply
	reply := &pb.TransactionReply{
		Success: result == pb.ResultType_SUCCESS,
		Message: n.getResultMessage(result),
		Ballot:  ballot.ToProto(),
		Result:  result,
	}

	// Store reply for exactly-once semantics (use clientMu)
	n.clientMu.Lock()
	n.clientLastReply[req.ClientId] = reply
	n.clientLastTS[req.ClientId] = req.Timestamp
	n.clientMu.Unlock()

	return reply, nil
}

// Accept handler (Phase 2a)
func (n *Node) Accept(ctx context.Context, req *pb.AcceptRequest) (*pb.AcceptedReply, error) {
	// 🔒 FINE-GRAINED: Use paxosMu for ballot/leader, logMu for log

	// Mark system as initialized (use paxosMu)
	n.paxosMu.Lock()
	if !n.systemInitialized {
		n.systemInitialized = true
	}

	// Check if node is active
	if !n.isActive {
		n.paxosMu.Unlock()
		return &pb.AcceptedReply{Success: false, Ballot: req.Ballot, SequenceNumber: req.SequenceNumber, NodeId: n.id}, nil
	}

	reqBallot := types.BallotFromProto(req.Ballot)

	// Ballot check
	if reqBallot.Compare(n.promisedBallot) < 0 {
		n.paxosMu.Unlock()
		return &pb.AcceptedReply{Success: false, Ballot: req.Ballot, SequenceNumber: req.SequenceNumber, NodeId: n.id}, nil
	}

	// Update promised ballot and leader ID
	n.promisedBallot = reqBallot
	n.leaderID = reqBallot.NodeID
	isLeader := n.isLeader
	n.paxosMu.Unlock()

	// Reset follower timer IMMEDIATELY on leader activity (ACCEPT is a message from leader)
	// CRITICAL: Do this BEFORE processing to prevent timer from firing during execution
	// MUST be called without holding paxosMu to avoid deadlock
	if !isLeader {
		n.resetLeaderTimer()
	}

	// Store in log
	if req.SequenceNumber > 0 {
		var ent *types.LogEntry
		if req.Phase != "" {
			ent = types.NewLogEntryWithPhase(reqBallot, req.SequenceNumber, req.Request, req.IsNoop, req.Phase)
		} else {
			ent = types.NewLogEntry(reqBallot, req.SequenceNumber, req.Request, req.IsNoop)
		}
		ent.Status = "A"

		n.logMu.Lock()
		if existing, ok := n.log[req.SequenceNumber]; ok {
			if reqBallot.GreaterThan(existing.Ballot) {
				n.log[req.SequenceNumber] = ent
			}
		} else {
			n.log[req.SequenceNumber] = ent
		}
		n.logMu.Unlock()

		// Handle 2PC phase if present
		if req.Phase != "" {
			n.handle2PCPhase(ent, req.Phase)
		}
	}

	return &pb.AcceptedReply{Success: true, Ballot: req.Ballot, SequenceNumber: req.SequenceNumber, NodeId: n.id}, nil
}

// Commit handles commit requests (Phase 2b)
func (n *Node) Commit(ctx context.Context, req *pb.CommitRequest) (*pb.CommitReply, error) {
	// 🔒 FINE-GRAINED: Check if node is active (use paxosMu)
	n.paxosMu.RLock()
	active := n.isActive
	isLeader := n.isLeader
	n.paxosMu.RUnlock()

	if !active {
		return &pb.CommitReply{Success: false}, nil
	}

	// Reset follower timer IMMEDIATELY on leader activity (COMMIT is a message from leader)
	// CRITICAL: Do this BEFORE processing commit to prevent timer from firing during execution
	// This ensures followers don't timeout while leader is actively processing transactions
	if !isLeader {
		n.resetLeaderTimer()
	}

	// Apply commit (skip seq=0 heartbeats)
	if req.SequenceNumber > 0 {
		n.commitAndExecute(req.SequenceNumber)
	}

	// Check if leader should create checkpoint
	// Checkpointing removed for consensus correctness

	return &pb.CommitReply{Success: true}, nil
}

// commitAndExecute commits a seq and executes up to that seq in order
// 🔒 FINE-GRAINED: Use logMu for log access
// backgroundExecutionThread runs as a single goroutine per node
// It executes committed transactions SEQUENTIALLY, guaranteeing linearizability
func (n *Node) backgroundExecutionThread() {
	log.Printf("Node %d: Background execution thread started (sequential execution)", n.id)

	for {
		select {
		case <-n.stopChan:
			log.Printf("Node %d: Background execution thread stopping", n.id)
			return
		case <-n.execNotify:
			// New sequence(s) committed, try to execute
		case <-time.After(500 * time.Microsecond):
			// Periodic check for any pending executions
		}

		// Execute all pending committed sequences in order
		for {
			n.logMu.RLock()
			nextSeq := n.lastExecuted + 1
			entry, exists := n.log[nextSeq]
			n.logMu.RUnlock()

			if !exists {
				break // No more entries
			}

			n.logMu.RLock()
			status := entry.Status
			n.logMu.RUnlock()

			if status != "C" {
				break // Not committed yet
			}

			// Execute this transaction (single-threaded, sequential)
			result := n.executeTransactionSequential(nextSeq, entry)

			// Update result and advance lastExecuted atomically
			n.logMu.Lock()
			if e, ok := n.log[nextSeq]; ok {
				e.Result = result
				e.Status = "E"
			}
			n.lastExecuted = nextSeq
			n.logMu.Unlock()
		}
	}
}

// commitAndExecute marks seq as committed and waits for background thread to execute it
func (n *Node) commitAndExecute(seq int32) pb.ResultType {
	n.logMu.Lock()
	entry, ok := n.log[seq]
	lastExec := n.lastExecuted

	// Already executed - return the stored result
	if seq <= lastExec {
		if entry != nil {
			result := entry.Result
			n.logMu.Unlock()
			return result
		}
		n.logMu.Unlock()
		return pb.ResultType_SUCCESS
	}

	if !ok {
		n.logMu.Unlock()
		return pb.ResultType_FAILED
	}

	// Mark as committed
	entry.Status = "C"
	n.logMu.Unlock()

	// Signal background thread
	select {
	case n.execNotify <- struct{}{}:
	default:
	}

	// Wait for background thread to execute this sequence
	maxWaitMs := 2000
	startTime := time.Now()

	for {
		n.logMu.RLock()
		currentLastExec := n.lastExecuted
		n.logMu.RUnlock()

		if currentLastExec >= seq {
			// Executed! Get the result
			n.logMu.RLock()
			if e, ok := n.log[seq]; ok {
				result := e.Result
				n.logMu.RUnlock()
				return result
			}
			n.logMu.RUnlock()
			return pb.ResultType_SUCCESS
		}

		if time.Since(startTime) > time.Duration(maxWaitMs)*time.Millisecond {
			return pb.ResultType_FAILED
		}

		time.Sleep(100 * time.Microsecond)
	}
}

// executeTransactionSequential executes a single transaction
// ONLY called by backgroundExecutionThread - guaranteed sequential execution
func (n *Node) executeTransactionSequential(seq int32, entry *types.LogEntry) pb.ResultType {
	// Already executed check (shouldn't happen but safety)
	n.logMu.RLock()
	if entry.Status == "E" {
		n.logMu.RUnlock()
		return pb.ResultType_SUCCESS
	}
	n.logMu.RUnlock()

	if entry.IsNoOp {
		return pb.ResultType_SUCCESS
	}

	tx := entry.Request.Transaction
	sender := tx.Sender
	recv := tx.Receiver
	amt := tx.Amount
	clientID := entry.Request.ClientId
	timestamp := entry.Request.Timestamp

	// Determine which cluster owns which items
	senderCluster := n.config.GetClusterForDataItem(sender)
	receiverCluster := n.config.GetClusterForDataItem(recv)
	isCrossShard := senderCluster != receiverCluster

	// For cross-shard transactions executed via 2PC, only process the items we own
	ownsSender := int32(senderCluster) == n.clusterID
	ownsReceiver := int32(receiverCluster) == n.clusterID

	if isCrossShard {
		// Cross-shard transaction - only process our part
		if !ownsSender && !ownsReceiver {
			return pb.ResultType_SUCCESS // Nothing to do for this node
		}

		// Determine which items to lock
		var itemsToProcess []int32
		if ownsSender {
			itemsToProcess = []int32{sender}
		} else {
			itemsToProcess = []int32{recv}
		}

		acquired, lockedItems := n.acquireLocks(itemsToProcess, clientID, timestamp)
		if !acquired {
			return pb.ResultType_FAILED
		}
		defer n.releaseLocks(lockedItems, clientID, timestamp)

		var changedItemID int32
		var result pb.ResultType
		n.balanceMu.Lock()

		if ownsSender {
			senderBalance := n.balances[sender]
			if senderBalance < amt {
				n.balanceMu.Unlock()
				return pb.ResultType_INSUFFICIENT_BALANCE
			}
			n.balances[sender] -= amt
			changedItemID = sender
			result = pb.ResultType_SUCCESS
		} else if ownsReceiver {
			n.balances[recv] += amt
			changedItemID = recv
			result = pb.ResultType_SUCCESS
		} else {
			n.balanceMu.Unlock()
			return pb.ResultType_FAILED
		}
		newBalance := n.balances[changedItemID]
		n.balanceMu.Unlock()

		// Save balance async
		go n.saveBalance(changedItemID, newBalance)

		// Record access pattern
		n.RecordTransactionAccess(sender, recv, isCrossShard)

		return result
	}

	// Regular intra-shard transaction - process both items
	items := []int32{sender, recv}
	acquired, lockedItems := n.acquireLocks(items, clientID, timestamp)

	if !acquired {
		return pb.ResultType_FAILED
	}
	defer n.releaseLocks(lockedItems, clientID, timestamp)

	// ATOMIC: Check balance and apply changes
	n.balanceMu.Lock()
	senderBalance := n.balances[sender]

	if senderBalance < amt {
		n.balanceMu.Unlock()
		return pb.ResultType_INSUFFICIENT_BALANCE
	}

	// Apply balance changes
	n.balances[sender] -= amt
	n.balances[recv] += amt
	newSenderBalance := n.balances[sender]
	newRecvBalance := n.balances[recv]
	n.balanceMu.Unlock()

	// WAL for rollback support
	txnID := fmt.Sprintf("txn-%s-%d", clientID, timestamp)
	n.createWALEntry(txnID, seq)
	n.writeToWAL(txnID, types.OpTypeDebit, sender, senderBalance, newSenderBalance)
	n.writeToWAL(txnID, types.OpTypeCredit, recv, n.balances[recv]-amt, newRecvBalance)

	// Record access pattern
	n.RecordTransactionAccess(sender, recv, senderCluster != receiverCluster)

	// Commit WAL and save balances async
	n.commitWAL(txnID)
	go func() {
		n.saveBalance(sender, newSenderBalance)
		n.saveBalance(recv, newRecvBalance)
	}()

	return pb.ResultType_SUCCESS
}
func (n *Node) getResultMessage(r pb.ResultType) string {
	switch r {
	case pb.ResultType_SUCCESS:
		return "Transaction successful"
	case pb.ResultType_INSUFFICIENT_BALANCE:
		return "Insufficient balance"
	default:
		return "Failed"
	}
}

// Utility debug/admin RPCs and print functions
func (n *Node) GetStatus(ctx context.Context, req *pb.StatusRequest) (*pb.StatusReply, error) {
	// Use fine-grained locks
	n.paxosMu.RLock()
	isLeader := n.isLeader
	ballotNum := n.currentBallot.Number
	leaderID := n.leaderID
	n.paxosMu.RUnlock()

	n.logMu.RLock()
	nextSeq := n.nextSeqNum
	logLen := int32(len(n.log))
	n.logMu.RUnlock()

	return &pb.StatusReply{
		NodeId:               n.id,
		IsLeader:             isLeader,
		CurrentBallotNumber:  ballotNum,
		CurrentLeaderId:      leaderID,
		NextSequenceNumber:   nextSeq,
		TransactionsReceived: logLen,
	}, nil
}

// GetPerformance returns performance metrics for this node
// Measures throughput and latency from client request to response
func (n *Node) GetPerformance(ctx context.Context, req *pb.GetPerformanceRequest) (*pb.GetPerformanceReply, error) {
	n.perfMu.RLock()
	defer n.perfMu.RUnlock()

	// Calculate averages (stored in microseconds, convert to milliseconds)
	var avgTxnTimeMs float64
	if n.totalTransactionCount > 0 {
		avgTxnTimeMs = float64(n.totalTransactionTimeMs) / float64(n.totalTransactionCount) / 1000.0
	}

	var avg2PCTimeMs float64
	if n.total2PCCount > 0 {
		avg2PCTimeMs = float64(n.total2PCTimeMs) / float64(n.total2PCCount)
	}

	// Calculate uptime
	uptimeSeconds := int64(time.Since(n.startTime).Seconds())

	// Calculate throughput (transactions per second)
	var throughputStr string
	if uptimeSeconds > 0 {
		throughput := float64(n.successfulTransactions) / float64(uptimeSeconds)
		throughputStr = fmt.Sprintf("%.2f TPS", throughput)
	} else {
		throughputStr = "N/A (< 1s uptime)"
	}

	reply := &pb.GetPerformanceReply{
		Success:                true,
		NodeId:                 n.id,
		ClusterId:              n.clusterID,
		TotalTransactions:      n.totalTransactions,
		SuccessfulTransactions: n.successfulTransactions,
		FailedTransactions:     n.failedTransactions,
		TwopcCoordinator:       n.twoPCCoordinator,
		TwopcParticipant:       n.twoPCParticipant,
		TwopcCommits:           n.twoPCCommits,
		TwopcAborts:            n.twoPCAborts,
		AvgTransactionTimeMs:   avgTxnTimeMs,
		Avg_2PcTimeMs:          avg2PCTimeMs,
		ElectionsStarted:       n.electionsStarted,
		ElectionsWon:           n.electionsWon,
		ProposalsMade:          n.proposalsMade,
		ProposalsAccepted:      n.proposalsAccepted,
		LocksAcquired:          n.locksAcquired,
		LocksTimeout:           n.locksTimeout,
		UptimeSeconds:          uptimeSeconds,
		Message:                throughputStr,
	}

	// Reset counters if requested
	if req.ResetCounters {
		n.perfMu.RUnlock()
		n.perfMu.Lock()
		n.totalTransactions = 0
		n.successfulTransactions = 0
		n.failedTransactions = 0
		n.twoPCCoordinator = 0
		n.twoPCParticipant = 0
		n.twoPCCommits = 0
		n.twoPCAborts = 0
		n.electionsStarted = 0
		n.electionsWon = 0
		n.proposalsMade = 0
		n.proposalsAccepted = 0
		n.locksAcquired = 0
		n.locksTimeout = 0
		n.totalTransactionTimeMs = 0
		n.totalTransactionCount = 0
		n.total2PCTimeMs = 0
		n.total2PCCount = 0
		n.startTime = time.Now()
		n.perfMu.Unlock()
		n.perfMu.RLock()
	}

	return reply, nil
}

func (n *Node) DebugPrintDB() {
	// Collect modified items (use balanceMu)
	n.balanceMu.RLock()
	modifiedItems := make(map[int32]int32)
	initialBalance := n.config.Data.InitialBalance
	for itemID, balance := range n.balances {
		if balance != initialBalance {
			modifiedItems[itemID] = balance
		}
	}
	n.balanceMu.RUnlock()

	fmt.Printf("\n╔════════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d (Cluster %d) - Database     ║\n", n.id, n.clusterID)
	fmt.Printf("╚════════════════════════════════════════╝\n")

	if len(modifiedItems) == 0 {
		fmt.Printf("  No modified items (all at initial balance %d)\n", initialBalance)
	} else {
		fmt.Printf("  Modified items:\n")
		// Print in sorted order for consistency
		itemIDs := make([]int32, 0, len(modifiedItems))
		for itemID := range modifiedItems {
			itemIDs = append(itemIDs, itemID)
		}
		// Simple sort
		for i := 0; i < len(itemIDs); i++ {
			for j := i + 1; j < len(itemIDs); j++ {
				if itemIDs[i] > itemIDs[j] {
					itemIDs[i], itemIDs[j] = itemIDs[j], itemIDs[i]
				}
			}
		}
		for _, itemID := range itemIDs {
			fmt.Printf("  Item %d: %d\n", itemID, modifiedItems[itemID])
		}
	}
	fmt.Println()
}

func (n *Node) PrintLog() {
	// Use logMu
	n.logMu.RLock()
	nextSeq := n.nextSeqNum
	// Copy log for printing (to avoid holding lock)
	logCopy := make(map[int32]*types.LogEntry)
	for k, v := range n.log {
		logCopy[k] = v
	}
	n.logMu.RUnlock()

	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d - Transaction Log                           ║\n", n.id)
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")
	for seq := int32(1); seq <= nextSeq; seq++ {
		if ent, ok := logCopy[seq]; ok {
			if ent.IsNoOp {
				fmt.Printf("  Seq %d: [NO-OP] Status: %s\n", seq, ent.Status)
			} else {
				tx := ent.Request.Transaction
				fmt.Printf("  Seq %d: (%s->%s:%d) Status: %s, Ballot: %s\n",
					seq, tx.Sender, tx.Receiver, tx.Amount, ent.Status, ent.Ballot.String())
			}
		}
	}
	fmt.Println()
}

func (n *Node) PrintStatus(seq int32) {
	n.logMu.RLock()
	ent, ok := n.log[seq]
	n.logMu.RUnlock()

	fmt.Printf("\n╔════════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d - Status for Seq %d        ║\n", n.id, seq)
	fmt.Printf("╚════════════════════════════════════════╝\n")
	if !ok {
		fmt.Printf("  Status: X (No entry)\n\n")
		return
	}
	fmt.Printf("  Status: %s\n", ent.Status)
	fmt.Printf("  Ballot: %s\n", ent.Ballot.String())
	if !ent.IsNoOp {
		tx := ent.Request.Transaction
		fmt.Printf("  Transaction: %s->%s:%d\n", tx.Sender, tx.Receiver, tx.Amount)
	} else {
		fmt.Printf("  Transaction: NO-OP\n")
	}
	fmt.Println()
}

func (n *Node) DebugPrintView() {
	n.logMu.RLock()
	newViewLen := len(n.newViewLog)
	viewCopy := make([]*pb.NewViewRequest, len(n.newViewLog))
	copy(viewCopy, n.newViewLog)
	n.logMu.RUnlock()

	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d - NEW-VIEW History                          ║\n", n.id)
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")
	if newViewLen == 0 {
		fmt.Println("  No NEW-VIEW messages yet")
		return
	}
	for i, nv := range viewCopy {
		b := types.BallotFromProto(nv.Ballot)
		fmt.Printf("\n  NEW-VIEW #%d: Ballot: %s (Leader Node %d) AcceptMsgs: %d\n", i+1, b.String(), b.NodeID, len(nv.AcceptMessages))
		for j, acc := range nv.AcceptMessages {
			if acc.IsNoop {
				fmt.Printf("    %d. Seq %d: NO-OP\n", j+1, acc.SequenceNumber)
			} else {
				tx := acc.Request.Transaction
				fmt.Printf("    %d. Seq %d: %s->%s:%d\n", j+1, acc.SequenceNumber, tx.Sender, tx.Receiver, tx.Amount)
			}
		}
	}
	fmt.Println()
}

func (n *Node) ShowStatus() {
	// Collect status with fine-grained locks
	n.paxosMu.RLock()
	isLeader := n.isLeader
	leaderID := n.leaderID
	currentBallot := n.currentBallot.String()
	promisedBallot := n.promisedBallot.String()
	isActive := n.isActive
	n.paxosMu.RUnlock()

	n.logMu.RLock()
	nextSeq := n.nextSeqNum
	lastExec := n.lastExecuted
	logLen := len(n.log)
	viewLen := len(n.newViewLog)
	n.logMu.RUnlock()

	fmt.Printf("\n╔════════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d - Current Status            ║\n", n.id)
	fmt.Printf("╚════════════════════════════════════════╝\n")
	leaderStr := "No (Follower)"
	if isLeader {
		leaderStr = "Yes (LEADER) 👑"
	}
	fmt.Printf("  Is Leader: %s\n", leaderStr)
	fmt.Printf("  Current Leader: Node %d\n", leaderID)
	fmt.Printf("  Current Ballot: %s\n", currentBallot)
	fmt.Printf("  Promised Ballot: %s\n", promisedBallot)
	fmt.Printf("  Next Sequence: %d\n", nextSeq)
	fmt.Printf("  Last Executed: %d\n", lastExec)
	fmt.Printf("  Log Entries: %d\n", logLen)
	fmt.Printf("  NEW-VIEW Count: %d\n", viewLen)
	fmt.Printf("  Active: %v\n", isActive)
	fmt.Println()
}

// SetActive allows external control to activate/deactivate a node
// This is kept simple to avoid any potential deadlocks
func (n *Node) SetActive(ctx context.Context, req *pb.SetActiveRequest) (*pb.SetActiveReply, error) {
	// DIAGNOSTIC: Log immediately when RPC is received
	log.Printf("Node %d: ⚙️  SetActive RPC RECEIVED: req.Active=%v", n.id, req.Active)

	n.paxosMu.Lock()
	wasActive := n.isActive
	n.isActive = req.Active
	wasLeader := n.isLeader
	currentLeaderID := n.leaderID

	log.Printf("Node %d: ⚙️  SetActive PROCESSING: wasActive=%v, nowActive=%v, wasLeader=%v, leaderID=%d",
		n.id, wasActive, n.isActive, wasLeader, currentLeaderID)

	// BOOTSTRAP FIX: Clear leader state for ALL nodes when going inactive
	// Each test set should start fresh with no knowledge of previous leaders
	if !req.Active {
		n.isLeader = false
		n.leaderID = -1                          // -1 indicates no known leader (fresh start)
		n.promisedBallot = types.NewBallot(0, 0) // Clear promised ballot for fresh elections
		n.currentBallot = types.NewBallot(0, n.id)
		log.Printf("Node %d: ⚙️  Set to INACTIVE - clearing ALL leader state (leaderID → -1, ballots → 0)", n.id)
	}

	currentActive := n.isActive
	n.paxosMu.Unlock()

	if req.Active {
		// Node is now active (either newly activated or already was active)
		if wasActive {
			log.Printf("Node %d: ✅ Node already ACTIVE", n.id)
		} else {
			log.Printf("Node %d: ✅ Node set to ACTIVE (fresh start, no prior leader knowledge)", n.id)
			// Don't trigger election here - wait for first transaction
			// The client will send to n1/n4/n7 first, and they will become leaders
		}
	} else {
		log.Printf("Node %d: ⏸️  Node set to INACTIVE (simulating failure)", n.id)

		// Stop heartbeat if was leader
		if wasLeader {
			n.stopHeartbeat()
		}
	}

	log.Printf("Node %d: ⚙️  SetActive RPC COMPLETED: returning Active=%v", n.id, currentActive)

	return &pb.SetActiveReply{
		Success: true,
		NodeId:  n.id,
		Active:  currentActive,
	}, nil
}

// GetLogEntry retrieves a specific log entry for recovery purposes
func (n *Node) GetLogEntry(ctx context.Context, req *pb.GetLogEntryRequest) (*pb.GetLogEntryReply, error) {
	seq := req.SequenceNumber

	// Check log (use logMu)
	n.logMu.RLock()
	entry, exists := n.log[seq]
	n.logMu.RUnlock()

	if exists {
		return &pb.GetLogEntryReply{
			Success: true,
			Entry: &pb.AcceptedEntry{
				Ballot:         entry.Ballot.ToProto(),
				SequenceNumber: seq,
				Request:        entry.Request,
				IsNoop:         entry.IsNoOp,
			},
		}, nil
	}

	// Entry not found
	return &pb.GetLogEntryReply{
		Success: false,
		Entry:   nil,
	}, nil
}
