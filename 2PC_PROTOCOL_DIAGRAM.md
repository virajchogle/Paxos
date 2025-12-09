# Two-Phase Commit Protocol - Visual Diagram

## 🎯 Complete Protocol Flow

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                         CROSS-SHARD TRANSACTION                                 │
│                     Transfer 100: Item 3001 → Item 6001                         │
│                     (Cluster 1 → Cluster 2)                                     │
└─────────────────────────────────────────────────────────────────────────────────┘

        CLIENT                 COORDINATOR              PARTICIPANT
       (User)              (Cluster 1 - Node 1)      (Cluster 2 - Node 4)
          │                       │                         │
          │  S(3001,6001,100)     │                         │
          │ ─────────────────────>│                         │
          │                       │                         │
          
╔═══════════════════════════════════════════════════════════════════════════════╗
║                          PHASE 1: PREPARE                                     ║
╚═══════════════════════════════════════════════════════════════════════════════╝

    COORDINATOR CHECKS & LOCKS
    ──────────────────────────
          │                       │
          │                       │ 1️⃣  Check Conditions:
          │                       │     ✓ Item 3001 not locked?
          │                       │     ✓ Balance >= 100?
          │                       │
          │                       │ 2️⃣  Lock Item 3001
          │                       │     🔒 locks[3001] = {clientID, timestamp}
          │                       │
          │                       │ 3️⃣  Save to WAL
          │                       │     💾 WAL[3001] = 150 (old balance)
          │                       │
          
    COORDINATOR PAXOS ROUND 1
    ─────────────────────────
          │                       │
          │                       │ 4️⃣  Initiate Paxos (PREPARE Phase)
          │                       │     Marker: 'P'
          │                       │
          │                       ├────────────────────┐
          │                       │ ACCEPT(b,s,'P',tx) │
          │                       │ ─────────────────> │ Node 2
          │                       │                    │
          │                       │ ACCEPTED           │
          │                       │ <───────────────── │
          │                       │                    │
          │                       │ ACCEPT(b,s,'P',tx) │
          │                       │ ─────────────────> │ Node 3
          │                       │                    │
          │                       │ ACCEPTED           │
          │                       │ <───────────────── │
          │                       ├────────────────────┘
          │                       │
          │                       │ 5️⃣  Majority Achieved!
          │                       │     Execute: 3001.balance -= 100
          │                       │     (150 → 50)
          │                       │
          
    SEND PREPARE TO PARTICIPANT
    ───────────────────────────
          │                       │
          │                       │ 6️⃣  PREPARE(txnID, tx, clientID)
          │                       │ ═══════════════════════════════>│
          │                       │                                 │
          
    PARTICIPANT CHECKS & LOCKS
    ──────────────────────────
          │                       │                                 │
          │                       │                         7️⃣  Check:
          │                       │                             ✓ Item 6001 not locked?
          │                       │                                 │
          │                       │                         8️⃣  Lock Item 6001
          │                       │                             🔒 locks[6001] = {clientID}
          │                       │                                 │
          │                       │                         9️⃣  Save to WAL
          │                       │                             💾 WAL[6001] = 200 (old)
          │                       │                                 │
          
    PARTICIPANT PAXOS ROUND 1
    ─────────────────────────
          │                       │                                 │
          │                       │                         🔟 Initiate Paxos (PREPARE)
          │                       │                             Marker: 'P'
          │                       │                                 │
          │                       │                    ┌────────────┤
          │                       │         Node 5 <───┤ ACCEPT     │
          │                       │                    │ (b,s,'P')  │
          │                       │         ACCEPTED ──┤            │
          │                       │                    │            │
          │                       │         Node 6 <───┤ ACCEPT     │
          │                       │                    │ (b,s,'P')  │
          │                       │         ACCEPTED ──┤            │
          │                       │                    └────────────┤
          │                       │                                 │
          │                       │                         1️⃣1️⃣ Majority!
          │                       │                             Execute: 6001.balance += 100
          │                       │                             (200 → 300)
          │                       │                                 │
          
    SEND PREPARED
    ─────────────
          │                       │                                 │
          │                       │        PREPARED(txnID)          │
          │                       │ <═══════════════════════════════│
          │                       │                                 │
          │                       │ 1️⃣2️⃣ Received PREPARED!         │
          │                       │                                 │

