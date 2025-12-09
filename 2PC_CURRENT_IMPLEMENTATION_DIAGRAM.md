# Two-Phase Commit (2PC) Protocol - Current Implementation
## Complete Flow Diagram with All Features

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                          CLIENT → COORDINATOR                                        │
│                     Transaction: 1001 → 3001, amount: 10                            │
│                     (Cluster 1 sender → Cluster 2 receiver)                         │
└─────────────────────────────────────────────────────────────────────────────────────┘

════════════════════════════════════════════════════════════════════════════════════════
                              PHASE 0: PRE-CHECKS
════════════════════════════════════════════════════════════════════════════════════════

COORDINATOR (Cluster 1 Leader)
    │
    ├─► Check for duplicate (clientID + timestamp)
    │   └─► If duplicate: return cached result ✓
    │
    ├─► Determine participant cluster
    │   └─► receiverCluster = GetClusterForDataItem(3001) = 2
    │   └─► receiverLeader = GetLeaderNodeForCluster(2) = node 4  ⚠️ STATIC BUG!
    │
    └─► ATOMIC Check & Lock (TOCTOU FIX)
        ├─► n.balanceMu.Lock()  🔒
        ├─► Check: is item 1001 locked? → NO
        ├─► Check: balance[1001] >= 10? → YES (balance: 100)
        ├─► Lock item 1001 (clientID, timestamp)
        ├─► Save to twoPCState.transactions[txnID]:
        │   └─► TxnID, Transaction, ClientID, Timestamp
        │   └─► Phase: "PREPARE"
        │   └─► LockedItems: [1001]
        │   └─► WALEntries: {1001: 100}  (old balance)
        └─► n.balanceMu.Unlock()  🔓

════════════════════════════════════════════════════════════════════════════════════════
                      PHASE 1: PREPARE (PARALLEL EXECUTION!)
════════════════════════════════════════════════════════════════════════════════════════

┌───────────────────────────────────┬───────────────────────────────────────────┐
│  COORDINATOR PATH (GOROUTINE 1)   │   PARTICIPANT PATH (GOROUTINE 2)          │
│  Runs Paxos in Cluster 1          │   Sends PREPARE to Cluster 2              │
└───────────────────────────────────┴───────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────────┐
│  GOROUTINE 1: Coordinator's Own Paxos                                           │
└─────────────────────────────────────────────────────────────────────────────────┘

COORDINATOR (Node 1 - Cluster 1 Leader)
    │
    ├─► processAsLeaderWithPhaseAndSeq(req, phase="P", seq=0)
    │   │
    │   ├─► Allocate NEW sequence: seq = 100
    │   │   └─► log: "Allocated NEW seq=100"
    │   │
    │   ├─► Create LogEntry with Phase="P"
    │   │   └─► entry = NewLogEntryWithPhase(ballot, 100, req, false, "P")
    │   │   └─► entry.AcceptedBy[node1] = true
    │   │   └─► log[100] = entry
    │   │
    │   ├─► handle2PCPhase(entry, "P")  ⚠️ CALLED ON LEADER!
    │   │   ├─► n.balanceMu.Lock()
    │   │   ├─► twoPCWAL[txnID][1001] = 100  (save old balance)
    │   │   │   └─► log: "Saved sender WAL[1001]=100"
    │   │   └─► n.balanceMu.Unlock()
    │   │
    │   ├─► Broadcast ACCEPT to peers (node 2, node 3)
    │   │   └─► AcceptRequest {
    │   │           ballot, seq=100, req, isNoOp=false,
    │   │           phase="P"  ✓ Phase marker included!
    │   │       }
    │   │
    │   └─► Wait for quorum (2/3 nodes)
    │       └─► ✅ Quorum achieved!
    │
    ├─► Mark entry as COMMITTED (entry.Status = "C")
    │
    ├─► Broadcast COMMIT to peers (node 2, node 3)
    │   └─► CommitRequest {
    │           ballot, seq=100, req, isNoOp=false,
    │           phase="P"  ✓ Phase marker included!
    │       }
    │
    ├─► Execute transaction (executeTransaction)
    │   ├─► ATOMIC: Check balance & deduct (TOCTOU FIX)
    │   │   ├─► n.balanceMu.Lock()
    │   │   ├─► Check: balance[1001] >= 10? → YES (100)
    │   │   ├─► Deduct: balance[1001] = 100 - 10 = 90
    │   │   ├─► entry.Result = SUCCESS
    │   │   └─► n.balanceMu.Unlock()
    │   │
    │   ├─► Update log: entry.Status = "E", lastExecuted = 100
    │   └─► Save balance to disk
    │
    └─► Return (reply, seq=100, nil) to goroutine channel

