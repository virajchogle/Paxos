# Leader Discovery Bug - Ready for Your Implementation

## 🐛 The Bug We Discovered

### **The Problem**
Cross-shard 2PC transactions fail because the coordinator sends PREPARE to the **configured leader** (from static config), but that node might **not be the actual leader**.

### **Evidence From Logs**

**Coordinator (Node 1)**:
```
Node 1: Sending PREPARE to participant cluster 2 leader 4
```

**Participant (Node 4) - NOT THE LEADER!**:
```
Node 4: 2PC[...]: Received PREPARE request for item 3001 (PARTICIPANT)
Node 4: 2PC[...]: Running Paxos for PREPARE phase (marker: 'P')
Node 4: ❌ No quorum for seq 1 phase 'P' (accepted=1, need=2)
Node 4: 2PC[...]: ❌ PREPARE consensus failed: <nil>
```

**Later in logs**:
```
Node 4: 📬 RECEIVED COMMIT seq=1 (isLeader=false, lastExecuted=0)
```

### **Root Cause**

**In `internal/node/twopc.go` - Line ~80:**
```go
receiverLeader := n.config.GetLeaderNodeForCluster(receiverCluster)
```

This uses **static configuration** which returns the first node in the cluster config (e.g., node 4), but the **actual dynamic leader** might be different (e.g., node 5 after an election).

### **The Flow**

```
1. Coordinator (Node 1) needs to do 2PC with cluster 2
2. Looks up configured leader: config.GetLeaderNodeForCluster(2) → returns 4
3. Sends PREPARE to node 4
4. Node 4 is NOT the leader (node 5 is!)
5. Node 4 tries to run Paxos anyway
6. Node 4 only gets 1 acceptance (itself) - needs 2 for quorum
7. PREPARE fails → Transaction aborts
```

---

## ✅ What's Working (Before the Bug)

Your system has:
- ✅ Full 2PC protocol with phase markers ('P', 'C', 'A')
- ✅ Parallel PREPARE execution (coordinator + participant)
- ✅ Sequence number reuse for COMMIT phase
- ✅ WAL for rollback (twoPCWAL on all nodes)
- ✅ TOCTOU race condition fix (atomic balance check + deduct)
- ✅ handle2PCPhase() for WAL management
- ✅ Forwarding for PREPARE, COMMIT, ABORT

---

## 🎯 What You Need to Implement

**Goal**: Make the coordinator discover the **actual dynamic leader** instead of using static config.

### **Your Options**

#### **Option 1: Reactive Discovery (Query-Based)**
```go
func (n *Node) discoverClusterLeader(clusterID int) (int32, error) {
    // Query all nodes in cluster
    // Find who claims to be leader
    // Return actual leader ID
}
```

#### **Option 2: Active Announcements (Push-Based)**
```go
// Leaders broadcast periodically
func (n *Node) announceLeadership() {
    if n.isLeader {
        broadcast("I am leader of cluster X")
    }
}

// All nodes cache announcements
leaderCache[clusterID] = leaderID
```

#### **Option 3: Forwarding (Simplest)**
```go
// In TwoPCPrepare:
if !n.isLeader {
    // Forward to actual leader
    return n.peerClients[n.leaderID].TwoPCPrepare(req)
}
```

---

## 📝 Where to Make Changes

### **File: `internal/node/twopc.go`**

**Current Code (~line 80)**:
```go
receiverCluster := n.config.GetClusterForDataItem(tx.Receiver)
receiverLeader := n.config.GetLeaderNodeForCluster(receiverCluster)  // ❌ STATIC!
```

**What You Need**:
```go
receiverCluster := n.config.GetClusterForDataItem(tx.Receiver)
receiverLeader, err := n.YOUR_DISCOVERY_FUNCTION(receiverCluster)  // ✅ DYNAMIC!
if err != nil {
    // Handle discovery failure
}
```

---

## 🧪 How to Test Your Fix

### **Test Case**
```bash
./scripts/stop_all.sh
./scripts/start_nodes.sh

# This transaction should succeed:
./bin/client -testfile testcases/official_tests_converted.csv
```

**Look for**:
```
✅ [1/6] 1001 → 3001: 1 units (C1→C2 cross-shard) - SUCCESS
```

### **Advanced Test: Kill Leader**
```bash
./scripts/start_nodes.sh

# Kill configured leader (node 4)
pkill -f "node.*5054"

# Send cross-shard transaction
# Your discovery should find node 5 instead!
```

---

## 📊 Success Criteria

Your implementation should:
1. ✅ Discover the **actual current leader** (not configured)
2. ✅ Handle leader being down (fallback or retry)
3. ✅ Work during leader elections (wait or retry)
4. ✅ Minimal performance overhead

---

## 💡 Implementation Tips

### **If You Choose Reactive Discovery**:
```go
func (n *Node) discoverClusterLeader(clusterID int) (int32, error) {
    nodes := n.config.GetNodesInCluster(clusterID)
    
    // Query all nodes in parallel
    for _, nodeID := range nodes {
        client := n.getCrossClusterClient(nodeID)
        status, err := client.GetStatus(...)
        
        if err == nil && status.IsLeader {
            return nodeID, nil
        }
    }
    
    return 0, errors.New("no leader found")
}
```

### **If You Choose Forwarding**:
```go
// In TwoPCPrepare (participant):
func (n *Node) TwoPCPrepare(req *TwoPCPrepareRequest) {
    if !n.isLeader && n.leaderID > 0 {
        // Forward to actual leader
        return n.peerClients[n.leaderID].TwoPCPrepare(req)
    }
    // Process normally...
}
```

### **If You Choose Active Announcements**:
```go
// Background goroutine in node.go:
func (n *Node) startLeaderAnnouncements() {
    ticker := time.NewTicker(2 * time.Second)
    for range ticker.C {
        if n.isLeader {
            n.broadcastLeaderStatus()
        }
    }
}
```

---

## 🎯 Where You Are Now

**Current State**:
- ✅ 2PC protocol fully implemented
- ✅ All features working
- ❌ Leader discovery bug present
- 🎯 **READY FOR YOU TO IMPLEMENT YOUR SOLUTION!**

---

## 📁 Key Files

- `internal/node/twopc.go` - Coordinator logic (line ~80)
- `internal/node/consensus.go` - Paxos execution
- `internal/node/node.go` - Node structure (add cache/state here)
- `proto/paxos.proto` - Add messages if needed

---

## 🚀 Next Steps

1. **Decide** which approach you want (reactive/active/forwarding)
2. **Implement** your discovery function
3. **Replace** `config.GetLeaderNodeForCluster()` with your function
4. **Test** with the test cases above
5. **Verify** it works when leaders change

---

**Good luck! The bug is clearly identified, and you have all the context you need!** 🎯