╔═══════════════════════════════════════════════════════════════════════════════╗
║                          PHASE 2: COMMIT                                      ║
╚═══════════════════════════════════════════════════════════════════════════════╝

    COORDINATOR PAXOS ROUND 2
    ─────────────────────────
          │                       │
          │                       │ 1️⃣3️⃣ Initiate Paxos (COMMIT Phase)
          │                       │     Marker: 'C'
          │                       │
          │                       ├────────────────────┐
          │                       │ ACCEPT(b,s,'C',tx) │
          │                       │ ─────────────────> │ Node 2
          │                       │                    │
          │                       │ ACCEPTED           │
          │                       │ <───────────────── │
          │                       │                    │
          │                       │ ACCEPT(b,s,'C',tx) │
          │                       │ ─────────────────> │ Node 3
          │                       │                    │
          │                       │ ACCEPTED           │
          │                       │ <───────────────── │
          │                       ├────────────────────┘
          │                       │
          │                       │ 1️⃣4️⃣ COMMIT decision replicated!
          │                       │
          
    SEND COMMIT TO PARTICIPANT
    ──────────────────────────
          │                       │
          │                       │ 1️⃣5️⃣ COMMIT(txnID)
          │                       │ ═══════════════════════════════>│
          │                       │                                 │
          
    PARTICIPANT PAXOS ROUND 2
    ─────────────────────────
          │                       │                                 │
          │                       │                         1️⃣6️⃣ Paxos (COMMIT)
          │                       │                             Marker: 'C'
          │                       │                                 │
          │                       │                    ┌────────────┤
          │                       │         Node 5 <───┤ ACCEPT     │
          │                       │                    │ (b,s,'C')  │
          │                       │         ACCEPTED ──┤            │
          │                       │                    │            │
          │                       │         Node 6 <───┤ ACCEPT     │
          │                       │                    │ (b,s,'C')  │
          │                       │         ACCEPTED ──┤            │
          │                       │                    └────────────┤
          │                       │                                 │
          │                       │                         1️⃣7️⃣ COMMIT replicated
          │                       │                                 │
          
    CLEANUP PARTICIPANT
    ───────────────────
          │                       │                                 │
          │                       │                         1️⃣8️⃣ Cleanup:
          │                       │                             🔓 Release lock[6001]
          │                       │                             🗑️  Delete WAL[6001]
          │                       │                                 │
          
    SEND ACK
    ────────
          │                       │                                 │
          │                       │         ACK(txnID)              │
          │                       │ <═══════════════════════════════│
          │                       │                                 │
          
    CLEANUP COORDINATOR
    ───────────────────
          │                       │
          │                       │ 1️⃣9️⃣ Cleanup:
          │                       │     🔓 Release lock[3001]
          │                       │     🗑️  Delete WAL[3001]
          │                       │
          
    REPLY TO CLIENT
    ───────────────
          │                       │
          │  ✅ SUCCESS           │
          │ <─────────────────────│
          │                       │
          ✓                       ✓                                ✓
     TRANSACTION COMMITTED     COMMITTED                      COMMITTED


═══════════════════════════════════════════════════════════════════════════════

                    ALTERNATIVE FLOW: ABORT SCENARIO

═══════════════════════════════════════════════════════════════════════════════

    IF PARTICIPANT REJECTS (e.g., receiver locked):
    ───────────────────────────────────────────────

          │                       │
          │                       │ 6️⃣  PREPARE(txnID, tx)
          │                       │ ═══════════════════════════════>│
          │                       │                                 │
          │                       │                         7️⃣  Check:
          │                       │                             ❌ Item 6001 LOCKED!
          │                       │                                 │
          │                       │                         8️⃣  Paxos (ABORT)
          │                       │                             Marker: 'A'
          │                       │                             Replicate abort
          │                       │                                 │
          │                       │         ABORT(reason)           │
          │                       │ <═══════════════════════════════│
          │                       │                                 │
          
    COORDINATOR ABORTS
    ──────────────────
          │                       │
          │                       │ 9️⃣  Paxos (ABORT Phase)
          │                       │     Marker: 'A'
          │                       │     Replicate abort decision
          │                       │
          │                       │ 🔟 Rollback using WAL:
          │                       │     3001.balance = 150 (restore)
          │                       │
          │                       │ 1️⃣1️⃣ Cleanup:
          │                       │     🔓 Release lock[3001]
          │                       │     🗑️  Delete WAL[3001]
          │                       │
          │                       │ 1️⃣2️⃣ ABORT(txnID, reason)
          │                       │ ═══════════════════════════════>│
          │                       │                                 │
          │                       │                         1️⃣3️⃣ Cleanup:
          │                       │                             🔓 Release lock
          │                       │                                 │
          │                       │         ACK                     │
          │                       │ <═══════════════════════════════│
          │                       │                                 │
          │  ❌ FAILED            │
          │ <─────────────────────│
          │  (reason: locked)     │
          │                       │
          ✓                       ✓                                ✓
     TRANSACTION ABORTED       ABORTED                          ABORTED