┌─────────────────────────────────────────────────────────────────────────────────┐
│  GOROUTINE 2: Send PREPARE to Participant                                       │
└─────────────────────────────────────────────────────────────────────────────────┘

COORDINATOR → PARTICIPANT
    │
    └─► RPC: TwoPCPrepare(TwoPCPrepareRequest {
            TransactionId: "2pc-client1-123456789",
            Transaction: {sender:1001, receiver:3001, amount:10},
            ClientId: "client1",
            Timestamp: 123456789,
            CoordinatorId: 1
        })

        ┌─────────────────────────────────────────────────────────────────┐
        │  PARTICIPANT (Node 4 - Cluster 2 Leader)                        │
        └─────────────────────────────────────────────────────────────────┘
        
        PARTICIPANT receives PREPARE
            │
            ├─► ATOMIC Check & Lock
            │   ├─► n.balanceMu.Lock()
            │   ├─► Check: is item 3001 locked? → NO
            │   ├─► Lock item 3001
            │   ├─► oldBalance = balance[3001] = 50
            │   ├─► Save to twoPCState.transactions[txnID]:
            │   │   └─► Phase: "PREPARE"
            │   │   └─► LockedItems: [3001]
            │   │   └─► WALEntries: {3001: 50}
            │   └─► n.balanceMu.Unlock()
            │
            ├─► Run Paxos in Cluster 2 with phase="P"
            │   │
            │   ├─► processAsLeaderWithPhaseAndSeq(req, "P", 0)
            │   │   ├─► Allocate NEW seq = 200
            │   │   ├─► Create LogEntry with Phase="P"
            │   │   ├─► handle2PCPhase(entry, "P")
            │   │   │   └─► twoPCWAL[txnID][3001] = 50
            │   │   │
            │   │   ├─► Broadcast ACCEPT to peers (node 5, node 6)
            │   │   │   └─► AcceptRequest {phase="P"}
            │   │   │
            │   │   └─► Wait for quorum → ✅ Achieved!
            │   │
            │   ├─► Broadcast COMMIT to peers
            │   │   └─► CommitRequest {phase="P"}
            │   │
            │   ├─► Execute transaction
            │   │   ├─► Credit: balance[3001] = 50 + 10 = 60
            │   │   └─► entry.Result = SUCCESS
            │   │
            │   └─► Return (reply, seq=200, nil)
            │
            ├─► Save PrepareSeq = 200 for reuse
            │
            └─► Return TwoPCPrepareReply {
                    Success: true,
                    TransactionId: txnID,
                    Message: "prepared",
                    ParticipantId: 4
                }

┌─────────────────────────────────────────────────────────────────────────────────┐
│  COORDINATOR: Wait for BOTH goroutines                                          │
└─────────────────────────────────────────────────────────────────────────────────┘

COORDINATOR
    │
    ├─► select {
    │       case coordResult = <-coordChan:  ✅ Own Paxos complete (seq=100)
    │       case partResult = <-partChan:    ✅ Participant PREPARED
    │   }
    │
    └─► ✅ BOTH COMPLETE! → Proceed to COMMIT

STATE AFTER PREPARE PHASE:
┌─────────────────────────────────────────────────────────────────────────────────┐
│ Cluster 1 (Coordinator):                  Cluster 2 (Participant):             │
│   • Log[100] = {phase:"P", executed}       • Log[200] = {phase:"P", executed}  │
│   • twoPCWAL[txnID][1001] = 100            • twoPCWAL[txnID][3001] = 50        │
│   • balance[1001] = 90 (DEDUCTED)          • balance[3001] = 60 (CREDITED)     │
│   • item 1001 LOCKED                       • item 3001 LOCKED                   │
│   • PrepareSeq = 100                       • PrepareSeq = 200                   │
└─────────────────────────────────────────────────────────────────────────────────┘

════════════════════════════════════════════════════════════════════════════════════════
                     PHASE 2: COMMIT (SEQUENCE NUMBER REUSE!)
════════════════════════════════════════════════════════════════════════════════════════

