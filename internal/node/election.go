package node

import (
	"context"
	"log"
	"sync"
	"time"

	"paxos-banking/internal/types"
	pb "paxos-banking/proto"
)

// StartLeaderElection initiates leader election (Paxos Phase 1)
func (n *Node) StartLeaderElection() {
	// 🔥 FIX: Don't start election if we have gaps in our own log
	// Check for gaps between lastExecuted and max sequence in log (use logMu)
	n.logMu.RLock()
	lastExec := n.lastExecuted
	maxSeq := lastExec
	for seq := range n.log {
		if seq > maxSeq {
			maxSeq = seq
		}
	}

	// Check if there are any gaps
	hasGaps := false
	for seq := lastExec + 1; seq < maxSeq; seq++ {
		if _, exists := n.log[seq]; !exists {
			hasGaps = true
			break
		}
	}
	n.logMu.RUnlock()

	if hasGaps {
		log.Printf("Node %d: Cannot start election - have gaps in log (lastExec=%d, maxSeq=%d). Waiting for current leader to fill gaps.",
			n.id, lastExec, maxSeq)
		return
	}

	// Cooldown + guard (use timerMu for lastPrepareTime)
	n.timerMu.Lock()
	if time.Since(n.lastPrepareTime) < n.prepareCooldown {
		n.timerMu.Unlock()
		return
	}
	n.lastPrepareTime = time.Now()
	n.timerMu.Unlock()

	// Compute tentative ballot (use paxosMu)
	// IMPORTANT: New ballot must be higher than BOTH currentBallot AND promisedBallot
	n.paxosMu.RLock()
	maxBallotNum := n.currentBallot.Number
	if n.promisedBallot.Number > maxBallotNum {
		maxBallotNum = n.promisedBallot.Number
	}
	localPromised := types.NewBallot(n.promisedBallot.Number, n.promisedBallot.NodeID)
	n.paxosMu.RUnlock()

	tentativeNum := maxBallotNum + 1
	tentative := types.NewBallot(tentativeNum, n.id)

	log.Printf("Node %d: 🗳️  Starting election with ballot %s", n.id, tentative.String())

	// Snapshot peers (read-only after init, no lock needed)
	peers := make(map[int32]pb.PaxosNodeClient, len(n.peerClients))
	for k, v := range n.peerClients {
		peers[k] = v
	}

	// Prepare collection: concurrent RPCs
	type promiseResult struct {
		reply *pb.PromiseReply
		err   error
	}
	promiseCh := make(chan promiseResult, len(peers)+1)
	var wg sync.WaitGroup

	// self-promise if allowed
	promiseCount := 0
	acceptLogs := make([][]*pb.AcceptedEntry, 0)
	if tentative.Compare(localPromised) > 0 {
		// Include our own accept log (getAcceptLog uses logMu internally)
		selfLog := n.getAcceptLog()
		acceptLogs = append(acceptLogs, selfLog)
		promiseCount++
	} else {
		log.Printf("Node %d: cannot self-promise; promisedBallot=%s", n.id, localPromised.String())
	}

	// send prepare to peers
	for pid, client := range peers {
		wg.Add(1)
		go func(peerID int32, cli pb.PaxosNodeClient) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			req := &pb.PrepareRequest{
				Ballot: tentative.ToProto(),
				NodeId: n.id,
			}
			resp, err := cli.Prepare(ctx, req)
			promiseCh <- promiseResult{reply: resp, err: err}
			_ = peerID
		}(pid, client)
	}

	go func() {
		wg.Wait()
		close(promiseCh)
	}()

	timeout := time.After(1800 * time.Millisecond)
