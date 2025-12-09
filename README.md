# Paxos Banking System

A distributed banking system implementing the Paxos consensus protocol with support for cross-shard transactions using Two-Phase Commit (2PC).

## Features

### Core Features
- ✅ **Paxos Consensus**: Multi-Paxos implementation for replicated state machines
- ✅ **Sharding**: Data partitioned across 3 clusters (9 nodes total)
- ✅ **Two-Phase Commit**: Cross-shard transaction support with 2PC protocol
- ✅ **Write-Ahead Log (WAL)**: Durability using PebbleDB
- ✅ **Leader Election**: Automatic leader election with priority and heartbeats
- ✅ **Banking Operations**: Account balances and transfers

### Performance Features
- ✅ **Parallel Processing**: Concurrent transaction processing
- ✅ **Client-Side Retry**: Automatic retry with queuing for failed transactions
- ✅ **Election Triggering**: Proactive leader election on cluster changes
- ✅ **Comprehensive Benchmarking**: Full performance testing suite

## Architecture

### Cluster Layout
```
Cluster 1 (Accounts 1-3000):      Nodes 1, 2, 3
Cluster 2 (Accounts 3001-6000):   Nodes 4, 5, 6
Cluster 3 (Accounts 6001-9000):   Nodes 7, 8, 9
```

### Transaction Types
- **Intra-shard**: Both sender and receiver in same cluster (single Paxos)
- **Cross-shard**: Sender and receiver in different clusters (2PC + Paxos)
- **Read-only**: Balance queries (no state change)

## Quick Start

### Prerequisites
- Go 1.21 or later
- Ports 50051-50059 available

### 1. Build
```bash
# Build all binaries
./scripts/build.sh

# Or build individually
go build -o bin/node cmd/node/main.go
go build -o bin/client cmd/client/main.go
go build -o bin/benchmark cmd/benchmark/main.go
```

### 2. Start Nodes
```bash
# Start all 9 nodes
./scripts/start_nodes.sh

# Or start individual node
./bin/node -id 1 -port 50051
```

### 3. Run Client
```bash
./bin/client
```

The interactive client supports:
- `S(sender, receiver, amount)` - Submit transaction
- `B(account)` - Query balance
- `F(nodeID)` - Fail/unfail node
- `next` - Process next test set
- `concurrency N` - Set parallel processing limit

### 4. Run Tests
```bash
# Load test file
./bin/client testcases/official_tests_converted.csv
```

## Benchmarking

### Overview

The system includes a comprehensive benchmarking suite that supports:

1. **Read-only vs Read-write Mix**: Configure percentage of queries vs transactions
2. **Intra-shard vs Cross-shard Mix**: Configure single-cluster vs 2PC transactions  
3. **Data Distributions**: Uniform, Zipf (realistic), or Hotspot (contention)

### Quick Benchmark

```bash
# Quick test (1000 transactions)
./scripts/run_benchmark.sh quick

# Default benchmark (10K transactions, balanced mix)
./scripts/run_benchmark.sh default

# High throughput test (50K transactions, unlimited TPS)
./scripts/run_benchmark.sh high-throughput

# Cross-shard heavy (80% cross-shard, 2PC testing)
./scripts/run_benchmark.sh cross-shard

# Stress test (100K transactions, 5000 TPS target)
./scripts/run_benchmark.sh stress

# All presets
./scripts/run_benchmark.sh all
```

### Custom Benchmark

```bash
./bin/benchmark \
  -transactions 20000 \
  -tps 1000 \
  -clients 30 \
  -cross-shard 30 \
  -read-only 10 \
  -distribution zipf \
  -warmup 10 \
  -detailed \
  -csv \
  -output results/my_test.csv
```

### Benchmark Parameters

| Parameter | Description | Values |
|-----------|-------------|--------|
| `-transactions` | Total transactions to execute | 1+ |
| `-tps` | Target TPS (0=unlimited) | 0+ |
| `-clients` | Concurrent clients | 1+ |
| `-cross-shard` | % cross-shard transactions | 0-100 |
| `-read-only` | % read-only queries | 0-100 |
| `-distribution` | Data access pattern | uniform, zipf, hotspot |
| `-warmup` | Warmup duration (seconds) | 0+ |
| `-detailed` | Show percentile statistics | flag |
| `-csv` | Export results to CSV | flag |
| `-output` | CSV file path | path |

