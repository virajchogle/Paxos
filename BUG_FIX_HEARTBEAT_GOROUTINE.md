# Critical Bug Fix: Heartbeat Goroutine Cleanup

## Your Question

> "Why did the leader timeout? Was node 4 not sending heartbeats and messages all this while?"

**Answer**: Node 4 WAS elected as leader, but **heartbeats stopped being sent** due to a critical bug in the heartbeat goroutine!

---

## The Problem

### Evidence from Logs

**Election Loop** (Node 4 and Node 5 alternating):
```
16:07:10.602  Node 4: 👑 Elected as LEADER
16:07:10.830  Node 5: ⏰ Leader timeout  ← Should NOT timeout if heartbeats sent!
16:07:10.944  Node 5: 👑 Elected as LEADER
16:07:11.366  Node 4: 👑 Elected as LEADER
16:07:11.599  Node 5: ⏰ Leader timeout  ← Again!
16:07:11.670  Node 5: 👑 Elected as LEADER
16:07:11.705  Node 4: 👑 Elected as LEADER
16:07:11.896  Node 5: ⏰ Leader timeout  ← Again!
16:07:12.259  Node 4: ⏰ Leader timeout  ← Now Node 4 too!
16:07:12.450  Node 5: ⏰ Leader timeout  ← Continuous!
```

**No Heartbeats in Logs**:
```bash
grep "heartbeat\|seq=0\|Received ACCEPT seq=0" logs/*.log
# → NO RESULTS!
```

Heartbeats (`seq=0`) are **NOT being logged** (by design), but they should reset followers' timers. The fact that timers keep expiring means **heartbeats are NOT being sent!**

---

## Root Cause: Heartbeat Goroutine Bug

**Location**: `internal/node/node.go`, lines 507-554

### The Bug

```go
func (n *Node) startHeartbeat() {
    // start only once
    n.paxosMu.Lock()
    if n.heartbeatStop != nil {  // ← Check if already running
        n.paxosMu.Unlock()
        return  // Already running
    }
    n.heartbeatStop = make(chan struct{})  // ← Set channel
    n.paxosMu.Unlock()

    go func() {
        ticker := time.NewTicker(n.heartbeatInterval)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                n.paxosMu.RLock()
                if !n.isLeader {  // ← Check if still leader
                    n.paxosMu.RUnlock()
                    return  // ← EXITS but doesn't clean up heartbeatStop! ❌
                }
                // Send heartbeats...
            }
        }
    }()
}
```

**The Problem**:
1. Goroutine exits when `!n.isLeader` at line 516
2. But `n.heartbeatStop` is **NOT cleaned up**!
3. Next time `startHeartbeat()` is called:
   - Line 500: `if n.heartbeatStop != nil` → **TRUE!**
   - Line 502: Returns early
   - New goroutine **NOT started**
   - **No heartbeats sent!**

---

## How This Breaks

### Scenario 1: FlushState

```
T=0ms   Node 4: Elected as leader
T=1ms   startHeartbeat() called
        • n.heartbeatStop = new channel
        • Goroutine starts sending heartbeats ✓

T=50ms  FlushState() called
        • n.isLeader = false (reset state)

T=60ms  Heartbeat goroutine tick
        • Checks n.isLeader → false
        • Returns (exits goroutine) ❌
        • n.heartbeatStop NOT cleaned up! ❌

T=100ms Node 4 elected leader again
        • startHeartbeat() called
        • Line 500: n.heartbeatStop != nil → TRUE
        • Returns early (doesn't start new goroutine) ❌
        • NO HEARTBEATS SENT! ❌

T=250ms Followers timeout
        • No heartbeats received
        • Start new election
        • Election loop begins!
```

### Scenario 2: Multiple Elections (Split-Brain)

```
T=0ms   Node 4: Elected as leader (ballot 10)
        • Starts heartbeat goroutine ✓

T=50ms  Node 5: Also elected as leader (ballot 11, higher!)
        • Node 5 sends ACCEPT with ballot 11
        • Node 4 receives it
        • Node 4: n.promisedBallot = 11, n.leaderID = 5
        • Node 4 realizes it's not leader: n.isLeader = false

T=60ms  Node 4's heartbeat goroutine
        • Checks n.isLeader → false
        • Exits ❌
        • n.heartbeatStop NOT cleaned up! ❌

T=100ms Node 4 elected leader again (ballot 12)
        • startHeartbeat() called
        • Line 500: n.heartbeatStop != nil → TRUE  
        • Returns early ❌
        • NO NEW HEARTBEATS!

Result: Endless election loop
```

