package redistribution

import (
	"fmt"
	"log"
	"sort"
)

// Partitioner implements hypergraph partitioning algorithms
type Partitioner struct {
	hypergraph *Hypergraph
	config     *PartitionConfig
}

// PartitionConfig defines partitioning parameters
type PartitionConfig struct {
	// Maximum imbalance ratio (e.g., 0.1 = 10% imbalance allowed)
	MaxImbalance float64

	// Minimum gain for a move to be considered
	MinGain int64

	// Maximum iterations for iterative improvement
	MaxIterations int

	// Early stop if improvement below this threshold
	ImprovementThreshold float64

	// Verbose logging
	Verbose bool
}

// DefaultPartitionConfig returns default configuration
func DefaultPartitionConfig() *PartitionConfig {
	return &PartitionConfig{
		MaxImbalance:         0.2,  // 20% imbalance allowed
		MinGain:              1,    // Any positive gain
		MaxIterations:        100,  // Max iterations
		ImprovementThreshold: 0.01, // 1% improvement threshold
		Verbose:              true,
	}
}

// NewPartitioner creates a new partitioner
func NewPartitioner(hg *Hypergraph, config *PartitionConfig) *Partitioner {
	if config == nil {
		config = DefaultPartitionConfig()
	}
	return &Partitioner{
		hypergraph: hg,
		config:     config,
	}
}

// PartitionResult represents the result of partitioning
type PartitionResult struct {
	// New partition assignments (item -> cluster)
	Assignment map[int32]int32

	// Items that need to be moved
	Moves []ItemMove

	// Statistics
	InitialCut   int64
	FinalCut     int64
	CutReduction float64
	Iterations   int
}

// ItemMove represents a data item that needs to be moved
type ItemMove struct {
	ItemID      int32
	FromCluster int32
	ToCluster   int32
	Gain        int64
}

// Partition runs the partitioning algorithm
func (p *Partitioner) Partition() (*PartitionResult, error) {
	if p.hypergraph == nil {
		return nil, fmt.Errorf("hypergraph is nil")
	}

	result := &PartitionResult{
		Assignment: make(map[int32]int32),
		Moves:      make([]ItemMove, 0),
	}

	// Record initial state
	result.InitialCut = p.hypergraph.ComputeEdgeCut()

	if p.config.Verbose {
		log.Printf("🔄 Starting partitioning - Initial edge cut: %d", result.InitialCut)
		stats := p.hypergraph.Stats()
		log.Printf("   Vertices: %d, Hyperedges: %d, Partitions: %d",
			stats.NumVertices, stats.NumHyperedges, stats.NumPartitions)
	}

	// Run FM-style iterative improvement
	p.fmPartition(result)

	// Record final state
	result.FinalCut = p.hypergraph.ComputeEdgeCut()
	if result.InitialCut > 0 {
		result.CutReduction = float64(result.InitialCut-result.FinalCut) / float64(result.InitialCut)
	}

	// Copy final assignment
	result.Assignment = p.hypergraph.GetPartitionAssignment()

	if p.config.Verbose {
		log.Printf("✅ Partitioning complete:")
		log.Printf("   Initial cut: %d", result.InitialCut)
		log.Printf("   Final cut: %d", result.FinalCut)
		log.Printf("   Cut reduction: %.2f%%", result.CutReduction*100)
		log.Printf("   Items to move: %d", len(result.Moves))
		log.Printf("   Iterations: %d", result.Iterations)
	}

	return result, nil
}

