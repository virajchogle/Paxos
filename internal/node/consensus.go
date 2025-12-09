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
			// No need for lock here - peerClients is read-only after initialization
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

	// 🚀 PIPELINING: Process with normal Paxos
	// executeTransaction will handle the cross-shard logic (debit or credit based on ownership)
	log.Printf("Node %d: 🚀 Processing transaction %d→%d via PIPELINED Paxos (cluster %d)",
		n.id, tx.Sender, tx.Receiver, n.clusterID)

	// PIPELINING: Start consensus asynchronously, return result via channel
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

	log.Printf("Node %d: 🚀 PARALLEL: Allocated seq=%d for client %s (parallel consensus)", n.id, seq, req.ClientId)

	// 🚀 RUN CONSENSUS DIRECTLY - Let fine-grained locking handle parallelism
	// No artificial semaphore limit - the system naturally throttles via execution serialization
	reply, _ := n.runPipelinedConsensus(seq, ballot, req, entry)
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
		log.Printf("Node %d: ❌ No quorum for seq %d (accepted=%d, need=%d) - removing from log", n.id, seq, acceptedCount, quorum)

		// CRITICAL: Remove entry from log immediately
		// Rationale:
		// - Client expects accurate feedback
		// - If we keep entry, NEW-VIEW might commit it later
		// - Client already received FAILED, but transaction would succeed
		// - This creates inconsistency between client state and cluster state
		// - Better to fail cleanly and let client retry if needed

		n.logMu.Lock()
		delete(n.log, seq)
		n.logMu.Unlock()

		return &pb.TransactionReply{
			Success: false,
			Message: "No quorum - transaction aborted",
			Result:  pb.ResultType_FAILED,
		}, nil
	}

	log.Printf("Node %d: ✅ Quorum for seq=%d (accepted=%d)", n.id, seq, acceptedCount)

	// Send Commit to peers (async, fire-and-forget)
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
				log.Printf("Node %d: ACCEPTED from node %d", n.id, r.nodeID)
			} else {
				log.Printf("Node %d: ACCEPT rejected by node %d (err=%v)", n.id, r.nodeID, r.err)
			}
			if n.hasQuorum(acceptedCount) {
				// we have quorum early; drain others optionally
				goto QUORUM
			}
		case <-timeout:
			log.Printf("Node %d: Timeout waiting accepts (got %d)", n.id, acceptedCount)
			goto QUORUM
		}
	}
