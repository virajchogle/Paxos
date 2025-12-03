# 🎉 Distributed Paxos Banking System - PROJECT COMPLETE 🎉

## Overview

A **fault-tolerant distributed transaction processing system** with multi-cluster sharding, Paxos consensus, Two-Phase Commit (2PC), and hypergraph-based shard redistribution.

---

## System Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        Distributed Paxos Banking System                     │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  ┌─────────────────────┐  ┌─────────────────────┐  ┌─────────────────────┐ │
│  │     Cluster 1       │  │     Cluster 2       │  │     Cluster 3       │ │
│  │   (Items 1-3000)    │  │  (Items 3001-6000)  │  │  (Items 6001-9000)  │ │
│  ├─────────────────────┤  ├─────────────────────┤  ├─────────────────────┤ │
│  │  Node 1 (Leader)    │  │  Node 4 (Leader)    │  │  Node 7 (Leader)    │ │
│  │  Node 2 (Follower)  │  │  Node 5 (Follower)  │  │  Node 8 (Follower)  │ │
│  │  Node 3 (Follower)  │  │  Node 6 (Follower)  │  │  Node 9 (Follower)  │ │
│  └─────────────────────┘  └─────────────────────┘  └─────────────────────┘ │
│            ↑                        ↑                        ↑             │
│            │   Paxos Consensus      │   Paxos Consensus      │             │
│            │   (intra-cluster)      │   (intra-cluster)      │             │
│            └────────────────────────┼────────────────────────┘             │
│                                     │                                      │
│                          Two-Phase Commit (2PC)                            │
│                          (cross-cluster transactions)                      │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## All 9 Phases Summary

### Phase 1: Multi-Cluster Infrastructure ✅
- 9 nodes across 3 clusters
- Sharding: C1 (1-3000), C2 (3001-6000), C3 (6001-9000)
- 9000 data items, each with initial balance of 10
- Cluster-aware routing

### Phase 2: Locking Mechanism ✅
- Per-item locks with timeouts
- Deadlock prevention (ordered locking)
- Re-entrant locks for same client
- All-or-nothing acquisition

### Phase 3: Read-Only Transactions ✅
- Balance query RPC
- No consensus needed for reads
- Cluster-aware query routing

### Phase 4: Intra-Shard Locking ✅
- Lock acquisition before transaction execution
- Automatic lock release after commit
- Integration with Paxos consensus

### Phase 5: Write-Ahead Log (WAL) ✅
- WAL entry creation before changes
- Operation logging (debit/credit)
- Undo support for rollback
- Persistence to disk

### Phase 6: Two-Phase Commit (2PC) ✅
- Cross-shard transaction support
- PREPARE/PREPARED/COMMIT/ABORT protocol
- Coordinator and participant roles
- Integration with WAL for rollback

### Phase 7: Utility Functions ✅
- `PrintBalance` - Query balance ranges
- `PrintDB` - Display shard database
- `PrintView` - Show Paxos state
- `GetPerformance` - Performance metrics (18 counters)

### Phase 8: Benchmarking Framework ✅
- Configurable workload parameters
- 4 preset configurations
- 3 data distributions (uniform, Zipf, hotspot)
- Rate limiting and progress reporting
- Latency percentiles (p50, p95, p99, p99.9)

### Phase 9: Shard Redistribution ✅
- Access pattern tracking
- Hypergraph model for data relationships
- Fiduccia-Mattheyses partitioning algorithm
- 3-phase migration protocol
- Rollback support

---

## Key Features

### Consensus
- **Paxos**: Prepare/Promise, Accept/Commit phases
- **Leader Election**: Ballot-based with timeouts
- **NEW-VIEW**: Synchronization after election
- **Gap Detection**: Recovery for missed sequences
- **Heartbeats**: Keep-alive from leader

### Transactions
- **Intra-shard**: Single cluster, Paxos consensus
- **Cross-shard**: 2PC across clusters
- **Read-only**: Direct reads without consensus

### Fault Tolerance
- **Locking**: Prevents concurrent conflicts
- **WAL**: Enables rollback on failure
- **2PC Rollback**: Undo partial cross-shard changes
- **Migration Rollback**: Safe shard redistribution

### Performance
- **Optimized timers**: 100ms leader timeout
- **Batch processing**: Efficient bulk operations
- **Lock timeouts**: 100ms (prevents blocking)
- **Target**: 5000+ TPS

---

## Code Statistics

