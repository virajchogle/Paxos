# No Quorum Fix: Remove Entry Immediately

## The Problem (Transaction 3)

**Transaction**: `3004 → 3006: 9 units`
**Balance**: `3004 = 9, needs 9` → **Should SUCCEED**

**What Happens**:
1. Leader allocates seq=3
2. Sends ACCEPT to followers
3. Only 1 follower responds (need 2 for quorum)
4. Returns "No quorum" error to client → Client sees **FAILED**
5. Entry stays in log[3]
6. Later: NEW-VIEW includes seq=3
7. Followers commit and execute seq=3 → Transaction **SUCCEEDS**
8. **Mismatch**: Client told FAILED, but it SUCCEEDED!

## The Solution

**Remove entry from log immediately on "No quorum"**

**Location**: `internal/node/consensus.go`, `runPipelinedConsensus()`, lines ~434-450

### BEFORE (Wrong):
```go
if acceptedCount < quorum {
    log.Printf("No quorum for seq %d", seq)
    // Entry stays in log ← BUG!
    return FAILED
}
// Later: NEW-VIEW includes this entry and commits it
```

### AFTER (Correct):
```go
if acceptedCount < quorum {
    log.Printf("No quorum for seq %d - removing from log", seq)
    
    // Remove entry from log immediately
    n.logMu.Lock()
    delete(n.log, seq)
    n.logMu.Unlock()
    
    return FAILED
}
// Entry gone → NEW-VIEW won't include it → No surprise commits!
```

## Rationale

### Why Remove Entry?

1. **Client Expectations** ✅
   - Client receives "FAILED"
   - Client expects transaction did NOT execute
   - If entry commits later, client state is wrong
   - Better to fail cleanly and consistently

2. **Avoid Surprise Commits** ✅
   - NEW-VIEW won't include removed entries
   - No mysterious "failed transactions" that succeed later
   - Clear semantics: FAILED means FAILED

3. **Simplicity** ✅
   - No arbitrary timeouts
   - No waiting for recovery
   - Immediate, deterministic behavior

### Trade-off: Lost Recovery

**What We Lose**:
- If more nodes come back, can't automatically recover this transaction
- Client must retry manually if needed

**Why This is OK**:
- "No quorum" indicates serious availability issue
- Automatic recovery is unpredictable (when? which transactions?)
- Client-driven retry is clearer and more controllable
- Client can implement exponential backoff, retry limits, etc.

## Comparison with Previous Approach

### Approach 1: Keep Entry + Wait 100ms (Removed)
```go
if acceptedCount < quorum {
    time.Sleep(100 * time.Millisecond)  // ❌ Arbitrary!
    if entry.Status == "E" {
        return entry.Result  // Maybe recovered?
    }
    return FAILED
}
```

**Problems**:
- ❌ Blocks client for 100ms
- ❌ Not enough time for NEW-VIEW (could need 200-500ms)
- ❌ If recovery takes 150ms, still returns FAILED but commits later
- ❌ Complex, unreliable

### Approach 2: Remove Entry Immediately (Current)
```go
if acceptedCount < quorum {
    delete(n.log, seq)  // ✅ Clean!
    return FAILED
}
```

**Benefits**:
- ✅ Immediate response to client
- ✅ Deterministic behavior
- ✅ No surprise commits later
- ✅ Simple, correct

## Expected Behavior After Fix

### Test Set 4:

**Before Fix**:
```
✅ [1/7] 3001 → 3002: 1 units
✅ [2/7] 3004 → 3005: 1 units
❌ [3/7] 3004 → 3006: 9 units - FAILED: No quorum
✅ [4/7] 3001 → 3003: 10 units (WRONG!)
Final balance[3004] = 0  ← Tx3 actually executed!
```

**After Fix**:
```
✅ [1/7] 3001 → 3002: 1 units
✅ [2/7] 3004 → 3005: 1 units
❌ [3/7] 3004 → 3006: 9 units - FAILED: No quorum
❌ [4/7] 3001 → 3003: 10 units - FAILED: ... (depends on execution order)
Final balance[3004] = 9  ← Tx3 did NOT execute!
```

Wait, but the user said "transaction 3/7 should have passed"!

**That means the "No quorum" is a TRANSIENT failure!**

## Re-thinking the Fix

If transaction 3 SHOULD SUCCEED, then "No quorum" is a transient issue:
- Maybe followers were slow to respond
- Maybe network delay
- But followers would have accepted if given more time

**Two Options**:

### Option A: Retry on No Quorum
Don't give up immediately - retry the ACCEPT phase:
```go
if acceptedCount < quorum {
    // Retry with longer timeout
    for retry := 0; retry < 3; retry++ {
        acceptedCount = retryAcceptPhase(seq)
        if acceptedCount >= quorum {
            break  // Success!
        }
    }
    
    if acceptedCount < quorum {
        delete(n.log, seq)
        return FAILED
    }
}
```

### Option B: Increase Timeout
Current timeout: 500ms
Maybe followers need more time to respond?

```go
ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
                                                         ^^^^ Too short?
```

Increase to 1-2 seconds?

## Question for You

Since transaction 3 SHOULD succeed:
1. Is "No quorum" a transient failure (slow responses)?
2. Should we retry the ACCEPT phase?
3. Or increase the timeout?
4. Or is this a test environment issue (nodes not fully started)?

Let me implement Option B (increase timeout) as a simple fix:
