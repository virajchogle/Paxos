package node

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	pb "paxos-banking/proto"
)

// ============================================================================
// PHASE 7: UTILITY FUNCTIONS FOR DEBUGGING AND MONITORING
// ============================================================================

// PrintBalance - Query balance of a specific data item or range
func (n *Node) PrintBalance(ctx context.Context, req *pb.PrintBalanceRequest) (*pb.PrintBalanceReply, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	// Determine range
	startItem := req.StartItem
	endItem := req.EndItem

	// Get this node's shard range
	cluster := n.config.Clusters[int(n.clusterID)]
	shardStart := int32(cluster.ShardStart)
	shardEnd := int32(cluster.ShardEnd)

	// Default to full shard if not specified
	if startItem == 0 {
		startItem = shardStart
	}
	if endItem == 0 {
		endItem = shardEnd
	}

	// Clamp to shard boundaries
	if startItem < shardStart {
		startItem = shardStart
	}
	if endItem > shardEnd {
		endItem = shardEnd
	}

	var balances []*pb.BalanceEntry
	var totalBalance int64
	minBalance := int32(999999)
	maxBalance := int32(-999999)
	totalItems := int32(0)

	for itemID := startItem; itemID <= endItem; itemID++ {
		if balance, exists := n.balances[itemID]; exists {
			totalItems++
			totalBalance += int64(balance)

			if balance < minBalance {
				minBalance = balance
			}
			if balance > maxBalance {
				maxBalance = balance
			}

			if !req.SummaryOnly {
				balances = append(balances, &pb.BalanceEntry{
					DataItem: itemID,
					Balance:  balance,
				})
			}
		}
	}

	log.Printf("Node %d: 📊 PrintBalance requested for range [%d-%d]: %d items, total=%d",
		n.id, startItem, endItem, totalItems, totalBalance)

	return &pb.PrintBalanceReply{
		Success:      true,
		Balances:     balances,
		TotalItems:   totalItems,
		TotalBalance: totalBalance,
		MinBalance:   minBalance,
		MaxBalance:   maxBalance,
		NodeId:       n.id,
		ClusterId:    n.clusterID,
	}, nil
}

