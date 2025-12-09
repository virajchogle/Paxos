# 2PC Lock Timeline - Coordinator Cluster
## Detailed Analysis of Lock Acquisition and Release

```
═════════════════════════════════════════════════════════════════════════════════════
                    COORDINATOR LOCK LIFECYCLE
═════════════════════════════════════════════════════════════════════════════════════

Transaction: 1001 → 3001, amount: 10
Coordinator: Node 1 (Cluster 1) - owns item 1001
Participant: Node 4 (Cluster 2) - owns item 3001

Timeline →
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

T=0ms    Client sends transaction to Coordinator (Node 1)
         │
         ↓

T=1ms    ┌─────────────────────────────────────────────────────────────────┐
         │ TwoPCCoordinator() called                                        │
         │ File: internal/node/twopc.go                                     │
         │ Lines: 40-305                                                    │
         └─────────────────────────────────────────────────────────────────┘
         │
         ├─► Check duplicate request
         │
         ↓

T=2ms    ┌─────────────────────────────────────────────────────────────────┐
         │ LOCK ACQUISITION (Line ~85-110)                                 │
         │                                                                  │
         │ n.balanceMu.Lock()  🔒                                          │
         │   │                                                              │
         │   ├─► Check: is item 1001 locked? → NO                          │
         │   ├─► Check: balance[1001] >= 10? → YES (100 >= 10)             │
         │   │                                                              │
         │   ├─► 🔒 LOCK ACQUIRED!                                         │
         │   │   n.locks[1001] = &DataItemLock{                            │
         │   │       clientID: "client1",                                   │
         │   │       timestamp: 123456789,                                  │
         │   │       lockedAt: time.Now()                                   │
         │   │   }                                                          │
         │   │                                                              │
         │   ├─► Save to twoPCState.transactions[txnID]:                   │
         │   │     LockedItems: [1001]                                     │
         │   │     WALEntries: {1001: 100}                                 │
         │   │                                                              │
         │   └─► n.balanceMu.Unlock()                                      │
         │                                                                  │
         └─────────────────────────────────────────────────────────────────┘

         Log: "🔒 Locking sender item 1001 (balance: 100)"

         ⚠️  LOCK IS NOW HELD! It will be held through:
             • PREPARE phase
             • COMMIT phase
             • Until cleanup is called

T=3ms    ┌─────────────────────────────────────────────────────────────────┐
         │ PHASE 1: PREPARE (Lines ~150-242)                               │
         │                                                                  │
         │ Spawn 2 parallel goroutines:                                    │
         │   • Goroutine 1: Coordinator's Paxos (Phase=P, seq=100)         │
         │   • Goroutine 2: Send PREPARE to participant                    │
         │                                                                  │
         │ 🔒 LOCK STILL HELD ON ITEM 1001                                 │
         └─────────────────────────────────────────────────────────────────┘

T=4-8ms  │
         │ Coordinator runs Paxos Phase=P
         │   • ACCEPT broadcast
         │   • Wait for quorum ✅
         │   • COMMIT broadcast
         │   • Execute: balance[1001] = 100 - 10 = 90
         │
         │ Participant PREPARES
         │   • Locks item 3001
         │   • Runs Paxos Phase=P
         │   • Executes: balance[3001] = 50 + 10 = 60
         │   • Returns PREPARED reply
         │
         │ 🔒 LOCK STILL HELD ON ITEM 1001
         │
         ↓

T=9ms    Both goroutines complete!
         ✅ Coordinator Paxos done (seq=100)
         ✅ Participant PREPARED

         Log: "✅ PREPARE phase complete on both clusters"

         🔒 LOCK STILL HELD ON ITEM 1001
         ↓

T=10ms   ┌─────────────────────────────────────────────────────────────────┐
         │ PHASE 2: COMMIT (Lines ~245-305)                                │
         │                                                                  │
         │ Get saved PrepareSeq = 100                                      │
         │                                                                  │
         │ Run Paxos Phase=C with seq=100 (REUSE!)                         │
         │   • ACCEPT broadcast (Phase=C)                                  │
         │   • Wait for quorum ✅                                          │
         │   • COMMIT broadcast (Phase=C)                                  │
         │   • handle2PCPhase("C") → DELETE twoPCWAL                       │
         │                                                                  │
         │ 🔒 LOCK STILL HELD ON ITEM 1001                                 │
         └─────────────────────────────────────────────────────────────────┘

         Log: "✅ COMMIT replicated in coordinator cluster"

T=12ms   Send COMMIT to participant (Line 268-291)
         │
         ├─► TwoPCCommit(txnID)  ──────→  Participant
         │                                  (Participant commits Phase=C)
         │
         ↓

T=13ms   Participant returns TwoPCCommitReply {Success: true}

         Log: "✅ Participant ACK received"

         🔒 LOCK STILL HELD ON ITEM 1001
         │
         ↓

T=14ms   ┌─────────────────────────────────────────────────────────────────┐
         │ CLEANUP CALLED! (Line 300)                                      │
         │                                                                  │
         │ n.cleanup2PCCoordinator(txnID, commit=true)                     │
         │                                                                  │
         │ File: internal/node/twopc.go                                    │
         │ Lines: 358-388                                                  │
         └─────────────────────────────────────────────────────────────────┘
         │
         ↓

T=15ms   ┌─────────────────────────────────────────────────────────────────┐
         │ LOCK RELEASE! (Lines 377-384)                                   │
         │                                                                  │
         │ n.balanceMu.Lock()  🔒                                          │
         │   │                                                              │
         │   ├─► For each itemID in txState.LockedItems:  [1001]           │
         │   │                                                              │
         │   │   lock = n.locks[1001]                                      │
         │   │   if lock.clientID == "client1":                            │
         │   │                                                              │
         │   │       🔓 LOCK RELEASED!                                     │
         │   │       delete(n.locks, 1001)                                 │
         │   │                                                              │
         │   │       Log: "🔓 Releasing lock on item 1001"                 │
         │   │                                                              │
         │   └─► Delete twoPCState.transactions[txnID]                     │
         │                                                                  │
         │   └─► n.balanceMu.Unlock()                                      │
         │                                                                  │
         └─────────────────────────────────────────────────────────────────┘

         ✅ LOCK RELEASED!

T=16ms   Cache result for exactly-once semantics
         Return SUCCESS to client

         Log: "✅ TRANSACTION COMMITTED"

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

SUMMARY:
─────────
Lock Acquired:  T=2ms  (before PREPARE)
Lock Released:  T=15ms (after COMMIT complete)

Total Lock Duration: ~13ms

Lock held through:
  ✅ PREPARE phase (Paxos + execution)
  ✅ COMMIT phase (Paxos)
  ✅ Participant COMMIT notification
  ❌ Released after all 2PC phases complete

═════════════════════════════════════════════════════════════════════════════════════
                         WHEN ARE LOCKS RELEASED?
═════════════════════════════════════════════════════════════════════════════════════

cleanup2PCCoordinator() is called in THREE scenarios:

1️⃣  SUCCESS PATH (Line 300)
    ─────────────────────────
    After COMMIT phase completes and participant ACK received
    │
    ├─► Coordinator's COMMIT Paxos ✅
    ├─► Participant COMMIT sent ✅
    ├─► Participant ACK received (or timeout with retry) ✅
    │
    └─► cleanup2PCCoordinator(txnID, commit=true)
        └─► 🔓 Release locks
        └─► Delete transaction state
        └─► Keep WAL changes (no rollback)

2️⃣  ABORT PATH (Line 351)
    ──────────────────────
    If PREPARE fails or COMMIT consensus fails
    │
    ├─► Run ABORT Paxos (Phase=A) with rollback
    │
    └─► cleanup2PCCoordinator(txnID, commit=false)
        └─► 🔄 Rollback using WAL (balance[1001] = 100)
        └─► 🔓 Release locks
        └─► Delete transaction state

3️⃣  EARLY ABORT (Line 142)
    ────────────────────────
    If participant is unreachable
    │
    └─► cleanup2PCCoordinator(txnID, commit=false)
        └─► 🔄 Rollback using WAL
        └─► 🔓 Release locks
        └─► Delete transaction state

═════════════════════════════════════════════════════════════════════════════════════
                          LOCK RELEASE CODE
═════════════════════════════════════════════════════════════════════════════════════

Location: internal/node/twopc.go, Lines 377-384

func (n *Node) cleanup2PCCoordinator(txnID string, commit bool) {
    n.balanceMu.Lock()
    defer n.balanceMu.Unlock()

    txState, exists := n.twoPCState.transactions[txnID]
    if !exists {
        return
    }

    if !commit && len(txState.WALEntries) > 0 {
        // ROLLBACK: Restore old balances from WAL
        for itemID, oldBalance := range txState.WALEntries {
            n.balances[itemID] = oldBalance  // balance[1001] = 100
        }
    }

    // ⬇️  LOCK RELEASE HERE! ⬇️
    for _, itemID := range txState.LockedItems {
        lock, exists := n.locks[itemID]
        if exists && lock.clientID == txState.ClientID {
            log.Printf("Node %d: 2PC[%s]: 🔓 Releasing lock on item %d", 
                       n.id, txnID, itemID)
            delete(n.locks, itemID)  // 🔓 UNLOCK!
        }
    }

    // Delete transaction state
    delete(n.twoPCState.transactions, txnID)
}

═════════════════════════════════════════════════════════════════════════════════════
                       WHY LOCKS ARE HELD SO LONG?
═════════════════════════════════════════════════════════════════════════════════════

This is CORRECT for 2PC protocol! 🎯

Reason 1: Transaction Already Executed in PREPARE
─────────────────────────────────────────────────
  • balance[1001] changed from 100 → 90 in PREPARE phase
  • If we release lock before COMMIT, another transaction could:
    ✅ Read old value (90)
    ❌ Modify it (e.g., 90 → 80)
    ❌ Then ABORT happens → rollback to 100
    ❌ But the other transaction's change (90→80) is lost!

Reason 2: COMMIT Must Be Atomic
────────────────────────────────
  • Lock ensures no one else touches item 1001 while we're:
    ✅ Running COMMIT Paxos
    ✅ Notifying participant
    ✅ Waiting for ACK

Reason 3: Prevent Cascading Aborts
───────────────────────────────────
  • If COMMIT fails, we need to ABORT with rollback
  • Lock prevents other transactions from seeing uncommitted state
  • This is "strict 2PL" (two-phase locking)

═════════════════════════════════════════════════════════════════════════════════════
                      COMPARISON: FOLLOWER NODES
═════════════════════════════════════════════════════════════════════════════════════

⚠️  IMPORTANT: Follower nodes (Node 2, Node 3) DON'T hold locks!

Why?
────
Only the LEADER (coordinator) needs to enforce mutual exclusion.
Followers replicate the transaction but don't need to block other transactions.

Leader (Node 1):
  • Acquires lock on item 1001 at T=2ms
  • Holds lock through PREPARE + COMMIT
  • Releases lock at T=15ms after cleanup

Followers (Node 2, Node 3):
  • Receive ACCEPT (Phase=P)
  • Execute transaction: balance[1001] = 90
  • Save WAL: twoPCWAL[txnID][1001] = 100
  • Receive ACCEPT (Phase=C)
  • Delete WAL: twoPCWAL[txnID]
  • ❌ NO LOCKS acquired or released!

═════════════════════════════════════════════════════════════════════════════════════
                        ABORT SCENARIO TIMELINE
═════════════════════════════════════════════════════════════════════════════════════

If participant PREPARE fails:

T=0ms    Lock acquired on item 1001
T=1ms    PREPARE phase starts (parallel)
T=5ms    Coordinator Paxos complete ✅
T=6ms    Participant PREPARE fails ❌ (e.g., item locked)
         │
         ↓
T=7ms    coordinatorAbort() called
         │
         ├─► Run Paxos Phase=A (ABORT)
         │     • ACCEPT broadcast (Phase=A)
         │     • COMMIT broadcast (Phase=A)
         │     • handle2PCPhase("A") → ROLLBACK
         │         balance[1001] = 100 (restore!)
         │         DELETE twoPCWAL
         │
         ├─► Send ABORT to participant
         │
         └─► cleanup2PCCoordinator(txnID, commit=false)
             │
             ├─► Rollback: balance[1001] = 100
             ├─► 🔓 Release lock on item 1001
             └─► Delete transaction state
         │
T=10ms   ❌ ABORT complete
         🔓 Lock released
         Return FAILED to client

═════════════════════════════════════════════════════════════════════════════════════
                         KEY TAKEAWAYS
═════════════════════════════════════════════════════════════════════════════════════

✅ Locks acquired EARLY (before PREPARE)
✅ Locks held through BOTH PREPARE and COMMIT phases
✅ Locks released LATE (after COMMIT complete and participant ACK)
✅ Total lock duration: ~13-15ms for successful transaction
✅ This is CORRECT for 2PC protocol (strict two-phase locking)
✅ Ensures atomicity and isolation

Lock Release Triggers:
  1️⃣  Success: After COMMIT Paxos + participant ACK (Line 300)
  2️⃣  Abort: After ABORT Paxos + rollback (Line 351)
  3️⃣  Early abort: If participant unreachable (Line 142)

═════════════════════════════════════════════════════════════════════════════════════
```

## Answer to Your Question

**When are locks released on the coordinator cluster?**

**Answer**: Locks are released in `cleanup2PCCoordinator()` which is called **AFTER the COMMIT phase completes**, specifically:

1. ✅ After coordinator runs COMMIT Paxos (Phase=C) with all followers
2. ✅ After sending COMMIT message to participant
3. ✅ After receiving participant ACK (or timeout with retries)

**Timeline**: Lock held for ~13-15ms total
- Acquired at T=2ms (before PREPARE)
- Released at T=15ms (after COMMIT complete)

**This is correct!** The lock must be held through both phases because:
- Transaction is **executed in PREPARE** (balance already changed)
- Need to prevent other transactions from seeing uncommitted state
- If COMMIT fails, we need to ABORT and rollback
- This implements "strict two-phase locking" for 2PC

The code is at **lines 377-384** in `internal/node/twopc.go`! 🎯