```

---

## 📊 State Transitions

```
                              ┌──────────┐
                              │  CLIENT  │
                              │ REQUEST  │
                              └────┬─────┘
                                   │
                                   ▼
                        ┌──────────────────┐
                        │  COORDINATOR     │
                        │  PRE-CHECK       │
                        │  • Lock free?    │
                        │  • Balance OK?   │
                        └────┬─────┬───────┘
                             │     │
                    ✓ PASS   │     │ ✗ FAIL
                             │     └────────────┐
                             ▼                  ▼
                    ┌─────────────────┐  ┌──────────┐
                    │ LOCK SENDER     │  │  ABORT   │
                    │ SAVE TO WAL     │  │  EARLY   │
                    └────┬────────────┘  └────┬─────┘
                         │                    │
                         ▼                    │
                ┌────────────────────┐        │
                │ PAXOS ROUND 1 ('P')│        │
                │  • Replicate       │        │
                │  • Execute         │        │
                └────┬───────────────┘        │
                     │                        │
                     ▼                        │
            ┌────────────────────┐            │
            │ SEND PREPARE       │            │
            │ TO PARTICIPANT     │            │
            └────┬───────────────┘            │
                 │                            │
                 ▼                            │
        ┌────────────────────┐                │
        │  PARTICIPANT       │                │
        │  CHECK LOCK        │                │
        └────┬─────┬─────────┘                │
             │     │                          │
    ✓ FREE   │     │ ✗ LOCKED                │
             │     └──────────────┐           │
             ▼                    ▼           │
    ┌─────────────────┐  ┌────────────────┐  │
    │ LOCK RECEIVER   │  │ PAXOS ABORT    │  │
    │ SAVE TO WAL     │  │ SEND ABORT     │  │
    └────┬────────────┘  └────┬───────────┘  │
         │                    │               │
         ▼                    │               │
┌────────────────────┐        │               │
│ PAXOS ROUND 1 ('P')│        │               │
│  • Replicate       │        │               │
│  • Execute         │        │               │
└────┬───────────────┘        │               │
     │                        │               │
     ▼                        │               │
┌────────────────────┐        │               │
│ SEND PREPARED      │        │               │
└────┬───────────────┘        │               │
     │                        │               │
     │    ┌───────────────────┘               │
     │    │                                   │
     ▼    ▼                                   │
┌──────────────────────┐                      │
│ COORDINATOR          │                      │
│ DECISION             │                      │
└──┬────────────────┬──┘                      │
   │                │                         │
   │ PREPARED       │ ABORT                   │
   │                │                         │
   ▼                ▼                         │
┌──────────┐  ┌──────────┐                   │
│ PAXOS    │  │ PAXOS    │                   │
│ COMMIT   │  │ ABORT    │                   │
│  ('C')   │  │  ('A')   │                   │
└────┬─────┘  └────┬─────┘                   │
     │             │                         │
     ▼             ▼                         │
┌──────────┐  ┌──────────┐                   │
│ SEND     │  │ SEND     │                   │
│ COMMIT   │  │ ABORT    │                   │
└────┬─────┘  └────┬─────┘                   │
     │             │                         │
     ▼             ▼                         │
┌──────────────────────┐                     │
│ PARTICIPANT          │                     │
│ PAXOS ROUND 2        │                     │
│  ('C' or 'A')        │                     │
└────┬────────────┬────┘                     │
     │            │                          │
  COMMIT       ABORT                         │
     │            │                          │
     ▼            ▼                          │
┌─────────┐  ┌──────────┐                    │
│ RELEASE │  │ ROLLBACK │                    │
│ LOCKS   │  │ + RELEASE│                    │
│ DELETE  │  │ LOCKS    │                    │
│ WAL     │  │ DELETE   │                    │
└────┬────┘  │ WAL      │                    │
     │       └────┬─────┘                    │
     │            │                          │
     ▼            ▼                          │
┌────────────────────┐                       │
│ SEND ACK           │                       │
└────┬───────────────┘                       │
     │                                       │
     ▼                                       ▼
┌────────────────────┐              ┌─────────────┐
│ CLEANUP            │              │ REPLY TO    │
│ COORDINATOR        │              │ CLIENT      │
│ • Release locks    │              │ (FAILED)    │
│ • Delete WAL       │              └─────────────┘
└────┬───────────────┘
     │
     ▼
