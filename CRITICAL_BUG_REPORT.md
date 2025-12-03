# 🚨 CRITICAL BUG REPORT - 2PC Replication Failure

## Bug Discovery Date: December 3, 2025
## Severity: CRITICAL - Violates core consistency requirement

---

## 🔍 Bug Description

**The 2PC implementation does NOT replicate cross-shard transactions through Paxos consensus, causing only leader nodes to execute transactions while follower nodes remain unsynced.**

---

## 📊 Test Evidence

### Test Performed
```bash
send 100 5000 5
# Cross-shard transaction: 5 units from item 100 (C1) to item 5000 (C2)
```

### Expected Result ✅
**All nodes in both clusters should have updated values:**
- Cluster 1 (items 1-3000): n1, n2, n3 should ALL show item 100 = 5
- Cluster 2 (items 3001-6000): n4, n5, n6 should ALL show item 5000 = 15

### Actual Result ❌
**Only leader nodes updated:**
```
Cluster 1 (Sender):
  n1 (leader): 5   ✅ Correct
  n2 (follower): 10 ❌ NOT UPDATED!
  n3 (follower): 10 ❌ NOT UPDATED!

Cluster 2 (Receiver):
  n4 (leader): 15  ✅ Correct
  n5 (follower): 10 ❌ NOT UPDATED!
  n6 (follower): 10 ❌ NOT UPDATED!
```

**Verification Commands:**
```bash
printbalance 100  → Output: n1 : 5, n2 : 10, n3 : 10
printbalance 5000 → Output: n4 : 15, n5 : 10, n6 : 10
```

---

## 🐛 Root Cause Analysis

### Location: `internal/node/twopc.go`

### Problem 1: TwoPCPrepare Handler (Lines 398-401)

```go
// CURRENT CODE (WRONG):
// Apply changes tentatively
n.mu.Lock()
n.balances[dataItem] = newBalance
n.mu.Unlock()
```

**Issue:** The participant directly modifies its own database WITHOUT running Paxos!

**What happens:**
1. Coordinator sends PREPARE to leader nodes only
2. Leader receives PREPARE, directly updates its own `balances` map
3. **Follower nodes NEVER participate - no ACCEPT messages sent!**
4. Only the leader knows about the transaction
5. Result: Database inconsistency within cluster

### Problem 2: No Paxos Integration

**Current Flow (BROKEN):**
```
Coordinator → PREPARE → Participant Leader
                ↓
           Direct DB update (leader only)
                ↓
           PREPARED reply
                ↓
Coordinator → COMMIT → Participant Leader  
                ↓
           WAL commit (leader only)
                ↓
           Done (followers never involved!)
```

**Correct Flow (REQUIRED):**
```
Coordinator → PREPARE → Participant Leader
                ↓
           Run Paxos Consensus:
             1. Assign sequence number
             2. Send ACCEPT to ALL nodes (n4, n5, n6)
             3. Wait for majority
             4. All nodes execute transaction
             5. All nodes update database
                ↓
           PREPARED reply
                ↓
Coordinator → COMMIT → Participant Leader
                ↓
           Run Paxos Consensus again:
             1. Send COMMIT to ALL nodes
             2. All nodes commit WAL
             3. All nodes persist to disk
                ↓
           Done (all nodes consistent!)
```

---

## 🔧 Required Fix

### Step 1: Modify TwoPCPrepare to Use Paxos

**File:** `internal/node/twopc.go` - `TwoPCPrepare` function

**Current (lines 349-416):**
```go
// Try to acquire lock on data item
items := []int32{dataItem}
acquired, lockedItems := n.acquireLocks(items, req.ClientId, req.Timestamp)

if !acquired {
    return &pb.TwoPCPrepareReply{Success: false, ...}
}

// Create WAL entry
walEntry := n.createWALEntry(txnID, 0)
n.writeToWAL(txnID, opType, dataItem, oldBalance, newBalance)

// Apply changes tentatively ← THIS IS THE BUG!
n.mu.Lock()
n.balances[dataItem] = newBalance
n.mu.Unlock()
```

