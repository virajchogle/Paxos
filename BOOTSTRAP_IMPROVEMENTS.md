# Bootstrap Mechanism Improvements: Complete Isolation Between Test Sets

## The Problem

As you correctly identified, the bootstrap mechanism wasn't providing complete isolation between test sets:

**Before**:
```
Test Set 1:
  • Node 7 learns Node 9 is leader (leaderID = 9)
  
Bootstrap (SetActive false → true):
  • Node 7 goes INACTIVE
  • leaderID is NOT cleared (only cleared if node was a leader!)
  • Node 7 comes back ACTIVE
  • Still has leaderID = 9 (stale information!) ❌
  
Test Set 2:
  • Node 7 "believes leader is 9" (from previous test set!)
  • Treating test sets as if they're connected
  • Not truly isolated ❌
```

## Your Requirements

> "the nodes should start with no knowledge of the previous action. the leaderID to be reset after every test case. maybe set to -1 or something and also the ballot numbers to be reset. the new test set should be treated as a new file."

**Absolutely correct!** Each test set should be completely fresh, as if nodes just started.

---

## The Fix

### Fix 1: Clear ALL Leader State on SetActive(false)

**Location**: `internal/node/consensus.go`, lines 1262-1268

**Before**:
```go
// Only clears leaderID if node WAS a leader
if !req.Active && wasLeader {
    n.isLeader = false
    n.leaderID = 0  // ← Only for leaders!
}
```

**After**:
```go
// BOOTSTRAP FIX: Clear leader state for ALL nodes
if !req.Active {
    n.isLeader = false
    n.leaderID = -1  // -1 = uninitialized (no knowledge)
    log.Printf("Node %d: ⚙️  Set to INACTIVE - clearing ALL leader state (leaderID → -1)", n.id)
}
```

**Key Changes**:
- Now clears `leaderID` for **ALL nodes** (not just leaders)
- Sets `leaderID = -1` (not 0) to indicate "uninitialized/no knowledge"
- Clear log message shows the reset

### Fix 2: Remove "Believes leader" Message

**Location**: `internal/node/consensus.go`, lines 1272-1288

**Before**:
```go
if req.Active {
    if wasActive {
        log.Printf("Node %d: ✅ Node already ACTIVE (leader=%d)", n.id, currentLeaderID)
    } else {
        log.Printf("Node %d: ✅ Node set to ACTIVE (was inactive)", n.id)
        if !wasLeader && currentLeaderID > 0 {
            log.Printf("Node %d: Believes leader is %d", n.id, currentLeaderID)  // ← Stale!
        }
    }
}
```

**After**:
```go
if req.Active {
    if wasActive {
        log.Printf("Node %d: ✅ Node already ACTIVE", n.id)
    } else {
        log.Printf("Node %d: ✅ Node set to ACTIVE (fresh start, no prior leader knowledge)", n.id)
        // Don't print leaderID - it should be -1 (no knowledge)
    }
}
```

**Key Changes**:
- Removed "Believes leader" message (misleading - should have no knowledge!)
- Added "fresh start, no prior leader knowledge" to be explicit

### Fix 3: Better FlushState Logging

**Location**: `internal/node/node.go`, line 1092

**Before**:
```go
log.Printf("Node %d: Ballot and leader state reset", n.id)
```

**After**:
```go
log.Printf("Node %d: Ballot and leader state reset (leaderID → -1, ballots → 0)", n.id)
```

**Key Changes**:
- Shows exactly what values are reset to
- Makes it clear the state is completely fresh

### Fix 4: Clarify leaderID Semantics

**Location**: `internal/node/consensus.go`, line 95

**Before**:
```go
if leaderID > 0 && leaderID != n.id {
```

**After**:
```go
// leaderID > 0 means we know a leader (leaderID = -1 means uninitialized, 0 means no leader)
if leaderID > 0 && leaderID != n.id {
```

**Key Changes**:
- Documented the meaning of different leaderID values
- `-1` = uninitialized (never seen a leader)
- `0` = no known leader (leader failed)
- `> 0` = known leader

---

## The New Behavior

### Test Set 1:
```
1. FlushState called:
   ✅ Database reset (all balances → 10)
   ✅ Logs cleared
   ✅ Ballots reset (0,0)
   ✅ leaderID → -1

2. All nodes set to INACTIVE:
   ✅ leaderID → -1 (for ALL nodes)
   ✅ isLeader → false
   ✅ Stop heartbeats if leader

3. Required nodes set to ACTIVE:
   ✅ "fresh start, no prior leader knowledge"
   ✅ No stale leaderID
   ✅ Wait for first transaction

4. Test runs:
   • Node 7 elected as leader (example)
   • leaderID = 7 learned by followers
   • Transactions execute
```

