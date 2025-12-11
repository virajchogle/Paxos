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

type MigrationState struct {
	mu               sync.RWMutex
	activeMigrations map[string]*ActiveMigration
	pendingItems     map[int32]string
}

type ActiveMigration struct {
	ID             string
	StartTime      time.Time
	Role           string
	Items          []int32
	TargetClusters map[int32]int32
	ReceivedItems  map[int32]int32
	Status         string
}

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
		// Try to load persisted access tracker data
		if err := n.loadAccessTrackerUnlocked(); err != nil {
			log.Printf("Node %d: No persisted access tracker data (starting fresh)", n.id)
		}
	}
}

// RecordTransactionAccess records a transaction for access pattern analysis
func (n *Node) RecordTransactionAccess(sender, receiver int32, isCross bool) {
	if n.accessTracker == nil {
		return
	}
	n.accessTracker.RecordTransaction(sender, receiver, isCross)

	// Persist access tracker periodically (every 100 transactions) to avoid excessive writes
	if n.accessTracker.GetTransactionCount()%100 == 0 {
		go func() {
			if err := n.saveAccessTracker(); err != nil {
				log.Printf("Node %d: Warning - failed to save access tracker: %v", n.id, err)
			}
		}()
	}
}

func (n *Node) MigrationPrepare(ctx context.Context, req *pb.MigrationPrepareRequest) (*pb.MigrationPrepareReply, error) {
	n.initMigration()

	n.migrationState.mu.Lock()
	defer n.migrationState.mu.Unlock()

	if _, exists := n.migrationState.activeMigrations[req.MigrationId]; exists {
		return &pb.MigrationPrepareReply{
			Success:     false,
			MigrationId: req.MigrationId,
			Message:     "migration already exists",
		}, nil
	}

	for _, itemID := range req.ItemIds {
		if existingMigID, locked := n.migrationState.pendingItems[itemID]; locked {
			return &pb.MigrationPrepareReply{
				Success:     false,
				MigrationId: req.MigrationId,
				Message:     fmt.Sprintf("item %d already locked for migration %s", itemID, existingMigID),
			}, nil
		}
	}

	preparedItems := make([]int32, 0, len(req.ItemIds))
	for _, itemID := range req.ItemIds {
		n.balanceMu.RLock()
		_, exists := n.balances[itemID]
		n.balanceMu.RUnlock()
		if !exists {
			continue
		}
		n.migrationState.pendingItems[itemID] = req.MigrationId
		preparedItems = append(preparedItems, itemID)
	}

	n.migrationState.activeMigrations[req.MigrationId] = &ActiveMigration{
		ID:             req.MigrationId,
		StartTime:      time.Now(),
		Role:           "source",
		Items:          preparedItems,
		TargetClusters: req.TargetClusters,
		Status:         "prepared",
	}

	return &pb.MigrationPrepareReply{
		Success:       true,
		MigrationId:   req.MigrationId,
		PreparedItems: preparedItems,
		Message:       fmt.Sprintf("prepared %d items", len(preparedItems)),
	}, nil
}

