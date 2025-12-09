# Paxos Banking System - Benchmarking Guide

## Overview

This benchmarking system provides comprehensive performance testing for the Paxos-based distributed banking system. It supports configurable workloads with various transaction mixes and data distribution patterns.

## Features

### ✅ Required Parameters (Per Project Specification)

1. **Read-only vs Read-write Transactions** (`-read-only` flag)
   - Configure percentage of read-only queries (balance checks)
   - Remainder are write transactions (transfers)
   - Range: 0-100%

2. **Intra-shard vs Cross-shard Transactions** (`-cross-shard` flag)
   - Configure percentage of cross-shard (2PC) transactions
   - Remainder are intra-shard (single Paxos cluster) transactions
   - Range: 0-100%

3. **Data Distribution** (`-distribution` flag)
   - **Uniform**: Equal probability of accessing all data items
   - **Zipf**: Power-law distribution (configurable skew parameter)
   - **Hotspot**: Small percentage of items receive most accesses

### 📊 Additional Features

- **Concurrent clients**: Simulate multiple simultaneous clients
- **Rate limiting**: Target specific TPS for controlled load
- **Warmup phase**: Warm up caches before measurement
- **Real-time progress**: Live statistics during execution
- **Detailed metrics**: Percentile latencies (p50, p95, p99, p99.9)
- **CSV export**: Export results for analysis
- **Preset configurations**: Quick access to common workloads

## Quick Start

### 1. Build the Benchmark Tool

```bash
# Build manually
go build -o bin/benchmark cmd/benchmark/main.go

# Or use the script (builds automatically)
./scripts/run_benchmark.sh quick
```

### 2. Start Your Paxos Nodes

Make sure all 9 nodes are running:

```bash
./scripts/start_nodes.sh
```

### 3. Run a Quick Test

```bash
./scripts/run_benchmark.sh quick
```

## Usage

### Using the Helper Script (Recommended)

```bash
# Quick test (1000 transactions)
./scripts/run_benchmark.sh quick

# Default benchmark (10K transactions, balanced mix)
./scripts/run_benchmark.sh default

# High throughput test (50K transactions, unlimited TPS)
./scripts/run_benchmark.sh high-throughput

# Cross-shard heavy workload (80% cross-shard)
./scripts/run_benchmark.sh cross-shard

# Stress test (100K transactions, 5000 TPS target)
./scripts/run_benchmark.sh stress

# Zipf distribution test (skewed access pattern)
./scripts/run_benchmark.sh zipf

# Hotspot test (10% of items get 80% of accesses)
./scripts/run_benchmark.sh hotspot

# Run all presets sequentially
./scripts/run_benchmark.sh all

# Custom benchmark
./scripts/run_benchmark.sh custom -transactions 20000 -clients 30 -cross-shard 50
```

### Using the Binary Directly

```bash
./bin/benchmark [options]
```

#### Common Options

```
Workload Options:
  -transactions N      Total transactions to execute
  -tps N              Target TPS (0=unlimited)
  -duration N         Run for N seconds (instead of transaction count)
  -clients N          Number of concurrent clients
  -cross-shard N      Percentage of cross-shard transactions (0-100)
  -read-only N        Percentage of read-only queries (0-100)
  -distribution TYPE  Data distribution (uniform/zipf/hotspot)

Output Options:
  -warmup N           Warmup duration in seconds
  -report N           Report progress every N seconds
  -detailed           Include detailed percentile statistics
  -csv                Export results to CSV
  -output FILE        CSV output file path

Connection Options:
  -nodes ADDR,ADDR    Comma-separated node addresses
```

## Examples

### Example 1: Basic Throughput Test

Test maximum throughput with intra-shard transactions only:

```bash
./bin/benchmark \
  -transactions 50000 \
  -tps 0 \
  -clients 50 \
  -cross-shard 0 \
  -read-only 0 \
  -distribution uniform \
  -detailed
```

