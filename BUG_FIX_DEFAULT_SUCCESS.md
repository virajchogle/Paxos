# Critical Bug Fix: Default Return Value in commitAndExecute()

## The Bug

**Location**: `internal/node/consensus.go`, line 774

**Bug**: Default return value is `SUCCESS` instead of `FAILED`

```go
finalResult := pb.ResultType_SUCCESS  // ❌ WRONG!
```

## What Happened

### Transaction 4: `3001 → 3003: 10 units`

**Timeline**:
1. balance[3001] = 9 (after tx1)
2. Tx4 gets allocated seq=4
3. Tx4 achieves quorum quickly (all nodes accept)
4. Tx4 calls `commitAndExecute(4)`
5. Loop tries to execute seq=1, 2, 3, 4 sequentially
6. **But seq=1-3 not committed yet!**
7. Loop breaks at seq=1: `"Seq 1 not committed yet, stopping execution"`
8. Loop never reaches seq=4
9. Never calls `executeTransaction(4)`
10. Returns `finalResult` = **SUCCESS** (the default!)
11. Client gets **SUCCESS** but transaction **NEVER EXECUTED**!

**Evidence from output**:
- balance[3001] = 9 (unchanged!)
- balance[3003] = 10 (unchanged!)
- Client got SUCCESS
- Transaction clearly didn't execute

## Root Cause

```go
// Line 774: Default to SUCCESS
finalResult := pb.ResultType_SUCCESS  // ❌ BUG!

// Lines 775-798: Loop to execute seq 1-4
for nextSeq := lastExec + 1; nextSeq <= seq; nextSeq++ {
    if nextStatus != "C" && nextStatus != "E" {
        log.Printf("Seq %d not committed yet, stopping execution")
        break  // ← Exits loop early!
    }
    
    result := n.executeTransaction(nextSeq, nextEntry)
    if nextSeq == seq {
        finalResult = result  // ← Only updates if we reach seq!
    }
}

// Line 800: Return final result
return finalResult  // ← Returns SUCCESS even though nothing executed!
```

**The Logic Error**:
- If loop breaks before reaching `seq`, `finalResult` is never updated
- Returns the default value: `SUCCESS`
- This is WRONG - if transaction didn't execute, it should return `FAILED`

## The Fix

**Change default from SUCCESS to FAILED**:

```go
finalResult := pb.ResultType_FAILED  // ✅ CORRECT!
```

**Why This Works**:
- If loop breaks early (uncommitted entries), returns `FAILED`
- If loop reaches the target `seq`, updates `finalResult` with actual execution result
- Only returns `SUCCESS` if transaction actually executed successfully

## Impact

### Before Fix:
```
✅ [4/7] 3001 → 3003: 10 units - SUCCESS (WRONG!)
  • balance[3001] = 9 (unchanged)
  • balance[3003] = 10 (unchanged)
  • Transaction didn't execute but returned SUCCESS
```

### After Fix:
```
❌ [4/7] 3001 → 3003: 10 units - FAILED (CORRECT!)
  • Transaction didn't execute
  • Returns FAILED
  • Client knows it failed
```

Or if executed:
```
❌ [4/7] 3001 → 3003: 10 units - FAILED: Insufficient balance (CORRECT!)
  • Transaction executed
  • Detected insufficient balance
  • Returns INSUFFICIENT_BALANCE
```

## Why This Bug Existed

The default was probably set to `SUCCESS` assuming the loop would always execute at least once. But with pipelined Paxos:
- Multiple transactions can commit out of order
- Later sequence might commit before earlier ones
- Loop breaks if earlier sequences not ready
- Default return value becomes critical

## Related Issue: Transaction 3

Transaction 3 has a different issue (covered in separate doc):
- Gets "No quorum"
- Returns FAILED immediately
- But NEW-VIEW later commits it
- Transaction actually succeeds on followers

That's a separate bug requiring a different fix.

## Testing

After this fix, test set 4 should show:

```
✅ [1/7] 3001 → 3002: 1 units
✅ [2/7] 3004 → 3005: 1 units
❌ [3/7] 3004 → 3006: 9 units - FAILED (quorum issue - separate bug)
❌ [4/7] 3001 → 3003: 10 units - FAILED (correct now!)
```

Or if tx3 is fixed to succeed:
```
✅ [3/7] 3004 → 3006: 9 units
❌ [4/7] 3001 → 3003: 10 units - FAILED: Insufficient balance
```

## Code Location

**File**: `internal/node/consensus.go`
**Function**: `commitAndExecute()`
**Line**: 774
**Change**: `pb.ResultType_SUCCESS` → `pb.ResultType_FAILED`

## Summary

**One-line fix, massive impact!**

Changing the default return value from SUCCESS to FAILED ensures that if a transaction doesn't execute (due to ordering issues), it correctly returns FAILED instead of misleadingly returning SUCCESS.

This is a critical correctness bug that could cause data inconsistency if clients believe transactions succeeded when they didn't.