// PrintDB - Display full database state for this node's shard
func (n *Node) PrintDB(ctx context.Context, req *pb.PrintDBRequest) (*pb.PrintDBReply, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	cluster := n.config.Clusters[int(n.clusterID)]
	shardStart := int32(cluster.ShardStart)
	shardEnd := int32(cluster.ShardEnd)

	var balances []*pb.BalanceEntry
	totalItems := int32(0)
	limit := req.Limit

	// Get all data items in order
	var itemIDs []int32
	for itemID := range n.balances {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Slice(itemIDs, func(i, j int) bool {
		return itemIDs[i] < itemIDs[j]
	})

	for _, itemID := range itemIDs {
		balance := n.balances[itemID]

		// Skip zero balance if requested
		if !req.IncludeZeroBalance && balance == 0 {
			continue
		}

		totalItems++
		balances = append(balances, &pb.BalanceEntry{
			DataItem: itemID,
			Balance:  balance,
		})

		// Apply limit
		if limit > 0 && totalItems >= limit {
			break
		}
	}

	message := fmt.Sprintf("Node %d manages shard [%d-%d] with %d items",
		n.id, shardStart, shardEnd, len(n.balances))

	log.Printf("Node %d: 💾 PrintDB requested: returning %d items (limit=%d, includeZero=%v)",
		n.id, len(balances), limit, req.IncludeZeroBalance)

	return &pb.PrintDBReply{
		Success:    true,
		Balances:   balances,
		TotalItems: int32(len(n.balances)),
		ShardStart: shardStart,
		ShardEnd:   shardEnd,
		NodeId:     n.id,
		ClusterId:  n.clusterID,
		Message:    message,
	}, nil
}

// PrintView - Show current Paxos state/view
func (n *Node) PrintView(ctx context.Context, req *pb.PrintViewRequest) (*pb.PrintViewReply, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	// Build recent log entries if requested
	var recentLog []*pb.LogEntrySummary
	if req.IncludeLog {
		logEntries := req.LogEntries
		if logEntries == 0 {
			logEntries = 10 // Default
		}

		// Get the last N entries
		startSeq := n.nextSeqNum - logEntries
		if startSeq < 1 {
			startSeq = 1
		}

		for seq := startSeq; seq < n.nextSeqNum; seq++ {
			if entry, exists := n.log[seq]; exists {
				if entry.Request != nil && entry.Request.Transaction != nil {
					recentLog = append(recentLog, &pb.LogEntrySummary{
						Sequence: seq,
						Sender:   entry.Request.Transaction.Sender,
						Receiver: entry.Request.Transaction.Receiver,
						Amount:   entry.Request.Transaction.Amount,
						ClientId: entry.Request.ClientId,
						Executed: seq <= n.lastExecuted,
					})
				}
			}
		}
	}

	// Log all NEW-VIEW messages to console
	log.Printf("Node %d: 🔍 PrintView requested - outputting %d NEW-VIEW messages", n.id, len(n.newViewLog))

	if len(n.newViewLog) > 0 {
		log.Printf("========== NEW-VIEW MESSAGES (Node %d) ==========", n.id)
		for i, nvMsg := range n.newViewLog {
			log.Printf("NEW-VIEW #%d:", i+1)
			log.Printf("  Ballot: (%d, %d)", nvMsg.Ballot.Number, nvMsg.Ballot.NodeId)
			log.Printf("  AcceptMessages: %d entries", len(nvMsg.AcceptMessages))
			log.Printf("  CheckpointSeq: %d", nvMsg.CheckpointSeq)

			// Log details of AcceptMessages
			if len(nvMsg.AcceptMessages) > 0 {
				log.Printf("  AcceptMessage sequences:")
				for j, acceptMsg := range nvMsg.AcceptMessages {
					if j < 5 || j >= len(nvMsg.AcceptMessages)-2 { // First 5 and last 2
						log.Printf("    Seq %d: IsNoop=%v", acceptMsg.SequenceNumber, acceptMsg.IsNoop)
					} else if j == 5 {
						log.Printf("    ... (%d more entries) ...", len(nvMsg.AcceptMessages)-7)
					}
				}
			}
		}
		log.Printf("=================================================")
	} else {
		log.Printf("No NEW-VIEW messages exchanged yet (Node %d)", n.id)
	}

	message := fmt.Sprintf("Node %d in Cluster %d: %s, NextSeq=%d, LastExec=%d, NEW-VIEWs=%d",
		n.id, n.clusterID,
		map[bool]string{true: "LEADER", false: "FOLLOWER"}[n.isLeader],
		n.nextSeqNum, n.lastExecuted, len(n.newViewLog))

	return &pb.PrintViewReply{
		Success:      true,
		NodeId:       n.id,
		ClusterId:    n.clusterID,
		IsLeader:     n.isLeader,
		LeaderId:     n.leaderID,
		BallotNumber: n.currentBallot.Number,
		BallotNodeId: n.currentBallot.NodeID,
		NextSequence: n.nextSeqNum,
		LastExecuted: n.lastExecuted,
		RecentLog:    recentLog,
		IsActive:     n.isActive,
		Message:      message,
	}, nil
}

// GetPerformance - Retrieve performance metrics
func (n *Node) GetPerformance(ctx context.Context, req *pb.GetPerformanceRequest) (*pb.GetPerformanceReply, error) {
	n.perfMu.RLock()

	// Calculate averages
	avgTxnTime := float64(0)
	if n.totalTransactionCount > 0 {
		avgTxnTime = float64(n.totalTransactionTimeMs) / float64(n.totalTransactionCount)
	}

	avg2PCTime := float64(0)
	if n.total2PCCount > 0 {
		avg2PCTime = float64(n.total2PCTimeMs) / float64(n.total2PCCount)
	}

	// Calculate uptime
	uptime := time.Since(n.startTime).Seconds()

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
		AvgTransactionTimeMs:   avgTxnTime,
		Avg_2PcTimeMs:          avg2PCTime,
		ElectionsStarted:       n.electionsStarted,
		ElectionsWon:           n.electionsWon,
		ProposalsMade:          n.proposalsMade,
		ProposalsAccepted:      n.proposalsAccepted,
		LocksAcquired:          n.locksAcquired,
		LocksTimeout:           n.locksTimeout,
		UptimeSeconds:          int64(uptime),
		Message:                fmt.Sprintf("Node %d performance metrics", n.id),
	}

	n.perfMu.RUnlock()

	// Reset counters if requested
	if req.ResetCounters {
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
		n.perfMu.Unlock()
		log.Printf("Node %d: 🔄 Performance counters reset", n.id)
	}

	log.Printf("Node %d: 📈 GetPerformance requested: Total=%d, Success=%d, Failed=%d, Uptime=%.0fs",
		n.id, reply.TotalTransactions, reply.SuccessfulTransactions, reply.FailedTransactions, uptime)

	return reply, nil
}

// ============================================================================
// HELPER FUNCTIONS TO INCREMENT PERFORMANCE COUNTERS
// ============================================================================

func (n *Node) incrementTransactionCounter(success bool, durationMs int64) {
	n.perfMu.Lock()
	defer n.perfMu.Unlock()

	n.totalTransactions++
	if success {
		n.successfulTransactions++
	} else {
		n.failedTransactions++
	}

	if durationMs > 0 {
		n.totalTransactionTimeMs += durationMs
		n.totalTransactionCount++
	}
}

func (n *Node) increment2PCCoordinator(commit bool, durationMs int64) {
	n.perfMu.Lock()
	defer n.perfMu.Unlock()

	n.twoPCCoordinator++
	if commit {
		n.twoPCCommits++
	} else {
		n.twoPCAborts++
	}

	if durationMs > 0 {
		n.total2PCTimeMs += durationMs
		n.total2PCCount++
	}
}

func (n *Node) increment2PCParticipant() {
	n.perfMu.Lock()
	defer n.perfMu.Unlock()
	n.twoPCParticipant++
}

func (n *Node) incrementElectionStarted() {
	n.perfMu.Lock()
	defer n.perfMu.Unlock()
	n.electionsStarted++
}

func (n *Node) incrementElectionWon() {
	n.perfMu.Lock()
	defer n.perfMu.Unlock()
	n.electionsWon++
}

func (n *Node) incrementProposalMade() {
	n.perfMu.Lock()
	defer n.perfMu.Unlock()
	n.proposalsMade++
}

func (n *Node) incrementProposalAccepted() {
	n.perfMu.Lock()
	defer n.perfMu.Unlock()
	n.proposalsAccepted++
}

func (n *Node) incrementLockAcquired() {
	n.perfMu.Lock()
	defer n.perfMu.Unlock()
	n.locksAcquired++
}

func (n *Node) incrementLockTimeout() {
	n.perfMu.Lock()
	defer n.perfMu.Unlock()
	n.locksTimeout++
}