// MigrationGetData retrieves data for items being migrated
// SIMPLIFIED: Just retrieves data without requiring prepare state
func (n *Node) MigrationGetData(ctx context.Context, req *pb.MigrationGetDataRequest) (*pb.MigrationGetDataReply, error) {
	log.Printf("Node %d: 📤 MigrationGetData - migration %s, %d items",
		n.id, req.MigrationId, len(req.ItemIds))

	// Get item data directly
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
// SIMPLIFIED: Directly adds items to balances and tracks as migrated
func (n *Node) MigrationSetData(ctx context.Context, req *pb.MigrationSetDataRequest) (*pb.MigrationSetDataReply, error) {
	log.Printf("Node %d: 📥 MigrationSetData - migration %s, %d items from cluster %d",
		n.id, req.MigrationId, len(req.Items), req.SourceCluster)

	n.initMigration()

	// Directly add items to balances and track as migrated
	n.balanceMu.Lock()
	if n.migratedInItems == nil {
		n.migratedInItems = make(map[int32]bool)
	}
	for _, item := range req.Items {
		n.balances[item.ItemId] = item.Balance
		n.migratedInItems[item.ItemId] = true
		log.Printf("Node %d: 📦 Added migrated item %d with balance %d", n.id, item.ItemId, item.Balance)
	}
	n.balanceMu.Unlock()

	// Persist to database
	if err := n.saveDatabase(); err != nil {
		log.Printf("Node %d: Warning - failed to save database: %v", n.id, err)
	}

	// Persist migrated items tracking
	if err := n.saveMigratedInItems(); err != nil {
		log.Printf("Node %d: Warning - failed to save migratedInItems: %v", n.id, err)
	}

	log.Printf("Node %d: ✅ Migrated %d items for migration %s", n.id, len(req.Items), req.MigrationId)

	return &pb.MigrationSetDataReply{
		Success:     true,
		MigrationId: req.MigrationId,
		Message:     fmt.Sprintf("migrated %d items", len(req.Items)),
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
		// Source: Remove items from local database AND track as migrated out
		n.balanceMu.Lock()
		if n.migratedOutItems == nil {
			n.migratedOutItems = make(map[int32]bool)
		}
		for _, itemID := range migration.Items {
			delete(n.balances, itemID)
			n.migratedOutItems[itemID] = true // Track as migrated out
			log.Printf("Node %d: Removed item %d (migrated out)", n.id, itemID)
		}
		n.balanceMu.Unlock()

		// Save database
		if err := n.saveDatabase(); err != nil {
			log.Printf("Node %d: Warning - failed to save database: %v", n.id, err)
		}

		// Save migrated out items tracking
		if err := n.saveMigratedOutItems(); err != nil {
			log.Printf("Node %d: Warning - failed to save migratedOutItems: %v", n.id, err)
		}

	} else if role == "target" {
		// Target: Move received items to main database AND track as migrated
		n.balanceMu.Lock()
		if n.migratedInItems == nil {
			n.migratedInItems = make(map[int32]bool)
		}
		for itemID, balance := range migration.ReceivedItems {
			n.balances[itemID] = balance
			n.migratedInItems[itemID] = true // Track as migrated in
			log.Printf("Node %d: Added item %d with balance %d (migrated in)", n.id, itemID, balance)
		}
		n.balanceMu.Unlock()

		// Save database
		if err := n.saveDatabase(); err != nil {
			log.Printf("Node %d: Warning - failed to save database: %v", n.id, err)
		}

		// Save migrated items tracking
		if err := n.saveMigratedInItems(); err != nil {
			log.Printf("Node %d: Warning - failed to save migratedInItems: %v", n.id, err)
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

	// Add vertices for all items in local shard (migration-aware)
	itemAccess := n.accessTracker.GetItemAccessMap()
	for itemID, count := range itemAccess {
		currentCluster := n.getClusterForDataItem(itemID)
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

	// Add vertices for all items (migration-aware)
	itemAccess := n.accessTracker.GetItemAccessMap()
	for itemID, count := range itemAccess {
		currentCluster := n.getClusterForDataItem(itemID)
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

// ============================================================================
// ACCESS TRACKER PERSISTENCE
// ============================================================================

const accessTrackerKey = "access_tracker_state"

// saveAccessTracker persists the access tracker state to PebbleDB
func (n *Node) saveAccessTracker() error {
	if n.accessTracker == nil || n.pebbleDB == nil {
		return nil
	}

	data, err := n.accessTracker.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize access tracker: %w", err)
	}

	// Write to PebbleDB
	key := []byte(accessTrackerKey)
	if err := n.pebbleDB.Set(key, data, nil); err != nil {
		return fmt.Errorf("failed to save access tracker to PebbleDB: %w", err)
	}

	log.Printf("Node %d: 💾 Saved access tracker state (%d bytes, %d transactions)",
		n.id, len(data), n.accessTracker.GetTransactionCount())
	return nil
}

// loadAccessTracker loads the access tracker state from PebbleDB
func (n *Node) loadAccessTracker() error {
	n.balanceMu.Lock()
	defer n.balanceMu.Unlock()
	return n.loadAccessTrackerUnlocked()
}

// loadAccessTrackerUnlocked loads access tracker without acquiring balanceMu (caller must hold lock)
func (n *Node) loadAccessTrackerUnlocked() error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	key := []byte(accessTrackerKey)
	data, closer, err := n.pebbleDB.Get(key)
	if err != nil {
		return fmt.Errorf("failed to get access tracker from PebbleDB: %w", err)
	}
	defer closer.Close()

	// Make a copy since data is only valid until closer.Close()
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	if n.accessTracker == nil {
		n.accessTracker = redistribution.NewAccessTracker(100000)
	}

	if err := n.accessTracker.Deserialize(dataCopy); err != nil {
		return fmt.Errorf("failed to deserialize access tracker: %w", err)
	}

	log.Printf("Node %d: ✅ Loaded access tracker state (%d transactions)",
		n.id, n.accessTracker.GetTransactionCount())
	return nil
}

// clearAccessTrackerFromDB removes the access tracker state from PebbleDB
func (n *Node) clearAccessTrackerFromDB() error {
	if n.pebbleDB == nil {
		return nil
	}

	key := []byte(accessTrackerKey)
	if err := n.pebbleDB.Delete(key, nil); err != nil {
		// Ignore "not found" errors
		return nil
	}

	log.Printf("Node %d: 🗑️  Cleared access tracker from PebbleDB", n.id)
	return nil
}

// ============================================================================
// MIGRATED ITEMS PERSISTENCE
// ============================================================================

const migratedInItemsKey = "migrated_in_items"

// saveMigratedInItems persists the migratedInItems map to PebbleDB
func (n *Node) saveMigratedInItems() error {
	if n.pebbleDB == nil {
		return nil
	}

	n.balanceMu.RLock()
	items := make([]int32, 0, len(n.migratedInItems))
	for itemID := range n.migratedInItems {
		items = append(items, itemID)
	}
	n.balanceMu.RUnlock()

	// Serialize as comma-separated values
	var data string
	for i, itemID := range items {
		if i > 0 {
			data += ","
		}
		data += fmt.Sprintf("%d", itemID)
	}

	key := []byte(migratedInItemsKey)
	if err := n.pebbleDB.Set(key, []byte(data), nil); err != nil {
		return fmt.Errorf("failed to save migratedInItems: %w", err)
	}

	log.Printf("Node %d: 💾 Saved %d migrated items", n.id, len(items))
	return nil
}

// loadMigratedInItems loads the migratedInItems map from PebbleDB
func (n *Node) loadMigratedInItems() error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	key := []byte(migratedInItemsKey)
	data, closer, err := n.pebbleDB.Get(key)
	if err != nil {
		return err // Not found is fine
	}
	defer closer.Close()

	n.balanceMu.Lock()
	defer n.balanceMu.Unlock()

	if n.migratedInItems == nil {
		n.migratedInItems = make(map[int32]bool)
	}

	// Parse comma-separated values
	dataStr := string(data)
	if dataStr == "" {
		return nil
	}

	for _, part := range splitCSV(dataStr) {
		var itemID int32
		if _, err := fmt.Sscanf(part, "%d", &itemID); err == nil {
			n.migratedInItems[itemID] = true
		}
	}

	log.Printf("Node %d: ✅ Loaded %d migrated items", n.id, len(n.migratedInItems))
	return nil
}

// splitCSV splits a comma-separated string
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	result := make([]string, 0)
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	return result
}

// clearMigratedInItems clears the migratedInItems from memory and PebbleDB
func (n *Node) clearMigratedInItems() error {
	n.balanceMu.Lock()
	n.migratedInItems = make(map[int32]bool)
	n.balanceMu.Unlock()

	if n.pebbleDB == nil {
		return nil
	}

	key := []byte(migratedInItemsKey)
	if err := n.pebbleDB.Delete(key, nil); err != nil {
		// Ignore "not found" errors
		return nil
	}

	log.Printf("Node %d: 🗑️  Cleared migrated-in items", n.id)
	return nil
}

// ============================================================================
// MIGRATED OUT ITEMS PERSISTENCE
// ============================================================================

const migratedOutItemsKey = "migrated_out_items"

// saveMigratedOutItems persists the migratedOutItems map to PebbleDB
func (n *Node) saveMigratedOutItems() error {
	if n.pebbleDB == nil {
		return nil
	}

	n.balanceMu.RLock()
	items := make([]int32, 0, len(n.migratedOutItems))
	for itemID := range n.migratedOutItems {
		items = append(items, itemID)
	}
	n.balanceMu.RUnlock()

	// Serialize as comma-separated values
	var data string
	for i, itemID := range items {
		if i > 0 {
			data += ","
		}
		data += fmt.Sprintf("%d", itemID)
	}

	key := []byte(migratedOutItemsKey)
	if err := n.pebbleDB.Set(key, []byte(data), nil); err != nil {
		return fmt.Errorf("failed to save migratedOutItems: %w", err)
	}

	log.Printf("Node %d: 💾 Saved %d migrated-out items", n.id, len(items))
	return nil
}

// loadMigratedOutItems loads the migratedOutItems map from PebbleDB
func (n *Node) loadMigratedOutItems() error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	key := []byte(migratedOutItemsKey)
	data, closer, err := n.pebbleDB.Get(key)
	if err != nil {
		return err // Not found is fine
	}
	defer closer.Close()

	n.balanceMu.Lock()
	defer n.balanceMu.Unlock()

	if n.migratedOutItems == nil {
		n.migratedOutItems = make(map[int32]bool)
	}

	// Parse comma-separated values
	dataStr := string(data)
	if dataStr == "" {
		return nil
	}

	for _, part := range splitCSV(dataStr) {
		var itemID int32
		if _, err := fmt.Sscanf(part, "%d", &itemID); err == nil {
			n.migratedOutItems[itemID] = true
		}
	}

	log.Printf("Node %d: ✅ Loaded %d migrated-out items", n.id, len(n.migratedOutItems))
	return nil
}

// clearMigratedOutItems clears the migratedOutItems from memory and PebbleDB
func (n *Node) clearMigratedOutItems() error {
	n.balanceMu.Lock()
	n.migratedOutItems = make(map[int32]bool)
	n.balanceMu.Unlock()

	if n.pebbleDB == nil {
		return nil
	}

	key := []byte(migratedOutItemsKey)
	if err := n.pebbleDB.Delete(key, nil); err != nil {
		// Ignore "not found" errors
		return nil
	}

	log.Printf("Node %d: 🗑️  Cleared migrated-out items", n.id)
	return nil
}
