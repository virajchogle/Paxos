# Multi-Cluster Paxos Implementation

## Phase 1: Multi-Cluster Infrastructure ✅ COMPLETED

### Overview
The system now supports **9 nodes organized into 3 clusters**, each managing a distinct shard of data.

### Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     CLIENT LAYER                             │
└─────────────────────────────────────────────────────────────┘
                              │
        ┌─────────────────────┼─────────────────────┐
        │                     │                     │
┌───────▼───────┐    ┌────────▼────────┐   ┌───────▼───────┐
│  CLUSTER 1    │    │   CLUSTER 2     │   │  CLUSTER 3    │
│  Nodes 1-3    │    │   Nodes 4-6     │   │  Nodes 7-9    │
│               │    │                 │   │               │
│  Shard:       │    │   Shard:        │   │  Shard:       │
│  Items 1-3000 │    │   Items 3001-   │   │  Items 6001-  │
│               │    │   6000          │   │  9000         │
└───────────────┘    └─────────────────┘   └───────────────┘
```

### Configuration Changes

#### 1. Updated `config/nodes.yaml`
- **3 clusters** with 3 nodes each
- **Fixed shard mapping:**
  - Cluster 1: Data items 1-3000 (nodes 1, 2, 3)
  - Cluster 2: Data items 3001-6000 (nodes 4, 5, 6)  
  - Cluster 3: Data items 6001-9000 (nodes 7, 8, 9)
- Each node knows its cluster assignment
- **9000 data items** with initial balance 10

#### 2. Enhanced Config Structure (`internal/config/config.go`)
New types:
- `ClusterConfig` - defines cluster ID, shard range, and member nodes
- `DataConfig` - specifies total data items and initial balance
- Removed `ClientConfig` (no longer using client IDs like "A", "B", etc.)

New helper functions:
- `GetClusterForNode(nodeID)` - returns cluster ID for a node
- `GetClusterForDataItem(itemID)` - returns which cluster manages a data item
- `GetNodesInCluster(clusterID)` - returns all nodes in a cluster
- `GetPeerNodesInCluster(nodeID, clusterID)` - returns peers within cluster
- `GetLeaderNodeForCluster(clusterID)` - returns expected leader (first node)

#### 3. Updated Node Structure (`internal/node/node.go`)
- Added `clusterID int32` field to identify which cluster the node belongs to
- Changed database from `map[string]int32` (client names) to `map[int32]int32` (data item IDs)
- Each node now only stores data items in its shard (not all 9000)
- Added `allClusterClients` and `allClusterConns` for cross-cluster communication (needed for 2PC)
- Modified `peerClients` to connect only to nodes within the same cluster
- Updated `quorumSize()` to calculate based on cluster size (3 nodes) not total nodes (9)

#### 4. Updated Transaction Model (`proto/paxos.proto`)
- Changed `Transaction` message from:
  ```protobuf
  message Transaction {
    string sender = 1;
    string receiver = 2;
    int32 amount = 3;
  }
  ```
- To:
  ```protobuf
  message Transaction {
    int32 sender = 1;      // Data item ID (1-9000)
    int32 receiver = 2;    // Data item ID (1-9000)
    int32 amount = 3;
  }
  ```

#### 5. Updated Consensus Logic (`internal/node/consensus.go`)
- `executeTransaction()` now works with int32 data item IDs instead of string client IDs
- `PrintDB()` now prints only **modified items** (not all 9000 items)
- Logs show cluster information: `"Node X (Cluster Y)"`

#### 6. Updated Startup Scripts
- `scripts/start_nodes.sh` now starts 9 nodes instead of 5
- Each cluster displayed with its shard range

### Current Capabilities

✅ **What Works:**
- 9 nodes start successfully in 3 clusters
- Each cluster maintains its own shard (1-3000, 3001-6000, 6001-9000)
- Nodes connect to peers within their cluster for consensus
- Nodes connect to all nodes across clusters for future 2PC
- Intra-cluster consensus using existing Multi-Paxos
- Leader election within each cluster
- Database persistence with data item IDs

### Testing the Infrastructure

```bash
# 1. Build the project
./scripts/build.sh

