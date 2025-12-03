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
	// Check for gaps between lastExecuted and max sequence in log
	n.mu.RLock()
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
	n.mu.RUnlock()

	if hasGaps {
		log.Printf("Node %d: Cannot start election - have gaps in log (lastExec=%d, maxSeq=%d). Waiting for current leader to fill gaps.",
			n.id, lastExec, maxSeq)
		return
	}

	// cooldown + guard
	n.mu.Lock()
	if time.Since(n.lastPrepareTime) < n.prepareCooldown {
		n.mu.Unlock()
		return
	}
	// compute tentative ballot locally (don't mutate shared ballot yet)
	// IMPORTANT: New ballot must be higher than BOTH currentBallot AND promisedBallot
	// Otherwise, we won't be able to self-promise if we've promised to a higher ballot
	maxBallotNum := n.currentBallot.Number
	if n.promisedBallot.Number > maxBallotNum {
		maxBallotNum = n.promisedBallot.Number
	}
	tentativeNum := maxBallotNum + 1
	tentative := types.NewBallot(tentativeNum, n.id)
	n.lastPrepareTime = time.Now()
	n.mu.Unlock()

	log.Printf("Node %d: 🗳️  Starting election with ballot %s", n.id, tentative.String())

	// Snapshot peers and local promised ballot under read lock
	n.mu.RLock()
	peers := make(map[int32]pb.PaxosNodeClient, len(n.peerClients))
	for k, v := range n.peerClients {
		peers[k] = v
	}
	localPromised := types.NewBallot(n.promisedBallot.Number, n.promisedBallot.NodeID)
	n.mu.RUnlock()

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
		// include our own accept log
		n.mu.RLock()
		selfLog := n.getAcceptLog()
		n.mu.RUnlock()
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

	// become leader and set currentBallot
	n.mu.Lock()
	n.isLeader = true
	n.leaderID = n.id
	n.currentBallot = tentative
	n.mu.Unlock()

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

	// 🎯 Mark system as initialized when receiving PREPARE
	n.mu.Lock()
	if !n.systemInitialized {
		n.systemInitialized = true
		n.mu.Unlock()
		log.Printf("Node %d: System initialized by PREPARE from node %d", n.id, req.NodeId)
	} else {
		n.mu.Unlock()
	}

	// Check if node is active
	n.mu.RLock()
	if !n.isActive {
		n.mu.RUnlock()
		log.Printf("Node %d: Rejecting PREPARE - node is INACTIVE", n.id)
		return &pb.PromiseReply{Success: false, NodeId: n.id}, nil
	}

	// Quick check without lock first
	if reqBallot.Compare(n.promisedBallot) <= 0 {
		n.mu.RUnlock()
		log.Printf("Node %d: Rejecting PREPARE - already promised higher", n.id)
		return &pb.PromiseReply{Success: false, NodeId: n.id}, nil
	}

	// Copy all log entries
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

	wasLeader := n.isLeader
	n.mu.RUnlock()

	// Now acquire write lock briefly
	n.mu.Lock()
	n.promisedBallot = reqBallot
	n.isLeader = false
	// DON'T set leaderID here - wait for NEW-VIEW to confirm the election succeeded
	// Setting it here causes nodes to think someone is leader even if election fails
	n.mu.Unlock()

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
	n.mu.RLock()
	defer n.mu.RUnlock()
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

	// ✅ FIX: Also include our own log to ensure no gaps
	n.mu.RLock()
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
	n.mu.RUnlock()

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

	n.mu.Lock()
	n.newViewLog = append(n.newViewLog, newView)
	n.nextSeqNum = maxSeq + 1
	n.mu.Unlock()

	log.Printf("Node %d: Sending NEW-VIEW with %d messages (covering seq 1-%d)", n.id, len(acceptMsgs), maxSeq)

	// broadcast new-view and wait for acknowledgments
	n.mu.RLock()
	peers := make(map[int32]pb.PaxosNodeClient, len(n.peerClients))
	for k, v := range n.peerClients {
		peers[k] = v
	}
	n.mu.RUnlock()

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

	// 🎯 Mark system as initialized when receiving NEW-VIEW
	n.mu.Lock()
	if !n.systemInitialized {
		n.systemInitialized = true
		n.mu.Unlock()
		ballot := types.BallotFromProto(req.Ballot)
		log.Printf("Node %d: System initialized by NEW-VIEW from node %d", n.id, ballot.NodeID)
	} else {
		n.mu.Unlock()
	}

	// Check if node is active
	n.mu.RLock()
	active := n.isActive
	n.mu.RUnlock()

	if !active {
		log.Printf("Node %d: Rejecting NEW-VIEW - node is INACTIVE", n.id)
		return &pb.NewViewReply{Success: false, NodeId: n.id}, nil
	}

	// Store NEW-VIEW message for PrintView (before processing)
	n.mu.Lock()
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
	n.mu.Unlock()

	// Process NEW-VIEW entries locally
	n.processNewView(req)

	return &pb.NewViewReply{Success: true, NodeId: n.id}, nil
}

