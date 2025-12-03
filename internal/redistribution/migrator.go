package redistribution

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	pb "paxos-banking/proto"
)

// MigrationStatus represents the status of a migration
type MigrationStatus int

const (
	MigrationStatusPending MigrationStatus = iota
	MigrationStatusPreparing
	MigrationStatusTransferring
	MigrationStatusCommitting
	MigrationStatusComplete
	MigrationStatusFailed
	MigrationStatusRolledBack
)

func (s MigrationStatus) String() string {
	switch s {
	case MigrationStatusPending:
		return "PENDING"
	case MigrationStatusPreparing:
		return "PREPARING"
	case MigrationStatusTransferring:
		return "TRANSFERRING"
	case MigrationStatusCommitting:
		return "COMMITTING"
	case MigrationStatusComplete:
		return "COMPLETE"
	case MigrationStatusFailed:
		return "FAILED"
	case MigrationStatusRolledBack:
		return "ROLLED_BACK"
	default:
		return "UNKNOWN"
	}
}

// Migrator coordinates data migration between clusters
type Migrator struct {
	mu sync.RWMutex

	// Client connections to all cluster leaders
	clusterLeaders map[int32]pb.PaxosNodeClient

	// Current migration state
	currentMigration *MigrationState

	// Migration history
	history []*MigrationRecord

	// Configuration
	config *MigrationConfig
}

// MigrationConfig defines migration parameters
type MigrationConfig struct {
	// Maximum items per batch
	BatchSize int

	// Timeout for each phase
	PrepareTimeout  time.Duration
	TransferTimeout time.Duration
	CommitTimeout   time.Duration

	// Retry configuration
	MaxRetries int
	RetryDelay time.Duration

	// Whether to pause transactions during migration
	PauseTransactions bool

	// Verbose logging
	Verbose bool
}

// DefaultMigrationConfig returns default configuration
func DefaultMigrationConfig() *MigrationConfig {
	return &MigrationConfig{
		BatchSize:         100,
		PrepareTimeout:    30 * time.Second,
		TransferTimeout:   60 * time.Second,
		CommitTimeout:     30 * time.Second,
		MaxRetries:        3,
		RetryDelay:        1 * time.Second,
		PauseTransactions: true,
		Verbose:           true,
	}
}

// MigrationState tracks current migration progress
type MigrationState struct {
	ID        string
	Plan      *MigrationPlan
	Status    MigrationStatus
	StartTime time.Time
	EndTime   time.Time
	Error     error

	// Progress tracking
	ItemsMoved   int
	TotalItems   int
	CurrentBatch int
	TotalBatches int

	// Per-item status
	ItemStatus map[int32]MigrationStatus
}

// MigrationRecord is a historical record of a completed migration
type MigrationRecord struct {
	ID           string
	StartTime    time.Time
	EndTime      time.Time
	ItemsMoved   int
	Status       MigrationStatus
	CutReduction float64
	Error        string
}

// NewMigrator creates a new migration coordinator
func NewMigrator(config *MigrationConfig) *Migrator {
	if config == nil {
		config = DefaultMigrationConfig()
	}

	return &Migrator{
		clusterLeaders: make(map[int32]pb.PaxosNodeClient),
		history:        make([]*MigrationRecord, 0),
		config:         config,
	}
}

// SetClusterLeader sets the client connection for a cluster leader
func (m *Migrator) SetClusterLeader(clusterID int32, client pb.PaxosNodeClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clusterLeaders[clusterID] = client
}

// ExecuteMigration executes a migration plan
func (m *Migrator) ExecuteMigration(ctx context.Context, plan *MigrationPlan) (*MigrationRecord, error) {
	m.mu.Lock()
	if m.currentMigration != nil && m.currentMigration.Status == MigrationStatusTransferring {
		m.mu.Unlock()
		return nil, fmt.Errorf("migration already in progress")
	}

	// Initialize migration state
	migrationID := fmt.Sprintf("mig_%d", time.Now().UnixNano())
	m.currentMigration = &MigrationState{
		ID:           migrationID,
		Plan:         plan,
		Status:       MigrationStatusPending,
		StartTime:    time.Now(),
		TotalItems:   len(plan.Moves),
		ItemsMoved:   0,
		TotalBatches: (len(plan.Moves) + m.config.BatchSize - 1) / m.config.BatchSize,
		ItemStatus:   make(map[int32]MigrationStatus),
	}

	for _, move := range plan.Moves {
		m.currentMigration.ItemStatus[move.ItemID] = MigrationStatusPending
	}
	m.mu.Unlock()

	if m.config.Verbose {
		log.Printf("🚀 Starting migration %s: %d items in %d batches",
			migrationID, m.currentMigration.TotalItems, m.currentMigration.TotalBatches)
	}

	// Execute migration phases
	var err error

	// Phase 1: Prepare (lock items, pause transactions)
	err = m.preparePhase(ctx)
	if err != nil {
		m.rollback(ctx)
		return m.recordMigration(err), err
	}

	// Phase 2: Transfer data
	err = m.transferPhase(ctx)
	if err != nil {
		m.rollback(ctx)
		return m.recordMigration(err), err
	}

	// Phase 3: Commit (update routing, release locks)
	err = m.commitPhase(ctx)
	if err != nil {
		m.rollback(ctx)
		return m.recordMigration(err), err
	}

	return m.recordMigration(nil), nil
}

