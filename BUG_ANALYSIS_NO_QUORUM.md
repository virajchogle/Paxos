# Critical Bug: "No Quorum" Returns Failure But Transaction Commits Later

## The Problem

**Transaction 3**: `3004 → 3006: 9 units`
- Leader (node 6) allocates seq=3
- Sends ACCEPT to followers
- Gets only 1 accept (need 2 for quorum)
- Returns `"No quorum"` error to client ❌
- Client thinks transaction FAILED
- BUT: Entry stays in log[3]
- Later: Leader sends NEW-VIEW including seq=3
- NEW-VIEW causes seq=3 to commit!
- Transaction actually SUCCEEDS ✅
- **Client was told it failed, but it succeeded!**

**Transaction 4**: `3001 → 3003: 10 units`
- Leader allocates seq=4
- Gets quorum successfully
- Returns SUCCESS to client ✅
- Client thinks transaction SUCCEEDED
- BUT: During execution, insufficient balance detected
- Transaction actually FAILS with INSUFFICIENT_BALANCE ❌
- **Client was told it succeeded, but it failed!**

## Root Cause

### Issue 1: Premature Response on No Quorum

**Location**: `internal/node/consensus.go`, `runPipelinedConsensus()`, lines ~381-388

```go
if acceptedCount < quorum {
    log.Printf("Node %d: ❌ No quorum for seq %d (accepted=%d, need=%d)", n.id, seq, acceptedCount, quorum)
    return &pb.TransactionReply{
        Success: false,
        Message: "No quorum",
        Result:  pb.ResultType_FAILED,
    }, nil  // ❌ Returns immediately!
}
```

**Problem**:
- Returns failure to client immediately
- BUT: Entry stays in log (line 557: "keeping in log for later NEW-VIEW")
- Later: NEW-VIEW recovery includes this entry
- NEW-VIEW commits the "failed" transaction
- Client already received failure!

### Issue 2: Response Before Execution

**Location**: Same function, lines ~407-424

```go
// Send Commit to peers (async, fire-and-forget)
for pid, cli := range peers {
    go func(peerID int32, c pb.PaxosNodeClient) {
        ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
        defer cancel()
        _, _ = c.Commit(ctx, commitReq)  // ❌ Fire-and-forget!
    }(pid, cli)
}

// Commit and execute locally
result := n.commitAndExecute(seq)  // ✅ Executes locally

// Prepare reply
reply := &pb.TransactionReply{
    Success: result == pb.ResultType_SUCCESS,
    Message: n.getResultMessage(result),
    Ballot:  ballot.ToProto(),
    Result:  result,  // ✅ Uses local execution result
}
```

**Problem**:
- Sends COMMIT to followers asynchronously (fire-and-forget)
- Executes locally and gets result
- Returns result to client based on local execution
- BUT: Followers haven't executed yet!
- If followers have different state, they might get different result
- Client gets SUCCESS, but followers might fail

## The Evidence

### From Logs:

**Node 6 (Leader)**:
```
2025/12/09 15:08:26.444398 Node 6: 🚀 PARALLEL: Allocated seq=3 for client client_3004
2025/12/09 15:08:26.474742 Node 6: ❌ No quorum for seq 3 (accepted=1, need=2)
... (returns "No quorum" to client)
2025/12/09 15:08:26.540621 Node 6: Sending NEW-VIEW with 3 messages (covering seq 1-3)
2025/12/09 15:08:26.552564 Node 6: Sending COMMIT messages for all NEW-VIEW sequences
```

**Node 4 (Follower)**:
```
2025/12/09 15:08:26.872550 Node 4: 🔍 executeTransaction seq=3: 3004→3006:9
2025/12/09 15:08:26.925582 Node 4: ✅ EXECUTED seq=3: 3004->3006:9 (new: 3004=0, 3006=19)
```

**Result**: Transaction 3 succeeded on followers but client got "No quorum" error!

---

**Node 4 (For Transaction 4)**:
```
2025/12/09 15:08:26.982565 Node 4: 🔍 executeTransaction seq=4: 3001→3003:10
2025/12/09 15:08:27.010556 Node 4: INSUFFICIENT BALANCE for item 3001 (has 9 needs 10)
2025/12/09 15:08:27.047550 Node 4: 🔄 COMMIT seq=4 executed with result=INSUFFICIENT_BALANCE
```

**Result**: Transaction 4 failed with insufficient balance but client got SUCCESS!

## Why This Happens

### Scenario for Transaction 3:

```
T=0ms   Leader allocates seq=3 for 3004→3006
T=1ms   Leader sends ACCEPT to followers
T=2ms   Only 1 follower accepts (maybe others slow/down)
T=3ms   Leader checks quorum: 1 < 2 → NO QUORUM
T=4ms   Leader returns "No quorum" to client ❌
T=5ms   Client sees FAILED
        
        Entry STAYS in log[3] (intentionally kept for recovery)
        
T=10ms  Leader processes other transactions
T=15ms  Leader becomes leader again (or stays leader)
T=20ms  Leader sends NEW-VIEW covering seq 1-3
T=25ms  NEW-VIEW includes seq=3 → sends to all followers
T=30ms  Followers receive seq=3 via NEW-VIEW
T=35ms  Followers commit and execute seq=3 → SUCCESS! ✅
        
        Transaction actually succeeded!
        But client was already told it failed!
```

