package benchmark

import (
	"fmt"
	"time"
)

// BenchmarkConfig defines parameters for a benchmark run
type BenchmarkConfig struct {
	// Workload parameters
	TotalTransactions int // Total number of transactions to execute
	TargetTPS         int // Target transactions per second (0 = unlimited)
	Duration          int // Duration in seconds (0 = use TotalTransactions)
	NumClients        int // Number of concurrent clients

	// Cluster configuration (supports configurable clusters)
	NumClusters int   // Number of clusters (default: 3)
	TotalItems  int32 // Total data items (default: 9000)

	// Transaction mix
	CrossShardPercent int // Percentage of cross-shard transactions (0-100)
	ReadOnlyPercent   int // Percentage of read-only queries (0-100)

	// Data distribution
	DataDistribution string  // "uniform", "zipf", "hotspot"
	ZipfS            float64 // Zipf parameter (default 1.0)
	HotspotPercent   int     // Percentage of items in hotspot (for hotspot distribution)
	HotspotAccess    int     // Percentage of accesses to hotspot items

	// Transaction parameters
	MinAmount int32 // Minimum transaction amount
	MaxAmount int32 // Maximum transaction amount

	// Warmup
	WarmupSeconds int // Warmup duration before measurements

	// Output
	ReportInterval int    // Print progress every N seconds (0 = only final)
	DetailedStats  bool   // Include detailed percentile statistics
	ExportCSV      bool   // Export results to CSV
	OutputFile     string // Output file path (if ExportCSV)
}

// DefaultConfig returns a reasonable default configuration
func DefaultConfig() *BenchmarkConfig {
	return &BenchmarkConfig{
		TotalTransactions: 10000,
		TargetTPS:         1000,
		Duration:          0,
		NumClients:        10,
		NumClusters:       3,    // Default 3 clusters (configurable)
		TotalItems:        9000, // Default 9000 items (configurable)
		CrossShardPercent: 20,
		ReadOnlyPercent:   10,
		DataDistribution:  "uniform",
		ZipfS:             1.0,
		HotspotPercent:    10,
		HotspotAccess:     80,
		MinAmount:         1,
		MaxAmount:         10,
		WarmupSeconds:     5,
		ReportInterval:    5,
		DetailedStats:     true,
		ExportCSV:         false,
		OutputFile:        "benchmark_results.csv",
	}
}

// HighThroughputConfig returns config optimized for max throughput
func HighThroughputConfig() *BenchmarkConfig {
	cfg := DefaultConfig()
	cfg.TotalTransactions = 50000
	cfg.TargetTPS = 0 // Unlimited
	cfg.NumClients = 50
	cfg.CrossShardPercent = 0 // Intra-shard only for max speed
	cfg.ReadOnlyPercent = 0
	cfg.WarmupSeconds = 10
	return cfg
}

// CrossShardHeavyConfig returns config with mostly cross-shard transactions
func CrossShardHeavyConfig() *BenchmarkConfig {
	cfg := DefaultConfig()
	cfg.TotalTransactions = 20000
	cfg.TargetTPS = 500
	cfg.NumClients = 20
	cfg.CrossShardPercent = 80
	cfg.ReadOnlyPercent = 10
	cfg.WarmupSeconds = 10
	return cfg
}

// StressTestConfig returns config for stress testing
func StressTestConfig() *BenchmarkConfig {
	cfg := DefaultConfig()
	cfg.TotalTransactions = 100000
	cfg.TargetTPS = 5000
	cfg.NumClients = 100
	cfg.CrossShardPercent = 30
	cfg.ReadOnlyPercent = 20
	cfg.WarmupSeconds = 15
	cfg.DetailedStats = true
	return cfg
}

