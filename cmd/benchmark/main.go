package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"paxos-banking/internal/benchmark"
)

func main() {
	// Define flags
	preset := flag.String("preset", "", "Preset configuration (default, high-throughput, cross-shard, stress)")
	totalTxns := flag.Int("transactions", 0, "Total number of transactions")
	targetTPS := flag.Int("tps", 0, "Target transactions per second (0=unlimited)")
	duration := flag.Int("duration", 0, "Duration in seconds (0=use transactions)")
	numClients := flag.Int("clients", 0, "Number of concurrent clients")
	crossShardPct := flag.Int("cross-shard", -1, "Percentage of cross-shard transactions (0-100)")
	readOnlyPct := flag.Int("read-only", -1, "Percentage of read-only queries (0-100)")
	distribution := flag.String("distribution", "", "Data distribution (uniform, zipf, hotspot)")
	skewness := flag.Float64("skewness", -1, "Zipf skewness parameter (0-1, higher=more skewed)")
	warmup := flag.Int("warmup", -1, "Warmup duration in seconds")
	reportInterval := flag.Int("report", -1, "Report progress every N seconds")
	detailedStats := flag.Bool("detailed", false, "Include detailed percentile statistics")
	exportCSV := flag.Bool("csv", false, "Export results to CSV")
	outputFile := flag.String("output", "benchmark_results.csv", "Output CSV file path")
	nodes := flag.String("nodes", "localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055,localhost:50056,localhost:50057,localhost:50058,localhost:50059", "Comma-separated list of node addresses")

	flag.Parse()

	// Get configuration
	var config *benchmark.BenchmarkConfig

	switch strings.ToLower(*preset) {
	case "high-throughput", "ht":
		config = benchmark.HighThroughputConfig()
		log.Println("📊 Using preset: High Throughput")
	case "cross-shard", "cs":
		config = benchmark.CrossShardHeavyConfig()
		log.Println("📊 Using preset: Cross-Shard Heavy")
	case "stress":
		config = benchmark.StressTestConfig()
		log.Println("📊 Using preset: Stress Test")
	default:
		config = benchmark.DefaultConfig()
		if *preset != "" && *preset != "default" {
			log.Printf("⚠️  Unknown preset '%s', using default\n", *preset)
		}
	}

	// Override with command-line flags
	if *totalTxns > 0 {
		config.TotalTransactions = *totalTxns
	}
	if *targetTPS >= 0 {
		config.TargetTPS = *targetTPS
	}
	if *duration > 0 {
		config.Duration = *duration
	}
	if *numClients > 0 {
		config.NumClients = *numClients
	}
	if *crossShardPct >= 0 {
		config.CrossShardPercent = *crossShardPct
	}
	if *readOnlyPct >= 0 {
		config.ReadOnlyPercent = *readOnlyPct
	}
	if *distribution != "" {
		config.DataDistribution = *distribution
	}
	if *skewness >= 0 {
		config.ZipfS = *skewness
		// Auto-set distribution to zipf if skewness is specified but distribution isn't
		if *distribution == "" {
			config.DataDistribution = "zipf"
		}
	}
	if *warmup >= 0 {
		config.WarmupSeconds = *warmup
	}
	if *reportInterval >= 0 {
		config.ReportInterval = *reportInterval
	}
	if *detailedStats {
		config.DetailedStats = true
	}
	if *exportCSV {
		config.ExportCSV = true
		config.OutputFile = *outputFile
	}

	// Parse node addresses
	nodeAddresses := strings.Split(*nodes, ",")
	for i, addr := range nodeAddresses {
		nodeAddresses[i] = strings.TrimSpace(addr)
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		log.Fatalf("❌ Invalid configuration: %v", err)
	}

	// Create and run benchmark
	runner, err := benchmark.NewBenchmarkRunner(config, nodeAddresses)
	if err != nil {
		log.Fatalf("❌ Failed to create benchmark runner: %v", err)
	}

	log.Printf("🎯 Estimated duration: %s\n", config.GetEstimatedDuration())

	if err := runner.Run(); err != nil {
		log.Fatalf("❌ Benchmark failed: %v", err)
	}
}

func init() {
	log.SetFlags(0) // Remove timestamp from logs

	// Print banner
	fmt.Println("╔════════════════════════════════════════════════════════╗")
	fmt.Println("║       Paxos Banking System - Benchmark Tool         ║")
	fmt.Println("╚════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Check if help was requested
	for _, arg := range os.Args[1:] {
		if arg == "-h" || arg == "-help" || arg == "--help" {
			printUsage()
			os.Exit(0)
		}
	}
}

func printUsage() {
	fmt.Println("Usage: benchmark [options]")
	fmt.Println()
	fmt.Println("Presets:")
	fmt.Println("  -preset default          Default configuration (balanced)")
	fmt.Println("  -preset high-throughput  Maximum throughput test")
	fmt.Println("  -preset cross-shard      Cross-shard heavy workload")
	fmt.Println("  -preset stress           Stress test configuration")
	fmt.Println()
	fmt.Println("Workload Options:")
	fmt.Println("  -transactions N          Total transactions to execute")
	fmt.Println("  -tps N                   Target TPS (0=unlimited)")
	fmt.Println("  -duration N              Run for N seconds (instead of transaction count)")
	fmt.Println("  -clients N               Number of concurrent clients")
	fmt.Println("  -cross-shard N           Percentage of cross-shard transactions (0-100)")
	fmt.Println("  -read-only N             Percentage of read-only queries (0-100)")
	fmt.Println("  -distribution TYPE       Data distribution (uniform/zipf/hotspot)")
	fmt.Println("  -skewness N              Zipf skewness parameter (0-1, default: 1.0)")
	fmt.Println("                           Higher values create more skewed access patterns")
	fmt.Println()
	fmt.Println("Output Options:")
	fmt.Println("  -warmup N                Warmup duration in seconds")
	fmt.Println("  -report N                Report progress every N seconds")
	fmt.Println("  -detailed                Include detailed percentile statistics")
	fmt.Println("  -csv                     Export results to CSV")
	fmt.Println("  -output FILE             CSV output file path")
	fmt.Println()
	fmt.Println("Connection Options:")
	fmt.Println("  -nodes ADDR,ADDR,...     Comma-separated node addresses")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  # Quick test")
	fmt.Println("  ./benchmark -transactions 1000 -clients 10")
	fmt.Println()
	fmt.Println("  # High throughput test")
	fmt.Println("  ./benchmark -preset high-throughput")
	fmt.Println()
	fmt.Println("  # Custom workload")
	fmt.Println("  ./benchmark -transactions 50000 -tps 5000 -clients 50 \\")
	fmt.Println("              -cross-shard 30 -read-only 20 -detailed")
	fmt.Println()
	fmt.Println("  # Stress test with reporting")
	fmt.Println("  ./benchmark -preset stress -report 10 -detailed -csv")
	fmt.Println()
}
