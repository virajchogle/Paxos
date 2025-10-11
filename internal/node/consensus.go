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
	// lookup last reply
	lastReply, hasReply := n.clientLastReply[req.ClientId]
	lastTS, hasTS := n.clientLastTS[req.ClientId]
	n.mu.RUnlock()

	if !active {
		return nil, fmt.Errorf("node inactive")
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
			}
		}
		// no leader known: try to start election and ask client to retry
		go n.StartLeaderElection()
		return nil, fmt.Errorf("no leader available yet")
	}

	// leader: process request
	return n.processAsLeader(req)
}

// processAsLeader runs Phase 2 (send accept; collect accepted; commit; execute)
func (n *Node) processAsLeader(req *pb.TransactionRequest) (*pb.TransactionReply, error) {
	// allocate sequence
	n.mu.Lock()
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
	n.acceptLog[seq] = entry
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
				if entry2, exists := n.acceptLog[seq]; exists {
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
		log.Printf("Node %d: No quorum for seq %d (%d/%d)", n.id, seq, acceptedCount, n.quorumSize())
		// optionally cleanup acceptLog[seq] or mark failed
		return nil, fmt.Errorf("no quorum for seq %d", seq)
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

	reqBallot := types.BallotFromProto(req.Ballot)

	// Only log ACCEPT messages with seq > 0 (seq=0 is reserved for heartbeats)
	if req.SequenceNumber > 0 {
		log.Printf("Node %d: Received ACCEPT seq=%d, ballot=%s", n.id, req.SequenceNumber, reqBallot.String())
	}

	// ballot check
	if reqBallot.Compare(n.promisedBallot) < 0 {
		if !req.IsNoop && req.SequenceNumber > 0 {
			log.Printf("Node %d: Rejecting ACCEPT - ballot too low", n.id)
		}
		n.mu.Unlock()
		return &pb.AcceptedReply{Success: false, Ballot: req.Ballot, SequenceNumber: req.SequenceNumber, NodeId: n.id}, nil
	}

	// update promised ballot to this ballot
	n.promisedBallot = reqBallot
	// leader ID updated (we just heard from leader)
	n.leaderID = reqBallot.NodeID

	// create or update accept log (but never store seq=0, reserved for heartbeats)
	if req.SequenceNumber > 0 {
		ent := types.NewLogEntry(reqBallot, req.SequenceNumber, req.Request, req.IsNoop)
		ent.Status = "A"
		if existing, ok := n.acceptLog[req.SequenceNumber]; ok {
			// merge: choose entry with higher ballot if necessary
			existingBallot := existing.Ballot
			if reqBallot.GreaterThan(existingBallot) {
				n.acceptLog[req.SequenceNumber] = ent
			}
		} else {
			n.acceptLog[req.SequenceNumber] = ent
		}
	}

	// Only log ACCEPTED messages with seq > 0 (seq=0 is reserved for heartbeats)
	if req.SequenceNumber > 0 {
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
	// Only log non-heartbeat COMMIT messages (seq > 0)
	if !req.IsNoop && req.SequenceNumber > 0 {
		log.Printf("Node %d: Received COMMIT seq=%d", n.id, req.SequenceNumber)
	}
	// apply commit locally (commitAndExecute handles ordering)
	n.commitAndExecute(req.SequenceNumber)

	// Check if leader should create checkpoint
	n.mu.Lock()
	if n.isLeader {
		n.commitsSinceCP++
		if n.commitsSinceCP >= n.checkpointPeriod {
			seq := req.SequenceNumber
			n.mu.Unlock()
			go n.createCheckpoint(seq)
		} else {
			n.mu.Unlock()
		}
	} else {
		n.mu.Unlock()
	}

	return &pb.CommitReply{Success: true}, nil
}

// commitAndExecute commits a seq and executes up to that seq in order
func (n *Node) commitAndExecute(seq int32) pb.ResultType {
	n.mu.Lock()
	// ensure entry exists
	entry, ok := n.acceptLog[seq]
	if !ok {
		n.mu.Unlock()
		log.Printf("Node %d: No entry to commit for seq %d", n.id, seq)
		return pb.ResultType_FAILED
	}
	// mark committed
	entry.Status = "C"
	n.committedLog[seq] = entry
	n.mu.Unlock()

	// execute all committed entries up to seq
	var finalResult pb.ResultType = pb.ResultType_SUCCESS
	for s := n.lastExecuted + 1; s <= seq; s++ {
		n.mu.RLock()
		comm, exists := n.committedLog[s]
		n.mu.RUnlock()
		if !exists {
			// can't execute beyond a gap
			break
		}
		res := n.executeTransaction(s, comm)
		finalResult = res
	}
	return finalResult
}

func (n *Node) executeTransaction(seq int32, entry *types.LogEntry) pb.ResultType {
	n.mu.Lock()
	defer n.mu.Unlock()

	if entry.Status == "E" {
		return pb.ResultType_SUCCESS
	}

	// ensure sequential execution
	if seq != n.lastExecuted+1 {
		log.Printf("Node %d: Cannot execute seq %d, last executed %d", n.id, seq, n.lastExecuted)
		return pb.ResultType_FAILED
	}

	if entry.IsNoOp {
		log.Printf("Node %d: Executing NO-OP seq %d", n.id, seq)
		entry.Status = "E"
		n.lastExecuted = seq
		return pb.ResultType_SUCCESS
	}

	tx := entry.Request.Transaction
	sender := tx.Sender
	recv := tx.Receiver
	amt := tx.Amount

	// check balance
	if n.balances[sender] < amt {
		log.Printf("Node %d: INSUFFICIENT BALANCE for %s (has %d needs %d)", n.id, sender, n.balances[sender], amt)
		entry.Status = "E"
		n.lastExecuted = seq
		return pb.ResultType_INSUFFICIENT_BALANCE
	}

	// apply
	n.balances[sender] -= amt
	n.balances[recv] += amt
	entry.Status = "E"
	n.lastExecuted = seq

	log.Printf("Node %d: ✅ EXECUTED seq=%d: %s->%s:%d (new: %s=%d, %s=%d)",
		n.id, seq, sender, recv, amt, sender, n.balances[sender], recv, n.balances[recv])

	// Save database state to disk
	n.mu.RUnlock()
	if err := n.saveDatabase(); err != nil {
		log.Printf("Node %d: ⚠️  Warning - failed to save database: %v", n.id, err)
	}
	n.mu.RLock()

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
		TransactionsReceived: int32(len(n.acceptLog)),
	}, nil
}

func (n *Node) PrintDB() {
	n.mu.RLock()
	defer n.mu.RUnlock()
	fmt.Printf("\n╔════════════════════════════════╗\n")
	fmt.Printf("║   Node %d - Database State    ║\n", n.id)
	fmt.Printf("╚════════════════════════════════╝\n")
	for _, clientID := range n.config.Clients.ClientIDs {
		if bal, ok := n.balances[clientID]; ok {
			fmt.Printf("  %s: %d\n", clientID, bal)
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
		if ent, ok := n.acceptLog[seq]; ok {
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
	ent, ok := n.acceptLog[seq]
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

func (n *Node) PrintView() {
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
	fmt.Printf("  Accepted Entries: %d\n", len(n.acceptLog))
	fmt.Printf("  Committed Entries: %d\n", len(n.committedLog))
	fmt.Printf("  NEW-VIEW Count: %d\n", len(n.newViewLog))
	fmt.Printf("  Active: %v\n", n.isActive)
	fmt.Println()
}