collectLoop:
	for {
		select {
		case pr, ok := <-promiseCh:
			if !ok {
				break collectLoop
			}
			if pr.err != nil {
				// log error and continue
				continue
			}
			if pr.reply != nil && pr.reply.Success {
				promiseCount++
				acceptLogs = append(acceptLogs, pr.reply.AcceptLog)
				log.Printf("Node %d: ✓ PROMISE from node %d", n.id, pr.reply.NodeId)
			}
			// early exit when quorum achieved
			if n.hasQuorum(promiseCount) {
				break collectLoop
			}
		case <-timeout:
			log.Printf("Node %d: Promise collection timed out", n.id)
			break collectLoop
		}
	}

	if !n.hasQuorum(promiseCount) {
		log.Printf("Node %d: ✗ No quorum (%d/%d)", n.id, promiseCount, n.quorumSize())
		return
	}

	// Become leader and set currentBallot (use paxosMu)
	n.paxosMu.Lock()
	n.isLeader = true
	n.leaderID = n.id
	n.currentBallot = tentative
	n.paxosMu.Unlock()

	log.Printf("Node %d: 👑 Elected as LEADER with quorum %d/%d", n.id, promiseCount, n.quorumSize())

	// Build NEW-VIEW and broadcast
	n.sendNewView(acceptLogs, tentative)

	// leader actions
	n.resetLeaderTimer()
	n.startHeartbeat()
}

// Prepare handles incoming PREPARE
func (n *Node) Prepare(ctx context.Context, req *pb.PrepareRequest) (*pb.PromiseReply, error) {
	reqBallot := types.BallotFromProto(req.Ballot)
	log.Printf("Node %d: Received PREPARE %s from node %d", n.id, reqBallot.String(), req.NodeId)

	// 🎯 Mark system as initialized when receiving PREPARE (use paxosMu)
	n.paxosMu.Lock()
	if !n.systemInitialized {
		n.systemInitialized = true
		n.paxosMu.Unlock()
		log.Printf("Node %d: System initialized by PREPARE from node %d", n.id, req.NodeId)
	} else {
		n.paxosMu.Unlock()
	}

	// Check if node is active and ballot (use paxosMu)
	n.paxosMu.RLock()
	if !n.isActive {
		n.paxosMu.RUnlock()
		log.Printf("Node %d: Rejecting PREPARE - node is INACTIVE", n.id)
		return &pb.PromiseReply{Success: false, NodeId: n.id}, nil
	}

	// Quick check without lock first
	if reqBallot.Compare(n.promisedBallot) <= 0 {
		n.paxosMu.RUnlock()
		log.Printf("Node %d: Rejecting PREPARE - node is INACTIVE", n.id)
		return &pb.PromiseReply{Success: false, NodeId: n.id}, nil
	}
	wasLeader := n.isLeader
	n.paxosMu.RUnlock()

	// Copy all log entries (use logMu)
	n.logMu.RLock()
	entries := make([]*pb.AcceptedEntry, 0, len(n.log))
	for seq, entry := range n.log {
		if seq == 0 || entry == nil {
			continue
		}
		entries = append(entries, &pb.AcceptedEntry{
			Ballot:         entry.Ballot.ToProto(),
			SequenceNumber: seq,
			Request:        entry.Request,
			IsNoop:         entry.IsNoOp,
		})
	}
	n.logMu.RUnlock()

	// Update promised ballot (use paxosMu)
	n.paxosMu.Lock()
	n.promisedBallot = reqBallot
	n.isLeader = false
	// DON'T set leaderID here - wait for NEW-VIEW to confirm the election succeeded
	n.paxosMu.Unlock()

	log.Printf("Node %d: ✓ PROMISE to node %d with %d entries", n.id, req.NodeId, len(entries))

	// Do expensive operations without any locks
	if wasLeader {
		n.stopHeartbeat()
	}
	n.resetLeaderTimer()

	return &pb.PromiseReply{
		Success:   true,
		Ballot:    req.Ballot,
		AcceptLog: entries,
		NodeId:    n.id,
	}, nil
}