### Example 2: Realistic Mixed Workload

80% intra-shard, 15% cross-shard, 5% read-only:

```bash
./bin/benchmark \
  -transactions 20000 \
  -tps 1000 \
  -clients 30 \
  -cross-shard 15 \
  -read-only 5 \
  -distribution uniform \
  -warmup 10 \
  -report 5 \
  -detailed
```

### Example 3: Hotspot Stress Test

Test behavior under contention (hotspot):

```bash
./bin/benchmark \
  -transactions 10000 \
  -clients 50 \
  -cross-shard 30 \
  -read-only 10 \
  -distribution hotspot \
  -detailed \
  -csv \
  -output results/hotspot_test.csv
```

### Example 4: Zipf Distribution (Realistic)

Simulate realistic access patterns (some accounts more popular):

```bash
./bin/benchmark \
  -transactions 25000 \
  -clients 40 \
  -cross-shard 25 \
  -read-only 15 \
  -distribution zipf \
  -detailed
```

### Example 5: Cross-Shard Performance Analysis

Focus on 2PC performance:

```bash
./bin/benchmark \
  -transactions 15000 \
  -clients 25 \
  -cross-shard 80 \
  -read-only 0 \
  -distribution uniform \
  -detailed \
  -csv \
  -output results/cross_shard_perf.csv
```

## Preset Configurations

### Default
- 10,000 transactions
- 1000 TPS target
- 10 concurrent clients
- 70% intra-shard, 20% cross-shard, 10% read-only
- Uniform distribution

### High Throughput
- 50,000 transactions
- Unlimited TPS
- 50 concurrent clients
- 100% intra-shard (no cross-shard or read-only)
- Optimized for maximum throughput

### Cross-Shard Heavy
- 20,000 transactions
- 500 TPS target
- 20 concurrent clients
- 10% intra-shard, 80% cross-shard, 10% read-only
- Tests 2PC performance under load

### Stress Test
- 100,000 transactions
- 5000 TPS target
- 100 concurrent clients
- 50% intra-shard, 30% cross-shard, 20% read-only
- Comprehensive system stress test

## Understanding the Output

### Console Output

```
╔════════════════════════════════════════════════════════╗
║              Benchmark Results                       ║
╚════════════════════════════════════════════════════════╝

Duration:           25.34 seconds
Total Transactions: 10000
Successful:         9987 (99.87%)
Failed:             13 (0.13%)
Throughput:         394.63 TPS

--- Latency Statistics ---
Average:            24.67 ms
Min:                3.21 ms
Max:                156.78 ms
p50:                21.45 ms
p95:                45.89 ms
p99:                78.23 ms
p99.9:              134.56 ms

--- Per-Type Statistics ---
Intra-shard: 7001 transactions, avg latency: 18.34 ms
Cross-shard: 1998 transactions, avg latency: 52.67 ms
Read-only: 1001 transactions, avg latency: 5.23 ms
```

### CSV Output

When `-csv` flag is used, results are exported to a CSV file with:
- Overall metrics (throughput, success rate, etc.)
- Latency percentiles
- Per-transaction-type statistics

## Data Distribution Patterns

### Uniform Distribution
- Equal probability for all data items
- Best for testing baseline performance
- No contention hotspots

### Zipf Distribution
- Power-law distribution (controlled by `-zipf-s` parameter)
- Simulates realistic access patterns
- Some items accessed much more frequently than others
- Default Zipf-s = 1.0 (moderate skew)

### Hotspot Distribution
- Small percentage of items receive majority of accesses
- Controlled by two parameters:
  - Hotspot size: % of items in hotspot (default 10%)
  - Hotspot access: % of accesses to hotspot items (default 80%)
- Creates high contention scenarios

## Performance Tuning Tips

1. **For Maximum Throughput**
   - Use `tps 0` (unlimited)
   - Increase concurrent clients
   - Use intra-shard transactions only
   - Use uniform distribution