┌────────────────────┐
│ REPLY TO CLIENT    │
│ (SUCCESS)          │
└────────────────────┘
```

---

## 🗄️ Data Structures Flow

```
┌─────────────────────────────────────────────────────────────────┐
│                    COORDINATOR (Node 1)                         │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  TwoPCState:                                                    │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ transactions[txnID] = {                                  │  │
│  │   TxnID: "2pc-client1-1234567890"                        │  │
│  │   Transaction: {Sender: 3001, Receiver: 6001, Amt: 100} │  │
│  │   ClientID: "client1"                                    │  │
│  │   Phase: "PREPARE" → "COMMIT"                            │  │
│  │   LockedItems: [3001]                                    │  │
│  │   WALEntries: {3001: 150}  // old balance                │  │
│  │ }                                                         │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                 │
│  Locks:                                                         │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ locks[3001] = {                                          │  │
│  │   clientID: "client1"                                    │  │
│  │   timestamp: 1234567890                                  │  │
│  │   lockedAt: 2025-12-09T10:30:00Z                        │  │
│  │ }                                                        │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                 │
│  Balances (During):                                             │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ balances[3001] = 50  (after debit)                       │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                 │
│  Paxos Log:                                                     │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ log[seq1] = {Status: "E", Phase: "P", Tx: {...}}        │  │
│  │ log[seq2] = {Status: "E", Phase: "C", Tx: {...}}        │  │
│  └──────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                    PARTICIPANT (Node 4)                         │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  TwoPCState:                                                    │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ transactions[txnID] = {                                  │  │
│  │   TxnID: "2pc-client1-1234567890"                        │  │
│  │   Transaction: {Sender: 3001, Receiver: 6001, Amt: 100} │  │
│  │   ClientID: "client1"                                    │  │
│  │   Phase: "PREPARE" → "COMMIT"                            │  │
│  │   LockedItems: [6001]                                    │  │
│  │   WALEntries: {6001: 200}  // old balance                │  │
│  │ }                                                         │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                 │
│  Locks:                                                         │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ locks[6001] = {                                          │  │
│  │   clientID: "client1"                                    │  │
│  │   timestamp: 1234567890                                  │  │
│  │   lockedAt: 2025-12-09T10:30:01Z                        │  │
│  │ }                                                        │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                 │
│  Balances (During):                                             │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ balances[6001] = 300  (after credit)                     │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                 │
│  Paxos Log:                                                     │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ log[seq1] = {Status: "E", Phase: "P", Tx: {...}}        │  │
│  │ log[seq2] = {Status: "E", Phase: "C", Tx: {...}}        │  │
│  └──────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
```

---

## 🔄 Message Sequence

```
Time    Coordinator (C1)              Network                Participant (C2)
────────────────────────────────────────────────────────────────────────────────

T0      Receive client request
        S(3001, 6001, 100)

T1      Check: locks[3001] ✓
        Check: balance[3001] >= 100 ✓

T2      Lock: locks[3001] = {...}
        WAL: WAL[3001] = 150

T3      Paxos Round 1 ('P')
        → ACCEPT to Node 2, 3

T4      ← ACCEPTED (majority)
        Execute: balance[3001] -= 100

T5      PREPARE ──────────────────────────────────────────────> Receive PREPARE

T6                                                              Check: locks[6001] ✓

T7                                                              Lock: locks[6001] = {...}
                                                                WAL: WAL[6001] = 200

T8                                                              Paxos Round 1 ('P')
                                                                → ACCEPT to Node 5, 6

T9                                                              ← ACCEPTED (majority)
                                                                Execute: balance[6001] += 100

T10     <────────────────────────────────────────── PREPARED

T11     Paxos Round 2 ('C')
        → ACCEPT to Node 2, 3

T12     ← ACCEPTED (majority)

T13     COMMIT ────────────────────────────────────────────────> Receive COMMIT

T14                                                              Paxos Round 2 ('C')
                                                                 → ACCEPT to Node 5, 6

T15                                                              ← ACCEPTED (majority)

T16                                                              Cleanup:
                                                                 • delete(locks[6001])
                                                                 • delete(WAL[6001])

T17     <──────────────────────────────────────────────── ACK

T18     Cleanup:
        • delete(locks[3001])
        • delete(WAL[3001])

T19     Reply SUCCESS → Client

═══════════════════════════════════════════════════════════════════════════════

        RESULT: Transaction committed across both clusters
                Item 3001: 150 → 50
                Item 6001: 200 → 300
```

---

## 📋 Key Features Illustrated

### 1. Two Rounds of Paxos per Cluster ✅
- **Round 1 (PREPARE)**: Both coordinator and participant run Paxos with marker 'P'
- **Round 2 (COMMIT/ABORT)**: Both run Paxos again with marker 'C' or 'A'

### 2. Lock Management ✅
- **Acquire**: During PREPARE phase, before Paxos
- **Hold**: Throughout both phases
- **Release**: After COMMIT/ABORT phase completes

### 3. Write-Ahead Log (WAL) ✅
- **Save**: Before execution (old balance)
- **Use**: For rollback on ABORT
- **Delete**: After successful COMMIT

### 4. Explicit Messages ✅
- **PREPARE**: Coordinator → Participant
- **PREPARED/ABORT**: Participant → Coordinator
- **COMMIT/ABORT**: Coordinator → Participant
- **ACK**: Participant → Coordinator

### 5. Atomicity ✅
- Both clusters commit or both abort
- No partial commits possible
- Rollback using WAL ensures consistency