func (n *Node) getAcceptLog() []*pb.AcceptedEntry {
	// Use logMu for log access
	n.logMu.RLock()
	defer n.logMu.RUnlock()
	entries := make([]*pb.AcceptedEntry, 0, len(n.log))
	for seq, entry := range n.log {
		// Skip seq=0 (reserved for heartbeats only, should not be in logs)
		if seq == 0 {
			continue
		}
		entries = append(entries, &pb.AcceptedEntry{
			Ballot:         entry.Ballot.ToProto(),
			SequenceNumber: seq,
			Request:        entry.Request,
			IsNoop:         entry.IsNoOp,
		})
	}
	return entries
}

func (n *Node) sendNewView(acceptLogs [][]*pb.AcceptedEntry, ballot *types.Ballot) {
	merged := n.mergeAcceptLogs(acceptLogs)

	// ✅ FIX: Also include our own log to ensure no gaps (use logMu)
	n.logMu.RLock()
	for seq, entry := range n.log {
		if seq == 0 {
			continue
		}
		if existing, exists := merged[seq]; exists {
			// Keep the one with higher ballot
			existingBallot := types.BallotFromProto(existing.Ballot)
			entryBallot := entry.Ballot
			if entryBallot.GreaterThan(existingBallot) {
				merged[seq] = &pb.AcceptedEntry{
					Ballot:         entryBallot.ToProto(),
					SequenceNumber: seq,
					Request:        entry.Request,
					IsNoop:         entry.IsNoOp,
				}
			}
		} else {
			// Add missing entry from our log
			merged[seq] = &pb.AcceptedEntry{
				Ballot:         entry.Ballot.ToProto(),
				SequenceNumber: seq,
				Request:        entry.Request,
				IsNoop:         entry.IsNoOp,
			}
		}
	}
	n.logMu.RUnlock()

	// Determine max seq in merged
	var maxSeq int32 = 0
	for seq := range merged {
		if seq > maxSeq {
			maxSeq = seq
		}
	}

	acceptMsgs := make([]*pb.AcceptRequest, 0, maxSeq)
	startSeq := int32(1)

	// 🔥 FIX: Only include sequences we actually have - don't fill gaps with NO-OPs
	// This prevents database divergence caused by replacing real transactions with NO-OPs
	for seq := startSeq; seq <= maxSeq; seq++ {
		if entry, ok := merged[seq]; ok {
			acceptMsgs = append(acceptMsgs, &pb.AcceptRequest{
				Ballot:         ballot.ToProto(),
				SequenceNumber: seq,
				Request:        entry.Request,
				IsNoop:         entry.IsNoop,
			})
		} else {
			// 🔥 FIX: Don't fill gaps! Log a warning instead.
			// A node shouldn't become leader if it has gaps (checked in StartLeaderElection)
			log.Printf("Node %d: ⚠️  WARNING: Gap at seq %d in NEW-VIEW - this should not happen!", n.id, seq)
		}
	}

	newView := &pb.NewViewRequest{
		Ballot:         ballot.ToProto(),
		AcceptMessages: acceptMsgs,
	}

	// Store NEW-VIEW and update nextSeqNum (use logMu)
	n.logMu.Lock()
	n.newViewLog = append(n.newViewLog, newView)
	n.nextSeqNum = maxSeq + 1
	n.logMu.Unlock()

	log.Printf("Node %d: Sending NEW-VIEW with %d messages (covering seq 1-%d)", n.id, len(acceptMsgs), maxSeq)

	// Broadcast new-view (peers are read-only after init)
	peers := make(map[int32]pb.PaxosNodeClient, len(n.peerClients))
	for k, v := range n.peerClients {
		peers[k] = v
	}

	// Send NEW-VIEW to all peers
	type nvResp struct {
		nodeID int32
		ok     bool
	}
	respCh := make(chan nvResp, len(peers))

	for pid, client := range peers {
		go func(peerID int32, c pb.PaxosNodeClient) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := c.NewView(ctx, newView)
			if err != nil {
				log.Printf("Node %d: Failed to send NEW-VIEW to node %d: %v", n.id, peerID, err)
				respCh <- nvResp{nodeID: peerID, ok: false}
			} else {
				respCh <- nvResp{nodeID: peerID, ok: true}
			}
		}(pid, client)
	}

	// Wait for responses (with timeout)
	acceptedCount := 1 // Leader counts itself
	timeout := time.After(3 * time.Second)
	got := 0
	for got < len(peers) {
		select {
		case r := <-respCh:
			got++
			if r.ok {
				acceptedCount++
			}
			// Check if we have quorum
			if n.hasQuorum(acceptedCount) {
				log.Printf("Node %d: NEW-VIEW acknowledged by quorum (%d/%d)",
					n.id, acceptedCount, n.quorumSize())
				goto DONE
			}
		case <-timeout:
			log.Printf("Node %d: NEW-VIEW timeout, got %d/%d acknowledgments",
				n.id, acceptedCount, n.quorumSize())
			goto DONE
		}
	}
DONE:

	// process locally (leader applies its own accept messages)
	n.processNewView(newView)

	if !n.hasQuorum(acceptedCount) {
		log.Printf("Node %d: ⚠️  NEW-VIEW did not achieve quorum (%d/%d)",
			n.id, acceptedCount, n.quorumSize())
	}

	// CRITICAL: After NEW-VIEW, send COMMIT messages for all sequences
	// This ensures accepted-but-not-committed sequences get committed
	log.Printf("Node %d: Sending COMMIT messages for all NEW-VIEW sequences", n.id)
	for _, acceptMsg := range newView.AcceptMessages {
		commitReq := &pb.CommitRequest{
			Ballot:         ballot.ToProto(),
			SequenceNumber: acceptMsg.SequenceNumber,
			Request:        acceptMsg.Request,
			IsNoop:         acceptMsg.IsNoop,
		}

		// Send to all peers
		for pid, cli := range peers {
			go func(peerID int32, c pb.PaxosNodeClient, req *pb.CommitRequest) {
				ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
				defer cancel()
				_, _ = c.Commit(ctx, req)
			}(pid, cli, commitReq)
		}
	}
}

func (n *Node) mergeAcceptLogs(logs [][]*pb.AcceptedEntry) map[int32]*pb.AcceptedEntry {
	merged := make(map[int32]*pb.AcceptedEntry)
	for _, l := range logs {
		for _, ent := range l {
			existing, ok := merged[ent.SequenceNumber]
			if !ok {
				merged[ent.SequenceNumber] = ent
			} else {
				existingBallot := types.BallotFromProto(existing.Ballot)
				entBallot := types.BallotFromProto(ent.Ballot)
				if entBallot.GreaterThan(existingBallot) {
					merged[ent.SequenceNumber] = ent
				}
			}
		}
	}
	return merged
}

func (n *Node) NewView(ctx context.Context, req *pb.NewViewRequest) (*pb.NewViewReply, error) {
	log.Printf("Node %d: Received NEW-VIEW with %d messages", n.id, len(req.AcceptMessages))

	// 🎯 Mark system as initialized when receiving NEW-VIEW (use paxosMu)
	n.paxosMu.Lock()
	if !n.systemInitialized {
		n.systemInitialized = true
		n.paxosMu.Unlock()
		ballot := types.BallotFromProto(req.Ballot)
		log.Printf("Node %d: System initialized by NEW-VIEW from node %d", n.id, ballot.NodeID)
	} else {
		n.paxosMu.Unlock()
	}

	// Check if node is active (use paxosMu)
	n.paxosMu.RLock()
	active := n.isActive
	n.paxosMu.RUnlock()

	if !active {
		log.Printf("Node %d: Rejecting NEW-VIEW - node is INACTIVE", n.id)
		return &pb.NewViewReply{Success: false, NodeId: n.id}, nil
	}

	// Store NEW-VIEW message for PrintView (use logMu)
	n.logMu.Lock()
	// Check if we already have this exact NEW-VIEW (avoid duplicates)
	alreadyStored := false
	for _, stored := range n.newViewLog {
		if stored.Ballot.Number == req.Ballot.Number &&
			stored.Ballot.NodeId == req.Ballot.NodeId {
			alreadyStored = true
			break
		}
	}
	if !alreadyStored {
		n.newViewLog = append(n.newViewLog, req)
	}
	n.logMu.Unlock()

	// Process NEW-VIEW entries locally
	n.processNewView(req)

	return &pb.NewViewReply{Success: true, NodeId: n.id}, nil
}