// Validate checks if the configuration is valid
func (c *BenchmarkConfig) Validate() error {
	if c.TotalTransactions <= 0 && c.Duration <= 0 {
		return fmt.Errorf("either TotalTransactions or Duration must be > 0")
	}
	if c.NumClients <= 0 {
		return fmt.Errorf("NumClients must be > 0")
	}
	if c.CrossShardPercent < 0 || c.CrossShardPercent > 100 {
		return fmt.Errorf("CrossShardPercent must be 0-100")
	}
	if c.ReadOnlyPercent < 0 || c.ReadOnlyPercent > 100 {
		return fmt.Errorf("ReadOnlyPercent must be 0-100")
	}
	if c.CrossShardPercent+c.ReadOnlyPercent > 100 {
		return fmt.Errorf("CrossShardPercent + ReadOnlyPercent must be <= 100")
	}
	if c.MinAmount <= 0 || c.MaxAmount <= 0 || c.MinAmount > c.MaxAmount {
		return fmt.Errorf("invalid amount range: min=%d, max=%d", c.MinAmount, c.MaxAmount)
	}

	validDistributions := map[string]bool{
		"uniform": true,
		"zipf":    true,
		"hotspot": true,
	}
	if !validDistributions[c.DataDistribution] {
		return fmt.Errorf("invalid DataDistribution: %s (must be uniform/zipf/hotspot)", c.DataDistribution)
	}

	return nil
}

// String returns a human-readable description of the config
func (c *BenchmarkConfig) String() string {
	str := "╔════════════════════════════════════════════════════════╗\n"
	str += "║           Benchmark Configuration                    ║\n"
	str += "╚════════════════════════════════════════════════════════╝\n"

	if c.Duration > 0 {
		str += fmt.Sprintf("Duration:            %d seconds\n", c.Duration)
	} else {
		str += fmt.Sprintf("Total Transactions:  %d\n", c.TotalTransactions)
	}

	str += fmt.Sprintf("Target TPS:          %s\n", map[bool]string{true: "Unlimited", false: fmt.Sprintf("%d", c.TargetTPS)}[c.TargetTPS == 0])
	str += fmt.Sprintf("Concurrent Clients:  %d\n", c.NumClients)
	str += fmt.Sprintf("\nTransaction Mix:\n")
	str += fmt.Sprintf("  Intra-shard:       %d%%\n", 100-c.CrossShardPercent-c.ReadOnlyPercent)
	str += fmt.Sprintf("  Cross-shard:       %d%%\n", c.CrossShardPercent)
	str += fmt.Sprintf("  Read-only:         %d%%\n", c.ReadOnlyPercent)
	str += fmt.Sprintf("\nData Distribution:   %s\n", c.DataDistribution)

	if c.DataDistribution == "zipf" {
		str += fmt.Sprintf("  Zipf parameter:    %.2f\n", c.ZipfS)
	} else if c.DataDistribution == "hotspot" {
		str += fmt.Sprintf("  Hotspot size:      %d%% of items\n", c.HotspotPercent)
		str += fmt.Sprintf("  Hotspot access:    %d%% of requests\n", c.HotspotAccess)
	}

	str += fmt.Sprintf("\nAmount Range:        %d - %d\n", c.MinAmount, c.MaxAmount)
	str += fmt.Sprintf("Warmup Duration:     %d seconds\n", c.WarmupSeconds)
	str += fmt.Sprintf("Report Interval:     %s\n", map[bool]string{true: "Final only", false: fmt.Sprintf("Every %d seconds", c.ReportInterval)}[c.ReportInterval == 0])
	str += fmt.Sprintf("Detailed Stats:      %v\n", c.DetailedStats)

	if c.ExportCSV {
		str += fmt.Sprintf("Export CSV:          %s\n", c.OutputFile)
	}

	return str
}

// GetEstimatedDuration returns estimated benchmark duration
func (c *BenchmarkConfig) GetEstimatedDuration() time.Duration {
	if c.Duration > 0 {
		return time.Duration(c.Duration+c.WarmupSeconds) * time.Second
	}

	if c.TargetTPS == 0 {
		// Unlimited TPS - estimate based on typical performance
		estimatedTPS := 1000 // Conservative estimate
		seconds := c.TotalTransactions / estimatedTPS
		return time.Duration(seconds+c.WarmupSeconds) * time.Second
	}

	seconds := c.TotalTransactions / c.TargetTPS
	return time.Duration(seconds+c.WarmupSeconds) * time.Second
}