COORDINATOR
    │
    ├─► Get saved PrepareSeq = 100
    │
    ├─► processAsLeaderWithPhaseAndSeq(req, phase="C", seq=100)  ⚠️ REUSE SEQ!
    │   │
    │   ├─► log: "REUSING seq=100"  (NOT allocating new!)
    │   │
    │   ├─► Update existing entry at seq=100
    │   │   └─► log[100].Phase = "C"  (change from "P" to "C")
    │   │   └─► log[100].Status = "A" (re-accept)
    │   │
    │   ├─► handle2PCPhase(entry, "C")  ⚠️ ALL NODES!
    │   │   ├─► n.balanceMu.Lock()
    │   │   ├─► DELETE twoPCWAL[txnID]  (no rollback needed!)
    │   │   │   └─► log: "Deleted WAL (changes committed)"
    │   │   └─► n.balanceMu.Unlock()
    │   │
    │   ├─► Broadcast ACCEPT to peers
    │   │   └─► AcceptRequest {seq=100, phase="C"}  ✓ Same seq!
    │   │
    │   ├─► Wait for quorum → ✅
    │   │
    │   └─► Broadcast COMMIT to peers
    │       └─► CommitRequest {seq=100, phase="C"}
    │
    └─► Send COMMIT to participant

        ┌─────────────────────────────────────────────────────────────────┐
        │  Send COMMIT to Participant                                     │
        └─────────────────────────────────────────────────────────────────┘
        
        COORDINATOR → PARTICIPANT
            │
            └─► RPC: TwoPCCommit(TwoPCCommitRequest {
                    TransactionId: txnID,
                    CoordinatorId: 1
                })

                PARTICIPANT
                    │
                    ├─► Get saved PrepareSeq = 200
                    │
                    ├─► processAsLeaderWithPhaseAndSeq(req, "C", 200)  ⚠️ REUSE!
                    │   ├─► log: "REUSING seq=200"
                    │   ├─► Update log[200].Phase = "C"
                    │   ├─► handle2PCPhase(entry, "C")
                    │   │   └─► DELETE twoPCWAL[txnID]
                    │   ├─► Broadcast ACCEPT {seq=200, phase="C"}
                    │   └─► Broadcast COMMIT {seq=200, phase="C"}
                    │
                    ├─► Cleanup: Release lock on item 3001
                    │
                    └─► Return TwoPCCommitReply {Success: true}

┌─────────────────────────────────────────────────────────────────────────────────┐
│  COORDINATOR: Final Cleanup                                                     │
└─────────────────────────────────────────────────────────────────────────────────┘

COORDINATOR
    │
    ├─► cleanup2PCCoordinator(txnID, commit=true)
    │   ├─► Release lock on item 1001  🔓
    │   └─► Delete twoPCState.transactions[txnID]
    │
    ├─► Cache result for exactly-once semantics
    │   └─► clientLastReply[clientID] = {Success:true, Result:SUCCESS}
    │
    └─► Return SUCCESS to client  ✅

════════════════════════════════════════════════════════════════════════════════════════
                              FOLLOWER NODES (Cluster 1)
════════════════════════════════════════════════════════════════════════════════════════

NODE 2, NODE 3 (Followers in Cluster 1)
    │
    ├─► Receive ACCEPT {seq=100, phase="P"}
    │   ├─► Create entry = NewLogEntryWithPhase(..., "P")
    │   ├─► Store: log[100] = entry
    │   ├─► handle2PCPhase(entry, "P")  ⚠️ FOLLOWERS TOO!
    │   │   └─► twoPCWAL[txnID][1001] = 100  (save WAL!)
    │   └─► Reply: AcceptedReply{Success:true}
    │
    ├─► Receive COMMIT {seq=100, phase="P"}
    │   ├─► Update: log[100].Phase = "P"
    │   ├─► Execute transaction
    │   │   └─► balance[1001] = 90
    │   └─► NO handle2PCPhase call here (only for 'C' and 'A')
    │
    ├─► Receive ACCEPT {seq=100, phase="C"}
    │   ├─► Update: log[100].Phase = "C"
    │   └─► Reply: AcceptedReply{Success:true}
    │
    └─► Receive COMMIT {seq=100, phase="C"}
        ├─► handle2PCPhase(entry, "C")  ⚠️ FOLLOWERS TOO!
        │   └─► DELETE twoPCWAL[txnID]  (commit WAL!)
        └─► Transaction complete!

════════════════════════════════════════════════════════════════════════════════════════
                         ABORT SCENARIO (If PREPARE Fails)
════════════════════════════════════════════════════════════════════════════════════════