// fmPartition implements Fiduccia-Mattheyses style partitioning
func (p *Partitioner) fmPartition(result *PartitionResult) {
	iteration := 0
	lastCut := p.hypergraph.ComputeEdgeCut()

	for iteration < p.config.MaxIterations {
		iteration++

		// Get move candidates
		candidates := p.hypergraph.GetMoveCandidates(p.config.MinGain)

		if len(candidates) == 0 {
			if p.config.Verbose {
				log.Printf("   Iteration %d: No beneficial moves found", iteration)
			}
			break
		}

		// Track which vertices we've moved this pass
		moved := make(map[int32]bool)
		passGain := int64(0)

		// Process candidates by gain (greedy)
		for _, candidate := range candidates {
			if moved[candidate.VertexID] {
				continue
			}

			// Check balance constraints
			if !p.canMove(candidate.VertexID, candidate.ToPart) {
				continue
			}

			// Perform the move
			err := p.hypergraph.MoveVertex(candidate.VertexID, candidate.ToPart)
			if err != nil {
				continue
			}

			moved[candidate.VertexID] = true
			passGain += candidate.Gain

			// Record the move
			result.Moves = append(result.Moves, ItemMove{
				ItemID:      candidate.VertexID,
				FromCluster: candidate.FromPart,
				ToCluster:   candidate.ToPart,
				Gain:        candidate.Gain,
			})
		}

		newCut := p.hypergraph.ComputeEdgeCut()

		if p.config.Verbose {
			log.Printf("   Iteration %d: Moved %d items, cut: %d -> %d (gain: %d)",
				iteration, len(moved), lastCut, newCut, passGain)
		}

		// Check for improvement
		if lastCut > 0 {
			improvement := float64(lastCut-newCut) / float64(lastCut)
			if improvement < p.config.ImprovementThreshold {
				if p.config.Verbose {
					log.Printf("   Stopping: improvement %.4f below threshold %.4f",
						improvement, p.config.ImprovementThreshold)
				}
				break
			}
		}

		if newCut >= lastCut {
			// No improvement, rollback might be needed
			break
		}

		lastCut = newCut
	}

	result.Iterations = iteration
}

// canMove checks if a vertex can be moved to target partition (balance constraints)
func (p *Partitioner) canMove(vertexID int32, targetPart int32) bool {
	vertex, exists := p.hypergraph.Vertices[vertexID]
	if !exists {
		return false
	}

	currentPart := vertex.CurrentPart
	currentSize := p.hypergraph.PartitionSizes[currentPart]
	targetSize := p.hypergraph.PartitionSizes[targetPart]

	// Calculate expected partition size
	totalVertices := int32(len(p.hypergraph.Vertices))
	expectedSize := totalVertices / p.hypergraph.NumPartitions

	// Check imbalance constraints
	maxSize := int32(float64(expectedSize) * (1 + p.config.MaxImbalance))
	minSize := int32(float64(expectedSize) * (1 - p.config.MaxImbalance))

	// Would target exceed max?
	if targetSize+1 > maxSize {
		return false
	}

	// Would source go below min?
	if currentSize-1 < minSize {
		return false
	}

	return true
}

