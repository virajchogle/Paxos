# Benchmark Quick Reference

## 🚀 Quick Start

```bash
# Build
go build -o bin/benchmark cmd/benchmark/main.go

# Run quick test
./scripts/run_benchmark.sh quick
```

## 📊 Common Commands

```bash
# Quick test (1K transactions)
./scripts/run_benchmark.sh quick

# Default balanced workload
./scripts/run_benchmark.sh default

# Max throughput test
./scripts/run_benchmark.sh high-throughput

# Cross-shard heavy (2PC testing)
./scripts/run_benchmark.sh cross-shard

# Stress test
./scripts/run_benchmark.sh stress

# Zipf distribution (realistic)
./scripts/run_benchmark.sh zipf

# Hotspot (high contention)
./scripts/run_benchmark.sh hotspot

# All presets
./scripts/run_benchmark.sh all
```

## 🎯 Key Parameters

| Parameter | Range | Description |
|-----------|-------|-------------|
| `-transactions` | 1+ | Total transactions to execute |
| `-tps` | 0+ | Target TPS (0=unlimited) |
| `-clients` | 1+ | Concurrent clients |
| `-cross-shard` | 0-100 | % cross-shard transactions |
| `-read-only` | 0-100 | % read-only queries |
| `-distribution` | uniform/zipf/hotspot | Data access pattern |
| `-warmup` | 0+ | Warmup seconds |
| `-detailed` | flag | Show percentile stats |
| `-csv` | flag | Export to CSV |
| `-output` | path | CSV output file |

## 💡 Example Scenarios

### Scenario 1: Maximum Throughput
Test how fast the system can go:
```bash
./bin/benchmark -transactions 50000 -tps 0 -clients 50 \
  -cross-shard 0 -read-only 0 -detailed
```

### Scenario 2: Realistic Mixed Load
70% intra, 20% cross, 10% reads:
```bash
./bin/benchmark -transactions 20000 -tps 1000 -clients 30 \
  -cross-shard 20 -read-only 10 -warmup 10 -detailed
```

### Scenario 3: 2PC Performance
Focus on cross-shard:
```bash
./bin/benchmark -transactions 10000 -clients 20 \
  -cross-shard 80 -read-only 0 -detailed -csv
```

### Scenario 4: Contention Test
Hotspot with high concurrency:
```bash
./bin/benchmark -transactions 15000 -clients 50 \
  -cross-shard 30 -distribution hotspot -detailed
```

### Scenario 5: Long-Running Test
Duration-based test:
```bash
./bin/benchmark -duration 120 -tps 2000 -clients 40 \
  -cross-shard 25 -read-only 15 -report 10 -detailed
```

## 📈 Understanding Results

### Typical Latencies
- **Intra-shard**: 10-30ms
- **Cross-shard**: 40-100ms
- **Read-only**: 5-15ms

### Key Metrics
- **Throughput**: Total TPS achieved
- **Success Rate**: Should be >99%
- **p99 Latency**: Critical for SLAs
- **Per-Type**: Compare intra vs cross-shard

### Optimization Tips
- ⬆️ Throughput: More clients, unlimited TPS, intra-shard only
- 📊 Realistic: Target TPS, mixed workload, Zipf distribution
- 🔥 Stress: Many clients, hotspot, cross-shard heavy
- 🎯 2PC: High cross-shard %, moderate clients

## 🔧 Troubleshooting

| Issue | Solution |
|-------|----------|
| Can't connect | Start nodes: `./scripts/start_nodes.sh` |
| Low throughput | ⬆️ clients, remove TPS limit |
| High failures | ⬇️ load, check node logs |
| Inconsistent | Use longer warmup, run multiple times |

## 📁 File Locations

- **Binary**: `bin/benchmark`
- **Results**: `results/*.csv`
- **Code**: `internal/benchmark/` and `cmd/benchmark/`
- **Script**: `scripts/run_benchmark.sh`
- **Guide**: `BENCHMARKING_GUIDE.md`

## 🎨 Distribution Patterns

### Uniform
```bash
-distribution uniform
```
Equal probability for all items. Baseline performance.

### Zipf (Realistic)
```bash
-distribution zipf
```
Power-law: popular items accessed more. Realistic workload.

### Hotspot (Contention)
```bash
-distribution hotspot
```
10% of items get 80% of accesses. Tests contention handling.

## 🚦 Before Running

1. ✅ Start all nodes: `./scripts/start_nodes.sh`
2. ✅ Build benchmark: `go build -o bin/benchmark cmd/benchmark/main.go`
3. ✅ Choose workload based on test goal
4. ✅ Use warmup for accurate results
5. ✅ Export CSV for detailed analysis

## 📞 Custom Nodes

```bash
export BENCHMARK_NODES="host1:50051,host2:50051,host3:50051"
./scripts/run_benchmark.sh default
```

Or:
```bash
./bin/benchmark -nodes "host1:50051,host2:50051" -transactions 1000
```

---

**Pro Tip**: Start with `quick`, then scale up to `default`, then customize!