QUORUM:

	if !n.hasQuorum(acceptedCount) {
		log.Printf("Node %d: No quorum for seq %d (%d/%d) - keeping in log for later NEW-VIEW", n.id, seq, acceptedCount, n.quorumSize())
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

	// Store in log (use logMu)
	if req.SequenceNumber > 0 {
		phaseStr := ""
		if req.Phase != "" {
			phaseStr = fmt.Sprintf(", phase='%s'", req.Phase)
		}
		log.Printf("Node %d: Received ACCEPT seq=%d, ballot=%s%s", n.id, req.SequenceNumber, reqBallot.String(), phaseStr)

		// Create log entry with phase marker
		var ent *types.LogEntry
		if req.Phase != "" {
			ent = types.NewLogEntryWithPhase(reqBallot, req.SequenceNumber, req.Request, req.IsNoop, req.Phase)
		} else {
			ent = types.NewLogEntry(reqBallot, req.SequenceNumber, req.Request, req.IsNoop)
		}
		ent.Status = "A"

		n.logMu.Lock()
		if existing, ok := n.log[req.SequenceNumber]; ok {
			// merge: choose entry with higher ballot if necessary
			existingBallot := existing.Ballot
			if reqBallot.GreaterThan(existingBallot) {
				n.log[req.SequenceNumber] = ent
			}
		} else {
			n.log[req.SequenceNumber] = ent
		}
		n.logMu.Unlock()

		// NEW: If this is a 2PC transaction (has phase marker), handle it
		if req.Phase != "" {
			log.Printf("Node %d: 2PC phase '%s' detected for seq=%d, calling handle2PCPhase", n.id, req.Phase, req.SequenceNumber)
			n.handle2PCPhase(ent, req.Phase)
		}

		log.Printf("Node %d: ✓ ACCEPTED seq=%d", n.id, req.SequenceNumber)
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

	n.logMu.RLock()
	lastExec := n.lastExecuted
	n.logMu.RUnlock()

	if !active {
		log.Printf("Node %d: ⚠️  COMMIT REJECTED - node is inactive", n.id)
		return &pb.CommitReply{Success: false}, nil
	}

	// Reset follower timer IMMEDIATELY on leader activity (COMMIT is a message from leader)
	// CRITICAL: Do this BEFORE processing commit to prevent timer from firing during execution
	// This ensures followers don't timeout while leader is actively processing transactions
	if !isLeader {
		n.resetLeaderTimer()
	}

	// Log and apply commit (skip seq=0 heartbeats)
	if req.SequenceNumber > 0 {
		if !req.IsNoop {
			log.Printf("Node %d: 📬 RECEIVED COMMIT seq=%d (isLeader=%v, lastExecuted=%d)",
				n.id, req.SequenceNumber, isLeader, lastExec)
		}
		result := n.commitAndExecute(req.SequenceNumber)
		log.Printf("Node %d: 🔄 COMMIT seq=%d executed with result=%v", n.id, req.SequenceNumber, result)
	}

	// Check if leader should create checkpoint
	// Checkpointing removed for consensus correctness

	return &pb.CommitReply{Success: true}, nil
}

// commitAndExecute commits a seq and executes up to that seq in order
// 🔒 FINE-GRAINED: Use logMu for log access
func (n *Node) commitAndExecute(seq int32) pb.ResultType {
	// Ensure entry exists and mark as committed (use logMu)
	n.logMu.Lock()
	entry, ok := n.log[seq]
	lastExec := n.lastExecuted

	log.Printf("Node %d: 🔍 commitAndExecute seq=%d: entry_exists=%v, lastExecuted=%d", n.id, seq, ok, lastExec)

	if !ok {
		// We received a COMMIT for a sequence we don't have in our log
		n.logMu.Unlock()

		if seq > lastExec+1 {
			log.Printf("Node %d: ❌ Missing log entry for seq %d (lastExecuted=%d) - will be filled by NEW-VIEW", n.id, seq, lastExec)
		} else {
			log.Printf("Node %d: ❌ Missing log entry for seq %d but it's next in line (lastExecuted=%d)", n.id, seq, lastExec)
		}
		return pb.ResultType_FAILED
	}

	// Mark committed
	oldStatus := entry.Status
	entry.Status = "C"
	log.Printf("Node %d: ✅ Marked seq=%d as COMMITTED (was %s)", n.id, seq, oldStatus)
	n.logMu.Unlock()

	// Sequential execution: Execute all committed entries from lastExecuted+1 up to seq
	// This ensures linearizability by maintaining sequential order
	log.Printf("Node %d: ▶️  Executing committed entries from %d to %d", n.id, lastExec+1, seq)

	finalResult := pb.ResultType_FAILED // ← CRITICAL FIX: Default to FAILED, not SUCCESS!
	for nextSeq := lastExec + 1; nextSeq <= seq; nextSeq++ {
		n.logMu.RLock()
		nextEntry, exists := n.log[nextSeq]
		n.logMu.RUnlock()

		if !exists {
			log.Printf("Node %d: ⚠️  Gap in log at seq %d (skipping for now)", n.id, nextSeq)
			break // Don't skip gaps - wait for recovery
		}

		n.logMu.RLock()
		nextStatus := nextEntry.Status
		n.logMu.RUnlock()

		if nextStatus != "C" && nextStatus != "E" {
			log.Printf("Node %d: ⚠️  Seq %d not committed yet (status=%s), stopping execution", n.id, nextSeq, nextStatus)
			break // Don't execute uncommitted entries
		}

		result := n.executeTransaction(nextSeq, nextEntry)
		if nextSeq == seq {
			finalResult = result // Return result of target sequence
		}
	}

	return finalResult
}

func (n *Node) executeTransaction(seq int32, entry *types.LogEntry) pb.ResultType {
	// 🔒 SEQUENTIAL EXECUTION: Use execMu to ensure linearizability
	// Only one transaction executes at a time
	n.execMu.Lock()
	defer n.execMu.Unlock()

	n.logMu.RLock()
	status := entry.Status
	n.logMu.RUnlock()

	if status == "E" {
		log.Printf("Node %d: executeTransaction seq=%d already executed, skipping", n.id, seq)
		return pb.ResultType_SUCCESS
	}

	log.Printf("Node %d: 🎬 executeTransaction START seq=%d (sequential)", n.id, seq)

	// Sequential execution: Verify this is the next expected sequence
	n.logMu.RLock()
	lastExec := n.lastExecuted
	n.logMu.RUnlock()

	if seq != lastExec+1 {
		log.Printf("Node %d: ⚠️  Seq %d out of order (lastExecuted=%d), skipping", n.id, seq, lastExec)
		return pb.ResultType_FAILED
	}

	if entry.IsNoOp {
		log.Printf("Node %d: Executing NO-OP seq %d", n.id, seq)
		n.logMu.Lock()
		entry.Status = "E"
		if seq > n.lastExecuted {
			n.lastExecuted = seq
		}
		n.logMu.Unlock()
		return pb.ResultType_SUCCESS
	}

	tx := entry.Request.Transaction
	sender := tx.Sender // Now int32 data item ID
	recv := tx.Receiver // Now int32 data item ID
	amt := tx.Amount
	clientID := entry.Request.ClientId
	timestamp := entry.Request.Timestamp

	log.Printf("Node %d: 🔍 executeTransaction seq=%d: %d→%d:%d", n.id, seq, sender, recv, amt)

	// Determine which cluster owns which items
	senderCluster := n.config.GetClusterForDataItem(sender)
	receiverCluster := n.config.GetClusterForDataItem(recv)
	isCrossShard := senderCluster != receiverCluster

	// For cross-shard transactions executed via 2PC, only process the items we own
	ownsSender := int32(senderCluster) == n.clusterID
	ownsReceiver := int32(receiverCluster) == n.clusterID

	if isCrossShard {
		// This is a cross-shard transaction - only process our part
		log.Printf("Node %d: 🔍 Cross-shard detected seq=%d: sender=%d(C%d), recv=%d(C%d), ourCluster=%d, ownsSender=%v, ownsRecv=%v",
			n.id, seq, sender, senderCluster, recv, receiverCluster, n.clusterID, ownsSender, ownsReceiver)

		if !ownsSender && !ownsReceiver {
			// We don't own either item - skip execution
			log.Printf("Node %d: Skipping cross-shard transaction seq %d (neither item in our cluster %d)",
				n.id, seq, n.clusterID)
			n.logMu.Lock()
			entry.Status = "E"
			if seq > n.lastExecuted {
				n.lastExecuted = seq
			}
			n.logMu.Unlock()
			return pb.ResultType_SUCCESS
		}

		// Determine which items to lock and process
		var itemsToProcess []int32
		if ownsSender {
			itemsToProcess = []int32{sender}
		} else {
			itemsToProcess = []int32{recv}
		}

		// Acquire locks (acquireLocks has its own lockMu, no conflict)
		acquired, lockedItems := n.acquireLocks(itemsToProcess, clientID, timestamp)

		if !acquired {
			log.Printf("Node %d: ⚠️  Failed to acquire locks for seq %d (items %v), marking as FAILED",
				n.id, seq, itemsToProcess)
			n.logMu.Lock()
			entry.Status = "E"
			if seq > n.lastExecuted {
				n.lastExecuted = seq
			}
			n.logMu.Unlock()
			return pb.ResultType_FAILED
		}

		// Ensure locks are released after execution
		defer n.releaseLocks(lockedItems, clientID, timestamp)

		// ATOMIC: Check balance and apply changes while holding balanceMu
		// This prevents TOCTOU race condition between check and deduct
		var changedItemID int32
		var result pb.ResultType
		n.balanceMu.Lock()

		if ownsSender {
			// Check and debit from sender ATOMICALLY
			senderBalance := n.balances[sender]
			if senderBalance < amt {
				n.balanceMu.Unlock()
				log.Printf("Node %d: INSUFFICIENT BALANCE for item %d (has %d needs %d)",
					n.id, sender, senderBalance, amt)
				n.logMu.Lock()
				entry.Status = "E"
				entry.Result = pb.ResultType_INSUFFICIENT_BALANCE
				if seq > n.lastExecuted {
					n.lastExecuted = seq
				}
				n.logMu.Unlock()
				return pb.ResultType_INSUFFICIENT_BALANCE
			}
			// Balance sufficient - deduct immediately (still holding lock)
			senderOldBalance := n.balances[sender]
			n.balances[sender] -= amt
			changedItemID = sender
			result = pb.ResultType_SUCCESS
			log.Printf("Node %d: ✅ EXECUTED seq=%d (2PC-DEBIT): item %d: %d→%d (owns sender, cluster %d)",
				n.id, seq, sender, senderOldBalance, n.balances[sender], n.clusterID)
		} else if ownsReceiver {
			// Credit to receiver
			recvOldBalance := n.balances[recv]
			n.balances[recv] += amt
			changedItemID = recv
			result = pb.ResultType_SUCCESS
			log.Printf("Node %d: ✅ EXECUTED seq=%d (2PC-CREDIT): item %d: %d→%d (owns receiver, cluster %d)",
				n.id, seq, recv, recvOldBalance, n.balances[recv], n.clusterID)
		} else {
			n.balanceMu.Unlock()
			log.Printf("Node %d: ⚠️  UNEXPECTED: Cross-shard but don't own sender or receiver!", n.id)
			n.logMu.Lock()
			entry.Status = "E"
			entry.Result = pb.ResultType_FAILED
			if seq > n.lastExecuted {
				n.lastExecuted = seq
			}
			n.logMu.Unlock()
			return pb.ResultType_FAILED
		}
		newBalance := n.balances[changedItemID]
		n.balanceMu.Unlock()

		// Update log and lastExecuted atomically (use logMu)
		n.logMu.Lock()
		entry.Status = "E"
		entry.Result = result
		// Update lastExecuted to highest contiguous sequence
		if seq > n.lastExecuted {
			n.lastExecuted = seq
		}
		n.logMu.Unlock()

		// Save only the changed balance (FAST! ~3-8ms)
		if err := n.saveBalance(changedItemID, newBalance); err != nil {
			log.Printf("Node %d: ⚠️  Warning - failed to save balance: %v", n.id, err)
		}

		// Record access pattern (no locks needed)
		n.RecordTransactionAccess(sender, recv, isCrossShard)

		log.Printf("Node %d: ✅ executeTransaction COMPLETE seq=%d (parallel)", n.id, seq)
		return pb.ResultType_SUCCESS
	}

	// Regular intra-shard transaction - process both items as before
	// Acquire locks on both sender and receiver (acquireLocks has its own lockMu)
	items := []int32{sender, recv}
	acquired, lockedItems := n.acquireLocks(items, clientID, timestamp)

	if !acquired {
		log.Printf("Node %d: ⚠️  Failed to acquire locks for seq %d (items %v), marking as FAILED",
			n.id, seq, items)
		n.logMu.Lock()
		entry.Status = "E" // Mark as executed but failed
		if seq > n.lastExecuted {
			n.lastExecuted = seq
		}
		n.logMu.Unlock()
		return pb.ResultType_FAILED
	}

	// Ensure locks are released after execution
	defer n.releaseLocks(lockedItems, clientID, timestamp)

	// ATOMIC: Check balance and apply changes while holding balanceMu
	// This prevents TOCTOU race condition between check and deduct
	n.balanceMu.Lock()
	senderBalance := n.balances[sender]

	if senderBalance < amt {
		n.balanceMu.Unlock()
		log.Printf("Node %d: INSUFFICIENT BALANCE for item %d (has %d needs %d)", n.id, sender, senderBalance, amt)
		n.logMu.Lock()
		entry.Status = "E"
		entry.Result = pb.ResultType_INSUFFICIENT_BALANCE
		if seq > n.lastExecuted {
			n.lastExecuted = seq
		}
		n.logMu.Unlock()
		return pb.ResultType_INSUFFICIENT_BALANCE
	}

	// Balance sufficient - record old values and apply changes atomically
	senderOldBalance := n.balances[sender]
	recvOldBalance := n.balances[recv]

	// Apply balance changes (still holding lock)
	n.balances[sender] -= amt
	n.balances[recv] += amt
	newSenderBalance := n.balances[sender]
	newRecvBalance := n.balances[recv]
	n.balanceMu.Unlock()

	// Phase 5: Create WAL entry after applying changes (for rollback if needed)
	txnID := fmt.Sprintf("txn-%s-%d", clientID, timestamp)
	n.createWALEntry(txnID, seq)

	// Write operations to WAL (has its own walMu)
	n.writeToWAL(txnID, types.OpTypeDebit, sender, senderOldBalance, newSenderBalance)
	n.writeToWAL(txnID, types.OpTypeCredit, recv, recvOldBalance, newRecvBalance)

	// Update log (use logMu)
	n.logMu.Lock()
	entry.Status = "E"
	entry.Result = pb.ResultType_SUCCESS
	if seq > n.lastExecuted {
		n.lastExecuted = seq
	}
	n.logMu.Unlock()

	log.Printf("Node %d: ✅ EXECUTED seq=%d: %d->%d:%d (new: %d=%d, %d=%d) [locked+WAL+parallel]",
		n.id, seq, sender, recv, amt, sender, newSenderBalance, recv, newRecvBalance)

	// Phase 9: Record transaction access pattern for redistribution analysis
	senderClusterForAccess := n.config.GetClusterForDataItem(sender)
	recvClusterForAccess := n.config.GetClusterForDataItem(recv)
	isCrossClusterForAccess := senderClusterForAccess != recvClusterForAccess
	n.RecordTransactionAccess(sender, recv, isCrossClusterForAccess)

	// Commit WAL entry (transaction successful) - has its own walMu
	if err := n.commitWAL(txnID); err != nil {
		log.Printf("Node %d: ⚠️  Warning - failed to commit WAL: %v", n.id, err)
	}

	// Save only changed balances (FAST! ~3-8ms per item)
	if err := n.saveBalance(sender, newSenderBalance); err != nil {
		log.Printf("Node %d: ⚠️  Warning - failed to save sender balance: %v", n.id, err)
	}
	if err := n.saveBalance(recv, newRecvBalance); err != nil {
		log.Printf("Node %d: ⚠️  Warning - failed to save receiver balance: %v", n.id, err)
	}

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