// OptimizeForLocalAccess runs a simpler optimization focusing on data locality
// This is useful when you want to minimize cross-cluster communication
func (p *Partitioner) OptimizeForLocalAccess(accessTracker *AccessTracker, threshold int64) *PartitionResult {
	result := &PartitionResult{
		Assignment: p.hypergraph.GetPartitionAssignment(),
		Moves:      make([]ItemMove, 0),
		InitialCut: p.hypergraph.ComputeEdgeCut(),
	}

	// Get top co-access pairs
	topPairs := accessTracker.GetTopCoAccessPairs(1000)

	if p.config.Verbose {
		log.Printf("🔄 Optimizing for local access - analyzing %d co-access pairs", len(topPairs))
	}

	// For each highly co-accessed pair, try to place them in same partition
	for _, entry := range topPairs {
		if entry.Count < threshold {
			continue
		}

		item1 := entry.Pair.First
		item2 := entry.Pair.Second

		part1 := p.hypergraph.Partition[item1]
		part2 := p.hypergraph.Partition[item2]

		if part1 == part2 {
			continue // Already in same partition
		}

		// Try moving item2 to item1's partition (or vice versa)
		gain1 := p.hypergraph.ComputeVertexGain(item2, part1)
		gain2 := p.hypergraph.ComputeVertexGain(item1, part2)

		var itemToMove, fromPart, toPart int32
		var bestGain int64

		if gain1 >= gain2 && gain1 > 0 && p.canMove(item2, part1) {
			itemToMove = item2
			fromPart = part2
			toPart = part1
			bestGain = gain1
		} else if gain2 > 0 && p.canMove(item1, part2) {
			itemToMove = item1
			fromPart = part1
			toPart = part2
			bestGain = gain2
		} else {
			continue // Can't improve
		}

		// Perform the move
		err := p.hypergraph.MoveVertex(itemToMove, toPart)
		if err != nil {
			continue
		}

		result.Moves = append(result.Moves, ItemMove{
			ItemID:      itemToMove,
			FromCluster: fromPart,
			ToCluster:   toPart,
			Gain:        bestGain,
		})
	}

	result.FinalCut = p.hypergraph.ComputeEdgeCut()
	result.Assignment = p.hypergraph.GetPartitionAssignment()

	if result.InitialCut > 0 {
		result.CutReduction = float64(result.InitialCut-result.FinalCut) / float64(result.InitialCut)
	}

	if p.config.Verbose {
		log.Printf("✅ Local access optimization complete:")
		log.Printf("   Cut: %d -> %d (%.2f%% reduction)",
			result.InitialCut, result.FinalCut, result.CutReduction*100)
		log.Printf("   Items to move: %d", len(result.Moves))
	}

	return result
}

// GetMigrationPlan generates a plan for migrating data items
func (p *Partitioner) GetMigrationPlan(result *PartitionResult) *MigrationPlan {
	plan := &MigrationPlan{
		Moves:                 make([]ItemMove, len(result.Moves)),
		MovesBySource:         make(map[int32][]ItemMove),
		MovesByTarget:         make(map[int32][]ItemMove),
		EstimatedCutReduction: result.CutReduction,
	}

	copy(plan.Moves, result.Moves)

	// Group by source and target
	for _, move := range result.Moves {
		plan.MovesBySource[move.FromCluster] = append(plan.MovesBySource[move.FromCluster], move)
		plan.MovesByTarget[move.ToCluster] = append(plan.MovesByTarget[move.ToCluster], move)
	}

	// Sort moves by cluster to ensure deterministic ordering
	for cluster := range plan.MovesBySource {
		sort.Slice(plan.MovesBySource[cluster], func(i, j int) bool {
			return plan.MovesBySource[cluster][i].ItemID < plan.MovesBySource[cluster][j].ItemID
		})
	}
	for cluster := range plan.MovesByTarget {
		sort.Slice(plan.MovesByTarget[cluster], func(i, j int) bool {
			return plan.MovesByTarget[cluster][i].ItemID < plan.MovesByTarget[cluster][j].ItemID
		})
	}

	return plan
}

// MigrationPlan represents a plan for data migration
type MigrationPlan struct {
	Moves                 []ItemMove
	MovesBySource         map[int32][]ItemMove
	MovesByTarget         map[int32][]ItemMove
	EstimatedCutReduction float64
}

// Summary returns a human-readable summary of the migration plan
func (mp *MigrationPlan) Summary() string {
	if len(mp.Moves) == 0 {
		return "No migrations needed"
	}

	summary := fmt.Sprintf("Migration Plan: %d items to move\n", len(mp.Moves))
	summary += fmt.Sprintf("Estimated cross-shard reduction: %.2f%%\n\n", mp.EstimatedCutReduction*100)

	summary += "By Source Cluster:\n"
	for cluster, moves := range mp.MovesBySource {
		summary += fmt.Sprintf("  Cluster %d: %d items leaving\n", cluster, len(moves))
	}

	summary += "\nBy Target Cluster:\n"
	for cluster, moves := range mp.MovesByTarget {
		summary += fmt.Sprintf("  Cluster %d: %d items arriving\n", cluster, len(moves))
	}

	return summary
}