func (n *Node) processNewView(req *pb.NewViewRequest) {
	ballot := types.BallotFromProto(req.Ballot)

	// Check and update Paxos state (use paxosMu and logMu)
	n.paxosMu.Lock()
	// Only accept NEW-VIEW if ballot is >= our promised ballot
	if ballot.Compare(n.promisedBallot) < 0 {
		n.paxosMu.Unlock()
		log.Printf("Node %d: Ignoring NEW-VIEW with lower ballot %s (promised: %s)",
			n.id, ballot.String(), n.promisedBallot.String())
		return
	}

	prevLeader := n.isLeader
	n.promisedBallot = ballot
	n.leaderID = ballot.NodeID
	n.isLeader = (n.id == ballot.NodeID)
	n.paxosMu.Unlock()

	// Compute max seq
	maxSeq := int32(0)
	for _, a := range req.AcceptMessages {
		if a.SequenceNumber > maxSeq {
			maxSeq = a.SequenceNumber
		}
	}

	// Find gaps (for logging and recovery) - use logMu
	n.logMu.RLock()
	missing := []int32{}
	lastExec := n.lastExecuted
	for seq := lastExec + 1; seq <= maxSeq; seq++ {
		if _, ok := n.log[seq]; !ok {
			missing = append(missing, seq)
		}
	}
	n.logMu.RUnlock()

	if len(missing) > 0 {
		log.Printf("Node %d: Detected gaps in log: %v (lastExec=%d, maxSeq=%d)", n.id, missing, lastExec, maxSeq)
	}

	// Stop heartbeat if we were leader but no longer are
	// MUST be called without holding n.mu to avoid deadlock
	if prevLeader && !n.isLeader {
		n.stopHeartbeat()
	}

	// reset follower timer to avoid immediate election
	n.resetLeaderTimer()

	// process each accept message: call Accept handler (local)
	for _, a := range req.AcceptMessages {
		// convert to pb.AcceptRequest and call Accept (local)
		n.Accept(context.Background(), a)
	}

	// After processing NEW-VIEW, commit and execute all sequences (use logMu)
	n.logMu.RLock()
	lastExecAfter := n.lastExecuted
	n.logMu.RUnlock()

	// Commit and execute all sequences from NEW-VIEW
	for seq := lastExecAfter + 1; seq <= maxSeq; seq++ {
		// First, commit the sequence if we have it in log
		n.logMu.Lock()
		entry, ok := n.log[seq]
		if ok && entry.Status != "C" && entry.Status != "E" {
			entry.Status = "C"
		}
		n.logMu.Unlock()

		// Now try to execute
		n.logMu.RLock()
		entry, exists := n.log[seq]
		n.logMu.RUnlock()

		if exists && (entry.Status == "C" || entry.Status == "E") {
			n.executeTransaction(seq, entry)
		} else {
			// 🔥 FIX: Don't fill gaps with NO-OPs! This causes database divergence.
			log.Printf("Node %d: ⚠️  Still missing seq %d after NEW-VIEW - waiting for data (NOT filling with NO-OP)", n.id, seq)
			break // Stop trying to execute further sequences
		}
	}

	n.logMu.RLock()
	finalExec := n.lastExecuted
	n.logMu.RUnlock()

	log.Printf("Node %d: NEW-VIEW processed, caught up from seq %d to %d", n.id, lastExecAfter, finalExec)
}