### Benchmark Documentation

For complete benchmarking documentation, see:
- **[BENCHMARKING_COMPLETE.md](BENCHMARKING_COMPLETE.md)** - Implementation summary & quick start
- **[BENCHMARKING_GUIDE.md](BENCHMARKING_GUIDE.md)** - Complete guide (10+ pages)
- **[BENCHMARK_QUICK_REFERENCE.md](BENCHMARK_QUICK_REFERENCE.md)** - Command cheat sheet
- **[BENCHMARKING_EXAMPLES.md](BENCHMARKING_EXAMPLES.md)** - Detailed examples & use cases

## Project Structure

```
Paxos/
├── cmd/
│   ├── node/         # Node server
│   ├── client/       # Interactive client
│   └── benchmark/    # Benchmark tool
│
├── internal/
│   ├── node/         # Node implementation
│   │   ├── consensus.go     # Paxos consensus
│   │   ├── election.go      # Leader election
│   │   ├── twopc.go         # Two-Phase Commit
│   │   ├── wal.go           # Write-Ahead Log
│   │   └── node.go          # Node server
│   ├── client/       # Client logic
│   ├── config/       # Configuration
│   ├── types/        # Shared types
│   └── benchmark/    # Benchmark suite
│       ├── config.go        # Benchmark configuration
│       ├── workload.go      # Workload generation
│       └── runner.go        # Benchmark execution
│
├── proto/            # Protocol buffers
├── scripts/          # Helper scripts
├── testcases/        # Test CSV files
├── config/           # Node configuration
├── bin/              # Compiled binaries
├── results/          # Benchmark results
└── logs/             # Node logs
```

## Protocol Details

### Paxos (Intra-shard)
1. Client sends transaction to cluster leader
2. Leader proposes value using Multi-Paxos
3. Acceptors vote on proposal
4. Once majority accepts, value is committed
5. Leader responds to client

### Two-Phase Commit (Cross-shard)
1. **Prepare Phase**:
   - Coordinator sends PREPARE to all participants
   - Each participant runs Paxos to commit transaction locally
   - Participants respond PREPARED or ABORT

2. **Commit Phase**:
   - If all PREPARED: Coordinator sends COMMIT
   - If any ABORT: Coordinator sends ABORT
   - Participants apply decision and respond

### Leader Election
- Timeout-based election (100-250ms with jitter)
- Priority nodes (1, 4, 7) have shorter timeouts
- NEW-VIEW messages establish leadership
- Heartbeats maintain leadership

## Performance

### Typical Performance
- **Intra-shard throughput**: 1000-2000 TPS
- **Cross-shard throughput**: 200-800 TPS
- **Intra-shard latency**: 10-30ms (avg)
- **Cross-shard latency**: 40-100ms (avg)
- **Read-only latency**: 5-15ms (avg)

### Benchmark Results
Run `./scripts/run_benchmark.sh all` for comprehensive performance analysis across:
- Various transaction mixes
- Different data distributions
- Multiple concurrency levels
- Intra-shard vs cross-shard performance

## Configuration

### Node Configuration (`config/nodes.yaml`)
```yaml
clusters:
  - id: 1
    nodes: [1, 2, 3]
    data_range: [1, 3000]
  - id: 2
    nodes: [4, 5, 6]
    data_range: [3001, 6000]
  - id: 3
    nodes: [7, 8, 9]
    data_range: [6001, 9000]

nodes:
  - id: 1
    address: "localhost:50051"
    cluster_id: 1
  # ... (nodes 2-9)
```

### Client Configuration
- **Max Concurrency**: 25 parallel transactions (configurable)
- **Retry Logic**: Automatic retry for failed transactions
- **Election Triggering**: 600ms wait after node failure
- **Unique Timestamps**: Client ID + counter for transaction uniqueness

## Logging