// preparePhase prepares clusters for migration
func (m *Migrator) preparePhase(ctx context.Context) error {
	m.mu.Lock()
	m.currentMigration.Status = MigrationStatusPreparing
	m.mu.Unlock()

	if m.config.Verbose {
		log.Printf("📋 Phase 1: Preparing migration...")
	}

	// Create context with timeout
	prepCtx, cancel := context.WithTimeout(ctx, m.config.PrepareTimeout)
	defer cancel()

	// Prepare each source cluster
	for clusterID, moves := range m.currentMigration.Plan.MovesBySource {
		client, exists := m.clusterLeaders[clusterID]
		if !exists {
			return fmt.Errorf("no leader for cluster %d", clusterID)
		}

		// Collect item IDs to prepare
		itemIDs := make([]int32, len(moves))
		for i, move := range moves {
			itemIDs[i] = move.ItemID
		}

		// Send prepare request
		req := &pb.MigrationPrepareRequest{
			MigrationId:    m.currentMigration.ID,
			ClusterId:      clusterID,
			ItemIds:        itemIDs,
			TargetClusters: make(map[int32]int32),
		}

		for _, move := range moves {
			req.TargetClusters[move.ItemID] = move.ToCluster
		}

		resp, err := client.MigrationPrepare(prepCtx, req)
		if err != nil {
			return fmt.Errorf("prepare failed for cluster %d: %w", clusterID, err)
		}

		if !resp.Success {
			return fmt.Errorf("prepare rejected by cluster %d: %s", clusterID, resp.Message)
		}

		if m.config.Verbose {
			log.Printf("   Cluster %d: prepared %d items", clusterID, len(itemIDs))
		}
	}

	return nil
}

// transferPhase transfers data between clusters
func (m *Migrator) transferPhase(ctx context.Context) error {
	m.mu.Lock()
	m.currentMigration.Status = MigrationStatusTransferring
	m.mu.Unlock()

	if m.config.Verbose {
		log.Printf("📦 Phase 2: Transferring data...")
	}

	// Create context with timeout
	xferCtx, cancel := context.WithTimeout(ctx, m.config.TransferTimeout)
	defer cancel()

	// Process moves in batches
	moves := m.currentMigration.Plan.Moves
	batchNum := 0

	for i := 0; i < len(moves); i += m.config.BatchSize {
		batchNum++
		end := i + m.config.BatchSize
		if end > len(moves) {
			end = len(moves)
		}

		batch := moves[i:end]

		if m.config.Verbose {
			log.Printf("   Batch %d/%d: %d items",
				batchNum, m.currentMigration.TotalBatches, len(batch))
		}

		// Group batch by source cluster
		bySource := make(map[int32][]ItemMove)
		for _, move := range batch {
			bySource[move.FromCluster] = append(bySource[move.FromCluster], move)
		}

		// Transfer from each source
		for sourceCluster, clusterMoves := range bySource {
			err := m.transferFromCluster(xferCtx, sourceCluster, clusterMoves)
			if err != nil {
				return fmt.Errorf("transfer failed from cluster %d: %w", sourceCluster, err)
			}
		}

		m.mu.Lock()
		m.currentMigration.CurrentBatch = batchNum
		m.currentMigration.ItemsMoved += len(batch)
		m.mu.Unlock()
	}

	return nil
}

// transferFromCluster transfers items from one cluster to their targets
func (m *Migrator) transferFromCluster(ctx context.Context, sourceCluster int32, moves []ItemMove) error {
	sourceClient, exists := m.clusterLeaders[sourceCluster]
	if !exists {
		return fmt.Errorf("no leader for source cluster %d", sourceCluster)
	}

	// Group by target cluster
	byTarget := make(map[int32][]ItemMove)
	for _, move := range moves {
		byTarget[move.ToCluster] = append(byTarget[move.ToCluster], move)
	}

	// For each target cluster
	for targetCluster, targetMoves := range byTarget {
		targetClient, exists := m.clusterLeaders[targetCluster]
		if !exists {
			return fmt.Errorf("no leader for target cluster %d", targetCluster)
		}

		// Get data from source
		itemIDs := make([]int32, len(targetMoves))
		for i, move := range targetMoves {
			itemIDs[i] = move.ItemID
		}

		getReq := &pb.MigrationGetDataRequest{
			MigrationId: m.currentMigration.ID,
			ItemIds:     itemIDs,
		}

		getResp, err := sourceClient.MigrationGetData(ctx, getReq)
		if err != nil {
			return fmt.Errorf("failed to get data from cluster %d: %w", sourceCluster, err)
		}

		// Send data to target
		setReq := &pb.MigrationSetDataRequest{
			MigrationId:   m.currentMigration.ID,
			SourceCluster: sourceCluster,
			Items:         getResp.Items,
		}

		setResp, err := targetClient.MigrationSetData(ctx, setReq)
		if err != nil {
			return fmt.Errorf("failed to set data on cluster %d: %w", targetCluster, err)
		}

		if !setResp.Success {
			return fmt.Errorf("data set rejected by cluster %d: %s", targetCluster, setResp.Message)
		}

		// Update item status
		m.mu.Lock()
		for _, itemID := range itemIDs {
			m.currentMigration.ItemStatus[itemID] = MigrationStatusTransferring
		}
		m.mu.Unlock()
	}

	return nil
}

// commitPhase commits the migration
func (m *Migrator) commitPhase(ctx context.Context) error {
	m.mu.Lock()
	m.currentMigration.Status = MigrationStatusCommitting
	m.mu.Unlock()

	if m.config.Verbose {
		log.Printf("✅ Phase 3: Committing migration...")
	}

	commitCtx, cancel := context.WithTimeout(ctx, m.config.CommitTimeout)
	defer cancel()

	// Commit on all involved clusters
	involvedClusters := make(map[int32]bool)
	for _, move := range m.currentMigration.Plan.Moves {
		involvedClusters[move.FromCluster] = true
		involvedClusters[move.ToCluster] = true
	}

	for clusterID := range involvedClusters {
		client, exists := m.clusterLeaders[clusterID]
		if !exists {
			return fmt.Errorf("no leader for cluster %d", clusterID)
		}

		req := &pb.MigrationCommitRequest{
			MigrationId: m.currentMigration.ID,
			ClusterId:   clusterID,
		}

		resp, err := client.MigrationCommit(commitCtx, req)
		if err != nil {
			return fmt.Errorf("commit failed for cluster %d: %w", clusterID, err)
		}

		if !resp.Success {
			return fmt.Errorf("commit rejected by cluster %d: %s", clusterID, resp.Message)
		}

		if m.config.Verbose {
			log.Printf("   Cluster %d: committed", clusterID)
		}
	}

	m.mu.Lock()
	m.currentMigration.Status = MigrationStatusComplete
	m.currentMigration.EndTime = time.Now()
	m.mu.Unlock()

	return nil
}

// rollback rolls back a failed migration
func (m *Migrator) rollback(ctx context.Context) {
	m.mu.Lock()
	if m.currentMigration == nil {
		m.mu.Unlock()
		return
	}
	migrationID := m.currentMigration.ID
	m.mu.Unlock()

	if m.config.Verbose {
		log.Printf("⚠️  Rolling back migration %s...", migrationID)
	}

	rollbackCtx, cancel := context.WithTimeout(ctx, m.config.CommitTimeout)
	defer cancel()

	// Send rollback to all clusters
	for clusterID, client := range m.clusterLeaders {
		req := &pb.MigrationRollbackRequest{
			MigrationId: migrationID,
			ClusterId:   clusterID,
		}

		_, err := client.MigrationRollback(rollbackCtx, req)
		if err != nil {
			log.Printf("   Warning: rollback failed for cluster %d: %v", clusterID, err)
		} else {
			if m.config.Verbose {
				log.Printf("   Cluster %d: rolled back", clusterID)
			}
		}
	}

	m.mu.Lock()
	m.currentMigration.Status = MigrationStatusRolledBack
	m.currentMigration.EndTime = time.Now()
	m.mu.Unlock()
}

// recordMigration records migration result and returns a record
func (m *Migrator) recordMigration(err error) *MigrationRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.currentMigration == nil {
		return nil
	}

	record := &MigrationRecord{
		ID:           m.currentMigration.ID,
		StartTime:    m.currentMigration.StartTime,
		EndTime:      time.Now(),
		ItemsMoved:   m.currentMigration.ItemsMoved,
		Status:       m.currentMigration.Status,
		CutReduction: m.currentMigration.Plan.EstimatedCutReduction,
	}

	if err != nil {
		record.Error = err.Error()
		if m.currentMigration.Status != MigrationStatusRolledBack {
			m.currentMigration.Status = MigrationStatusFailed
		}
		record.Status = m.currentMigration.Status
		m.currentMigration.Error = err
	}

	m.history = append(m.history, record)
	return record
}

// GetCurrentMigration returns the current migration state
func (m *Migrator) GetCurrentMigration() *MigrationState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.currentMigration == nil {
		return nil
	}

	// Return a copy
	state := *m.currentMigration
	state.ItemStatus = make(map[int32]MigrationStatus, len(m.currentMigration.ItemStatus))
	for k, v := range m.currentMigration.ItemStatus {
		state.ItemStatus[k] = v
	}

	return &state
}

// GetHistory returns migration history
func (m *Migrator) GetHistory() []*MigrationRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*MigrationRecord, len(m.history))
	copy(result, m.history)
	return result
}
