# Benchmark Examples & Use Cases

## Table of Contents
1. [Performance Testing](#performance-testing)
2. [Scalability Testing](#scalability-testing)
3. [2PC Performance Analysis](#2pc-performance-analysis)
4. [Contention Analysis](#contention-analysis)
5. [Comparison Studies](#comparison-studies)

---

## Performance Testing

### Example 1: Baseline Performance
Establish baseline performance with simple intra-shard transactions:

```bash
./bin/benchmark \
  -transactions 10000 \
  -clients 20 \
  -cross-shard 0 \
  -read-only 0 \
  -distribution uniform \
  -warmup 5 \
  -detailed \
  -csv \
  -output results/baseline.csv
```

**Expected Results:**
- Throughput: 500-2000 TPS
- Avg Latency: 10-30ms
- p99 Latency: 40-80ms

### Example 2: Peak Load Test
Find the maximum sustainable throughput:

```bash
./bin/benchmark \
  -transactions 50000 \
  -tps 0 \
  -clients 100 \
  -cross-shard 0 \
  -read-only 0 \
  -distribution uniform \
  -warmup 10 \
  -report 5 \
  -detailed \
  -csv \
  -output results/peak_load.csv
```

**What to observe:**
- Maximum TPS achieved
- When latencies start to increase significantly
- Success rate (should stay >99%)

---

## Scalability Testing

### Example 3: Client Scaling Study
Test how throughput scales with number of clients:

```bash
# Run with different client counts
for clients in 10 20 30 40 50 60 70 80 90 100; do
  ./bin/benchmark \
    -transactions 10000 \
    -clients $clients \
    -cross-shard 20 \
    -read-only 10 \
    -distribution uniform \
    -warmup 5 \
    -detailed \
    -csv \
    -output "results/scaling_clients_${clients}.csv"
  
  sleep 5  # Cool down between runs
done
```

**Analysis:**
- Plot: Clients (x-axis) vs Throughput (y-axis)
- Identify optimal client count
- Check when adding clients stops improving throughput

### Example 4: Load Ramp Test
Gradually increase load to find breaking point:

```bash
# Ramp from 100 to 5000 TPS
for tps in 100 500 1000 2000 3000 4000 5000; do
  ./bin/benchmark \
    -transactions 10000 \
    -tps $tps \
    -clients 50 \
    -cross-shard 25 \
    -read-only 10 \
    -distribution uniform \
    -warmup 5 \
    -detailed \
    -csv \
    -output "results/ramp_${tps}tps.csv"
  
  sleep 10
done
```

---

## 2PC Performance Analysis

### Example 5: Cross-Shard Percentage Study
Compare performance as cross-shard % increases:

```bash
# Test different cross-shard percentages
for cs_pct in 0 10 20 30 40 50 60 70 80 90 100; do
  ./bin/benchmark \
    -transactions 10000 \
    -clients 30 \
    -cross-shard $cs_pct \
    -read-only 0 \
    -distribution uniform \
    -warmup 5 \
    -detailed \
    -csv \
    -output "results/crossshard_${cs_pct}pct.csv"
  
  sleep 5
done
```

**Analysis:**
- Compare intra-shard vs cross-shard latencies
- Observe 2PC overhead
- Check if 2PC causes throughput degradation

### Example 6: Pure 2PC Test
Focus exclusively on cross-shard transactions:

```bash
./bin/benchmark \
  -transactions 15000 \
  -tps 500 \
  -clients 25 \
  -cross-shard 100 \
  -read-only 0 \
  -distribution uniform \
  -warmup 10 \
  -report 5 \
  -detailed \
  -csv \
  -output results/pure_2pc.csv
```

**Expected Results:**
- Higher latencies than intra-shard
- p99 latency: 80-150ms
- Throughput: 200-800 TPS

---

## Contention Analysis

### Example 7: Distribution Comparison
Compare uniform vs Zipf vs hotspot:

```bash
# Uniform (baseline)
./bin/benchmark \
  -transactions 10000 \
  -clients 40 \
  -cross-shard 30 \
  -read-only 10 \
  -distribution uniform \
  -warmup 5 \
  -detailed \
  -csv \
  -output results/dist_uniform.csv

sleep 5

# Zipf (realistic)
./bin/benchmark \
  -transactions 10000 \
  -clients 40 \
  -cross-shard 30 \
  -read-only 10 \
  -distribution zipf \
  -warmup 5 \
  -detailed \
  -csv \
  -output results/dist_zipf.csv

sleep 5

# Hotspot (high contention)
./bin/benchmark \
  -transactions 10000 \
  -clients 40 \
  -cross-shard 30 \
  -read-only 10 \
  -distribution hotspot \
  -warmup 5 \
  -detailed \
  -csv \
  -output results/dist_hotspot.csv
```

**Analysis:**
- Uniform: Lowest contention, best throughput
- Zipf: Moderate contention, realistic
- Hotspot: High contention, worst p99 latencies

### Example 8: Hotspot Stress Test
Test system under extreme contention:

```bash
./bin/benchmark \
  -transactions 20000 \
  -clients 80 \
  -cross-shard 40 \
  -read-only 5 \
  -distribution hotspot \
  -warmup 10 \
  -report 5 \
  -detailed \
  -csv \
  -output results/hotspot_stress.csv
```

**What to observe:**
- Significantly higher p99 latencies
- Potential deadlocks or aborts
- Throughput degradation

---

## Comparison Studies

### Example 9: Read-Write Mix Comparison
Test different read-write ratios:

```bash
for ro_pct in 0 10 20 30 40 50; do
  ./bin/benchmark \
    -transactions 10000 \
    -clients 30 \
    -cross-shard 20 \
    -read-only $ro_pct \
    -distribution uniform \
    -warmup 5 \
    -detailed \
    -csv \
    -output "results/readonly_${ro_pct}pct.csv"
  
  sleep 5
done
```

**Analysis:**
- Read-only queries should be fastest
- More reads → higher overall throughput
- Lower contention with more reads

### Example 10: End-to-End Comprehensive Test
Full system evaluation:

```bash
# Create comprehensive test suite
./bin/benchmark \
  -transactions 50000 \
  -tps 2000 \
  -clients 60 \
  -cross-shard 35 \
  -read-only 15 \
  -distribution zipf \
  -warmup 15 \
  -report 10 \
  -detailed \
  -csv \
  -output results/comprehensive_test.csv
```

**This tests:**
- Mixed workload (50% intra, 35% cross, 15% read)
- Realistic access pattern (Zipf)
- Moderate load (2000 TPS target)
- Sufficient warmup
- Real-time monitoring

---

## Automated Test Suite

### Complete Benchmark Suite Script

Create `scripts/run_full_suite.sh`:

```bash
#!/bin/bash

echo "Starting comprehensive benchmark suite..."
mkdir -p results/suite_$(date +%Y%m%d_%H%M%S)
OUTDIR="results/suite_$(date +%Y%m%d_%H%M%S)"

# Test 1: Baseline
echo "Test 1/10: Baseline Performance"
./bin/benchmark -transactions 10000 -clients 20 -cross-shard 0 \
  -read-only 0 -detailed -csv -output "$OUTDIR/01_baseline.csv"
sleep 5

# Test 2: Mixed Workload
echo "Test 2/10: Mixed Workload"
./bin/benchmark -transactions 10000 -clients 30 -cross-shard 30 \
  -read-only 10 -detailed -csv -output "$OUTDIR/02_mixed.csv"
sleep 5

# Test 3: 2PC Heavy
echo "Test 3/10: 2PC Heavy"
./bin/benchmark -transactions 10000 -clients 25 -cross-shard 80 \
  -read-only 0 -detailed -csv -output "$OUTDIR/03_2pc_heavy.csv"
sleep 5

# Test 4: Read Heavy
echo "Test 4/10: Read Heavy"
./bin/benchmark -transactions 10000 -clients 40 -cross-shard 10 \
  -read-only 50 -detailed -csv -output "$OUTDIR/04_read_heavy.csv"
sleep 5

# Test 5: Zipf Distribution
echo "Test 5/10: Zipf Distribution"
./bin/benchmark -transactions 10000 -clients 30 -cross-shard 30 \
  -read-only 10 -distribution zipf -detailed -csv -output "$OUTDIR/05_zipf.csv"
sleep 5

# Test 6: Hotspot
echo "Test 6/10: Hotspot"
./bin/benchmark -transactions 10000 -clients 30 -cross-shard 30 \
  -read-only 10 -distribution hotspot -detailed -csv -output "$OUTDIR/06_hotspot.csv"
sleep 5

# Test 7: High Throughput
echo "Test 7/10: High Throughput"
./bin/benchmark -transactions 50000 -tps 0 -clients 50 -cross-shard 0 \
  -read-only 0 -detailed -csv -output "$OUTDIR/07_high_throughput.csv"
sleep 5

# Test 8: Stress Test
echo "Test 8/10: Stress Test"
./bin/benchmark -transactions 30000 -tps 3000 -clients 80 -cross-shard 30 \
  -read-only 20 -detailed -csv -output "$OUTDIR/08_stress.csv"
sleep 5

# Test 9: Low Concurrency
echo "Test 9/10: Low Concurrency"
./bin/benchmark -transactions 5000 -clients 5 -cross-shard 25 \
  -read-only 10 -detailed -csv -output "$OUTDIR/09_low_concurrency.csv"
sleep 5

# Test 10: High Concurrency
echo "Test 10/10: High Concurrency"
./bin/benchmark -transactions 20000 -clients 100 -cross-shard 25 \
  -read-only 10 -detailed -csv -output "$OUTDIR/10_high_concurrency.csv"

echo "Suite complete! Results in: $OUTDIR"
```

---

## Interpreting Results

### Performance Metrics to Track

1. **Throughput (TPS)**
   - Baseline: 1000-2000 TPS (intra-shard only)
   - Mixed: 500-1500 TPS
   - 2PC Heavy: 200-800 TPS

2. **Latency (ms)**
   - Intra-shard avg: 10-30ms
   - Cross-shard avg: 40-100ms
   - Read-only avg: 5-15ms

3. **Success Rate**
   - Target: >99%
   - <95% indicates problems

4. **Percentile Latencies**
   - p50: Typical user experience
   - p95: Most users
   - p99: SLA compliance (critical)
   - p99.9: Worst case

### Red Flags

- ⚠️ Success rate <99%
- ⚠️ p99 latency >500ms
- ⚠️ Throughput declining over time
- ⚠️ High variance between runs
- ⚠️ Memory/CPU trending upward

---

## Next Steps After Benchmarking

1. **Identify Bottlenecks**
   - Profile the slowest operations
   - Check which transaction type is slowest
   - Monitor system resources

2. **Optimize**
   - Tune Paxos parameters
   - Optimize 2PC protocol
   - Improve data structures

3. **Validate**
   - Re-run benchmarks
   - Compare before/after
   - Ensure no regressions

4. **Document**
   - Record baseline metrics
   - Track optimization improvements
   - Set performance targets

---

**Remember**: Always run benchmarks multiple times and average the results for accuracy!