# 2. Start all 9 nodes
./scripts/start_nodes.sh

# 3. Test intra-shard transactions
# Cluster 1: items 1-3000
# Cluster 2: items 3001-6000
# Cluster 3: items 6001-9000

# You should see logs like:
# Node 1 (Cluster 1): Initialized 3000 data items (range 1-3000)
# Node 4 (Cluster 2): Initialized 3000 data items (range 3001-6000)
# Node 7 (Cluster 3): Initialized 3000 data items (range 6001-9000)
```

### Test Case Format
See `testcases/test_multicluster.csv` for example:
```csv
Set Number,Transactions,Live Nodes
1,"(100, 200, 2)","[n1, n2, n3, n4, n5, n6, n7, n8, n9]"
```
- Transaction (100, 200, 2): transfer 2 units from item 100 to item 200
- Both items are in Cluster 1's shard, so this is an **intra-shard transaction**

---

## Next Steps (Phase 2-9)

### Phase 2: Locking Mechanism
- [ ] Add lock table to node structure
- [ ] Implement lock acquisition/release for transactions
- [ ] Handle lock conflicts (skip transaction if locked)

### Phase 3: Read-Only Transactions
- [ ] Implement balance queries (read without consensus)
- [ ] Client retry mechanism on timeout

### Phase 4: Intra-Shard with Locking
- [ ] Modify existing consensus to check locks before Accept
- [ ] Release locks after execution

### Phase 5: Write-Ahead Log (WAL)
- [ ] Add WAL data structure for rollback
- [ ] Implement undo operations

### Phase 6: Two-Phase Commit (2PC) 🔥 CRITICAL
- [ ] Implement coordinator role
- [ ] Implement participant role
- [ ] Add PREPARE/PREPARED/COMMIT/ABORT messages
- [ ] Handle all failure scenarios

### Phase 7: Utility Functions
- [ ] PrintBalance(itemID)
- [ ] PrintDB() - only modified items
- [ ] PrintView() - NEW-VIEW history
- [ ] Performance() - throughput & latency

### Phase 8: Benchmarking
- [ ] Configurable read/write ratio
- [ ] Configurable intra/cross-shard ratio
- [ ] Configurable data distribution (uniform vs skewed)

### Phase 9: Shard Redistribution
- [ ] Hypergraph partitioning algorithm
- [ ] Data movement between clusters
- [ ] PrintReshard() function

---

## Architecture Decisions

### Why Separate Cluster Connections?
- `peerClients`: Only nodes in same cluster (for fast intra-cluster Paxos consensus)
- `allClusterClients`: All nodes across all clusters (for 2PC cross-shard transactions)

### Why Each Node Stores Only Its Shard?
- Memory efficiency: 3000 items per node instead of 9000
- Matches real distributed database architecture
- Clear separation of data ownership

### Why Fixed Shard Mapping?
- Project requirement: must use provided shard ranges
- Simplifies initial implementation
- Phase 9 will add dynamic resharding

---

## Performance Targets

Based on project requirements:
- **Target:** 1,000 read-write transactions per second **per cluster**
- **Total system:** 3,000 transactions per second across all 3 clusters
- **Bonus points** for achieving this throughput

---

## File Changes Summary

**Modified:**
- `config/nodes.yaml` - Added cluster configuration
- `internal/config/config.go` - New cluster-aware config structure
- `internal/node/node.go` - Added clusterID, changed database to int32
- `internal/node/consensus.go` - Updated transaction execution
- `proto/paxos.proto` - Changed Transaction to use int32 
- `proto/paxos.pb.go` - Manually updated generated code
- `scripts/start_nodes.sh` - Start 9 nodes instead of 5

**Created:**
- `testcases/test_multicluster.csv` - Multi-cluster test cases
- `MULTICLUSTER_README.md` - This file
