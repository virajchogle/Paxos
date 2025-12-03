package redistribution

import (
	"fmt"
	"sort"
)

// Hypergraph represents a hypergraph for data partitioning
// - Vertices: data items
// - Hyperedges: transactions (connecting items that are accessed together)
type Hypergraph struct {
	// Vertices (data items)
	Vertices map[int32]*Vertex

	// Hyperedges (transaction patterns)
	Hyperedges []*Hyperedge

	// Current partition assignment
	// vertex -> partition (cluster ID)
	Partition map[int32]int32

	// Number of partitions (clusters)
	NumPartitions int32

	// Partition sizes (for balance constraints)
	PartitionSizes map[int32]int32

	// Partition capacity constraints
	MinPartitionSize int32
	MaxPartitionSize int32
}

// Vertex represents a data item in the hypergraph
type Vertex struct {
	ID          int32
	Weight      int64           // Access frequency
	CurrentPart int32           // Current partition
	Neighbors   map[int32]int64 // Adjacent vertices and edge weights
}

// Hyperedge represents a transaction pattern (connecting multiple vertices)
type Hyperedge struct {
	ID       int64
	Vertices []int32 // Data items involved
	Weight   int64   // Frequency of this transaction pattern
}

// NewHypergraph creates a new hypergraph
func NewHypergraph(numPartitions int32, totalItems int32) *Hypergraph {
	itemsPerPartition := totalItems / numPartitions

	return &Hypergraph{
		Vertices:         make(map[int32]*Vertex),
		Hyperedges:       make([]*Hyperedge, 0),
		Partition:        make(map[int32]int32),
		NumPartitions:    numPartitions,
		PartitionSizes:   make(map[int32]int32),
		MinPartitionSize: itemsPerPartition / 2, // Allow 50% underfill
		MaxPartitionSize: itemsPerPartition * 2, // Allow 100% overfill
	}
}

// AddVertex adds a vertex (data item) to the hypergraph
func (hg *Hypergraph) AddVertex(id int32, weight int64, partition int32) {
	hg.Vertices[id] = &Vertex{
		ID:          id,
		Weight:      weight,
		CurrentPart: partition,
		Neighbors:   make(map[int32]int64),
	}
	hg.Partition[id] = partition
	hg.PartitionSizes[partition]++
}

// AddHyperedge adds a hyperedge (transaction pattern) to the hypergraph
func (hg *Hypergraph) AddHyperedge(vertices []int32, weight int64) {
	if len(vertices) < 2 {
		return // Need at least 2 vertices for an edge
	}

	hg.Hyperedges = append(hg.Hyperedges, &Hyperedge{
		ID:       int64(len(hg.Hyperedges)),
		Vertices: vertices,
		Weight:   weight,
	})

	// Update neighbor relationships (convert hyperedge to graph edges)
	for i := 0; i < len(vertices); i++ {
		for j := i + 1; j < len(vertices); j++ {
			v1, v2 := vertices[i], vertices[j]
			if vertex, exists := hg.Vertices[v1]; exists {
				vertex.Neighbors[v2] += weight
			}
			if vertex, exists := hg.Vertices[v2]; exists {
				vertex.Neighbors[v1] += weight
			}
		}
	}
}

// BuildFromAccessTracker builds hypergraph from access pattern data
func (hg *Hypergraph) BuildFromAccessTracker(tracker *AccessTracker, currentMapping func(int32) int32) {
	// Add vertices from item access counts
	itemAccess := tracker.GetItemAccessMap()
	for itemID, count := range itemAccess {
		partition := currentMapping(itemID)
		hg.AddVertex(itemID, count, partition)
	}

	// Add hyperedges from co-access patterns
	coAccess := tracker.GetCoAccessMatrix()
	for pair, count := range coAccess {
		hg.AddHyperedge([]int32{pair.First, pair.Second}, count)
	}
}

// ComputeEdgeCut calculates the total weight of edges crossing partition boundaries
func (hg *Hypergraph) ComputeEdgeCut() int64 {
	var cut int64

	for _, edge := range hg.Hyperedges {
		// Check if this hyperedge spans multiple partitions
		partitions := make(map[int32]bool)
		for _, v := range edge.Vertices {
			if part, exists := hg.Partition[v]; exists {
				partitions[part] = true
			}
		}

		// If more than one partition, this is a cut edge
		if len(partitions) > 1 {
			cut += edge.Weight
		}
	}

	return cut
}