### Scenario for Transaction 4:

```
T=0ms   Leader allocates seq=4 for 3001→3003
T=1ms   Leader sends ACCEPT to followers
T=2ms   All followers accept → QUORUM! ✅
T=3ms   Leader returns SUCCESS to client ✅
T=4ms   Client sees SUCCESS
        
T=5ms   Leader executes locally: balance[3001]=9, needs 10
T=6ms   Leader: INSUFFICIENT BALANCE
T=7ms   Leader updates entry.Result = INSUFFICIENT_BALANCE
        
T=8ms   Followers receive COMMIT
T=9ms   Followers execute: balance[3001]=9, needs 10
T=10ms  Followers: INSUFFICIENT BALANCE ❌
        
        Transaction actually failed!
        But client was already told it succeeded!
```

## The Fix

### Option 1: Remove Entry on No Quorum

If no quorum achieved, remove entry from log immediately:

```go
if acceptedCount < quorum {
    log.Printf("Node %d: ❌ No quorum for seq %d - REMOVING from log", n.id, seq)
    
    // Remove from log so it won't be included in NEW-VIEW
    n.logMu.Lock()
    delete(n.log, seq)
    n.logMu.Unlock()
    
    return &pb.TransactionReply{
        Success: false,
        Message: "No quorum",
        Result:  pb.ResultType_FAILED,
    }, nil
}
```

**Pros**:
- Client gets immediate feedback
- Transaction won't commit later
- Clear semantics

**Cons**:
- Loses recovery opportunity
- If more nodes come back, can't retry
- Wastes the allocated sequence number

### Option 2: Wait for Execution Before Returning (RECOMMENDED)

Don't return to client until transaction is durably committed AND executed:

```go
// Wait for COMMIT to complete on majority
// Then wait for EXECUTION to complete
// THEN return result to client

// This ensures:
// 1. Transaction is durable (majority committed)
// 2. Transaction is executed (result known)
// 3. Client gets accurate result
```

**Pros**:
- Client gets accurate result always
- No mismatch between client and cluster state
- Correct semantics

**Cons**:
- Higher latency
- Client waits for full commit+execute

### Option 3: Retry on No Quorum

Instead of returning error, retry the transaction:

```go
if acceptedCount < quorum {
    log.Printf("Node %d: No quorum for seq %d - will retry in NEW-VIEW", n.id, seq)
    
    // DON'T return to client yet
    // Wait for NEW-VIEW to include this entry
    // When NEW-VIEW commits it, THEN return to client
    
    // This requires async response handling
}
```

**Pros**:
- Makes use of NEW-VIEW recovery
- Eventually consistent
- Client gets final result

**Cons**:
- Complex implementation
- Higher latency
- Need async client response mechanism

## Recommended Solution

**Implement Option 2: Wait for Execution**

The current implementation already executes locally before returning (line 408):

```go
result := n.commitAndExecute(seq)
```

But the problem is:
1. On "No quorum", it returns before executing
2. Followers execute asynchronously

**Fix**:
1. On "No quorum", remove from log immediately (don't keep for NEW-VIEW)
2. Ensure followers execute BEFORE leader returns to client
3. Use the ACTUAL execution result (from consensus, not just local)

## Implementation

```go
// In runPipelinedConsensus():

if acceptedCount < quorum {
    log.Printf("Node %d: ❌ No quorum for seq %d - removing from log", n.id, seq)
    
    // Clean up: remove from log since we can't commit
    n.logMu.Lock()
    delete(n.log, seq)
    n.logMu.Unlock()
    
    return &pb.TransactionReply{
        Success: false,
        Message: "No quorum - transaction aborted",
        Result:  pb.ResultType_FAILED,
    }, nil
}

// After getting quorum, ensure commit completes before returning
log.Printf("Node %d: ✅ Quorum for seq=%d, committing...", n.id, seq)

// Send COMMIT and WAIT for majority to confirm
commitReq := &pb.CommitRequest{...}
commitConfirmed := 1 // leader
var wg sync.WaitGroup
for pid, cli := range peers {
    wg.Add(1)
    go func(peerID int32, c pb.PaxosNodeClient) {
        defer wg.Done()
        ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
        defer cancel()
        resp, err := c.Commit(ctx, commitReq)
        if err == nil && resp.Success {
            atomic.AddInt32(&commitConfirmed, 1)
        }
    }(pid, cli)
}
wg.Wait()

// Now execute locally
result := n.commitAndExecute(seq)

// Return ACTUAL result to client
reply := &pb.TransactionReply{
    Success: result == pb.ResultType_SUCCESS,
    Message: n.getResultMessage(result),
    Result:  result,
}
```

## Testing

After fix, verify:

```
Test Set 4:
✅ [1/7] 3001 → 3002: 1 units - SUCCESS
✅ [2/7] 3004 → 3005: 1 units - SUCCESS
✅ [3/7] 3004 → 3006: 9 units - SUCCESS (was: FAILED)
❌ [4/7] 3001 → 3003: 10 units - FAILED: Insufficient balance (was: SUCCESS)
```

This is the CORRECT behavior!
