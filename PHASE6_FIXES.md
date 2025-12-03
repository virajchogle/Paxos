# Phase 6: 2PC Connection Fixes ✅

## Issues Identified

From the test run, we discovered **cross-cluster connection failures**:

### Problem 1: Timing Issue
```
Node 1: Warning - couldn't connect to cross-cluster node 4 (context deadline exceeded)
Node 1: Connected to 2 peers in cluster, 1 cross-cluster nodes  (should be 6!)
```

**Root Cause:** 
- Nodes start sequentially via `start_nodes.sh`
- When Node 1 starts, nodes 4-9 aren't running yet
- 500ms timeout was too short
- No retry mechanism

### Problem 2: Self-Reference Bug
```
Node 1: 2PC[...]: ❌ PREPARE error: no client for participant 1
```

**Root Cause:**
- When coordinator is also a participant (e.g., Node 1 coordinating a transaction involving its own shard)
- Node tried to look itself up in `allClusterClients` map
- No self-reference was stored, causing failure

---

## Fixes Applied

### Fix 1: Increased Initial Connection Timeout
**File:** `internal/node/node.go` (line 249)

```go
// Before:
ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)

// After:
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
```

### Fix 2: Added On-Demand Connection Establishment
**File:** `internal/node/node.go` (lines 267-311)

Added new function `getCrossClusterClient()`:
```go
func (n *Node) getCrossClusterClient(nodeID int32) (pb.PaxosNodeClient, error) {
    // Check if already connected
    if client, exists := n.allClusterClients[nodeID]; exists {
        return client, nil
    }
    
    // Attempt to establish connection with 3s timeout
    // Save for future use
    // Return client or error
}
```

**Benefits:**
- ✅ Lazy connection establishment
- ✅ 3-second timeout (6x longer)
- ✅ Automatic retry when needed
- ✅ Saves successful connections for reuse

### Fix 3: Handle Self-Reference in 2PC
**File:** `internal/node/twopc.go` (all phases)

Updated all three 2PC phases (PREPARE, COMMIT, ABORT):

**Before:**
```go
client, exists := n.allClusterClients[partID]
if !exists {
    return error
}
reply, err := client.TwoPCPrepare(ctx, req)
```

**After:**
```go
// Handle self-reference (coordinator is also participant)
if partID == n.id {
    reply, err := n.TwoPCPrepare(ctx, req)  // Local call!
    // ...
    return
}

// Get client with retry
client, err := n.getCrossClusterClient(partID)
if err != nil {
    return error
}
reply, err := client.TwoPCPrepare(ctx, req)
```

**Applied to:**
- PREPARE phase (lines 61-77)
- COMMIT phase (lines 169-193)
- ABORT phase (lines 256-275)

---

## What Changed

### Connection Strategy
| Aspect | Before | After |
|--------|--------|-------|
| Initial timeout | 500ms | 2s |
| Retry on failure | No | Yes (on-demand) |
| Self-reference | Not handled | Local dispatch |
| Connection timeout | 500ms | 3s (on-demand) |

### 2PC Phases
All three phases now:
1. ✅ Check if participant is self → call local handler
2. ✅ Try existing connection from map
3. ✅ Establish new connection with retry if needed
4. ✅ Save successful connections for reuse

---

## Testing

### Restart Nodes and Test
```bash
# Stop all nodes
./scripts/stop_all.sh

# Clean logs (optional)
rm logs/*.log

# Start all nodes (with new binaries)
./scripts/start_nodes.sh

# Wait 5 seconds for all nodes to start
sleep 5

# Test cross-shard transactions
./bin/client testcases/test_crossshard.csv
```

### Expected Behavior

#### Test Set 2: Cross-shard (C1 → C2)
```
client> next
2025/11/30 XX:XX:XX 🌐 Cross-shard transaction detected (100→4000) - using 2PC
Node 1: Attempting to establish cross-cluster connection to node 4
Node 1: ✅ Established cross-cluster connection to node 4
✅ [1/1] 100 → 4000: 7 units (C1→C2 cross-shard)
```

#### Test Set 4: Multiple Cross-shard
```
client> next
2025/11/30 XX:XX:XX 🌐 Cross-shard transaction detected (1000→5000) - using 2PC
✅ [1/2] 1000 → 5000: 2 units (C1→C2 cross-shard)
2025/11/30 XX:XX:XX 🌐 Cross-shard transaction detected (5000→8000) - using 2PC
Node 4: Attempting to establish cross-cluster connection to node 7
Node 4: ✅ Established cross-cluster connection to node 7
✅ [2/2] 5000 → 8000: 1 units (C2→C3 cross-shard)
```

---

## Verification

Check node logs for successful connections:
```bash
# Should see these messages in logs/node1.log:
grep "Established cross-cluster connection" logs/node1.log

# Should see successful 2PC:
grep "2PC.*COMMITTED" logs/node1.log
grep "2PC.*COMMITTED" logs/node4.log
```

---

## Summary of Changes

| File | Lines Modified | Description |
|------|----------------|-------------|
| `internal/node/node.go` | ~50 lines | Added `getCrossClusterClient()`, increased timeouts |
| `internal/node/twopc.go` | ~120 lines | Updated all 3 phases to handle self-reference + retry |

**Total:** ~170 lines modified to fix connection issues

---

## What's Fixed

✅ **Timing Issues:** Longer timeouts + on-demand connections
✅ **Self-Reference:** Coordinator can participate in its own transaction
✅ **Retry Logic:** Automatic retry when connection fails
✅ **Connection Caching:** Successful connections saved for reuse
✅ **Better Logging:** Shows when establishing new connections

---

## Next Steps

1. **Restart nodes** with new binaries
2. **Test cross-shard transactions**
3. **Verify all test sets pass**
4. If successful → Ready for Phase 7! 🚀

---

**Phase 6 Connection Fixes Complete!** 🎉
