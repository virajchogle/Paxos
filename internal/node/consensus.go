package node

import (
	"context"
	"fmt"
	"log"
	"time"

	"paxos-banking/internal/types"
	pb "paxos-banking/proto"
)

// SubmitTransaction handles client RPCs. This is the client entrypoint.
func (n *Node) SubmitTransaction(ctx context.Context, req *pb.TransactionRequest) (*pb.TransactionReply, error) {
	// quick snapshot checks
	n.mu.RLock()
	isLeader := n.isLeader
	leaderID := n.leaderID
	active := n.isActive
	systemInit := n.systemInitialized
	nodeID := n.id
	// lookup last reply
	lastReply, hasReply := n.clientLastReply[req.ClientId]
	lastTS, hasTS := n.clientLastTS[req.ClientId]
	n.mu.RUnlock()

	if !active {
		return nil, fmt.Errorf("node inactive")
	}

	// 🎯 Bootstrap: If system not initialized, handle first transaction
	if !systemInit {
		n.mu.Lock()
		if !n.systemInitialized { // Double-check under lock
			n.systemInitialized = true
			n.mu.Unlock()

			if nodeID == 1 {
				// Node 1: Start leader election
				log.Printf("Node %d: 🚀 First transaction received - bootstrapping system and starting leader election", nodeID)
				// Start the leader timer which will trigger election
				n.resetLeaderTimer()
				// Give a small head start to ensure node 1 starts the election
				time.Sleep(100 * time.Millisecond)
				go n.StartLeaderElection()
			} else {
				// Other nodes: Forward to node 1 to trigger bootstrap
				log.Printf("Node %d: 🔄 First transaction received - forwarding to node 1 to bootstrap system", nodeID)
				n.mu.RLock()
				node1Client, hasNode1 := n.peerClients[1]
				n.mu.RUnlock()

				if hasNode1 {
					// Forward to node 1
					ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					resp, err := node1Client.SubmitTransaction(ctx2, req)
					if err == nil {
						return resp, nil
					}
					log.Printf("Node %d: Failed to forward to node 1: %v - will wait for election", nodeID, err)
				} else {
					log.Printf("Node %d: Not connected to node 1 - will wait for election", nodeID)
				}
			}
		} else {
			n.mu.Unlock()
		}

		// Update local variables after initialization
		n.mu.RLock()
		isLeader = n.isLeader
		leaderID = n.leaderID
		n.mu.RUnlock()
	}

	// exactly-once: if request timestamp <= last processed timestamp, resend last reply
	if hasReply && hasTS && req.Timestamp <= lastTS {
		log.Printf("Node %d: Duplicate request from %s (ts=%d, last=%d) -> resending", n.id, req.ClientId, req.Timestamp, lastTS)
		return lastReply, nil
	}

	// if not leader, forward to leader if known
	if !isLeader {
		if leaderID > 0 && leaderID != n.id {
			n.mu.RLock()
			client, ok := n.peerClients[leaderID]
			n.mu.RUnlock()
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
				n.mu.Lock()
				n.leaderID = 0 // Clear failed leader
				n.mu.Unlock()

				// Fall through to wait for election below
				log.Printf("Node %d: Leader failed, will wait for new election", n.id)
			}
		}

		// No leader known - start election and wait for it to complete
		go n.StartLeaderElection()

		// Wait up to 3 seconds for a leader to be elected
		maxWait := 3000 * time.Millisecond
		pollInterval := 100 * time.Millisecond
		deadline := time.Now().Add(maxWait)

		log.Printf("Node %d: No leader, waiting for election to complete...", n.id)

		for time.Now().Before(deadline) {
			time.Sleep(pollInterval)

			n.mu.RLock()
			currentLeaderID := n.leaderID
			nowLeader := n.isLeader
			n.mu.RUnlock()

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
				n.mu.RLock()
				client, exists := n.peerClients[currentLeaderID]
				n.mu.RUnlock()

				if exists {
					ctx3, cancel3 := context.WithTimeout(context.Background(), 3*time.Second)
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

	// Intra-shard OR we're the receiver cluster: process with normal Paxos
	// executeTransaction will handle the cross-shard logic (debit or credit based on ownership)
	log.Printf("Node %d: Processing transaction %d→%d via normal Paxos (cluster %d)",
		n.id, tx.Sender, tx.Receiver, n.clusterID)
	return n.processAsLeader(req)
}

// ============================================================================
// PHASE 3: READ-ONLY TRANSACTIONS (Balance Queries)
// ============================================================================

// QueryBalance handles read-only balance queries
// No Paxos consensus needed - just read from local replica
// No locking needed - read-only operation
func (n *Node) QueryBalance(ctx context.Context, req *pb.BalanceQueryRequest) (*pb.BalanceQueryReply, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	dataItemID := req.DataItemId

	// Check if this node's cluster owns this data item
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

	// Read balance from local replica (no consensus needed)
	balance, exists := n.balances[dataItemID]
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

// processAsLeader runs Phase 2 (send accept; collect accepted; commit; execute)
func (n *Node) processAsLeader(req *pb.TransactionRequest) (*pb.TransactionReply, error) {
	// ✅ FIX: Check if this exact request is already in log
	n.mu.Lock()

	// Check log for duplicate
	for seq, entry := range n.log {
		if entry.Request != nil &&
			entry.Request.ClientId == req.ClientId &&
			entry.Request.Timestamp == req.Timestamp {
			status := entry.Status
			n.mu.Unlock()

			log.Printf("Node %d: Duplicate request %s:%d already in log at seq %d (status: %s)",
				n.id, req.ClientId, req.Timestamp, seq, status)

			// If already executed or committed, return success
			if status == "E" || status == "C" {
				// Look for cached reply
				lastReply, hasReply := n.clientLastReply[req.ClientId]
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

	// allocate sequence
	seq := n.nextSeqNum
	n.nextSeqNum++
	// copy ballot
	ballot := types.NewBallot(n.currentBallot.Number, n.currentBallot.NodeID)
	n.mu.Unlock()

	log.Printf("Node %d: Processing as LEADER - seq=%d", n.id, seq)

	// create log entry and mark accepted by self
	entry := types.NewLogEntry(ballot, seq, req, false)
	entry.AcceptedBy = make(map[int32]bool)
	entry.AcceptedBy[n.id] = true
	entry.Status = "A" // accepted by leader
	n.mu.Lock()
	n.log[seq] = entry
	n.mu.Unlock()

	acceptReq := &pb.AcceptRequest{
		Ballot:         ballot.ToProto(),
		SequenceNumber: seq,
		Request:        req,
		IsNoop:         false,
	}

	// send Accept requests concurrently
	type respT struct {
		nodeID int32
		ok     bool
		err    error
	}
	n.mu.RLock()
	peers := make(map[int32]pb.PaxosNodeClient, len(n.peerClients))
	for k, v := range n.peerClients {
		peers[k] = v
	}
	n.mu.RUnlock()

	respCh := make(chan respT, len(peers))
	for pid, client := range peers {
		go func(pid int32, cli pb.PaxosNodeClient) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	timeout := time.After(3 * time.Second)
	got := 0
	for got < len(peers) {
		select {
		case r := <-respCh:
			got++
			if r.ok {
				acceptedCount++
				n.mu.Lock()
				if entry2, exists := n.log[seq]; exists {
					entry2.AcceptedBy[r.nodeID] = true
				}
				n.mu.Unlock()
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
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
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

	// store reply for exactly-once semantics
	n.mu.Lock()
	n.clientLastReply[req.ClientId] = reply
	n.clientLastTS[req.ClientId] = req.Timestamp
	n.mu.Unlock()

	return reply, nil
}

// Accept handler (Phase 2a)
func (n *Node) Accept(ctx context.Context, req *pb.AcceptRequest) (*pb.AcceptedReply, error) {
	n.mu.Lock()

	// 🎯 Mark system as initialized when receiving ACCEPT
	if !n.systemInitialized {
		n.systemInitialized = true
	}

	// Check if node is active
	if !n.isActive {
		n.mu.Unlock()
		return &pb.AcceptedReply{Success: false, Ballot: req.Ballot, SequenceNumber: req.SequenceNumber, NodeId: n.id}, nil
	}

	reqBallot := types.BallotFromProto(req.Ballot)

	// ballot check
	if reqBallot.Compare(n.promisedBallot) < 0 {
		n.mu.Unlock()
		return &pb.AcceptedReply{Success: false, Ballot: req.Ballot, SequenceNumber: req.SequenceNumber, NodeId: n.id}, nil
	}

	// update promised ballot to this ballot
	n.promisedBallot = reqBallot
	// leader ID updated (we just heard from leader)
	n.leaderID = reqBallot.NodeID

	// Store in log (skip seq=0 which is used for heartbeats)
	if req.SequenceNumber > 0 {
		log.Printf("Node %d: Received ACCEPT seq=%d, ballot=%s", n.id, req.SequenceNumber, reqBallot.String())

		ent := types.NewLogEntry(reqBallot, req.SequenceNumber, req.Request, req.IsNoop)
		ent.Status = "A"
		if existing, ok := n.log[req.SequenceNumber]; ok {
			// merge: choose entry with higher ballot if necessary
			existingBallot := existing.Ballot
			if reqBallot.GreaterThan(existingBallot) {
				n.log[req.SequenceNumber] = ent
			}
		} else {
			n.log[req.SequenceNumber] = ent
		}

		log.Printf("Node %d: ✓ ACCEPTED seq=%d", n.id, req.SequenceNumber)
	}
	n.mu.Unlock()

	// reset follower timer on leader activity
	// MUST be called without holding n.mu to avoid deadlock
	n.resetLeaderTimer()

	return &pb.AcceptedReply{Success: true, Ballot: req.Ballot, SequenceNumber: req.SequenceNumber, NodeId: n.id}, nil
}

// Commit handles commit requests (Phase 2b)
func (n *Node) Commit(ctx context.Context, req *pb.CommitRequest) (*pb.CommitReply, error) {
	// Check if node is active
	n.mu.RLock()
	active := n.isActive
	isLeader := n.isLeader
	lastExec := n.lastExecuted
	n.mu.RUnlock()

	if !active {
		log.Printf("Node %d: ⚠️  COMMIT REJECTED - node is inactive", n.id)
		return &pb.CommitReply{Success: false}, nil
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
func (n *Node) commitAndExecute(seq int32) pb.ResultType {
	n.mu.Lock()
	// ensure entry exists
	entry, ok := n.log[seq]
	lastExec := n.lastExecuted

	log.Printf("Node %d: 🔍 commitAndExecute seq=%d: entry_exists=%v, lastExecuted=%d", n.id, seq, ok, lastExec)

	if !ok {
		// We received a COMMIT for a sequence we don't have in our log
		// This means we missed the ACCEPT message (possibly while inactive)
		n.mu.Unlock()

		if seq > lastExec+1 {
			log.Printf("Node %d: ❌ Missing log entry for seq %d (lastExecuted=%d) - will be filled by NEW-VIEW", n.id, seq, lastExec)
			// Don't trigger recovery here to avoid infinite loops
			// The NEW-VIEW protocol will handle this
		} else {
			log.Printf("Node %d: ❌ Missing log entry for seq %d but it's next in line (lastExecuted=%d)", n.id, seq, lastExec)
		}
		return pb.ResultType_FAILED
	}

	// mark committed
	oldStatus := entry.Status
	entry.Status = "C"
	log.Printf("Node %d: ✅ Marked seq=%d as COMMITTED (was %s)", n.id, seq, oldStatus)
	n.mu.Unlock()

	// execute all committed entries sequentially from lastExecuted+1
	var finalResult pb.ResultType = pb.ResultType_SUCCESS
	n.mu.RLock()
	lastExec = n.lastExecuted
	n.mu.RUnlock()

	log.Printf("Node %d: 🏃 Starting execution loop from seq=%d to reach seq=%d", n.id, lastExec+1, seq)

	for {
		nextSeq := lastExec + 1
		n.mu.RLock()
		comm, exists := n.log[nextSeq]
		status := ""
		if exists {
			status = comm.Status
		}
		n.mu.RUnlock()

		log.Printf("Node %d: 🔄 Checking seq=%d: exists=%v, status=%s", n.id, nextSeq, exists, status)

		// Only execute if entry exists and is committed or executed
		if !exists || (comm.Status != "C" && comm.Status != "E") {
			// No more consecutive committed sequences
			if nextSeq <= seq {
				log.Printf("Node %d: ⚠️  Gap at seq %d prevents execution (exists=%v, status=%s) - target seq %d",
					n.id, nextSeq, exists, status, seq)
			}
			break
		}

		log.Printf("Node %d: ▶️  Executing seq=%d", n.id, nextSeq)
		res := n.executeTransaction(nextSeq, comm)
		log.Printf("Node %d: ✅ Executed seq=%d with result=%v", n.id, nextSeq, res)

		if nextSeq == seq {
			finalResult = res
		}
		lastExec = nextSeq
	}

	log.Printf("Node %d: 🏁 Execution loop finished at seq=%d (target was %d), result=%v", n.id, lastExec, seq, finalResult)
	return finalResult
}

func (n *Node) executeTransaction(seq int32, entry *types.LogEntry) pb.ResultType {
	n.mu.Lock()
	defer n.mu.Unlock()

	log.Printf("Node %d: 🎬 executeTransaction START seq=%d, lastExecuted=%d", n.id, seq, n.lastExecuted)

	if entry.Status == "E" {
		log.Printf("Node %d: executeTransaction seq=%d already executed, skipping", n.id, seq)
		return pb.ResultType_SUCCESS
	}

	// ensure sequential execution
	if seq != n.lastExecuted+1 {
		log.Printf("Node %d: ❌ Cannot execute seq %d, last executed %d (sequence gap!)", n.id, seq, n.lastExecuted)
		return pb.ResultType_FAILED
	}

	if entry.IsNoOp {
		log.Printf("Node %d: Executing NO-OP seq %d", n.id, seq)
		entry.Status = "E"
		n.lastExecuted = seq
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
			entry.Status = "E"
			n.lastExecuted = seq
			return pb.ResultType_SUCCESS
		}

		// Determine which items to lock and process
		var itemsToProcess []int32
		if ownsSender {
			itemsToProcess = []int32{sender}
		} else {
			itemsToProcess = []int32{recv}
		}

		// Phase 2: Acquire locks only on items we own
		n.mu.Unlock()
		acquired, lockedItems := n.acquireLocks(itemsToProcess, clientID, timestamp)
		n.mu.Lock()

		if !acquired {
			log.Printf("Node %d: ⚠️  Failed to acquire locks for seq %d (items %v), marking as FAILED",
				n.id, seq, itemsToProcess)
			entry.Status = "E"
			n.lastExecuted = seq
			return pb.ResultType_FAILED
		}

		// Ensure locks are released after execution
		defer func() {
			n.mu.Unlock()
			n.releaseLocks(lockedItems, clientID, timestamp)
			n.mu.Lock()
		}()

		// Check balance only if we're processing the sender
		if ownsSender {
			if n.balances[sender] < amt {
				log.Printf("Node %d: INSUFFICIENT BALANCE for item %d (has %d needs %d)",
					n.id, sender, n.balances[sender], amt)
				entry.Status = "E"
				n.lastExecuted = seq
				return pb.ResultType_INSUFFICIENT_BALANCE
			}
		}

		// Apply only our part of the transaction
		if ownsSender {
			// Debit from sender
			senderOldBalance := n.balances[sender]
			n.balances[sender] -= amt
			log.Printf("Node %d: ✅ EXECUTED seq=%d (2PC-DEBIT): item %d: %d→%d (owns sender, cluster %d)",
				n.id, seq, sender, senderOldBalance, n.balances[sender], n.clusterID)
		} else if ownsReceiver {
			// Credit to receiver
			recvOldBalance := n.balances[recv]
			n.balances[recv] += amt
			log.Printf("Node %d: ✅ EXECUTED seq=%d (2PC-CREDIT): item %d: %d→%d (owns receiver, cluster %d)",
				n.id, seq, recv, recvOldBalance, n.balances[recv], n.clusterID)
		} else {
			log.Printf("Node %d: ⚠️  UNEXPECTED: Cross-shard but don't own sender or receiver!", n.id)
		}

		entry.Status = "E"
		n.lastExecuted = seq

		// Save database state
		n.mu.Unlock()
		if err := n.saveDatabase(); err != nil {
			log.Printf("Node %d: ⚠️  Warning - failed to save database: %v", n.id, err)
		}
		n.mu.Lock()

		// Record access pattern (after releasing lock to avoid deadlock)
		n.mu.Unlock()
		n.RecordTransactionAccess(sender, recv, isCrossShard)
		n.mu.Lock()

		return pb.ResultType_SUCCESS
	}

	// Regular intra-shard transaction - process both items as before
	// Phase 2: Acquire locks on both sender and receiver before executing
	// Temporarily release main mutex to acquire locks (avoid holding multiple locks)
	n.mu.Unlock()
	items := []int32{sender, recv}
	acquired, lockedItems := n.acquireLocks(items, clientID, timestamp)
	n.mu.Lock()

	if !acquired {
		log.Printf("Node %d: ⚠️  Failed to acquire locks for seq %d (items %v), marking as FAILED",
			n.id, seq, items)
		entry.Status = "E" // Mark as executed but failed
		n.lastExecuted = seq
		return pb.ResultType_FAILED
	}

	// Ensure locks are released after execution
	defer func() {
		n.mu.Unlock()
		n.releaseLocks(lockedItems, clientID, timestamp)
		n.mu.Lock()
	}()

	// check balance
	if n.balances[sender] < amt {
		log.Printf("Node %d: INSUFFICIENT BALANCE for item %d (has %d needs %d)", n.id, sender, n.balances[sender], amt)
		entry.Status = "E"
		n.lastExecuted = seq
		return pb.ResultType_INSUFFICIENT_BALANCE
	}

	// Phase 5: Create WAL entry before applying changes (for future 2PC rollback)
	txnID := fmt.Sprintf("txn-%s-%d", clientID, timestamp)
	n.createWALEntry(txnID, seq)

	// Record old values in WAL before modifying
	senderOldBalance := n.balances[sender]
	recvOldBalance := n.balances[recv]

	// Write operations to WAL
	n.mu.Unlock() // Release main lock while writing to WAL
	n.writeToWAL(txnID, types.OpTypeDebit, sender, senderOldBalance, senderOldBalance-amt)
	n.writeToWAL(txnID, types.OpTypeCredit, recv, recvOldBalance, recvOldBalance+amt)
	n.mu.Lock() // Re-acquire main lock

	// apply
	n.balances[sender] -= amt
	n.balances[recv] += amt
	entry.Status = "E"
	n.lastExecuted = seq

	log.Printf("Node %d: ✅ EXECUTED seq=%d: %d->%d:%d (new: %d=%d, %d=%d) [locked+WAL]",
		n.id, seq, sender, recv, amt, sender, n.balances[sender], recv, n.balances[recv])

	// Phase 9: Record transaction access pattern for redistribution analysis
	senderClusterForAccess := n.config.GetClusterForDataItem(sender)
	recvClusterForAccess := n.config.GetClusterForDataItem(recv)
	isCrossClusterForAccess := senderClusterForAccess != recvClusterForAccess
	n.RecordTransactionAccess(sender, recv, isCrossClusterForAccess)

	// Commit WAL entry (transaction successful)
	n.mu.Unlock()
	if err := n.commitWAL(txnID); err != nil {
		log.Printf("Node %d: ⚠️  Warning - failed to commit WAL: %v", n.id, err)
	}
	n.mu.Lock()

	// Save database state to disk (must release lock first to avoid holding it during I/O)
	n.mu.Unlock()
	if err := n.saveDatabase(); err != nil {
		log.Printf("Node %d: ⚠️  Warning - failed to save database: %v", n.id, err)
	}
	n.mu.Lock()

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

// Utility debug/admin RPCs and print functions follow (unchanged style)
func (n *Node) GetStatus(ctx context.Context, req *pb.StatusRequest) (*pb.StatusReply, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return &pb.StatusReply{
		NodeId:               n.id,
		IsLeader:             n.isLeader,
		CurrentBallotNumber:  n.currentBallot.Number,
		CurrentLeaderId:      n.leaderID,
		NextSequenceNumber:   n.nextSeqNum,
		TransactionsReceived: int32(len(n.log)),
	}, nil
}

func (n *Node) DebugPrintDB() {
	n.mu.RLock()
	defer n.mu.RUnlock()

	// Collect modified items (those that don't have initial balance)
	modifiedItems := make(map[int32]int32)
	initialBalance := n.config.Data.InitialBalance
	for itemID, balance := range n.balances {
		if balance != initialBalance {
			modifiedItems[itemID] = balance
		}
	}

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
	n.mu.RLock()
	defer n.mu.RUnlock()
	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d - Transaction Log                           ║\n", n.id)
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")
	for seq := int32(1); seq <= n.nextSeqNum; seq++ {
		if ent, ok := n.log[seq]; ok {
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
	n.mu.RLock()
	defer n.mu.RUnlock()
	fmt.Printf("\n╔════════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d - Status for Seq %d        ║\n", n.id, seq)
	fmt.Printf("╚════════════════════════════════════════╝\n")
	ent, ok := n.log[seq]
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
	n.mu.RLock()
	defer n.mu.RUnlock()
	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d - NEW-VIEW History                          ║\n", n.id)
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")
	if len(n.newViewLog) == 0 {
		fmt.Println("  No NEW-VIEW messages yet")
		return
	}
	for i, nv := range n.newViewLog {
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
	n.mu.RLock()
	defer n.mu.RUnlock()
	fmt.Printf("\n╔════════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d - Current Status            ║\n", n.id)
	fmt.Printf("╚════════════════════════════════════════╝\n")
	leaderStr := "No (Follower)"
	if n.isLeader {
		leaderStr = "Yes (LEADER) 👑"
	}
	fmt.Printf("  Is Leader: %s\n", leaderStr)
	fmt.Printf("  Current Leader: Node %d\n", n.leaderID)
	fmt.Printf("  Current Ballot: %s\n", n.currentBallot.String())
	fmt.Printf("  Promised Ballot: %s\n", n.promisedBallot.String())
	fmt.Printf("  Next Sequence: %d\n", n.nextSeqNum)
	fmt.Printf("  Last Executed: %d\n", n.lastExecuted)
	fmt.Printf("  Log Entries: %d\n", len(n.log))
	fmt.Printf("  NEW-VIEW Count: %d\n", len(n.newViewLog))
	fmt.Printf("  Active: %v\n", n.isActive)
	fmt.Println()
}

// SetActive allows external control to activate/deactivate a node
// This is kept simple to avoid any potential deadlocks
func (n *Node) SetActive(ctx context.Context, req *pb.SetActiveRequest) (*pb.SetActiveReply, error) {
	// DIAGNOSTIC: Log immediately when RPC is received
	log.Printf("Node %d: ⚙️  SetActive RPC RECEIVED: req.Active=%v", n.id, req.Active)

	n.mu.Lock()
	wasActive := n.isActive
	n.isActive = req.Active
	wasLeader := n.isLeader
	currentLeaderID := n.leaderID

	log.Printf("Node %d: ⚙️  SetActive PROCESSING: wasActive=%v, nowActive=%v, wasLeader=%v, leaderID=%d",
		n.id, wasActive, n.isActive, wasLeader, currentLeaderID)

	// If becoming inactive and was leader, step down
	if !req.Active && wasLeader {
		n.isLeader = false
		n.leaderID = 0 // No known leader
		log.Printf("Node %d: ⚙️  Was leader, stepping down and clearing leaderID", n.id)
	}

	currentActive := n.isActive
	n.mu.Unlock()

	if req.Active {
		// Node is now active (either newly activated or already was active)
		if wasActive {
			log.Printf("Node %d: ✅ Node already ACTIVE (leader=%d)", n.id, currentLeaderID)
		} else {
			log.Printf("Node %d: ✅ Node set to ACTIVE (was inactive)", n.id)

			// Start election if no known leader (checkForGaps will handle gap detection)
			if !wasLeader && currentLeaderID <= 0 {
				log.Printf("Node %d: No known leader - starting election", n.id)
				go n.StartLeaderElection()
			} else if !wasLeader && currentLeaderID > 0 {
				log.Printf("Node %d: Believes leader is %d", n.id, currentLeaderID)
			}
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
	n.mu.RLock()
	defer n.mu.RUnlock()

	seq := req.SequenceNumber

	// Check log
	if entry, exists := n.log[seq]; exists {
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
