package benchmark

import (
	"math"
	"math/rand"
	"time"
)

// ClusterRange defines the data item range for a cluster
type ClusterRange struct {
	ID    int
	Start int32
	End   int32
}

// WorkloadGenerator generates transactions according to configuration
type WorkloadGenerator struct {
	config        *BenchmarkConfig
	rng           *rand.Rand
	zipfGen       *ZipfGenerator
	totalItems    int32          // Total data items across all shards
	hotspotStart  int32          // Start of hotspot range
	hotspotEnd    int32          // End of hotspot range
	numClusters   int            // Number of clusters (configurable)
	clusterRanges []ClusterRange // Range for each cluster (configurable)
}

// NewWorkloadGenerator creates a new workload generator
func NewWorkloadGenerator(config *BenchmarkConfig, totalItems int32) *WorkloadGenerator {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	wg := &WorkloadGenerator{
		config:     config,
		rng:        rng,
		totalItems: totalItems,
	}

	// Initialize cluster ranges (supports configurable clusters)
	wg.initClusterRanges()

	// Initialize distribution-specific generators
	switch config.DataDistribution {
	case "zipf":
		wg.zipfGen = NewZipfGenerator(uint64(totalItems), config.ZipfS, rng)
	case "hotspot":
		hotspotSize := int32(float64(totalItems) * float64(config.HotspotPercent) / 100.0)
		wg.hotspotStart = 1
		wg.hotspotEnd = hotspotSize
	}

	return wg
}

// initClusterRanges initializes cluster ranges based on configuration
func (wg *WorkloadGenerator) initClusterRanges() {
	// Default: 3 clusters with equal distribution
	numClusters := wg.config.NumClusters
	if numClusters <= 0 {
		numClusters = 3
	}
	wg.numClusters = numClusters

	itemsPerCluster := wg.totalItems / int32(numClusters)
	wg.clusterRanges = make([]ClusterRange, numClusters)

	start := int32(1)
	for i := 0; i < numClusters; i++ {
		end := start + itemsPerCluster - 1
		if i == numClusters-1 {
			// Last cluster gets remaining items
			end = wg.totalItems
		}
		wg.clusterRanges[i] = ClusterRange{
			ID:    i + 1,
			Start: start,
			End:   end,
		}
		start = end + 1
	}
}

// GenerateTransaction generates a single transaction (supports configurable clusters)
func (wg *WorkloadGenerator) GenerateTransaction() *Transaction {
	txn := &Transaction{}

	// Determine transaction type
	roll := wg.rng.Intn(100)

	if roll < wg.config.ReadOnlyPercent {
		// Read-only query
		txn.Type = TxnTypeReadOnly
		txn.Sender = wg.selectDataItem()
		txn.Receiver = 0
		txn.Amount = 0
	} else if roll < wg.config.ReadOnlyPercent+wg.config.CrossShardPercent {
		// Cross-shard transaction (supports configurable clusters)
		txn.Type = TxnTypeCrossShard
		// Select two different random clusters
		cluster1 := wg.rng.Intn(wg.numClusters) + 1
		cluster2 := wg.rng.Intn(wg.numClusters) + 1
		for cluster2 == cluster1 && wg.numClusters > 1 {
			cluster2 = wg.rng.Intn(wg.numClusters) + 1
		}
		txn.Sender = wg.selectDataItemFromCluster(cluster1)
		txn.Receiver = wg.selectDataItemFromCluster(cluster2)
		txn.Amount = wg.selectAmount()
	} else {
		// Intra-shard transaction (supports configurable clusters)
		txn.Type = TxnTypeIntraShard
		cluster := wg.rng.Intn(wg.numClusters) + 1
		txn.Sender = wg.selectDataItemFromCluster(cluster)
		txn.Receiver = wg.selectDataItemFromCluster(cluster)
		// Make sure sender != receiver
		for txn.Receiver == txn.Sender {
			txn.Receiver = wg.selectDataItemFromCluster(cluster)
		}
		txn.Amount = wg.selectAmount()
	}

	return txn
}

// selectDataItem selects a data item according to the distribution
func (wg *WorkloadGenerator) selectDataItem() int32 {
	switch wg.config.DataDistribution {
	case "zipf":
		return int32(wg.zipfGen.Next()) + 1 // 1-indexed
	case "hotspot":
		roll := wg.rng.Intn(100)
		if roll < wg.config.HotspotAccess {
			// Access hotspot
			return wg.hotspotStart + wg.rng.Int31n(wg.hotspotEnd-wg.hotspotStart+1)
		}
		// Access cold items
		return wg.hotspotEnd + 1 + wg.rng.Int31n(wg.totalItems-wg.hotspotEnd)
	default: // uniform
		return wg.rng.Int31n(wg.totalItems) + 1
	}
}

