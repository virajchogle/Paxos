# How to Run the Multi-Cluster Paxos System

## Quick Start Guide

### Step 1: Build Everything
```bash
cd /Users/viraj/Desktop/Projects/paxos/Paxos

# Build node and client binaries
go build -o bin/node cmd/node/main.go
go build -o bin/client cmd/client/main.go
```

### Step 2: Start All 9 Nodes
```bash
# Make sure no old nodes are running
./scripts/stop_all.sh

# Start all 9 nodes (will open 9 terminal windows)
./scripts/start_nodes.sh
```

**Expected output in each node:**
```
Node 1 (Cluster 1): Initialized 3000 data items (range 1-3000)
Node 4 (Cluster 2): Initialized 3000 data items (range 3001-6000)
Node 7 (Cluster 3): Initialized 3000 data items (range 6001-9000)
```

### Step 3: Run the Client
```bash
# Option 1: With test file (positional argument - now works!)
./bin/client testcases/test_multicluster.csv

# Option 2: With flag
./bin/client -testfile testcases/test_multicluster.csv
```

**Expected output:**
```
✓ Connected to node 1
✓ Connected to node 2
...
✓ Connected to node 9

✅ Client Manager Ready
Loaded 4 test sets from testcases/test_multicluster.csv
```

### Step 4: Run Transactions
```
client> next
Processing 2 transactions from 2 unique senders...
✅ [1/2] 100 → 200: 2 units
✅ [2/2] 150 → 250: 3 units

client> send 4000 4100 5
Sending: 4000 → 4100: 5 units... ✅

client> send 7000 7100 3
Sending: 7000 → 7100: 3 units... ✅
```

---

## Test File Format

### Example: `testcases/test_multicluster.csv`
```csv
Set Number	Transactions	Live Nodes
1	(100, 200, 2)	[n1, n2, n3, n4, n5, n6, n7, n8, n9]
	(150, 250, 3)	
2	(1500, 1600, 5)	[n1, n2, n3, n4, n5, n6, n7, n8, n9]
3	(4000, 4100, 1)	[n1, n2, n3, n4, n5, n6, n7, n8, n9]
4	(7000, 7100, 2)	[n1, n2, n3, n4, n5, n6, n7, n8, n9]
```

**Format:**
- `Set Number`: Test set ID (blank for continuation of previous set)
- `Transactions`: `(sender_id, receiver_id, amount)` where IDs are 1-9000
- `Live Nodes`: Which nodes should be active `[n1, n2, ...]`

**Cluster Assignment:**
- Items 1-3000 → Cluster 1 (nodes 1, 2, 3)
- Items 3001-6000 → Cluster 2 (nodes 4, 5, 6)
- Items 6001-9000 → Cluster 3 (nodes 7, 8, 9)

---

## Transaction Types

### Intra-Shard (Same Cluster)
```
(100, 200, 5)     ← Both in Cluster 1
(4000, 4100, 3)   ← Both in Cluster 2
(7000, 7100, 2)   ← Both in Cluster 3
```

### Cross-Shard (Different Clusters) - NOT YET IMPLEMENTED
```
(100, 4000, 5)    ← Cluster 1 → Cluster 2 (needs 2PC)
(1500, 7000, 3)   ← Cluster 1 → Cluster 3 (needs 2PC)
(4000, 7000, 2)   ← Cluster 2 → Cluster 3 (needs 2PC)
```

---

## Common Issues

### Issue: "Connection refused" for nodes 6-9
**Cause:** Only 5 nodes are running (old setup)
**Fix:** 
```bash
./scripts/stop_all.sh
./scripts/start_nodes.sh  # This will start all 9 nodes
```

### Issue: "Failed to parse transaction... parsing 'A': invalid syntax"
**Cause:** Using old test file with string client IDs (A, B, C)
**Fix:** Use test file with int32 data item IDs:
```bash
./bin/client testcases/test_multicluster.csv
```

### Issue: "panic: invalid Go type int32 for field paxos.Transaction.sender"
**Cause:** Old binary from before protobuf fix
**Fix:** Rebuild:
```bash
go build -o bin/client cmd/client/main.go
go build -o bin/node cmd/node/main.go
```

---

## Verifying the System

### Check Node Logs
Each node terminal should show:
```
Node X (Cluster Y): Initialized Z data items (range A-B) with balance 10
Node X (Cluster Y): Connected to Y peers in cluster, Z cross-cluster nodes
Node X (Cluster Y): Ready (standby mode - waiting for transaction)
```

### Test Commands
```bash
# Start the system
./scripts/start_nodes.sh

# In another terminal
./bin/client testcases/test_multicluster.csv

# Interactive mode
client> send 100 200 5      # Intra-shard (Cluster 1)
client> send 1500 1600 3    # Intra-shard (Cluster 1)
client> send 4000 4100 2    # Intra-shard (Cluster 2)
client> send 7000 7100 1    # Intra-shard (Cluster 3)
```

---

## Current System Status

### ✅ Implemented (Phase 1)
- 9 nodes in 3 clusters
- Sharded data storage (3000 items per cluster)
- Int32 data item IDs (1-9000)
- Intra-cluster Paxos consensus
- Client can submit transactions
- Cluster-aware connections

### ⏳ To Be Implemented
- Phase 2: Locking mechanism
- Phase 3: Read-only transactions
- Phase 4: Intra-shard with locks
- Phase 5: Write-Ahead Log (WAL)
- Phase 6: Two-Phase Commit (2PC) for cross-shard
- Phase 7: Utility functions (PrintBalance, PrintDB, etc.)
- Phase 8: Benchmarking
- Phase 9: Shard redistribution

---

## Next Steps

After starting all 9 nodes and verifying the system works:

1. **Implement Phase 2: Locking** to handle concurrent transactions
2. **Implement Phase 3: Read-only transactions** for balance queries
3. **Implement Phase 6: 2PC** for cross-shard transactions

---

## Need Help?

See documentation:
- `MULTICLUSTER_README.md` - Architecture overview
- `PHASE1_COMPLETE.md` - What changed in Phase 1
- Project PDF - Full requirements
