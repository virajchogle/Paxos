# 2PC Consensus Integration - Remaining Work

## ✅ What's Done

1. ✅ Added `Phase` field to `LogEntry` struct
2. ✅ Added `Phase` field to `AcceptRequest` proto message
3. ✅ Added `Phase` field to `CommitRequest` proto message
4. ✅ Generated proto getters for `Phase` field
5. ✅ Parallel execution in 2PC coordinator
6. ✅ Sequence number tracking in `TwoPCTransaction`

## ⚠️ What's Partially Done (Stubs)

The `processAsLeaderWithPhaseAndSeq()` function exists but is a **stub**:
```go
func (n *Node) processAsLeaderWithPhaseAndSeq(req *pb.TransactionRequest, phase string, seq int32) (*pb.TransactionReply, int32, error) {
    // TODO: Extend consensus.go to support phase markers and sequence reuse
    reply, err := n.processAsLeader(req)
    
    // Returns dummy sequence
    if seq == 0 {
        n.logMu.Lock()
        seq = n.nextSeqNum - 1
        n.logMu.Unlock()
    }
    
    return reply, seq, err
}
```

## ❌ What's Missing (Critical)

### 1. **Consensus Layer Must Accept Phase Parameter**

**File**: `internal/node/consensus.go`

Need to modify `processAsLeader()` to:
- Accept `phase` parameter
- Accept optional `seq` parameter for reuse
- Pass phase to ACCEPT messages
- Store phase in log entries

**Current**:
```go
func (n *Node) processAsLeader(req *pb.TransactionRequest) (*pb.TransactionReply, error) {
    // Gets new sequence every time
    seqNum := n.nextSeqNum
    n.nextSeqNum++
    
    // Creates ACCEPT without phase
    acceptReq := &pb.AcceptRequest{
        Ballot:         ballot,
        SequenceNumber: seqNum,
        Request:        req,
        IsNoop:         false,
    }
    // No Phase field set!
}
```

**Needed**:
```go
func (n *Node) processAsLeaderWithSeq(req *pb.TransactionRequest, phase string, reuseSeq int32) (*pb.TransactionReply, int32, error) {
    // If reuseSeq != 0, use that instead of getting new
    var seqNum int32
    if reuseSeq != 0 {
        seqNum = reuseSeq
    } else {
        seqNum = n.nextSeqNum
        n.nextSeqNum++
    }
    
    // Create ACCEPT with phase
    acceptReq := &pb.AcceptRequest{
        Ballot:         ballot,
        SequenceNumber: seqNum,
        Request:        req,
        IsNoop:         false,
        Phase:          phase, // NEW!
    }
    
    // Store phase in log entry
    entry := types.NewLogEntryWithPhase(ballot, seqNum, req, false, phase)
}
```

### 2. **ALL Nodes Must Handle Phase Markers in ACCEPT**

**File**: `internal/node/consensus.go` - `Accept()` handler

When ANY node (leader or follower) receives ACCEPT:

```go
func (n *Node) Accept(ctx context.Context, req *pb.AcceptRequest) (*pb.AcceptedReply, error) {
    // ... existing ballot checks ...
    
    // Create log entry WITH phase
    entry := types.NewLogEntryWithPhase(
        ballot,
        req.SequenceNumber,
        req.Request,
        req.IsNoop,
        req.Phase, // NEW!
    )
    
    // Store in log
    n.log[req.SequenceNumber] = entry
    
    // NEW: If this is a 2PC transaction, handle WAL and locks
    if req.Phase != "" {
        n.handle2PCPhase(entry, req.Phase)
    }
}
```

### 3. **ALL Nodes Must Handle Phase Markers in COMMIT**

**File**: `internal/node/consensus.go` - `Commit()` handler

```go
func (n *Node) Commit(ctx context.Context, req *pb.CommitRequest) (*pb.CommitReply, error) {
    // ... existing code ...
    
    // Execute transaction
    n.executeTransaction(entry)
    
    // NEW: If 2PC transaction, handle phase-specific cleanup
    if req.Phase != "" {
        n.handle2PCCommitPhase(entry, req.Phase)
    }
}
```

### 4. **ALL Nodes Need 2PC State Management**

**File**: `internal/node/node.go`

Currently, only leader tracks 2PC state. **ALL nodes need**:

```go
type Node struct {
    // ... existing fields ...
    
    // 2PC state for ALL nodes (not just leader)
    twoPCState TwoPCState // Already added
    
    // Each node tracks its own locks and WAL for 2PC
    twoPCLocks  map[int32]*DataItemLock // Per-node 2PC locks
    twoPCWAL    map[string]map[int32]int32 // txnID -> (itemID -> oldBalance)
}
```

### 5. **ALL Nodes Must Handle 2PC Phases**

**New function needed** in `internal/node/consensus.go`:

```go
// handle2PCPhase is called by ALL nodes when they accept a 2PC transaction
func (n *Node) handle2PCPhase(entry *types.LogEntry, phase string) {
    tx := entry.Request.Transaction
    txnID := fmt.Sprintf("2pc-%s-%d", entry.Request.ClientId, entry.Request.Timestamp)
    
    switch phase {
    case "P": // PREPARE phase
        // 1. Save old balance to WAL (before execution)
        n.balanceMu.Lock()
        if n.twoPCWAL[txnID] == nil {
            n.twoPCWAL[txnID] = make(map[int32]int32)
        }
        
        // Determine which item this node is responsible for
        if n.config.GetClusterForDataItem(tx.Sender) == int(n.clusterID) {
            // This node handles sender
            n.twoPCWAL[txnID][tx.Sender] = n.balances[tx.Sender]
        }
        if n.config.GetClusterForDataItem(tx.Receiver) == int(n.clusterID) {
            // This node handles receiver
            n.twoPCWAL[txnID][tx.Receiver] = n.balances[tx.Receiver]
        }
        n.balanceMu.Unlock()
        
        // 2. Lock is handled by leader, followers just track
        log.Printf("Node %d: 2PC PREPARE phase for %s", n.id, txnID)
        
    case "C": // COMMIT phase
        // 1. Keep the transaction result
        // 2. Delete WAL entry
        n.balanceMu.Lock()
        delete(n.twoPCWAL, txnID)
        n.balanceMu.Unlock()
        
        log.Printf("Node %d: 2PC COMMIT phase for %s - WAL deleted", n.id, txnID)
        
    case "A": // ABORT phase
        // 1. Rollback using WAL
        n.balanceMu.Lock()
        if walEntries, exists := n.twoPCWAL[txnID]; exists {
            for itemID, oldBalance := range walEntries {
                log.Printf("Node %d: 2PC ABORT - rolling back item %d: %d -> %d",
                    n.id, itemID, n.balances[itemID], oldBalance)
                n.balances[itemID] = oldBalance
            }
            delete(n.twoPCWAL, txnID)
        }
        n.balanceMu.Unlock()
        
        log.Printf("Node %d: 2PC ABORT phase for %s - rolled back", n.id, txnID)
    }
}
```

### 6. **Sequence Reuse Logic**

When COMMIT phase reuses PREPARE sequence:

```go
// In processAsLeaderWithSeq()
if reuseSeq != 0 {
    // Check if log entry already exists at this sequence
    n.logMu.RLock()
    existingEntry, exists := n.log[reuseSeq]
    n.logMu.RUnlock()
    
    if exists {
        // Verify it's the same transaction
        if existingEntry.Request.ClientId == req.ClientId &&
           existingEntry.Request.Timestamp == req.Timestamp {
            // This is the second phase (COMMIT/ABORT) for same transaction
            // Update the phase marker in existing entry
            n.logMu.Lock()
            existingEntry.Phase = phase // Update from 'P' to 'C' or 'A'
            n.logMu.Unlock()
            
            log.Printf("Node %d: Updated phase for seq %d from '%s' to '%s'",
                n.id, reuseSeq, existingEntry.Phase, phase)
        }
    }
}
```

## 📊 Integration Points

### Timeline of 2PC with Full Integration