// ComputeVertexGain calculates the gain from moving a vertex to a new partition
// Using Fiduccia-Mattheyses (FM) style gain calculation
func (hg *Hypergraph) ComputeVertexGain(vertexID int32, targetPart int32) int64 {
	vertex, exists := hg.Vertices[vertexID]
	if !exists {
		return 0
	}

	currentPart := hg.Partition[vertexID]
	if currentPart == targetPart {
		return 0 // Already in target partition
	}

	var gain int64

	// For each neighbor, calculate contribution
	for neighborID, edgeWeight := range vertex.Neighbors {
		neighborPart := hg.Partition[neighborID]

		if neighborPart == currentPart {
			// This edge will become a cut edge (negative gain)
			gain -= edgeWeight
		} else if neighborPart == targetPart {
			// This cut edge will be removed (positive gain)
			gain += edgeWeight
		}
		// If neighbor is in a third partition, no change
	}

	return gain
}

// MoveVertex moves a vertex to a new partition
func (hg *Hypergraph) MoveVertex(vertexID int32, targetPart int32) error {
	vertex, exists := hg.Vertices[vertexID]
	if !exists {
		return fmt.Errorf("vertex %d not found", vertexID)
	}

	oldPart := vertex.CurrentPart

	// Check capacity constraints
	if hg.PartitionSizes[targetPart] >= hg.MaxPartitionSize {
		return fmt.Errorf("partition %d at capacity (%d)", targetPart, hg.MaxPartitionSize)
	}
	if hg.PartitionSizes[oldPart] <= hg.MinPartitionSize {
		return fmt.Errorf("partition %d below minimum size (%d)", oldPart, hg.MinPartitionSize)
	}

	// Move the vertex
	vertex.CurrentPart = targetPart
	hg.Partition[vertexID] = targetPart
	hg.PartitionSizes[oldPart]--
	hg.PartitionSizes[targetPart]++

	return nil
}

// GetPartitionAssignment returns current partition assignments
func (hg *Hypergraph) GetPartitionAssignment() map[int32]int32 {
	result := make(map[int32]int32, len(hg.Partition))
	for k, v := range hg.Partition {
		result[k] = v
	}
	return result
}

// GetVerticesInPartition returns all vertices in a partition
func (hg *Hypergraph) GetVerticesInPartition(partition int32) []int32 {
	var vertices []int32
	for v, p := range hg.Partition {
		if p == partition {
			vertices = append(vertices, v)
		}
	}
	sort.Slice(vertices, func(i, j int) bool { return vertices[i] < vertices[j] })
	return vertices
}

// GetMoveCandidates returns vertices that could be moved to improve cut
// Returns pairs of (vertex, targetPartition, gain) sorted by gain
func (hg *Hypergraph) GetMoveCandidates(minGain int64) []MoveCandidate {
	var candidates []MoveCandidate

	for vertexID := range hg.Vertices {
		currentPart := hg.Partition[vertexID]

		// Try moving to each other partition
		for targetPart := int32(1); targetPart <= hg.NumPartitions; targetPart++ {
			if targetPart == currentPart {
				continue
			}

			gain := hg.ComputeVertexGain(vertexID, targetPart)
			if gain >= minGain {
				candidates = append(candidates, MoveCandidate{
					VertexID: vertexID,
					FromPart: currentPart,
					ToPart:   targetPart,
					Gain:     gain,
				})
			}
		}
	}

	// Sort by gain (descending)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Gain > candidates[j].Gain
	})

	return candidates
}

// MoveCandidate represents a potential vertex move
type MoveCandidate struct {
	VertexID int32
	FromPart int32
	ToPart   int32
	Gain     int64
}

// Stats returns hypergraph statistics
func (hg *Hypergraph) Stats() HypergraphStats {
	var totalVertexWeight int64
	for _, v := range hg.Vertices {
		totalVertexWeight += v.Weight
	}

	var totalEdgeWeight int64
	for _, e := range hg.Hyperedges {
		totalEdgeWeight += e.Weight
	}

	partSizes := make([]int32, 0, len(hg.PartitionSizes))
	for _, size := range hg.PartitionSizes {
		partSizes = append(partSizes, size)
	}

	return HypergraphStats{
		NumVertices:       int32(len(hg.Vertices)),
		NumHyperedges:     int32(len(hg.Hyperedges)),
		TotalVertexWeight: totalVertexWeight,
		TotalEdgeWeight:   totalEdgeWeight,
		NumPartitions:     hg.NumPartitions,
		PartitionSizes:    partSizes,
		EdgeCut:           hg.ComputeEdgeCut(),
	}
}

// HypergraphStats contains hypergraph statistics
type HypergraphStats struct {
	NumVertices       int32
	NumHyperedges     int32
	TotalVertexWeight int64
	TotalEdgeWeight   int64
	NumPartitions     int32
	PartitionSizes    []int32
	EdgeCut           int64
}
