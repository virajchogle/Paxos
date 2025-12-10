package node

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"paxos-banking/internal/redistribution"
	pb "paxos-banking/proto"
)

// ============================================================================
// PHASE 9: SHARD REDISTRIBUTION / DATA MIGRATION
// ============================================================================

// MigrationState tracks migration state for a node
type MigrationState struct {
	mu sync.RWMutex

	// Active migrations
	activeMigrations map[string]*ActiveMigration

	// Pending items (locked for migration)
	pendingItems map[int32]string // itemID -> migrationID
}

// ActiveMigration represents an active migration for this node
type ActiveMigration struct {
	ID             string
	StartTime      time.Time
	Role           string // "source" or "target"
	Items          []int32
	TargetClusters map[int32]int32 // itemID -> targetCluster
	ReceivedItems  map[int32]int32 // itemID -> balance (for target)
	Status         string
}

// initMigration initializes migration state on a node
func (n *Node) initMigration() {
	n.balanceMu.Lock()
	defer n.balanceMu.Unlock()

	if n.migrationState == nil {
		n.migrationState = &MigrationState{
			activeMigrations: make(map[string]*ActiveMigration),
			pendingItems:     make(map[int32]string),
		}
	}

	if n.accessTracker == nil {
		n.accessTracker = redistribution.NewAccessTracker(100000)
	}
}

// RecordTransactionAccess records a transaction for access pattern analysis
func (n *Node) RecordTransactionAccess(sender, receiver int32, isCross bool) {
	if n.accessTracker == nil {
		return
	}
	n.accessTracker.RecordTransaction(sender, receiver, isCross)
}

// MigrationPrepare prepares items for migration (locks them)
func (n *Node) MigrationPrepare(ctx context.Context, req *pb.MigrationPrepareRequest) (*pb.MigrationPrepareReply, error) {
	log.Printf("Node %d: 📦 MigrationPrepare - migration %s, %d items",
		n.id, req.MigrationId, len(req.ItemIds))

	n.initMigration()

	n.migrationState.mu.Lock()
	defer n.migrationState.mu.Unlock()

	// Check if migration already exists
	if _, exists := n.migrationState.activeMigrations[req.MigrationId]; exists {
		return &pb.MigrationPrepareReply{
			Success:     false,
			MigrationId: req.MigrationId,
			Message:     "migration already exists",
		}, nil
	}

	// Check if any items are already locked for migration
	for _, itemID := range req.ItemIds {
		if existingMigID, locked := n.migrationState.pendingItems[itemID]; locked {
			return &pb.MigrationPrepareReply{
				Success:     false,
				MigrationId: req.MigrationId,
				Message:     fmt.Sprintf("item %d already locked for migration %s", itemID, existingMigID),
			}, nil
		}
	}

	// Lock items
	preparedItems := make([]int32, 0, len(req.ItemIds))
	for _, itemID := range req.ItemIds {
		// Verify we own this item
		n.balanceMu.RLock()
		_, exists := n.balances[itemID]
		n.balanceMu.RUnlock()

		if !exists {
			log.Printf("Node %d: Warning - item %d not found in local database", n.id, itemID)
			continue
		}

		n.migrationState.pendingItems[itemID] = req.MigrationId
		preparedItems = append(preparedItems, itemID)
	}

	// Create migration record
	n.migrationState.activeMigrations[req.MigrationId] = &ActiveMigration{
		ID:             req.MigrationId,
		StartTime:      time.Now(),
		Role:           "source",
		Items:          preparedItems,
		TargetClusters: req.TargetClusters,
		Status:         "prepared",
	}

	log.Printf("Node %d: ✅ Prepared %d items for migration %s", n.id, len(preparedItems), req.MigrationId)

	return &pb.MigrationPrepareReply{
		Success:       true,
		MigrationId:   req.MigrationId,
		PreparedItems: preparedItems,
		Message:       fmt.Sprintf("prepared %d items", len(preparedItems)),
	}, nil
}