COORDINATOR (if participant PREPARE fails)
    │
    ├─► coordinatorAbort(txnID, ...)
    │   │
    │   ├─► Get PrepareSeq = 100
    │   │
    │   ├─► processAsLeaderWithPhaseAndSeq(req, "A", 100)  ⚠️ REUSE SEQ!
    │   │   ├─► Update log[100].Phase = "A"
    │   │   ├─► handle2PCPhase(entry, "A")
    │   │   │   ├─► twoPCWAL[txnID][1001] = 100
    │   │   │   ├─► ROLLBACK: balance[1001] = 100  (restore!)
    │   │   │   │   └─► log: "Rolled back item 1001: 90 → 100"
    │   │   │   └─► DELETE twoPCWAL[txnID]
    │   │   │
    │   │   └─► Broadcast ACCEPT/COMMIT {seq=100, phase="A"}
    │   │
    │   ├─► Send ABORT to participant
    │   │   └─► TwoPCAbort(txnID, reason)
    │   │       └─► Participant also runs phase="A" with rollback
    │   │
    │   └─► cleanup2PCCoordinator(txnID, commit=false)
    │       ├─► ROLLBACK using WAL (already done in handle2PCPhase)
    │       ├─► Release lock on item 1001
    │       └─► Delete transaction state
    │
    └─► Return FAILED to client  ❌

════════════════════════════════════════════════════════════════════════════════════════
                            KEY FEATURES SUMMARY
════════════════════════════════════════════════════════════════════════════════════════

✅ 1. PARALLEL PREPARE EXECUTION
   • Coordinator's Paxos + Participant PREPARE run simultaneously
   • Use goroutines + channels to wait for BOTH
   • Significant performance improvement

✅ 2. PHASE MARKERS IN PAXOS
   • Every AcceptRequest/CommitRequest has phase field ("P", "C", "A", "")
   • LogEntry tracks phase
   • Enables proper 2PC semantics

✅ 3. SEQUENCE NUMBER REUSE
   • PREPARE: allocate NEW sequence (e.g., 100)
   • COMMIT: REUSE same sequence (100)
   • ABORT: REUSE same sequence (100)
   • Critical for maintaining transaction atomicity

✅ 4. WAL ON ALL NODES (twoPCWAL)
   • Not just leader - ALL nodes maintain WAL
   • Enables rollback on any node (even after leader failure)
   • handle2PCPhase() called on Accept AND Commit

✅ 5. TOCTOU FIX
   • Check balance AND deduct within SAME lock
   • Prevents race: check → (interrupt) → deduct
   • Applied to both coordinator and participant

✅ 6. COMPLETE STATE MANAGEMENT
   • TwoPCState tracks active transactions (leader only)
   • twoPCWAL tracks balances (ALL nodes)
   • PrepareSeq saved for reuse
   • LockedItems tracked for cleanup

✅ 7. EXACTLY-ONCE SEMANTICS
   • Duplicate detection (clientID + timestamp)
   • Result caching (clientLastReply)
   • Retries return cached result

✅ 8. PROPER CLEANUP
   • Release locks (coordinator + participant)
   • Delete WAL on COMMIT
   • Rollback WAL on ABORT
   • Clean transaction state

════════════════════════════════════════════════════════════════════════════════════════
                          TIMING DIAGRAM
════════════════════════════════════════════════════════════════════════════════════════

Time →
  0ms  Client sends transaction
       │
  1ms  Coordinator: check duplicate, check balance, lock item 1001
       │
  2ms  ┌─────────────────────────────┬──────────────────────────────┐
       │ GOROUTINE 1                  │ GOROUTINE 2                  │
       │ Coordinator Paxos            │ Send PREPARE to Participant  │
       │                              │                              │
  3ms  │ Allocate seq=100             │ RPC call started             │
  4ms  │ ACCEPT broadcast             │ ...waiting...                │
  5ms  │ Wait for quorum              │ Participant receives         │
  6ms  │ ✅ Quorum achieved           │ Participant locks item 3001  │
  7ms  │ COMMIT broadcast             │ Participant runs Paxos       │
  8ms  │ Execute: balance[1001]=90    │ Participant executes         │
  9ms  │ Return to channel            │ Return to channel            │
       └─────────────────────────────┴──────────────────────────────┘
       │
 10ms  Coordinator: BOTH complete! → Proceed to COMMIT
       │
 11ms  Coordinator: Run Paxos phase="C" seq=100 (REUSE!)
 12ms  Coordinator: ACCEPT broadcast (phase="C")
 13ms  Coordinator: COMMIT broadcast (phase="C")
 14ms  Coordinator: Send COMMIT to Participant
       │
 15ms  Participant: Run Paxos phase="C" seq=200 (REUSE!)
 16ms  Participant: ACCEPT/COMMIT broadcast
 17ms  Participant: Cleanup, release lock
 18ms  Participant: Return TwoPCCommitReply
       │
 19ms  Coordinator: Cleanup, release lock
 20ms  Coordinator: Return SUCCESS to client  ✅