Node logs are written to `logs/nodeN.log`:
```bash
# Monitor specific node
tail -f logs/node1.log

# Search for errors
grep "ERROR" logs/*.log

# Monitor elections
grep "Elected as LEADER" logs/*.log
```

## Testing

### Test Files
Test cases in CSV format:
```csv
S(1,2,100)
B(1)
F(n4)
S(3001,6001,50)
```

### Test Commands
- `S(sender, receiver, amount)` - Submit transaction
- `B(account)` - Balance query
- `F(nX)` - Fail/unfail node X
- `// comment` - Comment line
- Empty lines are ignored

### Official Tests
```bash
./bin/client testcases/official_tests_converted.csv
```

## Scripts

| Script | Description |
|--------|-------------|
| `scripts/build.sh` | Build all binaries |
| `scripts/start_nodes.sh` | Start all 9 nodes |
| `scripts/stop_all.sh` | Stop all nodes |
| `scripts/run_benchmark.sh` | Run benchmarks |

## Troubleshooting

### Nodes won't start
- Check ports 50051-50059 are available
- Check `config/nodes.yaml` is valid
- Remove old data: `rm -rf data/`

### Transactions failing
- Check nodes are running: `ps aux | grep node`
- Check logs: `tail -f logs/*.log`
- Verify leaders elected: `grep "LEADER" logs/*.log`

### Low benchmark performance
- Increase concurrent clients
- Remove TPS limit (`-tps 0`)
- Use intra-shard only (`-cross-shard 0`)
- Check system resources (CPU, memory)

### High latencies
- Check for leader churn in logs
- Reduce concurrent load
- Use uniform distribution (less contention)
- Check network latency

## Development

### Building from Source
```bash
# Install dependencies
go mod download

# Build all
./scripts/build.sh

# Run tests
go test ./...

# Format code
go fmt ./...
```

### Adding New Features
1. Define protocol buffers in `proto/paxos.proto`
2. Generate Go code: `protoc --go_out=. --go-grpc_out=. proto/paxos.proto`
3. Implement in `internal/node/` or `internal/client/`
4. Update tests and documentation

## Architecture Decisions

### Why Multi-Paxos?
- Efficient: Skips prepare phase for established leader
- Proven: Well-studied consensus protocol
- Fault-tolerant: Tolerates f failures in 2f+1 nodes

### Why 2PC for Cross-Shard?
- Atomic: All-or-nothing semantics for distributed transactions
- Simple: Clear commit/abort decision
- Compatible: Works with existing Paxos clusters

### Why PebbleDB?
- Embedded: No separate database server
- Fast: LSM-tree based, optimized for writes
- Reliable: Well-tested (based on RocksDB design)

## Performance Tuning

### For Maximum Throughput
- Increase concurrent clients (50-100)
- Use unlimited TPS (`-tps 0`)
- Focus on intra-shard (`-cross-shard 0`)
- Use uniform distribution

### For Realistic Testing
- Mixed workload (70% intra, 20% cross, 10% read)
- Target production TPS
- Use Zipf distribution
- Include warmup phase

### For 2PC Analysis
- High cross-shard percentage (80%+)
- Moderate clients (20-30)
- Monitor p99 latencies
- Export detailed CSV

## Future Enhancements

Potential areas for improvement:
- [ ] Dynamic resharding (partially implemented)
- [ ] Read optimization (read leases)
- [ ] Snapshot-based recovery
- [ ] Batch commit for higher throughput
- [ ] Multi-version concurrency control
- [ ] Geo-replication support

## References

- Lamport, L. (1998). "The Part-Time Parliament"
- Ongaro, D., & Ousterhout, J. (2014). "In Search of an Understandable Consensus Algorithm"
- Gray, J., & Lamport, L. (2006). "Consensus on Transaction Commit"

## License

Educational project for distributed systems coursework.

## Contributors

- Viraj (Implementation)

---

**Get started**: `./scripts/start_nodes.sh` then `./bin/client`  
**Benchmark**: `./scripts/run_benchmark.sh quick`  
**Documentation**: See `BENCHMARKING_COMPLETE.md` for benchmarking guide