// MigrationGetData retrieves data for items being migrated
func (n *Node) MigrationGetData(ctx context.Context, req *pb.MigrationGetDataRequest) (*pb.MigrationGetDataReply, error) {
	log.Printf("Node %d: 📤 MigrationGetData - migration %s, %d items",
		n.id, req.MigrationId, len(req.ItemIds))

	n.initMigration()

	// Verify migration exists
	n.migrationState.mu.RLock()
	migration, exists := n.migrationState.activeMigrations[req.MigrationId]
	n.migrationState.mu.RUnlock()

	if !exists {
		return &pb.MigrationGetDataReply{
			Success:     false,
			MigrationId: req.MigrationId,
			Message:     "migration not found",
		}, nil
	}

	if migration.Status != "prepared" {
		return &pb.MigrationGetDataReply{
			Success:     false,
			MigrationId: req.MigrationId,
			Message:     fmt.Sprintf("migration not in prepared state (current: %s)", migration.Status),
		}, nil
	}

	// Get item data
	items := make([]*pb.MigrationDataItem, 0, len(req.ItemIds))
	n.balanceMu.RLock()
	for _, itemID := range req.ItemIds {
		if balance, exists := n.balances[itemID]; exists {
			items = append(items, &pb.MigrationDataItem{
				ItemId:  itemID,
				Balance: balance,
			})
		}
	}
	n.balanceMu.RUnlock()

	log.Printf("Node %d: Retrieved %d items for migration %s", n.id, len(items), req.MigrationId)

	return &pb.MigrationGetDataReply{
		Success:     true,
		MigrationId: req.MigrationId,
		Items:       items,
		Message:     fmt.Sprintf("retrieved %d items", len(items)),
	}, nil
}

// MigrationSetData stores data items from a migration
func (n *Node) MigrationSetData(ctx context.Context, req *pb.MigrationSetDataRequest) (*pb.MigrationSetDataReply, error) {
	log.Printf("Node %d: 📥 MigrationSetData - migration %s, %d items from cluster %d",
		n.id, req.MigrationId, len(req.Items), req.SourceCluster)

	n.initMigration()

	n.migrationState.mu.Lock()

	// Create or get migration record
	migration, exists := n.migrationState.activeMigrations[req.MigrationId]
	if !exists {
		migration = &ActiveMigration{
			ID:            req.MigrationId,
			StartTime:     time.Now(),
			Role:          "target",
			Items:         make([]int32, 0),
			ReceivedItems: make(map[int32]int32),
			Status:        "receiving",
		}
		n.migrationState.activeMigrations[req.MigrationId] = migration
	}

	// Store items temporarily (not in main DB yet)
	for _, item := range req.Items {
		migration.ReceivedItems[item.ItemId] = item.Balance
		migration.Items = append(migration.Items, item.ItemId)
	}

	n.migrationState.mu.Unlock()

	log.Printf("Node %d: ✅ Received %d items for migration %s", n.id, len(req.Items), req.MigrationId)

	return &pb.MigrationSetDataReply{
		Success:     true,
		MigrationId: req.MigrationId,
		Message:     fmt.Sprintf("received %d items", len(req.Items)),
	}, nil
}