**Required Change:**
```go
// Try to acquire lock on data item
items := []int32{dataItem}
acquired, lockedItems := n.acquireLocks(items, req.ClientId, req.Timestamp)

if !acquired {
    return &pb.TwoPCPrepareReply{Success: false, ...}
}

// Check balance (before consensus)
n.mu.RLock()
if isDebit && n.balances[dataItem] < amount {
    n.mu.RUnlock()
    n.releaseLocks(lockedItems, req.ClientId, req.Timestamp)
    return &pb.TwoPCPrepareReply{Success: false, ...}
}
n.mu.RUnlock()

// ========================================================
// RUN PAXOS CONSENSUS TO REPLICATE TO ALL NODES
// ========================================================

// Create transaction request for Paxos
paxosReq := &pb.TransactionRequest{
    ClientId:  req.ClientId,
    Timestamp: req.Timestamp,
    Transaction: &pb.Transaction{
        Sender:   tx.Sender,
        Receiver: tx.Receiver,
        Amount:   tx.Amount,
    },
    TwopcPhase: "PREPARE",  // Mark as 2PC PREPARE phase
    TwopcId:    txnID,
}

// If this node is the leader, run Paxos consensus
if n.isLeader {
    n.mu.Lock()
    seqNum := n.nextSeqNum
    n.nextSeqNum++
    n.mu.Unlock()

    // Send ACCEPT to all nodes in cluster
    acceptReq := &pb.AcceptRequest{
        Ballot:         n.currentBallot.ToProto(),
        SequenceNumber: seqNum,
        Request:        paxosReq,
        IsNoop:         false,
    }

    // Broadcast to all peers in cluster
    var acceptWg sync.WaitGroup
    acceptCount := 1 // Leader counts itself
    acceptMu := sync.Mutex{}

    for peerID, peerClient := range n.peerClients {
        acceptWg.Add(1)
        go func(pid int32, client pb.PaxosNodeClient) {
            defer acceptWg.Done()
            ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
            defer cancel()
            
            reply, err := client.Accept(ctx, acceptReq)
            if err == nil && reply.Success {
                acceptMu.Lock()
                acceptCount++
                acceptMu.Unlock()
            }
        }(peerID, peerClient)
    }

    acceptWg.Wait()

    // Check if we have quorum
    if !n.hasQuorum(acceptCount) {
        n.releaseLocks(lockedItems, req.ClientId, req.Timestamp)
        return &pb.TwoPCPrepareReply{
            Success: false,
            Message: "failed to achieve quorum for 2PC PREPARE",
        }, nil
    }

    // Send COMMIT to all nodes
    commitReq := &pb.CommitRequest{
        Ballot:         n.currentBallot.ToProto(),
        SequenceNumber: seqNum,
        Request:        paxosReq,
        IsNoop:         false,
    }

    for peerID, peerClient := range n.peerClients {
        go func(pid int32, client pb.PaxosNodeClient) {
            ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
            defer cancel()
            client.Commit(ctx, commitReq)
        }(peerID, peerClient)
    }

    // Execute locally (leader)
    n.executeTransaction(seqNum, paxosReq)
} else {
    // If not leader, return error (should not happen)
    return &pb.TwoPCPrepareReply{
        Success: false,
        Message: "not leader, cannot process 2PC PREPARE",
    }, nil
}

// Now all nodes have executed the transaction via Paxos
// Create WAL entry for rollback capability
walEntry := n.createWALEntry(txnID, 0)
walEntry.Status = types.WALStatusPrepared

log.Printf("Node %d: 2PC[%s]: ✅ PREPARED via Paxos consensus", n.id, txnID)

return &pb.TwoPCPrepareReply{
    Success:       true,
    TransactionId: txnID,
    Message:       "prepared via Paxos consensus",
    ParticipantId: n.id,
}, nil
```

### Step 2: Modify TwoPCCommit Similarly

**Required:** Run another round of Paxos consensus for the COMMIT phase to ensure all nodes commit their WAL entries and persist to disk.

### Step 3: Modify TwoPCAbort Similarly

**Required:** Run Paxos consensus for ABORT phase to ensure all nodes rollback via WAL.

---

## ✅ Verification Steps

After implementing the fix:

1. **Test simple cross-shard:**
   ```bash
   flush
   send 100 5000 5
   printbalance 100
   printbalance 5000
   ```
   
2. **Expected output:**
   ```
   printbalance 100  → n1 : 5, n2 : 5, n3 : 5  ✅
   printbalance 5000 → n4 : 15, n5 : 15, n6 : 15 ✅
   ```

3. **Test with node failures:**
   ```bash
   flush
   send 100 5000 5
   # Should still work with minority failures
   ```

4. **Test all edge cases:**
   ```bash
   ./bin/client -testfile testcases/edge_01_coordinator_failures.csv
   # Verify consistency after each test set
   ```

---

## 📊 Impact Assessment

### Current State
- ❌ **Consistency:** BROKEN - nodes within cluster have different values
- ❌ **2PC:** Partially working - only leaders execute
- ✅ **Intra-shard:** Working perfectly
- ✅ **Locking:** Working correctly
- ✅ **Leader Election:** Working

### After Fix
- ✅ **Consistency:** ALL nodes in cluster will have identical values
- ✅ **2PC:** Full replication through Paxos
- ✅ **Edge Cases:** Can handle leader failures, node failures, etc.
- ✅ **Production Ready:** Meets all requirements

---

## 🎯 Priority

**PRIORITY:** CRITICAL

**Why:**
- Violates fundamental state machine replication guarantee
- Makes 11 edge case test files untestable
- Project cannot be submitted with this bug
- Must be fixed before demo

**Estimated Fix Time:** 2-3 hours
1. Implement Paxos integration in TwoPCPrepare (1 hour)
2. Implement Paxos integration in TwoPCCommit (30 min)
3. Implement Paxos integration in TwoPCAbort (30 min)
4. Test and verify (1 hour)

---

## 📝 Additional Notes

### What Works ✅
- Intra-shard transactions with full Paxos replication
- Lock acquisition and contention handling
- F(ni)/R(ni) command parsing
- FLUSH functionality
- PrintDB, PrintBalance, PrintView
- CSV parsing

### What's Broken ❌
- **Cross-shard transaction replication (CRITICAL)**

### Implications
- Cannot test coordinator failures (edge_01)
- Cannot test participant failures (edge_02)
- Cannot test leader election during 2PC (edge_05)
- Cannot verify WAL rollback (edge_06+)
- All 11 edge case files untestable until fixed

---

## 🔍 Code Locations

**Files to Modify:**
1. `internal/node/twopc.go` - TwoPCPrepare function (line 315-417)
2. `internal/node/twopc.go` - TwoPCCommit function (line 420-450)
3. `internal/node/twopc.go` - TwoPCAbort function (line 453-485)

**Key Insight:**
The 2PC handlers must NOT directly modify `n.balances`. Instead, they must create a Paxos consensus instance and let the normal `executeTransaction` path handle the actual database updates on ALL nodes.

---

## 🎯 Conclusion

**Status:** CRITICAL BUG FOUND AND DIAGNOSED

**Action Required:** Fix 2PC-Paxos integration before proceeding

**Once Fixed:** Re-run all edge case tests to verify correct behavior

**Timeline:** Fix should be implemented before December 7 submission deadline

---

**Bug Report Complete** ✅