func (n *Node) processNewView(req *pb.NewViewRequest) {
	ballot := types.BallotFromProto(req.Ballot)

	// Acquire lock briefly to update state
	n.mu.Lock()

	// Only accept NEW-VIEW if ballot is >= our promised ballot
	// This prevents accepting stale NEW-VIEW messages from lower ballots
	if ballot.Compare(n.promisedBallot) < 0 {
		n.mu.Unlock()
		log.Printf("Node %d: Ignoring NEW-VIEW with lower ballot %s (promised: %s)",
			n.id, ballot.String(), n.promisedBallot.String())
		return
	}

	prevLeader := n.isLeader
	n.promisedBallot = ballot // Update our promised ballot
	n.leaderID = ballot.NodeID
	n.isLeader = (n.id == ballot.NodeID)

	// compute max seq
	maxSeq := int32(0)
	for _, a := range req.AcceptMessages {
		if a.SequenceNumber > maxSeq {
			maxSeq = a.SequenceNumber
		}
	}

	// find gaps (for logging and recovery)
	missing := []int32{}
	lastExec := n.lastExecuted
	for seq := lastExec + 1; seq <= maxSeq; seq++ {
		if _, ok := n.log[seq]; !ok {
			missing = append(missing, seq)
		}
	}

	if len(missing) > 0 {
		log.Printf("Node %d: Detected gaps in log: %v (lastExec=%d, maxSeq=%d)", n.id, missing, lastExec, maxSeq)
	}

	n.mu.Unlock()

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

	// After processing NEW-VIEW, commit and execute all sequences
	// All sequences in NEW-VIEW are considered committed since they were part of the elected view
	n.mu.RLock()
	lastExecAfter := n.lastExecuted
	n.mu.RUnlock()

	// Commit and execute all sequences from NEW-VIEW
	for seq := lastExecAfter + 1; seq <= maxSeq; seq++ {
		// First, commit the sequence if we have it in log
		n.mu.Lock()
		if entry, ok := n.log[seq]; ok {
			if entry.Status != "C" && entry.Status != "E" {
				entry.Status = "C"
			}
		}
		n.mu.Unlock()

		// Now try to execute
		n.mu.RLock()
		entry, exists := n.log[seq]
		n.mu.RUnlock()
		if exists && (entry.Status == "C" || entry.Status == "E") {
			n.executeTransaction(seq, entry)
		} else {
			// 🔥 FIX: Don't fill gaps with NO-OPs! This causes database divergence.
			// Missing sequences should be received from the leader or other nodes.
			// We'll wait for them to arrive naturally.
			log.Printf("Node %d: ⚠️  Still missing seq %d after NEW-VIEW - waiting for data (NOT filling with NO-OP)", n.id, seq)
			// Don't execute - this will leave a gap that must be filled by actual data
			break // Stop trying to execute further sequences
		}
	}

	n.mu.RLock()
	finalExec := n.lastExecuted
	n.mu.RUnlock()

	log.Printf("Node %d: NEW-VIEW processed, caught up from seq %d to %d", n.id, lastExecAfter, finalExec)
}