// MigrationCommit finalizes a migration
func (n *Node) MigrationCommit(ctx context.Context, req *pb.MigrationCommitRequest) (*pb.MigrationCommitReply, error) {
	log.Printf("Node %d: ✅ MigrationCommit - migration %s", n.id, req.MigrationId)

	n.initMigration()

	n.migrationState.mu.Lock()
	migration, exists := n.migrationState.activeMigrations[req.MigrationId]
	if !exists {
		n.migrationState.mu.Unlock()
		return &pb.MigrationCommitReply{
			Success:     false,
			MigrationId: req.MigrationId,
			Message:     "migration not found",
		}, nil
	}

	role := migration.Role
	n.migrationState.mu.Unlock()

	if role == "source" {
		// Source: Remove items from local database
		n.balanceMu.Lock()
		for _, itemID := range migration.Items {
			delete(n.balances, itemID)
			log.Printf("Node %d: Removed item %d (migrated out)", n.id, itemID)
		}
		n.balanceMu.Unlock()

		// Save database
		if err := n.saveDatabase(); err != nil {
			log.Printf("Node %d: Warning - failed to save database: %v", n.id, err)
		}

	} else if role == "target" {
		// Target: Move received items to main database
		n.balanceMu.Lock()
		for itemID, balance := range migration.ReceivedItems {
			n.balances[itemID] = balance
			log.Printf("Node %d: Added item %d with balance %d (migrated in)", n.id, itemID, balance)
		}
		n.balanceMu.Unlock()

		// Save database
		if err := n.saveDatabase(); err != nil {
			log.Printf("Node %d: Warning - failed to save database: %v", n.id, err)
		}
	}

	// Cleanup
	n.migrationState.mu.Lock()
	for _, itemID := range migration.Items {
		delete(n.migrationState.pendingItems, itemID)
	}
	delete(n.migrationState.activeMigrations, req.MigrationId)
	n.migrationState.mu.Unlock()

	log.Printf("Node %d: Migration %s committed (%s role)", n.id, req.MigrationId, role)

	return &pb.MigrationCommitReply{
		Success:     true,
		MigrationId: req.MigrationId,
		Message:     fmt.Sprintf("migration committed (%s)", role),
	}, nil
}

// MigrationRollback rolls back a migration
func (n *Node) MigrationRollback(ctx context.Context, req *pb.MigrationRollbackRequest) (*pb.MigrationRollbackReply, error) {
	log.Printf("Node %d: ⚠️  MigrationRollback - migration %s", n.id, req.MigrationId)

	n.initMigration()

	n.migrationState.mu.Lock()
	defer n.migrationState.mu.Unlock()

	migration, exists := n.migrationState.activeMigrations[req.MigrationId]
	if !exists {
		return &pb.MigrationRollbackReply{
			Success:     true,
			MigrationId: req.MigrationId,
			Message:     "migration not found (already cleaned up?)",
		}, nil
	}

	// Release locks on items
	for _, itemID := range migration.Items {
		delete(n.migrationState.pendingItems, itemID)
	}

	// If we're the target and received items, discard them (they weren't committed)
	if migration.Role == "target" {
		migration.ReceivedItems = make(map[int32]int32)
	}

	delete(n.migrationState.activeMigrations, req.MigrationId)

	log.Printf("Node %d: Migration %s rolled back", n.id, req.MigrationId)

	return &pb.MigrationRollbackReply{
		Success:     true,
		MigrationId: req.MigrationId,
		Message:     "migration rolled back",
	}, nil
}

// GetAccessStats returns access pattern statistics
func (n *Node) GetAccessStats(ctx context.Context, req *pb.GetAccessStatsRequest) (*pb.GetAccessStatsReply, error) {
	log.Printf("Node %d: 📊 GetAccessStats", n.id)

	n.initMigration()

	if n.accessTracker == nil {
		return &pb.GetAccessStatsReply{
			Success: false,
			Message: "access tracker not initialized",
		}, nil
	}

	stats := n.accessTracker.GetStats()
	topPairs := n.accessTracker.GetTopCoAccessPairs(20)

	// Convert to protobuf
	coAccessPairs := make([]*pb.CoAccessPair, len(topPairs))
	for i, pair := range topPairs {
		coAccessPairs[i] = &pb.CoAccessPair{
			Item1: pair.Pair.First,
			Item2: pair.Pair.Second,
			Count: pair.Count,
		}
	}

	if req.GetReset_() {
		n.accessTracker.Reset()
		log.Printf("Node %d: Access stats reset", n.id)
	}

	return &pb.GetAccessStatsReply{
		Success:                true,
		TotalTransactions:      stats.TotalTransactions,
		CrossShardTransactions: stats.CrossShardTransactions,
		CrossShardRatio:        n.accessTracker.GetCrossShardRatio(),
		UniqueItemsAccessed:    stats.UniqueItemsAccessed,
		TopCoAccess:            coAccessPairs,
		Message:                fmt.Sprintf("Stats since %v", stats.LastReset.Format(time.RFC3339)),
	}, nil
}