// selectDataItemFromCluster selects a data item from a specific cluster (supports configurable clusters)
func (wg *WorkloadGenerator) selectDataItemFromCluster(cluster int) int32 {
	// Get range from configurable cluster ranges
	if cluster < 1 || cluster > len(wg.clusterRanges) {
		// Fallback to cluster 1
		cluster = 1
	}

	r := wg.clusterRanges[cluster-1]
	rangeSize := r.End - r.Start + 1

	switch wg.config.DataDistribution {
	case "zipf":
		// Map zipf to cluster range
		zipfVal := wg.zipfGen.Next()
		scaledVal := (zipfVal % uint64(rangeSize))
		return r.Start + int32(scaledVal)
	case "hotspot":
		// Check if hotspot is in this cluster
		if wg.hotspotStart >= r.Start && wg.hotspotStart <= r.End {
			roll := wg.rng.Intn(100)
			if roll < wg.config.HotspotAccess {
				// Hotspot access (limited to cluster range)
				hotspotInCluster := min(wg.hotspotEnd, r.End) - wg.hotspotStart + 1
				return wg.hotspotStart + wg.rng.Int31n(hotspotInCluster)
			}
		}
		// Uniform within cluster
		return r.Start + wg.rng.Int31n(rangeSize)
	default: // uniform
		return r.Start + wg.rng.Int31n(rangeSize)
	}
}

// selectAmount selects a random amount within configured range
func (wg *WorkloadGenerator) selectAmount() int32 {
	if wg.config.MinAmount == wg.config.MaxAmount {
		return wg.config.MinAmount
	}
	rangeSize := wg.config.MaxAmount - wg.config.MinAmount + 1
	return wg.config.MinAmount + wg.rng.Int31n(rangeSize)
}

func min(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

// ============================================================================
// Transaction Type Definition
// ============================================================================

type TransactionType int

const (
	TxnTypeIntraShard TransactionType = iota
	TxnTypeCrossShard
	TxnTypeReadOnly
)

func (t TransactionType) String() string {
	switch t {
	case TxnTypeIntraShard:
		return "Intra-shard"
	case TxnTypeCrossShard:
		return "Cross-shard"
	case TxnTypeReadOnly:
		return "Read-only"
	default:
		return "Unknown"
	}
}

type Transaction struct {
	Type     TransactionType
	Sender   int32
	Receiver int32
	Amount   int32
}

// ============================================================================
// Zipf Generator
// ============================================================================

// ZipfGenerator generates Zipf-distributed random numbers
type ZipfGenerator struct {
	rng   *rand.Rand
	n     uint64
	s     float64
	v     float64
	q     float64
	theta float64
	zeta2 float64
	alpha float64
	zetan float64
	eta   float64
}

// NewZipfGenerator creates a new Zipf generator
func NewZipfGenerator(n uint64, s float64, rng *rand.Rand) *ZipfGenerator {
	if n == 0 {
		n = 1
	}

	zg := &ZipfGenerator{
		rng:   rng,
		n:     n,
		s:     s,
		theta: s,
	}

	zg.zeta2 = zg.zeta(2)
	zg.alpha = 1.0 / (1.0 - s)
	zg.zetan = zg.zeta(n)
	zg.eta = (1.0 - math.Pow(2.0/float64(n), 1.0-s)) / (1.0 - zg.zeta2/zg.zetan)

	return zg
}

// Next returns the next Zipf-distributed value (0-indexed)
func (zg *ZipfGenerator) Next() uint64 {
	u := zg.rng.Float64()
	uz := u * zg.zetan

	if uz < 1.0 {
		return 0
	}

	if uz < 1.0+math.Pow(0.5, zg.theta) {
		return 1
	}

	return uint64(float64(zg.n) * math.Pow(zg.eta*u-zg.eta+1.0, zg.alpha))
}

// zeta computes the zeta function for Zipf
func (zg *ZipfGenerator) zeta(n uint64) float64 {
	sum := 0.0
	for i := uint64(1); i <= n; i++ {
		sum += 1.0 / math.Pow(float64(i), zg.theta)
	}
	return sum
}
