# Cluster-Aware Transaction Routing

## Overview

The client now implements **cluster-aware routing** as specified in the paper. Transactions are intelligently routed to the appropriate cluster based on the data items involved.

## Key Changes

### 1. Cluster Leader Tracking
```go
clusterLeaders map[int32]int32  // clusterID -> leaderNodeID
```
- Tracks the current leader of each cluster independently
- Updated dynamically based on node responses
- Initialized with the first node of each cluster

### 2. Data Item to Cluster Mapping
```go
func getClusterForDataItem(dataItemID int32) int32
```
- Determines which cluster owns a specific data item
- Based on shard ranges from config:
  - Cluster 1: items 1-3000
  - Cluster 2: items 3001-6000
  - Cluster 3: items 6001-9000

### 3. Transaction Routing Logic
```go
func getTargetNodeForTransaction(sender, receiver int32) (int32, bool)
```
- **Intra-shard**: Routes to leader of the shard's cluster
- **Cross-shard**: Routes to sender's cluster leader (will be 2PC coordinator)
- Returns `(targetNodeID, isCrossShard)`

### 4. Cluster-Scoped Retry
- If initial leader fails, retries **only within the same cluster**
- No longer broadcasts to all 9 nodes
- More efficient and respects cluster boundaries

## How It Works

### Intra-Shard Transaction Example
```
Transaction: (100, 200, 2)  // Both in Cluster 1
```
1. Client determines both items are in Cluster 1
2. Routes to Cluster 1's leader (e.g., node 1)
3. If node 1 fails, tries nodes 2 and 3 (other Cluster 1 nodes)
4. Transaction executes using Paxos within Cluster 1

### Cross-Shard Transaction Example (2PC - not yet implemented)
```
Transaction: (100, 4000, 5)  // Cluster 1 → Cluster 2
```
1. Client determines sender in Cluster 1, receiver in Cluster 2
2. Routes to Cluster 1's leader (sender's cluster)
3. Cluster 1 leader becomes 2PC coordinator
4. Coordinator will contact Cluster 2 as participant (future Phase 6)

## Visual Feedback

The client now shows which cluster handled each transaction:

```
✅ [1/2] 100 → 200: 2 units (Cluster 1)
✅ [2/2] 4000 → 4100: 5 units (Cluster 2)
```

For cross-shard (when 2PC is implemented):
```
✅ [1/1] 100 → 4000: 3 units (C1→C2 cross-shard)
```

## Testing

### Test File: `testcases/test_cluster_routing.csv`

```csv
Set Number	Transactions	Live Nodes
1	(100, 200, 2)	[n1, n2, n3, n4, n5, n6, n7, n8, n9]
	(150, 250, 3)	
2	(4000, 4100, 5)	[n1, n2, n3, n4, n5, n6, n7, n8, n9]
	(4500, 4600, 2)	
3	(7000, 7100, 3)	[n1, n2, n3, n4, n5, n6, n7, n8, n9]
	(7500, 7600, 1)	
```

### Expected Behavior

**Before (incorrect):**
- All transactions sent to node 1 (Cluster 1 leader)
- Transactions for items 4000, 7000 failed with "insufficient balance"
- Node 1 had no data for those items

**After (correct):**
- Set 1 transactions → Cluster 1 (nodes 1-3) ✅
- Set 2 transactions → Cluster 2 (nodes 4-6) ✅
- Set 3 transactions → Cluster 3 (nodes 7-9) ✅
- All transactions succeed with correct data access

## Running the Test

```bash
# Make sure all 9 nodes are running
./scripts/start_nodes.sh

# Run the client with the routing test
./bin/client testcases/test_cluster_routing.csv

# Process test sets
client> next  # Cluster 1 transactions
client> next  # Cluster 2 transactions
client> next  # Cluster 3 transactions
```

### Expected Output

```
Processing Test Set 1
✅ [1/2] 100 → 200: 2 units (Cluster 1)
✅ [2/2] 150 → 250: 3 units (Cluster 1)

Processing Test Set 2
✅ [1/2] 4000 → 4100: 5 units (Cluster 2)
✅ [2/2] 4500 → 4600: 2 units (Cluster 2)

Processing Test Set 3
✅ [1/2] 7000 → 7100: 3 units (Cluster 3)
✅ [2/2] 7500 → 7600: 1 units (Cluster 3)
```

## Verification

### Check Node Logs

**Node 1 (Cluster 1) log should show:**
```
Executed transaction: 100 → 200: 2 units
Executed transaction: 150 → 250: 3 units
```

**Node 4 (Cluster 2) log should show:**
```
Executed transaction: 4000 → 4100: 5 units
Executed transaction: 4500 → 4600: 2 units
```

**Node 7 (Cluster 3) log should show:**
```
Executed transaction: 7000 → 7100: 3 units
Executed transaction: 7500 → 7600: 1 units
```

### Interactive Testing

```bash
# Send transactions to different clusters manually
client> send 100 200 5      # → Cluster 1
client> send 4000 4100 3    # → Cluster 2
client> send 7000 7100 2    # → Cluster 3
```

## Benefits

1. **Correct Data Access**: Each cluster only accesses its own shard data
2. **Load Distribution**: Transactions distributed across clusters
3. **Efficient Retries**: Only retry within relevant cluster
4. **Scalability**: Foundation for cross-shard transactions (2PC)
5. **Per-Cluster Leader Election**: Each cluster has independent Paxos consensus

## Next Steps

With cluster-aware routing complete, the system is ready for:
- **Phase 2**: Locking mechanism for concurrent transactions
- **Phase 6**: Two-Phase Commit (2PC) for cross-shard transactions

## Technical Details

### Configuration
```yaml
clusters:
  1:
    shard_start: 1
    shard_end: 3000
    nodes: [1, 2, 3]
  2:
    shard_start: 3001
    shard_end: 6000
    nodes: [4, 5, 6]
  3:
    shard_start: 6001
    shard_end: 9000
    nodes: [7, 8, 9]
```

### Cluster Leader Discovery
- Initialized with first node of each cluster
- Updated on successful transaction response
- Independent leader election per cluster
- No global leader (distributed coordination)

## Troubleshooting

### Issue: Transaction still going to wrong cluster
**Check:** Run `./bin/client testcases/test_cluster_routing.csv` and verify output shows correct cluster numbers

### Issue: "No available nodes in cluster"
**Fix:** Ensure all nodes in the target cluster are running with `./scripts/start_nodes.sh`

### Issue: Cross-shard transaction fails
**Expected:** Cross-shard (2PC) is not yet implemented. You'll see:
```
⚠️  Cross-shard transaction detected (100→4000) - 2PC not yet implemented
```