// TriggerRebalance initiates shard rebalancing
func (n *Node) TriggerRebalance(ctx context.Context, req *pb.TriggerRebalanceRequest) (*pb.TriggerRebalanceReply, error) {
	log.Printf("Node %d: 🔄 TriggerRebalance (dry_run=%v)", n.id, req.DryRun)

	n.initMigration()

	// Only leader can trigger rebalance
	n.balanceMu.RLock()
	isLeader := n.isLeader
	n.balanceMu.RUnlock()

	if !isLeader {
		return &pb.TriggerRebalanceReply{
			Success: false,
			Message: "only leader can trigger rebalance",
		}, nil
	}

	if n.accessTracker == nil {
		return &pb.TriggerRebalanceReply{
			Success: false,
			Message: "access tracker not initialized",
		}, nil
	}

	// Build SIMPLE GRAPH from access patterns
	numClusters := int32(len(n.config.Clusters))

	graph := redistribution.NewSimpleGraph(numClusters, 0)

	// Add vertices for all items in local shard
	itemAccess := n.accessTracker.GetItemAccessMap()
	for itemID, count := range itemAccess {
		currentCluster := int32(n.config.GetClusterForDataItem(itemID))
		graph.AddVertex(itemID, count, currentCluster)
	}

	// Add edges from co-access patterns (simple graph: each edge is between 2 items)
	coAccess := n.accessTracker.GetCoAccessMatrix()
	for pair, count := range coAccess {
		graph.AddEdge(pair.First, pair.Second, count)
	}

	// Configure simple partitioner
	config := redistribution.DefaultSimplePartitionConfig()
	if req.MinGain > 0 {
		config.MinGain = int64(req.MinGain)
	}
	if req.MaxImbalance > 0 {
		config.MaxImbalance = float64(req.MaxImbalance)
	}
	config.Verbose = true

	// Run SIMPLE GRAPH partitioning
	partitioner := redistribution.NewSimplePartitioner(graph, config)
	result, err := partitioner.Partition()
	if err != nil {
		return &pb.TriggerRebalanceReply{
			Success: false,
			Message: fmt.Sprintf("partitioning failed: %v", err),
		}, nil
	}

	// Convert moves to protobuf
	moves := make([]*pb.MigrationMove, len(result.Moves))
	for i, move := range result.Moves {
		moves[i] = &pb.MigrationMove{
			ItemId:        move.ItemID,
			FromCluster:   move.FromCluster,
			ToCluster:     move.ToCluster,
			EstimatedGain: move.Gain,
		}
	}

	reply := &pb.TriggerRebalanceReply{
		Success:      true,
		InitialCut:   result.InitialCut,
		FinalCut:     result.FinalCut,
		CutReduction: result.CutReduction,
		ItemsToMove:  int32(len(result.Moves)),
		Moves:        moves,
	}

	if req.DryRun {
		reply.Message = fmt.Sprintf("Dry run: %d items would be moved, cut reduction: %.2f%%",
			len(result.Moves), result.CutReduction*100)
		log.Printf("Node %d: Rebalance dry run complete: %s", n.id, reply.Message)
	} else {
		// TODO: Execute actual migration
		// For now, just return the plan
		reply.MigrationId = fmt.Sprintf("rebal_%d", time.Now().UnixNano())
		reply.Message = fmt.Sprintf("Rebalance planned: %d items to move, migration_id=%s",
			len(result.Moves), reply.MigrationId)
		log.Printf("Node %d: Rebalance planned: %s", n.id, reply.Message)
	}

	return reply, nil
}

