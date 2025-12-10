# Paxos Banking System

A distributed banking system implementing Multi-Paxos consensus with Two-Phase Commit (2PC) for cross-shard transactions.

## Features

- **Multi-Paxos Consensus**: Replicated state machine across 3 clusters (9 nodes)
- **Two-Phase Commit**: Cross-shard transaction support with 2PC protocol
- **Write-Ahead Log**: Durability using PebbleDB
- **Leader Election**: Automatic election with priority and heartbeats
- **Checkpointing**: Configurable checkpoint intervals for recovery
- **Shard Redistribution**: Graph partitioning for optimal data placement
- **Comprehensive Benchmarking**: Configurable workloads with skewness support

## Architecture

```
Cluster 1 (Items 1-3000):      Nodes 1, 2, 3    (Leader: n1)
Cluster 2 (Items 3001-6000):   Nodes 4, 5, 6    (Leader: n4)
Cluster 3 (Items 6001-9000):   Nodes 7, 8, 9    (Leader: n7)
```

## Quick Start

### Prerequisites
- Go 1.21+
- Ports 50051-50059 available

### Build
```bash
./scripts/build.sh
```

### Start Nodes
```bash
# For testing (initial balance = 10)
./scripts/start_nodes.sh

# For benchmarking (initial balance = 100000)
INITIAL_BALANCE=100000 ./scripts/start_nodes.sh
```

### Run Client
```bash
./bin/client -testfile testcases/official_tests_converted.csv
```

## Client Commands

| Command | Description |
|---------|-------------|
| `next` | Process next test set |
| `send <s> <r> <amt>` | Send transaction |
| `balance <id>` | Query balance (single node) |
| `printbalance <id>` | PrintBalance - all nodes in cluster |
| `printdb` | PrintDB - modified items on all 9 nodes |
| `printview` | PrintView - NEW-VIEW messages |
| `printreshard` | PrintReshard - resharding triplets |
| `performance` | Get performance metrics |
| `flush` | Reset system state |

## Benchmarking

```bash
# Quick test
./scripts/run_benchmark.sh quick

# Custom benchmark with skewness
./bin/benchmark \
  -transactions 10000 \
  -clients 20 \
  -cross-shard 30 \
  -read-only 10 \
  -distribution zipf \
  -skewness 0.8 \
  -detailed

# Available presets
./scripts/run_benchmark.sh default|high-throughput|cross-shard|stress|all
```

### Benchmark Parameters

| Parameter | Description |
|-----------|-------------|
| `-transactions N` | Total transactions |
| `-tps N` | Target TPS (0=unlimited) |
| `-clients N` | Concurrent clients |
| `-cross-shard N` | % cross-shard (0-100) |
| `-read-only N` | % read-only (0-100) |
| `-distribution` | uniform, zipf, hotspot |
| `-skewness N` | Zipf skewness (0-1) |
| `-detailed` | Show percentiles |
| `-csv` | Export to CSV |

## Configuration

### Initial Balance
- **Default**: 10 (per project spec)
- **Override**: Set `INITIAL_BALANCE` environment variable

### Checkpoint Interval
Configure in `config/nodes.yaml`:
```yaml
data:
  checkpoint_interval: 100  # Checkpoint every 100 transactions
```

### Configurable Clusters (Bonus Feature)

The system supports configurable number of clusters and nodes per cluster. The **9000 data items** (per project spec) are automatically divided among the clusters. Use the `configgen` tool to generate custom configurations:

```bash
# Generate config for 4 clusters with 5 nodes each (20 total nodes, 2250 items/cluster)
./bin/configgen -clusters=4 -nodes-per-cluster=5

# Generate config for 5 clusters with 3 nodes each (15 total nodes, 1800 items/cluster)
./bin/configgen -clusters=5 -nodes-per-cluster=3

# Start nodes with custom configuration
CONFIG_FILE=config/nodes.yaml ./scripts/start_nodes.sh
```

**configgen Parameters:**

| Parameter | Default | Description |
|-----------|---------|-------------|
| `-clusters` | 3 | Number of clusters |
| `-nodes-per-cluster` | 3 | Nodes per cluster |
| `-items` | 9000 | Total data items (per project spec) |
| `-base-port` | 50051 | Starting port number |
| `-initial-balance` | 10 | Initial balance per item |
| `-output` | config/nodes.yaml | Output file path |

**Example Configurations:**

```bash
# 4 clusters × 5 nodes = 20 nodes (9000 items ÷ 4 = 2250 items/cluster)
./bin/configgen -clusters=4 -nodes-per-cluster=5
# Cluster 1: Nodes 1-5,   Items 1-2250
# Cluster 2: Nodes 6-10,  Items 2251-4500
# Cluster 3: Nodes 11-15, Items 4501-6750
# Cluster 4: Nodes 16-20, Items 6751-9000

# 2 clusters × 4 nodes = 8 nodes (9000 items ÷ 2 = 4500 items/cluster)
./bin/configgen -clusters=2 -nodes-per-cluster=4

# 6 clusters × 2 nodes = 12 nodes (9000 items ÷ 6 = 1500 items/cluster)
./bin/configgen -clusters=6 -nodes-per-cluster=2
```

The generated configuration automatically:
- Divides 9000 data items evenly across clusters
- Assigns consecutive node IDs starting from 1
- Creates proper shard ranges for each cluster
- First node in each cluster becomes the expected leader

## Project Structure

```
Paxos/
├── cmd/
│   ├── node/         # Node server
│   ├── client/       # Interactive client
│   └── benchmark/    # Benchmark tool
├── internal/
│   ├── node/         # Node implementation
│   ├── benchmark/    # Benchmark suite
│   ├── redistribution/ # Shard redistribution
│   └── types/        # Shared types
├── proto/            # Protocol buffers
├── scripts/          # Helper scripts
├── testcases/        # Test CSV files
└── config/           # Configuration
```

## Test Format

```csv
Set,Commands,LiveNodes
1,"(21,700,2)","[n1,n2,n3,n4,n5,n7,n9]"
1,"F(n3)",
1,"(3001,4650,2)",
2,"(7800)",
2,"R(n6)",
```

## Protocol Details

### Intra-Shard (Multi-Paxos)
1. Client → Leader: Transaction
2. Leader → Acceptors: Accept
3. Acceptors → Leader: Accepted
4. Leader commits and executes

### Cross-Shard (2PC + Paxos)
1. **Prepare**: Coordinator sends PREPARE to participant
2. Both clusters run Paxos for prepare phase
3. **Commit/Abort**: Based on participant response
4. Both clusters run Paxos for commit phase

## Scripts

| Script | Description |
|--------|-------------|
| `scripts/build.sh` | Build all binaries |
| `scripts/start_nodes.sh` | Start all 9 nodes |
| `scripts/stop_all.sh` | Stop all nodes |
| `scripts/run_benchmark.sh` | Run benchmarks |

## Troubleshooting

- **Nodes won't start**: Check ports 50051-50059, remove `data/` and `logs/`
- **Transactions failing**: Check `logs/nodeN.log` for errors
- **Low performance**: Use `INITIAL_BALANCE=100000` for benchmarks