```
PHASE 1: PREPARE
─────────────────

Leader:
  1. Lock sender locally
  2. Call processAsLeaderWithSeq(req, "P", 0)
  3. Gets new seq (e.g., 100)
  4. Sends ACCEPT(seq=100, phase="P") to all cluster nodes
  
All Nodes (including followers):
  5. Receive ACCEPT(seq=100, phase="P")
  6. Store in log: log[100] = {Phase: "P", ...}
  7. Call handle2PCPhase(entry, "P")
  8. Save to WAL: twoPCWAL[txnID][item] = oldBalance
  9. Execute transaction (debit/credit)
  10. Reply ACCEPTED
  
Leader:
  11. Majority achieved
  12. Send COMMIT(seq=100, phase="P") to all
  
All Nodes:
  13. Mark entry as executed
  14. Entry status: "E", Phase: "P"

PHASE 2: COMMIT
───────────────

Leader:
  15. Call processAsLeaderWithSeq(req, "C", 100) // REUSE seq 100!
  16. Sends ACCEPT(seq=100, phase="C") to all cluster nodes
  
All Nodes:
  17. Receive ACCEPT(seq=100, phase="C")
  18. Update log: log[100].Phase = "C" (or create new entry)
  19. Call handle2PCPhase(entry, "C")
  20. Delete WAL: delete(twoPCWAL[txnID])
  21. Release locks (leader only)
  22. Reply ACCEPTED
  
Leader:
  23. Majority achieved
  24. Send COMMIT(seq=100, phase="C") to all
  
All Nodes:
  25. Transaction complete!
```

## 📝 Summary of Changes Needed

### High Priority (Critical for Correctness)

1. **Modify `processAsLeader`** → `processAsLeaderWithSeq` to accept phase and optional sequence
2. **Modify `Accept` handler** to store phase in log entry
3. **Modify `Commit` handler** to pass phase to execution
4. **Add `handle2PCPhase`** function for ALL nodes to manage WAL
5. **Initialize `twoPCWAL`** map in ALL nodes

### Medium Priority (For Full Spec Compliance)

6. **Sequence reuse logic** when COMMIT phase uses PREPARE's sequence
7. **Two entries per sequence** - store both 'P' and 'C' phases at same sequence
8. **Follower lock tracking** (currently only leader manages locks)

### Low Priority (Nice to Have)

9. **Phase marker in NewView** for recovery
10. **Phase marker in checkpoint** for state snapshot
11. **Metrics** for 2PC phase transitions

## 🚀 Quick Implementation Path

To get a **working** implementation quickly:

1. **Implement `processAsLeaderWithSeq`** (replaces stub)
2. **Add phase to ACCEPT message** creation
3. **Add `handle2PCPhase` for WAL management** on all nodes
4. **Initialize `twoPCWAL` map** in Node struct

These 4 changes will give you a **90% complete** implementation that works correctly for most cases.

## 🧪 Testing Strategy

1. **Test PREPARE phase**:
   - Start transaction
   - Check all nodes have WAL entry
   - Verify execution occurred

2. **Test COMMIT phase**:
   - After PREPARE, send COMMIT
   - Verify WAL deleted on all nodes
   - Verify transaction persists

3. **Test ABORT phase**:
   - After PREPARE, send ABORT
   - Verify rollback on all nodes
   - Verify WAL deleted

4. **Test sequence reuse**:
   - Check PREPARE and COMMIT use same sequence
   - Verify log has both phases

5. **Test follower recovery**:
   - Stop follower during PREPARE
   - Restart follower
   - Verify WAL syncs correctly

## 📚 Files That Need Modification

| File | What Needs To Change |
|------|---------------------|
| `internal/node/consensus.go` | processAsLeader, Accept, Commit handlers |
| `internal/node/twopc.go` | processAsLeaderWithPhaseAndSeq implementation |
| `internal/node/node.go` | Initialize twoPCWAL map |

## ⚠️ Current State

**What Works**:
- ✅ 2PC flow at coordinator/participant level
- ✅ Parallel execution
- ✅ Locks and WAL at leader
- ✅ Protocol messages with phase markers
- ✅ Data structures support phases

**What Doesn't Work Yet**:
- ❌ Phase markers not passed to Paxos
- ❌ Followers don't maintain WAL
- ❌ Sequence not actually reused
- ❌ Two phases at same sequence not stored
- ❌ Rollback only works on leader

**Impact**: The 2PC **appears** to work but:
- If leader fails mid-2PC, followers can't rollback (no WAL)
- If follower becomes leader, it doesn't know about 2PC state
- Logs don't show correct phase markers
- Recovery after crash won't work correctly

## ✅ Recommendation

The current implementation is **functionally correct** for the happy path but **not specification-compliant** for fault tolerance. The missing pieces are needed for:
1. **Crash recovery** during 2PC
2. **Leader failover** during 2PC
3. **Spec compliance** (all nodes should track 2PC state)

For a **production system**, these must be implemented. For a **demo/prototype**, the current implementation works for basic testing.

---

**The foundation is in place - now we need to wire it all together!** 🔧