// PrintReshard triggers resharding using SIMPLE GRAPH PARTITIONING
// and outputs triplets (item_id, from_cluster, to_cluster)
func (n *Node) PrintReshard(ctx context.Context, req *pb.PrintReshardRequest) (*pb.PrintReshardReply, error) {
	log.Printf("Node %d: 📝 PrintReshard requested (execute=%v)", n.id, req.Execute)

	n.initMigration()

	// Only leader can trigger reshard
	n.balanceMu.RLock()
	isLeader := n.isLeader
	n.balanceMu.RUnlock()

	if !isLeader {
		return &pb.PrintReshardReply{
			Success: false,
			Message: "only leader can trigger resharding",
		}, nil
	}

	if n.accessTracker == nil {
		return &pb.PrintReshardReply{
			Success: false,
			Message: "access tracker not initialized",
		}, nil
	}

	// Build SIMPLE GRAPH from access patterns
	numClusters := int32(len(n.config.Clusters))
	totalItems := int32(n.config.Data.TotalItems)

	graph := redistribution.NewSimpleGraph(numClusters, totalItems)

	// Add vertices for all items
	itemAccess := n.accessTracker.GetItemAccessMap()
	for itemID, count := range itemAccess {
		currentCluster := int32(n.config.GetClusterForDataItem(itemID))
		graph.AddVertex(itemID, count, currentCluster)
	}

	// Add edges from co-access patterns (simple graph: each edge is between 2 items)
	coAccess := n.accessTracker.GetCoAccessMatrix()
	for pair, count := range coAccess {
		graph.AddEdge(pair.First, pair.Second, count)
	}

	// Configure simple partitioner
	config := redistribution.DefaultSimplePartitionConfig()
	if req.MinGain > 0 {
		config.MinGain = int64(req.MinGain)
	}
	config.Verbose = true

	// Run SIMPLE GRAPH partitioning
	partitioner := redistribution.NewSimplePartitioner(graph, config)
	result, err := partitioner.Partition()
	if err != nil {
		return &pb.PrintReshardReply{
			Success: false,
			Message: fmt.Sprintf("partitioning failed: %v", err),
		}, nil
	}

	// Convert moves to triplets
	triplets := make([]*pb.ReshardTriplet, len(result.Moves))
	for i, move := range result.Moves {
		triplets[i] = &pb.ReshardTriplet{
			ItemId:      move.ItemID,
			FromCluster: move.FromCluster,
			ToCluster:   move.ToCluster,
		}
	}

	// Output triplets to log
	log.Printf("========== RESHARD RESULTS (Node %d) ==========", n.id)
	log.Printf("Using: SIMPLE GRAPH PARTITIONING")
	log.Printf("Initial edge cut: %d (cross-shard transactions)", result.InitialCut)
	log.Printf("Final edge cut: %d", result.FinalCut)
	log.Printf("Cut reduction: %.2f%%", result.CutReduction*100)
	log.Printf("Items to move: %d", len(triplets))
	log.Printf("")
	log.Printf("Migration triplets (item_id, from_cluster, to_cluster):")
	for _, t := range triplets {
		log.Printf("  (%d, c%d, c%d)", t.ItemId, t.FromCluster, t.ToCluster)
	}
	log.Printf("===============================================")

	message := fmt.Sprintf("Resharding complete: %d items to move, %.2f%% cross-shard reduction",
		len(triplets), result.CutReduction*100)

	if req.Execute {
		// TODO: Execute actual migration
		message += " (execution not yet implemented)"
	}

	return &pb.PrintReshardReply{
		Success:  true,
		Triplets: triplets,
		Message:  message,
	}, nil
}