2. **For Realistic Testing**
   - Set target TPS to expected production load
   - Use mixed workload (intra + cross-shard + read-only)
   - Use Zipf distribution
   - Include warmup phase

3. **For Stress Testing**
   - High concurrent clients (50-100)
   - High target TPS
   - Include cross-shard transactions
   - Use hotspot distribution

4. **For 2PC Performance**
   - Focus on cross-shard transactions (80%+)
   - Moderate concurrent clients
   - Monitor p99 latencies

## Interpreting Results

### Key Metrics

- **Throughput (TPS)**: Transactions per second achieved
- **Success Rate**: Percentage of successful transactions
- **Average Latency**: Mean transaction latency
- **p50/p95/p99**: Percentile latencies (p99 is critical for SLAs)
- **Per-Type Metrics**: Compare intra-shard vs cross-shard performance

### Expected Behavior

- Intra-shard transactions: 10-30ms typical
- Cross-shard transactions: 40-100ms typical (due to 2PC)
- Read-only queries: 5-15ms typical
- Higher concurrency → higher throughput but higher latencies
- Hotspot distribution → increased contention → higher p99 latencies

## Troubleshooting

### "Failed to connect to node"
- Ensure all nodes are running: `./scripts/start_nodes.sh`
- Check node addresses in `-nodes` flag

### Low Throughput
- Check node CPU/memory usage
- Increase concurrent clients
- Remove TPS limit (`-tps 0`)
- Check for errors in node logs

### High Failure Rate
- Check node logs for errors
- Reduce concurrent load
- Increase RPC timeouts in code

### Inconsistent Results
- Use longer warmup period (`-warmup 15`)
- Run multiple iterations
- Check for background system load

## Architecture

The benchmarking system consists of:

1. **Config**: Define benchmark parameters
2. **Workload Generator**: Generate transactions according to distribution
3. **Benchmark Runner**: Execute transactions with concurrent clients
4. **Statistics Collector**: Track latencies and success rates
5. **Rate Limiter**: Control TPS to target value
6. **Reporter**: Real-time and final reporting

## File Structure

```
internal/benchmark/
├── config.go      # Configuration and presets
├── workload.go    # Workload generation (uniform, zipf, hotspot)
├── runner.go      # Main benchmark runner and statistics

cmd/benchmark/
└── main.go        # CLI entry point

scripts/
└── run_benchmark.sh  # Helper script for common benchmarks
```

## Advanced Usage

### Custom Node Addresses

```bash
export BENCHMARK_NODES="node1:50051,node2:50051,node3:50051"
./scripts/run_benchmark.sh default
```

Or directly:

```bash
./bin/benchmark -nodes "node1:50051,node2:50051" -transactions 1000
```

### Duration-Based Testing

Run for fixed duration instead of transaction count:

```bash
./bin/benchmark -duration 60 -tps 1000 -clients 20
```

### Continuous Monitoring

Run with progress reports for long tests:

```bash
./bin/benchmark \
  -transactions 100000 \
  -clients 50 \
  -report 10 \
  -detailed
```

## Best Practices

1. **Always use warmup** for accurate measurements
2. **Run multiple iterations** and average results
3. **Export to CSV** for detailed analysis
4. **Monitor node logs** during benchmarks
5. **Test incrementally** - start small, scale up
6. **Document your tests** - record parameters and results

## Contributing

To add new benchmark presets:

1. Edit `internal/benchmark/config.go`
2. Add preset function (e.g., `MyCustomConfig()`)
3. Update CLI in `cmd/benchmark/main.go`
4. Add to helper script `scripts/run_benchmark.sh`

## References

- YCSB: Yahoo! Cloud Serving Benchmark
- TPC-C: Transaction Processing Council Benchmark
- SmallBank: Banking application benchmark

---

**Ready to benchmark!** Start with `./scripts/run_benchmark.sh quick` and scale up from there.