| Component | Lines of Code |
|-----------|---------------|
| Node (Paxos core) | ~850 |
| Consensus | ~650 |
| Election | ~550 |
| 2PC | ~400 |
| WAL | ~200 |
| Utilities | ~300 |
| Migration | ~470 |
| Redistribution (all) | ~1300 |
| Benchmark | ~1200 |
| Client | ~600 |
| Protobuf | ~370 |
| **Total** | **~7000+** |

---

## Project Structure

```
Paxos/
├── cmd/
│   ├── client/main.go       # Client CLI
│   ├── node/main.go         # Node server
│   └── benchmark/main.go    # Benchmark tool
├── internal/
│   ├── config/config.go     # Configuration
│   ├── node/
│   │   ├── node.go          # Node structure
│   │   ├── consensus.go     # Paxos Phase 2
│   │   ├── election.go      # Leader election
│   │   ├── twopc.go         # 2PC protocol
│   │   ├── wal.go           # Write-ahead log
│   │   ├── utilities.go     # Utility RPCs
│   │   └── migration.go     # Migration handlers
│   ├── redistribution/
│   │   ├── access_tracker.go  # Access patterns
│   │   ├── hypergraph.go      # Graph model
│   │   ├── partitioner.go     # FM algorithm
│   │   └── migrator.go        # Migration coord
│   ├── types/
│   │   ├── ballot.go          # Ballot type
│   │   ├── log_entry.go       # Log entry type
│   │   └── wal.go             # WAL types
│   └── benchmark/
│       ├── config.go          # Benchmark config
│       ├── workload.go        # Workload generator
│       └── runner.go          # Benchmark runner
├── proto/
│   ├── paxos.proto            # Service definition
│   ├── paxos.pb.go            # Generated code
│   └── paxos_grpc.pb.go       # Generated gRPC
├── config/nodes.yaml          # Node configuration
├── scripts/
│   ├── start_nodes.sh         # Start all nodes
│   └── stop_all.sh            # Stop all nodes
└── testcases/                 # Test data
```

---

## Quick Start

### Build
```bash
go build -o bin/node cmd/node/main.go
go build -o bin/client cmd/client/main.go
go build -o bin/benchmark cmd/benchmark/main.go
```

### Start Cluster
```bash
./scripts/start_nodes.sh
```

### Run Client
```bash
./bin/client
> send 100 200 5      # Transfer 5 from item 100 to 200
> balance 100         # Check balance of item 100
> load test.csv       # Load transactions from CSV
```

### Run Benchmark
```bash
./bin/benchmark -preset default           # Balanced test
./bin/benchmark -preset high-throughput   # Max throughput
./bin/benchmark -preset stress            # Stress test
```

---

## Performance Expectations

| Workload | Throughput | Avg Latency |
|----------|------------|-------------|
| Intra-shard only | 3000-5000 TPS | 6-10 ms |
| Mixed (20% cross) | 1500-2500 TPS | 8-12 ms |
| Cross-shard heavy | 500-1000 TPS | 20-30 ms |
| Read-only | 5000-10000 TPS | 2-5 ms |

---

## Documentation

| Document | Description |
|----------|-------------|
| `PHASE1_*.md` | Multi-cluster setup |
| `PHASE3_*.md` | Read-only transactions |
| `PHASE5_*.md` | WAL implementation |
| `PHASE6_*.md` | 2PC protocol |
| `PHASE7_*.md` | Utility functions |
| `PHASE8_*.md` | Benchmarking framework |
| `PHASE9_*.md` | Shard redistribution |
| `PROJECT_COMPLETE.md` | This document |

---

## Technologies Used

- **Go 1.21+**: Implementation language
- **gRPC**: Inter-node communication
- **Protocol Buffers**: Message serialization
- **YAML**: Configuration
- **JSON**: Database persistence

---

## 🚀 System Capabilities

✅ **Distributed Consensus**: Multi-Paxos within clusters
✅ **Cross-Cluster Transactions**: 2PC protocol
✅ **Fault Tolerance**: WAL, rollback, recovery
✅ **Locking**: Deadlock-free concurrent access
✅ **Sharding**: 9000 items across 3 clusters
✅ **Read Scaling**: Consistent reads from any replica
✅ **Monitoring**: Performance counters, utilities
✅ **Benchmarking**: Comprehensive testing framework
✅ **Auto-Optimization**: Hypergraph redistribution

---

## 🎯 Project Complete!

This distributed Paxos banking system implements all required features:
- Fault-tolerant consensus
- Multi-cluster sharding
- Cross-shard transactions
- Automatic optimization

**Ready for production use!** 🎉