Total: ~20ms for cross-shard transaction

════════════════════════════════════════════════════════════════════════════════════════
                        DATA STRUCTURES
════════════════════════════════════════════════════════════════════════════════════════

type Node struct {
    // ... other fields ...
    
    // 2PC State (Leader only)
    twoPCState TwoPCState  // Active transactions
    
    // WAL (ALL nodes - leader + followers)
    twoPCWAL map[string]map[int32]int32  // txnID → (itemID → oldBalance)
}

type TwoPCState struct {
    mu           sync.RWMutex
    transactions map[string]*TwoPCTransaction
}

type TwoPCTransaction struct {
    TxnID       string                 // "2pc-client1-123456789"
    Transaction *pb.Transaction        // Sender, Receiver, Amount
    ClientID    string                 // "client1"
    Timestamp   int64                  // 123456789
    Phase       string                 // "PREPARE", "COMMIT", "ABORT"
    PrepareSeq  int32                  // 100 (saved for reuse!)
    Prepared    bool                   // Participant prepared?
    LockedItems []int32                // [1001] or [3001]
    WALEntries  map[int32]int32        // {1001: 100} - old balances
    CreatedAt   time.Time
    LastContact time.Time
}

type LogEntry struct {
    Ballot     *Ballot
    SeqNum     int32
    Request    *pb.TransactionRequest
    IsNoOp     bool
    Status     string  // "A", "C", "E"
    AcceptedBy map[int32]bool
    Phase      string        // "P", "C", "A", ""  ⚠️ NEW!
    Result     pb.ResultType // SUCCESS, INSUFFICIENT_BALANCE, FAILED  ⚠️ NEW!
}

════════════════════════════════════════════════════════════════════════════════════════
                         IMPLEMENTATION FILES
════════════════════════════════════════════════════════════════════════════════════════

internal/node/twopc.go (886 lines)
    • TwoPCCoordinator() - Main coordinator logic
    • TwoPCPrepare() - Participant PREPARE handler
    • TwoPCCommit() - Participant COMMIT handler
    • TwoPCAbort() - Participant ABORT handler
    • handle2PCPhase() - WAL management (ALL nodes)
    • processAsLeaderWithPhaseAndSeq() - Paxos with phase & seq reuse
    • cleanup functions

internal/node/consensus.go (1320 lines)
    • Accept() - Modified to handle phase markers
    • Commit() - Modified to handle phase markers
    • executeTransaction() - TOCTOU fix (atomic check+deduct)

internal/node/node.go (1158 lines)
    • Node struct with twoPCState, twoPCWAL
    • DataItemLock for fine-grained locking

internal/types/log_entry.go (42 lines)
    • Phase field
    • Result field
    • NewLogEntryWithPhase() constructor

proto/paxos.proto
    • Phase field in AcceptRequest
    • Phase field in CommitRequest

proto/paxos.pb.go
    • GetPhase() methods

════════════════════════════════════════════════════════════════════════════════════════
                         ⚠️  KNOWN BUG (FOR YOU TO FIX!)
════════════════════════════════════════════════════════════════════════════════════════

Line 80 in internal/node/twopc.go:
    receiverLeader := n.config.GetLeaderNodeForCluster(receiverCluster)
                      ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
                      STATIC! Returns configured leader, not actual leader!

Problem:
    • Config says node 4 is leader
    • Actual leader is node 5 (after election)
    • Coordinator sends PREPARE to node 4
    • Node 4 can't achieve quorum → FAIL

Your Task:
    Implement dynamic leader discovery!

════════════════════════════════════════════════════════════════════════════════════════
```

## Summary

This implementation has **ALL** the 2PC features working correctly:
- ✅ Parallel execution (performance)
- ✅ Phase markers (correct semantics)
- ✅ Sequence reuse (atomicity)
- ✅ WAL on all nodes (fault tolerance)
- ✅ TOCTOU fix (correctness)
- ✅ Complete state management
- ✅ Proper cleanup and rollback

**Only missing**: Dynamic leader discovery (static config bug at line 80).