### Test Set 2 (Bootstrap):
```
1. FlushState called:
   ✅ Database reset (all balances → 10)
   ✅ Logs cleared
   ✅ Ballots reset (0,0)
   ✅ leaderID → -1

2. All nodes set to INACTIVE:
   ✅ leaderID → -1 (EVEN if Node 7 was leader!)
   ✅ isLeader → false
   ✅ Node 7 forgets it was ever a leader

3. Required nodes set to ACTIVE:
   ✅ "fresh start, no prior leader knowledge"
   ✅ NO memory of Node 7 being leader in Test Set 1
   ✅ Completely independent!

4. Test runs:
   • Fresh election (maybe Node 9 becomes leader)
   • No influence from Test Set 1 ✓
   • True isolation ✓
```

---

## Impact

### Before (Stale State):
```
Test Set 1: Node 7 is leader
Bootstrap:  Node 7 still "remembers" ❌
Test Set 2: "Believes leader is 7" ❌
            Not truly isolated
```

### After (Fresh State):
```
Test Set 1: Node 7 is leader
Bootstrap:  leaderID → -1 (forget everything) ✅
Test Set 2: No knowledge of Test Set 1 ✅
            Truly isolated ✅
            "fresh start, no prior leader knowledge"
```

---

## What Gets Reset Now

### FlushState (between test sets):
1. ✅ **Database**: All balances → initial value (10)
2. ✅ **Paxos Logs**: All entries cleared
3. ✅ **Ballots**: currentBallot → (0, nodeID), promisedBallot → (0, 0)
4. ✅ **Leader State**: isLeader → false, leaderID → -1
5. ✅ **System Init**: systemInitialized → false
6. ✅ **Client Cache**: clientLastReply, clientLastTS cleared
7. ✅ **Locks**: All locks released
8. ✅ **WAL**: 2PC write-ahead log cleared
9. ✅ **Sequence Numbers**: nextSeqNum → 1, lastExecuted → 0

### SetActive(false) → SetActive(true):
1. ✅ **isActive**: false → true
2. ✅ **isLeader**: false (for all nodes)
3. ✅ **leaderID**: -1 (no knowledge, for all nodes)
4. ✅ **Heartbeats**: Stopped if was leader
5. ✅ **Timer**: Reset when activating

---

## leaderID Semantics

The system now uses three distinct values for `leaderID`:

| Value | Meaning | When Set |
|-------|---------|----------|
| `-1` | Uninitialized (no knowledge) | Bootstrap, fresh start |
| `0` | No known leader (leader failed) | Leader timeout, forward failed |
| `> 0` | Known leader (node ID) | Received message from leader |

This makes debugging much clearer:
- `-1` in logs → "We've never seen a leader in this test set"
- `0` in logs → "We had a leader but they failed"
- `7` in logs → "Node 7 is our current leader"

---

## Build Status

✅ **All changes compiled successfully**

```bash
go build -o bin/node cmd/node/main.go
go build -o bin/client cmd/client/main.go
```

---

## Testing

**To test the improved bootstrap**:

```bash
# Stop old nodes
pkill -f "bin/node"

# Start new nodes (with heartbeat fix + bootstrap improvements)
./scripts/start_nodes.sh

# Run tests
./bin/client -testfile testcases/official_tests_converted.csv
```

**What to look for**:

1. ✅ "Set to INACTIVE - clearing ALL leader state (leaderID → -1)"
2. ✅ "Node set to ACTIVE (fresh start, no prior leader knowledge)"
3. ❌ NO "Believes leader is X" messages between test sets
4. ✅ Each test set starts with fresh elections
5. ✅ No influence from previous test sets

---

## Summary

**Your Insight**:
> "the nodes should start with no knowledge of the previous action"

**Was absolutely correct!** The old code preserved `leaderID` for followers across test sets, breaking isolation.

**The Fix**:
1. Clear `leaderID → -1` for ALL nodes when going inactive
2. Use `-1` to indicate "no knowledge" (vs `0` for "leader failed")
3. Remove misleading "Believes leader" messages
4. Better logging to show the complete reset

**Result**:
- ✅ Complete isolation between test sets
- ✅ Each test set starts fresh (as if nodes just booted)
- ✅ No stale state from previous test sets
- ✅ True bootstrap semantics

**This matches your vision perfectly!** Each test set is now treated as a completely new file, with no memory of previous activity. 🎯