---

## The Fix

**Add cleanup when goroutine exits**:

```go
go func() {
    ticker := time.NewTicker(n.heartbeatInterval)
    defer ticker.Stop()
    
    // ✅ NEW: Ensure we clean up heartbeatStop when exiting
    defer func() {
        n.paxosMu.Lock()
        n.heartbeatStop = nil  // ← Clean up!
        n.paxosMu.Unlock()
    }()
    
    for {
        select {
        case <-ticker.C:
            n.paxosMu.RLock()
            if !n.isLeader {
                n.paxosMu.RUnlock()
                log.Printf("Node %d: ❌ Heartbeat goroutine exiting - no longer leader", n.id)
                return  // ← Now properly cleans up via defer!
            }
            // Send heartbeats...
    }
}()
```

**What Changed**:
1. Added `defer` to clean up `n.heartbeatStop = nil`
2. Added log message when goroutine exits
3. Now can restart properly when elected leader again!

---

## Impact

### Before Fix:
```
✅ Node elected as leader
❌ Heartbeat goroutine exits on state change
❌ n.heartbeatStop not cleaned up
❌ Next election: startHeartbeat() returns early
❌ No heartbeats sent
❌ Followers timeout
❌ Endless election loop!
```

### After Fix:
```
✅ Node elected as leader
✅ Heartbeat goroutine starts
✅ If steps down: goroutine exits AND cleans up
✅ Next election: startHeartbeat() starts new goroutine
✅ Heartbeats sent properly
✅ Followers don't timeout
✅ Stable leadership!
```

---

## Why This Bug Was Hard to Find

1. **Silent failure**: Heartbeats (`seq=0`) are not logged
2. **Looks like it's working**: Leader is elected successfully
3. **Symptom looks like network issue**: "No quorum" suggests connectivity problems
4. **Split-brain confusion**: Multiple leaders make debugging harder

The only way to find it:
- Notice followers timing out despite having a leader
- Notice no heartbeat logs (but they're silent by design!)
- Check if heartbeat goroutine is actually running
- Find the cleanup bug

---

## Testing

After this fix:

1. **Stable leadership**:
   ```
   Node 4: 👑 Elected as LEADER
   (stays leader for extended period)
   (no more rapid elections)
   ```

2. **No timeout loops**:
   ```
   (no more "⏰ Leader timeout" messages from active followers)
   ```

3. **Better transaction success rate**:
   - Fewer "No quorum" failures
   - Cross-shard 2PC should work
   - More predictable behavior

---

## Related Issues

This heartbeat bug was causing:

1. **Transaction 3/4 "No quorum" failures**
   - Leaders kept changing
   - Transactions sent to old leader
   - Old leader stepped down during processing
   - Result: "No quorum"

2. **2PC failures**
   - Participant cluster leader unstable
   - Can't achieve quorum for PREPARE
   - Transaction aborts

3. **Split-brain appearance**
   - Multiple nodes think they're leader
   - Actually: leaders keep changing rapidly
   - Heartbeats not maintaining stable leadership

---

## Build Status

✅ **Build successful**

```bash
cd /Users/viraj/Desktop/Projects/paxos/Paxos
go build -o bin/node cmd/node/main.go
```

---

## Next Steps

1. **Restart all nodes** to apply fix:
   ```bash
   pkill -f "bin/node"
   ./scripts/start_nodes.sh
   ```

2. **Test**:
   ```bash
   ./bin/client -testfile testcases/official_tests_converted.csv
   ```

3. **Verify**:
   - Stable leadership (fewer elections)
   - No rapid election loops
   - Better transaction success rate
   - Cross-shard 2PC working

---

## Summary

**Your Question**: "Why did the leader timeout? Was node 4 not sending heartbeats?"

**Answer**: Node 4 WAS elected as leader, but the heartbeat goroutine **exited prematurely** and didn't restart, causing followers to timeout and trigger endless elections!

**Root Cause**: Heartbeat goroutine didn't clean up `heartbeatStop` channel when exiting, preventing restarts

**Fix**: Added `defer` cleanup to properly reset `heartbeatStop` when goroutine exits

**Impact**: This should **massively improve** system stability and reduce "No quorum" failures!

This was a **critical bug** that explained the endless election loops and many of the "No quorum" failures we were seeing! 🎯
