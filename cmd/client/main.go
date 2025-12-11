// cmd/client/main.go (ENHANCED VERSION)
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"paxos-banking/internal/config"
	"paxos-banking/internal/redistribution"
	"paxos-banking/internal/utils"
	pb "paxos-banking/proto"
)

type ClientManager struct {
	clients        map[string]*Client
	testSets       []utils.TestSet
	currentSet     int
	nodeClients    map[int32]pb.PaxosNodeClient
	currentLeader  int32
	clusterLeaders map[int32]int32 // clusterID -> leaderNodeID
	config         *config.Config  // Store config for cluster lookups
	mu             sync.Mutex      // Protect currentLeader and clusterLeaders
	pendingQueue   []utils.Command // Queue for commands that failed due to no quorum

	// Transaction history for resharding analysis
	txnHistoryMu sync.Mutex
	txnHistory   []TransactionRecord

	// Migration tracking - maps itemID to new cluster after migration
	migratedItems   map[int32]int32
	migratedItemsMu sync.RWMutex

	// Client-side latency tracking (per project spec)
	latencyMu        sync.Mutex
	latencies        []time.Duration // All transaction latencies
	totalTxns        int64
	successfulTxns   int64
	failedTxns       int64
	crossShardTxns   int64
	intraShardTxns   int64
	testSetStartTime time.Time
}

// TransactionRecord records a transaction for resharding analysis
type TransactionRecord struct {
	Sender   int32
	Receiver int32
	IsCross  bool // Was it a cross-shard transaction?
}

type Client struct {
	id            string
	timestamp     int64
	mu            sync.Mutex  // Protect timestamp and timer
	timer         *time.Timer // Individual timeout timer
	retryTimeout  time.Duration
	lastRequestTS int64 // Track last request timestamp
}

func main() {
	testFile := flag.String("testfile", "testcases/test1.csv", "Test CSV file")
	configFile := flag.String("config", "config/nodes.yaml", "Config file")
	flag.Parse()

	// Support positional argument for testfile (backward compatibility)
	if flag.NArg() > 0 {
		testFileArg := flag.Arg(0)
		testFile = &testFileArg
	}

	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	testSets, err := utils.ReadTestFile(*testFile)
	if err != nil {
		log.Fatalf("Failed to read test file: %v", err)
	}

	// Successfully loaded test sets

	mgr := &ClientManager{
		clients:        make(map[string]*Client),
		testSets:       testSets,
		currentSet:     0,
		nodeClients:    make(map[int32]pb.PaxosNodeClient),
		currentLeader:  1, // Start with node 1
		clusterLeaders: make(map[int32]int32),
		config:         cfg,
		pendingQueue:   make([]utils.Command, 0),
		migratedItems:  make(map[int32]int32),
	}

	// Initialize cluster leaders with first node of each cluster
	for clusterID := range cfg.Clusters {
		nodes := cfg.GetNodesInCluster(clusterID)
		if len(nodes) > 0 {
			mgr.clusterLeaders[int32(clusterID)] = int32(nodes[0])
		}
	}

	// No longer need individual clients per data item (changed from string client IDs to int32 data item IDs)
	// Transactions now reference data item IDs (1-9000) directly

	// Connect to all nodes across all clusters (supports configurable clusters)
	totalNodes := cfg.GetTotalNodes()
	numClusters := cfg.GetNumClusters()
	fmt.Printf("Connecting to %d nodes across %d clusters...\n", totalNodes, numClusters)

	for _, nodeID := range cfg.GetAllNodeIDs() {
		address := cfg.GetNodeAddress(int(nodeID))

		conn, err := grpc.NewClient(address,
			grpc.WithTransportCredentials(insecure.NewCredentials()))

		if err != nil {
			log.Printf("Warning: Couldn't connect to node %d: %v", nodeID, err)
			continue
		}

		mgr.nodeClients[nodeID] = pb.NewPaxosNodeClient(conn)
		fmt.Printf("✓ Connected to node %d\n", nodeID)
	}

	if len(mgr.nodeClients) == 0 {
		log.Fatal("Failed to connect to any nodes")
	}

	fmt.Printf("\n✅ Client Manager Ready\n")
	fmt.Printf("Loaded %d test sets from %s\n\n", len(testSets), *testFile)

	// Wait for nodes to stabilize and elect a leader
	// Nodes have 150-300ms timeout before starting election, plus election time (~200ms)
	fmt.Println("Waiting for nodes to initialize and elect leader...")
	time.Sleep(1 * time.Second)

	mgr.runInteractive()
}

// setNodeActive sets a node to active or inactive mode with retry logic
func (m *ClientManager) setNodeActive(nodeID int32, active bool) error {
	client, exists := m.nodeClients[nodeID]
	if !exists {
		return fmt.Errorf("node %d not connected", nodeID)
	}

	req := &pb.SetActiveRequest{
		NodeId: nodeID,
		Active: active,
	}

	// Retry up to 3 times with increasing timeout
	maxRetries := 3
	baseTimeout := 2 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		timeout := baseTimeout * time.Duration(attempt)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)

		resp, err := client.SetActive(ctx, req)
		cancel()

		if err == nil && resp.Success {
			return nil
		}

		if attempt < maxRetries {
			log.Printf("Node %d: SetActive attempt %d/%d failed: %v, retrying...", nodeID, attempt, maxRetries, err)
			time.Sleep(time.Duration(attempt) * time.Second) // Backoff: 1s, 2s, 3s
		} else {
			return fmt.Errorf("RPC failed after %d attempts: %v", maxRetries, err)
		}
	}

	return fmt.Errorf("node rejected request after all retries")
}

func (m *ClientManager) runInteractive() {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("\n╔══════════════════════════════════════════════╗")
	fmt.Println("║   Paxos Banking System - Client Manager     ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println("\nCommands:")
	fmt.Println("  next           - Process next test set")
	fmt.Println("  repeat         - Repeat the last test set")
	fmt.Println("  skip           - Skip next test set")
	fmt.Println("  status         - Show current status")
	fmt.Println("  leader         - Show current leader")
	fmt.Println("  retry          - Retry queued transactions")
	fmt.Println("  send <s> <r> <amt> - Send single transaction (s,r=data item IDs 1-9000)")
	fmt.Println("  balance <id>   - Query balance of data item (read-only, no consensus)")
	fmt.Println("  printbalance <id> - PrintBalance function (all nodes in cluster)")
	fmt.Println("  printdb        - PrintDB function (all 9 nodes in parallel)")
	fmt.Println("  printview      - PrintView function (NEW-VIEW messages)")
	fmt.Println("  printreshard   - PrintReshard function (analyze and show triplets)")
	fmt.Println("  executereshard - Execute resharding (actually move data items)")
	fmt.Println("  flush          - Flush system state (preserves resharding data)")
	fmt.Println("  flushall       - Flush system state (clears resharding data)")
	fmt.Println("  performance    - Get performance metrics from all nodes")
	fmt.Println("  help           - Show this help")
	fmt.Println("  quit           - Exit")
	fmt.Println()

	for {
		fmt.Print("client> ")
		if !scanner.Scan() {
			break
		}

		cmd := strings.TrimSpace(scanner.Text())
		parts := strings.Fields(cmd)

		if len(parts) == 0 {
			continue
		}

		switch parts[0] {
		case "next":
			m.processNextSet()
		case "repeat":
			m.repeatLastSet()
		case "skip":
			m.skipNextSet()
		case "status":
			m.showStatus()
		case "leader":
			m.showLeader()
		case "retry":
			fmt.Println("Manually retrying queued transactions...")
			m.retryQueuedTransactions()
		case "send":
			if len(parts) == 4 {
				m.sendSingleTransaction(parts[1], parts[2], parts[3])
			} else {
				fmt.Println("Usage: send <sender> <receiver> <amount>")
			}
		case "balance":
			if len(parts) == 2 {
				m.queryBalance(parts[1])
			} else {
				fmt.Println("Usage: balance <data_item_id>")
			}
		case "printbalance":
			if len(parts) == 2 {
				m.printBalanceAllNodes(parts[1])
			} else {
				fmt.Println("Usage: printbalance <data_item_id>")
			}
		case "printdb":
			m.printDBAllNodes()
		case "printview":
			m.printViewAllNodes()
		case "printreshard":
			m.printReshard()
		case "executereshard":
			m.executeReshard()
		case "flush":
			m.flushAllNodes(false) // Don't reset access tracker by default
		case "flushall", "flushreshard":
			m.flushAllNodes(true) // Reset everything including access tracker
		case "performance", "perf":
			m.performanceAllNodes()
		case "help":
			m.showHelp()
		case "quit", "exit":
			fmt.Println("Goodbye!")
			return
		default:
			fmt.Printf("Unknown command: %s (type 'help' for commands)\n", parts[0])
		}
	}
}

// repeatLastSet repeats the last processed test set
func (m *ClientManager) repeatLastSet() {
	if m.currentSet == 0 {
		fmt.Println("No test set has been processed yet. Use 'next' first.")
		return
	}

	// Decrement to go back to the last processed set
	m.currentSet--
	fmt.Printf("🔁 Repeating Test Set %d...\n", m.testSets[m.currentSet].SetNumber)

	// Call processNextSet which will increment currentSet back and process
	m.processNextSet()
}

// skipNextSet skips the next test set without processing (per project spec)
func (m *ClientManager) skipNextSet() {
	if m.currentSet >= len(m.testSets) {
		fmt.Println("No more test sets to skip")
		return
	}

	skippedSet := m.testSets[m.currentSet]
	m.currentSet++

	fmt.Printf("⏭️  Skipped Test Set %d (%d commands)\n", skippedSet.SetNumber, len(skippedSet.Commands))

	if m.currentSet < len(m.testSets) {
		nextSet := m.testSets[m.currentSet]
		fmt.Printf("   Next: Test Set %d (%d commands)\n", nextSet.SetNumber, len(nextSet.Commands))
	} else {
		fmt.Println("   No more test sets remaining")
	}
}

func (m *ClientManager) processNextSet() {
	if m.currentSet >= len(m.testSets) {
		fmt.Println("No more test sets")
		return
	}

	// Clear txnHistory at start of each test set - resharding only analyzes current set
	m.txnHistoryMu.Lock()
	m.txnHistory = nil
	m.txnHistoryMu.Unlock()
	m.resetClientMetrics()

	m.flushAllNodes(false)
	time.Sleep(100 * time.Millisecond)

	set := m.testSets[m.currentSet]
	m.currentSet++

	fmt.Printf("--- Test Set %d ---\n", set.SetNumber)

	hasQuorum := len(set.ActiveNodes) >= 3
	if !hasQuorum {
		fmt.Printf("⚠️  No quorum (%d nodes)\n", len(set.ActiveNodes))
	}

	// Bootstrap: deactivate all, then activate required nodes (supports configurable clusters)
	for _, nodeID := range m.config.GetAllNodeIDs() {
		m.setNodeActive(nodeID, false)
	}
	time.Sleep(200 * time.Millisecond)

	for _, nodeID := range set.ActiveNodes {
		m.setNodeActive(nodeID, true)
	}

	// Trigger elections on expected leaders (supports configurable clusters)
	activeMap := make(map[int32]bool)
	for _, nodeID := range set.ActiveNodes {
		activeMap[nodeID] = true
	}
	// Build expected leaders dynamically from config
	expectedLeaders := m.config.GetExpectedLeaders()
	for clusterID, leaderID := range expectedLeaders {
		if activeMap[leaderID] {
			if nodeClient, exists := m.nodeClients[leaderID]; exists {
				// Get first data item in this cluster's shard
				start, _ := m.config.GetClusterRange(int(clusterID))
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				nodeClient.QueryBalance(ctx, &pb.BalanceQueryRequest{DataItemId: start})
				cancel()
			}
		}
	}

	time.Sleep(1000 * time.Millisecond)

	const PARALLEL_TRANSACTIONS = 5

	successCount := 0
	pendingTxns := []utils.Command{}

	for _, cmd := range set.Commands {
		switch cmd.Type {
		case "fail":
			if len(pendingTxns) > 0 {
				successCount += m.sendParallel(pendingTxns, PARALLEL_TRANSACTIONS, hasQuorum)
				pendingTxns = nil
			}
			fmt.Printf("F(n%d)\n", cmd.NodeID)
			m.setNodeActive(cmd.NodeID, false)
			time.Sleep(500 * time.Millisecond)

		case "recover":
			if len(pendingTxns) > 0 {
				successCount += m.sendParallel(pendingTxns, PARALLEL_TRANSACTIONS, hasQuorum)
				pendingTxns = nil
			}
			fmt.Printf("R(n%d)\n", cmd.NodeID)
			m.setNodeActive(cmd.NodeID, true)
			time.Sleep(1 * time.Second)

		case "balance":
			if len(pendingTxns) > 0 {
				successCount += m.sendParallel(pendingTxns, PARALLEL_TRANSACTIONS, hasQuorum)
				pendingTxns = nil
			}
			fmt.Printf("Balance(%d)\n", cmd.Transaction.Sender)
			m.queryBalance(fmt.Sprintf("%d", cmd.Transaction.Sender))

		case "transaction":
			// Accumulate transactions for parallel execution
			pendingTxns = append(pendingTxns, cmd)
		}
	}

	// Flush any remaining pending transactions
	if len(pendingTxns) > 0 {
		successCount += m.sendParallel(pendingTxns, PARALLEL_TRANSACTIONS, hasQuorum)
	}

	m.mu.Lock()
	queueSize := len(m.pendingQueue)
	m.mu.Unlock()

	fmt.Printf("\nTest Set %d: %d successful", set.SetNumber, successCount)
	if queueSize > 0 {
		totalRetried := m.retryQueuedTransactionsUntilDone()
		successCount += totalRetried
		m.mu.Lock()
		finalQueueSize := len(m.pendingQueue)
		m.mu.Unlock()
		if finalQueueSize > 0 {
			fmt.Printf(", %d failed", finalQueueSize)
		}
	}
	fmt.Println()
}

func (m *ClientManager) retryQueuedTransactionsUntilDone() int {
	totalSuccess := 0
	round := 0
	for {
		m.mu.Lock()
		queueLen := len(m.pendingQueue)
		if queueLen == 0 {
			m.mu.Unlock()
			return totalSuccess
		}
		queuedCmds := make([]utils.Command, queueLen)
		copy(queuedCmds, m.pendingQueue)
		m.pendingQueue = make([]utils.Command, 0)
		m.mu.Unlock()

		round++
		time.Sleep(100 * time.Millisecond)

		roundSuccess := 0
		for _, cmd := range queuedCmds {
			if cmd.Type != "transaction" {
				continue
			}
			txn := cmd.Transaction
			err := m.sendTransaction(txn.Sender, txn.Receiver, txn.Amount)
			if err == nil {
				fmt.Printf("  ✅ %d→%d: %d (retry)\n", txn.Sender, txn.Receiver, txn.Amount)
				roundSuccess++
			} else {
				errMsg := err.Error()
				isPermanent := strings.Contains(errMsg, "permanently failed") ||
					strings.Contains(errMsg, "Insufficient balance") ||
					strings.Contains(errMsg, "ABORTED") ||
					strings.Contains(errMsg, "2PC failed")
				if isPermanent {
					fmt.Printf("  ❌ %d→%d: %d - %v\n", txn.Sender, txn.Receiver, txn.Amount, err)
				} else {
					m.mu.Lock()
					m.pendingQueue = append(m.pendingQueue, cmd)
					m.mu.Unlock()
				}
			}
		}
		totalSuccess += roundSuccess
		if roundSuccess == 0 {
			return totalSuccess
		}
	}
}

// retryQueuedTransactions processes all queued commands once (manual retry command)
func (m *ClientManager) retryQueuedTransactions() {
	m.mu.Lock()
	if len(m.pendingQueue) == 0 {
		m.mu.Unlock()
		return
	}

	// Copy queue and clear it
	queuedCmds := make([]utils.Command, len(m.pendingQueue))
	copy(queuedCmds, m.pendingQueue)
	m.pendingQueue = make([]utils.Command, 0)
	m.mu.Unlock()

	successCount := 0
	for i, cmd := range queuedCmds {
		if cmd.Type != "transaction" {
			continue // Only retry transactions
		}

		txn := cmd.Transaction
		err := m.sendTransaction(txn.Sender, txn.Receiver, txn.Amount)
		if err == nil {
			fmt.Printf("   ✅ [%d/%d] %d → %d: %d units - SUCCESS\n",
				i+1, len(queuedCmds), txn.Sender, txn.Receiver, txn.Amount)
			successCount++
		} else {
			// Re-queue if still failing
			fmt.Printf("   ⏸️  [%d/%d] %d → %d: %d units - Still failing, re-queued\n",
				i+1, len(queuedCmds), txn.Sender, txn.Receiver, txn.Amount)
			m.mu.Lock()
			m.pendingQueue = append(m.pendingQueue, cmd)
			m.mu.Unlock()
		}

		if i < len(queuedCmds)-1 {
			time.Sleep(1 * time.Millisecond)
		}
	}

	fmt.Printf("✅ Retry complete: %d/%d succeeded\n\n", successCount, len(queuedCmds))
}

// triggerLeaderFailure deactivates the current leader to simulate failure
func (m *ClientManager) triggerLeaderFailure() {
	m.mu.Lock()
	currentLeader := m.currentLeader
	m.mu.Unlock()

	fmt.Printf("   Current leader: Node %d\n", currentLeader)
	fmt.Printf("   💀 Deactivating Node %d...\n", currentLeader)

	// Deactivate the current leader
	err := m.setNodeActive(currentLeader, false)
	if err != nil {
		fmt.Printf("   ⚠️  Failed to deactivate node %d: %v\n", currentLeader, err)
	} else {
		fmt.Printf("   ✅ Node %d deactivated\n", currentLeader)
	}

	// Wait for new leader election
	fmt.Printf("   ⏳ Waiting for new leader election...\n")
	time.Sleep(1 * time.Second)

	// Try to discover the new leader
	m.discoverNewLeader()
	fmt.Println()
}

// discoverNewLeader queries all nodes to find the new leader
func (m *ClientManager) discoverNewLeader() {
	fmt.Printf("   🔍 Discovering new leader...\n")

	for nodeID, client := range m.nodeClients {
		if nodeID == m.currentLeader {
			// Skip the deactivated leader
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := client.GetStatus(ctx, &pb.StatusRequest{NodeId: nodeID})
		cancel()

		if err != nil {
			continue
		}

		if resp.IsLeader {
			m.mu.Lock()
			m.currentLeader = nodeID
			m.mu.Unlock()
			fmt.Printf("   ✅ New leader discovered: Node %d\n", nodeID)
			return
		}
	}

	// If no leader found, just pick an active node
	for nodeID := range m.nodeClients {
		if nodeID != m.currentLeader {
			m.mu.Lock()
			m.currentLeader = nodeID
			m.mu.Unlock()
			fmt.Printf("   ⚠️  No leader confirmed, defaulting to Node %d\n", nodeID)
			return
		}
	}
}

// getClusterForDataItem determines which cluster owns a data item
func (m *ClientManager) getClusterForDataItem(dataItemID int32) int32 {
	// Check if item has been migrated
	m.migratedItemsMu.RLock()
	if newCluster, migrated := m.migratedItems[dataItemID]; migrated {
		m.migratedItemsMu.RUnlock()
		return newCluster
	}
	m.migratedItemsMu.RUnlock()

	// Use default range-based partitioning
	for clusterID, clusterCfg := range m.config.Clusters {
		if dataItemID >= int32(clusterCfg.ShardStart) && dataItemID <= int32(clusterCfg.ShardEnd) {
			return int32(clusterID)
		}
	}
	// Default to cluster 1 if not found
	return 1
}

// getTargetNodeForTransaction determines which node should handle a transaction
func (m *ClientManager) getTargetNodeForTransaction(sender, receiver int32) (int32, bool) {
	senderCluster := m.getClusterForDataItem(sender)
	receiverCluster := m.getClusterForDataItem(receiver)

	isCrossShard := senderCluster != receiverCluster

	m.mu.Lock()
	targetNode, exists := m.clusterLeaders[senderCluster]
	m.mu.Unlock()

	if !exists {
		// BOOTSTRAP: Prefer expected leaders (n1, n4, n7) - one per cluster
		// This ensures consistent leader election across test sets
		expectedLeaders := map[int32]int32{
			1: 1, // Cluster 1 → Node 1
			2: 4, // Cluster 2 → Node 4
			3: 7, // Cluster 3 → Node 7
		}

		if expectedLeader, hasExpected := expectedLeaders[senderCluster]; hasExpected {
			// Check if expected leader is available
			if _, isAvailable := m.nodeClients[expectedLeader]; isAvailable {
				targetNode = expectedLeader
				m.mu.Lock()
				m.clusterLeaders[senderCluster] = targetNode
				m.mu.Unlock()
				return targetNode, isCrossShard
			}
		}

		// Fallback: use first node in sender's cluster
		nodes := m.config.GetNodesInCluster(int(senderCluster))
		if len(nodes) > 0 {
			targetNode = int32(nodes[0])
			m.mu.Lock()
			m.clusterLeaders[senderCluster] = targetNode
			m.mu.Unlock()
		} else {
			targetNode = 1 // Last resort fallback
		}
	}

	return targetNode, isCrossShard
}

// sendTransactionWithRetry sends transaction with retry logic
func (m *ClientManager) sendTransactionWithRetry(sender, receiver int32, amount int32) error {
	// CLIENT-SIDE LATENCY: Start timing (per project spec)
	txnStart := time.Now()

	// Generate unique client ID based on sender data item
	clientID := fmt.Sprintf("client_%d", sender)

	// Use simple timestamp counter
	timestamp := time.Now().UnixNano()

	req := &pb.TransactionRequest{
		ClientId:  clientID,
		Timestamp: timestamp,
		Transaction: &pb.Transaction{
			Sender:   sender,
			Receiver: receiver,
			Amount:   amount,
		},
	}

	// Determine target node based on cluster-aware routing
	targetNode, isCrossShard := m.getTargetNodeForTransaction(sender, receiver)

	nodeClient, exists := m.nodeClients[targetNode]
	if !exists || nodeClient == nil {
		// Fallback: try other nodes in the same cluster
		senderCluster := m.getClusterForDataItem(sender)
		nodes := m.config.GetNodesInCluster(int(senderCluster))

		for _, nodeID := range nodes {
			nc, ok := m.nodeClients[int32(nodeID)]
			if ok && nc != nil {
				nodeClient = nc
				targetNode = int32(nodeID)
				m.mu.Lock()
				m.clusterLeaders[senderCluster] = targetNode
				m.mu.Unlock()
				break
			}
		}
	}

	if nodeClient == nil {
		return fmt.Errorf("no available nodes in cluster for data item %d", sender)
	}

	// Create context with timeout (balanced for throughput and reliability)
	ctx, cancel := context.WithTimeout(context.Background(), 2000*time.Millisecond) // 2 seconds for transaction completion under heavy load
	defer cancel()

	resp, err := nodeClient.SubmitTransaction(ctx, req)
	if err != nil {
		// Timeout occurred - retry with other nodes in the SAME CLUSTER
		senderCluster := m.getClusterForDataItem(sender)
		clusterNodes := m.config.GetNodesInCluster(int(senderCluster))

		for _, nodeID := range clusterNodes {
			if int32(nodeID) == targetNode {
				continue // Already tried this one
			}

			nc, ok := m.nodeClients[int32(nodeID)]
			if !ok || nc == nil {
				continue
			}

			ctx2, cancel2 := context.WithTimeout(context.Background(), 2000*time.Millisecond) // 2 seconds for retries
			resp, err = nc.SubmitTransaction(ctx2, req)
			cancel2()

			if err == nil && resp != nil && resp.Success {
				// Update cluster leader
				m.mu.Lock()
				m.clusterLeaders[senderCluster] = int32(nodeID)
				m.mu.Unlock()
				break
			}
		}

		if err != nil || resp == nil || !resp.Success {
			if resp != nil && !resp.Success {
				return fmt.Errorf("transaction failed: %s", resp.Message)
			}
			return fmt.Errorf("failed to submit to cluster %d: %v", senderCluster, err)
		}
	}

	// Update cluster leader based on response
	if resp != nil && resp.Ballot != nil {
		senderCluster := m.getClusterForDataItem(sender)
		m.mu.Lock()
		m.clusterLeaders[senderCluster] = resp.Ballot.NodeId
		m.mu.Unlock()
	}

	// Record ALL attempted transactions for resharding analysis
	// (even failed ones represent co-access patterns that would benefit from resharding)
	m.recordTransaction(sender, receiver, isCrossShard)

	// CLIENT-SIDE LATENCY: Record timing (per project spec)
	latency := time.Since(txnStart)
	m.recordLatency(latency, resp != nil && resp.Success, isCrossShard)

	// Check if transaction actually succeeded
	if resp == nil {
		return fmt.Errorf("no response received")
	}

	if !resp.Success {
		return fmt.Errorf("transaction failed: %s", resp.Message)
	}

	return nil
}

// recordTransaction records a transaction for resharding analysis
func (m *ClientManager) recordTransaction(sender, receiver int32, isCross bool) {
	m.txnHistoryMu.Lock()
	defer m.txnHistoryMu.Unlock()

	m.txnHistory = append(m.txnHistory, TransactionRecord{
		Sender:   sender,
		Receiver: receiver,
		IsCross:  isCross,
	})

	// Keep history bounded (last 100K transactions)
	if len(m.txnHistory) > 100000 {
		m.txnHistory = m.txnHistory[len(m.txnHistory)-100000:]
	}
}

// recordLatency records client-side latency (per project spec)
func (m *ClientManager) recordLatency(latency time.Duration, success bool, isCrossShard bool) {
	m.latencyMu.Lock()
	defer m.latencyMu.Unlock()

	m.latencies = append(m.latencies, latency)
	m.totalTxns++

	if success {
		m.successfulTxns++
	} else {
		m.failedTxns++
	}

	if isCrossShard {
		m.crossShardTxns++
	} else {
		m.intraShardTxns++
	}

	// Keep latencies bounded (last 100K)
	if len(m.latencies) > 100000 {
		m.latencies = m.latencies[len(m.latencies)-100000:]
	}
}

// resetClientMetrics resets client-side metrics for a new test set
func (m *ClientManager) resetClientMetrics() {
	m.latencyMu.Lock()
	defer m.latencyMu.Unlock()

	m.latencies = nil
	m.totalTxns = 0
	m.successfulTxns = 0
	m.failedTxns = 0
	m.crossShardTxns = 0
	m.intraShardTxns = 0
	m.testSetStartTime = time.Now()
}

// sendTransaction is kept for backward compatibility and single transaction sends
func (m *ClientManager) sendTransaction(sender, receiver int32, amount int32) error {
	return m.sendTransactionWithRetry(sender, receiver, amount)
}

// sendParallel sends multiple transactions concurrently with limited parallelism
func (m *ClientManager) sendParallel(cmds []utils.Command, concurrency int, hasQuorum bool) int {
	if len(cmds) == 0 {
		return 0
	}

	type TxnResult struct {
		Index       int
		Cmd         utils.Command
		Success     bool
		Error       error
		ShouldQueue bool
	}

	results := make(chan TxnResult, len(cmds))
	sem := make(chan struct{}, concurrency) // Limit concurrent transactions
	var wg sync.WaitGroup

	if len(cmds) > 1 {
		fmt.Printf("Sending %d transactions...\n", len(cmds))
	}

	for i, cmd := range cmds {
		if cmd.Type != "transaction" {
			continue
		}

		wg.Add(1)
		sem <- struct{}{} // Acquire slot

		go func(idx int, command utils.Command) {
			defer wg.Done()
			defer func() { <-sem }() // Release slot

			txn := command.Transaction

			// If no quorum, queue immediately
			if !hasQuorum {
				results <- TxnResult{
					Index:       idx,
					Cmd:         command,
					Success:     false,
					ShouldQueue: true,
				}
				return
			}

			// Send transaction
			err := m.sendTransactionWithRetry(txn.Sender, txn.Receiver, txn.Amount)

			shouldQueue := false
			if err != nil {
				errMsg := err.Error()
				// Permanent failures - don't retry:
				// - "permanently failed" = 2PC abort consensus completed
				// - "Insufficient balance" = business logic rejection
				// - "ABORTED" = 2PC explicitly aborted
				// - "2PC failed" = any 2PC failure (cross-shard lock conflicts are permanent)
				isPermanent := strings.Contains(errMsg, "permanently failed") ||
					strings.Contains(errMsg, "Insufficient balance") ||
					strings.Contains(errMsg, "ABORTED") ||
					strings.Contains(errMsg, "2PC failed")

				if !isPermanent {
					// Intra-shard failures should retry (locks are brief)
					shouldQueue = true
				}
			}

			results <- TxnResult{
				Index:       idx,
				Cmd:         command,
				Success:     err == nil,
				Error:       err,
				ShouldQueue: shouldQueue,
			}
		}(i, cmd)
	}

	wg.Wait()
	close(results)

	// Collect and display results
	successCount := 0
	resultSlice := make([]TxnResult, 0, len(cmds))
	for result := range results {
		resultSlice = append(resultSlice, result)
	}

	// Sort by index to maintain order in output
	sort.Slice(resultSlice, func(i, j int) bool {
		return resultSlice[i].Index < resultSlice[j].Index
	})

	for _, result := range resultSlice {
		txn := result.Cmd.Transaction
		senderCluster := m.getClusterForDataItem(txn.Sender)
		receiverCluster := m.getClusterForDataItem(txn.Receiver)
		crossShard := ""
		if senderCluster != receiverCluster {
			crossShard = " (cross)"
		}

		if result.Success {
			fmt.Printf("✅ %d→%d: %d%s\n", txn.Sender, txn.Receiver, txn.Amount, crossShard)
			successCount++
		} else if result.ShouldQueue {
			fmt.Printf("⏸️  %d→%d: %d - queued (%v)\n", txn.Sender, txn.Receiver, txn.Amount, result.Error)
			m.mu.Lock()
			m.pendingQueue = append(m.pendingQueue, result.Cmd)
			m.mu.Unlock()
		} else {
			fmt.Printf("❌ %d→%d: %d - %v\n", txn.Sender, txn.Receiver, txn.Amount, result.Error)
		}
	}

	return successCount
}

func (m *ClientManager) sendSingleTransaction(senderStr, receiverStr, amountStr string) {
	var sender, receiver, amount int32
	fmt.Sscanf(senderStr, "%d", &sender)
	fmt.Sscanf(receiverStr, "%d", &receiver)
	fmt.Sscanf(amountStr, "%d", &amount)

	fmt.Printf("Sending: %d → %d: %d units... ", sender, receiver, amount)
	err := m.sendTransaction(sender, receiver, amount)
	if err != nil {
		fmt.Printf("❌ FAILED: %v\n", err)
	} else {
		fmt.Printf("✅\n")
	}
}

// queryBalance queries the balance of a data item (linearizable read via leader)
func (m *ClientManager) queryBalance(dataItemIDStr string) {
	var dataItemID int32
	if _, err := fmt.Sscanf(dataItemIDStr, "%d", &dataItemID); err != nil {
		fmt.Printf("❌ Invalid data item ID: %s\n", dataItemIDStr)
		return
	}

	cluster := m.config.GetClusterForDataItem(dataItemID)
	clusterNodes := m.config.GetNodesInCluster(cluster)

	// Retry up to 3 times (wait for potential election from concurrent writes)
	var resp *pb.BalanceQueryReply
	var err error
	var lastMsg string

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(500 * time.Millisecond)
		}

		// Try leader first, then other nodes (they will forward to leader)
		leaderID := m.clusterLeaders[int32(cluster)]
		nodesToTry := []int32{leaderID}
		for _, nid := range clusterNodes {
			if int32(nid) != leaderID {
				nodesToTry = append(nodesToTry, int32(nid))
			}
		}

		for _, nodeID := range nodesToTry {
			client, exists := m.nodeClients[nodeID]
			if !exists || client == nil {
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			resp, err = client.QueryBalance(ctx, &pb.BalanceQueryRequest{
				DataItemId: dataItemID,
			})
			cancel()

			if err == nil && resp != nil && resp.Success {
				fmt.Printf("Balance(%d) = %d\n", dataItemID, resp.Balance)
				return
			}
			if resp != nil && resp.Message != "" {
				lastMsg = resp.Message
			}
		}
	}

	if err != nil {
		fmt.Printf("❌ Query failed: %v\n", err)
		return
	}
	if lastMsg != "" {
		fmt.Printf("❌ Query failed: %s\n", lastMsg)
	} else {
		fmt.Printf("❌ Query failed: no available leader\n")
	}
}

func (m *ClientManager) showStatus() {
	fmt.Println("\n╔════════════════════════════════════╗")
	fmt.Println("║   Client Manager Status            ║")
	fmt.Println("╚════════════════════════════════════╝")
	fmt.Printf("Total Test Sets: %d\n", len(m.testSets))
	fmt.Printf("Completed: %d\n", m.currentSet)
	fmt.Printf("Remaining: %d\n", len(m.testSets)-m.currentSet)
	fmt.Printf("Connected Nodes: %d\n", len(m.nodeClients))
	fmt.Printf("Current Leader: Node %d\n", m.currentLeader)

	m.mu.Lock()
	queueSize := len(m.pendingQueue)
	m.mu.Unlock()

	if queueSize > 0 {
		fmt.Printf("⏸️  Pending Transactions: %d (queued due to no quorum)\n", queueSize)
	}

	if m.currentSet < len(m.testSets) {
		next := m.testSets[m.currentSet]
		fmt.Printf("\nNext Test Set: %d\n", next.SetNumber)
		fmt.Printf("  Commands: %d\n", len(next.Commands))
		fmt.Printf("  Active Nodes: %v\n", next.ActiveNodes)
	}
	fmt.Println()
}

func (m *ClientManager) showLeader() {
	fmt.Printf("\nCurrent leader: Node %d\n", m.currentLeader)

	// Query all nodes for their view of the leader
	fmt.Println("\nQuerying all nodes...")
	for nodeID, client := range m.nodeClients {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := client.GetStatus(ctx, &pb.StatusRequest{NodeId: nodeID})
		cancel()

		if err != nil {
			fmt.Printf("  Node %d: ❌ (unreachable)\n", nodeID)
		} else {
			leaderStr := "follower"
			if resp.IsLeader {
				leaderStr = "🌟 LEADER"
			}
			fmt.Printf("  Node %d: %s (thinks leader is %d)\n",
				nodeID, leaderStr, resp.CurrentLeaderId)
		}
	}
	fmt.Println()
}

func (m *ClientManager) showHelp() {
	fmt.Println(`
Commands:
  next/repeat/skip     - Process/repeat/skip test set
  status/leader        - Show status/leader info
  send <s> <r> <amt>   - Send transaction
  balance <id>         - Quick balance query
  printbalance <id>    - PrintBalance (all cluster nodes)
  printdb              - PrintDB (all 9 nodes)
  printview            - PrintView (NEW-VIEW messages)
  printreshard         - Analyze resharding
  executereshard       - Execute resharding
  flush                - Reset system state (preserves resharding data)
  flushall             - Reset system state (clears resharding data)
  performance          - Show metrics
  quit                 - Exit

Note: Resharding data persists across test sets and flushes until 'flushall' is called.`)
}

// printBalanceAllNodes implements PrintBalance(id) - query all 3 nodes in the cluster
func (m *ClientManager) printBalanceAllNodes(dataItemIDStr string) {
	var dataItemID int32
	if _, err := fmt.Sscanf(dataItemIDStr, "%d", &dataItemID); err != nil {
		fmt.Printf("❌ Invalid data item ID: %s\n", dataItemIDStr)
		return
	}

	// Determine which cluster owns this item (checks migratedItems first)
	cluster := m.getClusterForDataItem(dataItemID)
	clusterNodes := m.config.GetNodesInCluster(int(cluster))

	// Query all nodes in parallel
	type nodeBalance struct {
		nodeID  int32
		balance int32
		err     error
	}

	resultChan := make(chan nodeBalance, len(clusterNodes))

	for _, nodeID := range clusterNodes {
		go func(nid int32) {
			client, exists := m.nodeClients[nid]
			if !exists {
				resultChan <- nodeBalance{nodeID: nid, err: fmt.Errorf("not connected")}
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			resp, err := client.QueryBalance(ctx, &pb.BalanceQueryRequest{
				DataItemId: dataItemID,
			})

			if err != nil {
				resultChan <- nodeBalance{nodeID: nid, err: err}
			} else if !resp.Success {
				resultChan <- nodeBalance{nodeID: nid, err: fmt.Errorf(resp.Message)}
			} else {
				resultChan <- nodeBalance{nodeID: nid, balance: resp.Balance, err: nil}
			}
		}(int32(nodeID))
	}

	// Collect results
	results := make(map[int32]int32)
	errors := make(map[int32]error)

	for i := 0; i < len(clusterNodes); i++ {
		result := <-resultChan
		if result.err != nil {
			errors[result.nodeID] = result.err
		} else {
			results[result.nodeID] = result.balance
		}
	}

	// Print in exact format per project spec: n4 : 8, n5 : 8, n6 : 10
	output := ""
	for _, nodeID := range clusterNodes {
		if balance, ok := results[int32(nodeID)]; ok {
			if output != "" {
				output += ", "
			}
			output += fmt.Sprintf("n%d : %d", nodeID, balance)
		} else if err, ok := errors[int32(nodeID)]; ok {
			if output != "" {
				output += ", "
			}
			output += fmt.Sprintf("n%d : ERROR", nodeID)
			_ = err // Log error silently
		}
	}

	fmt.Printf("%s\n", output)
}

// printDBAllNodes implements PrintDB - query all nodes in parallel (supports configurable clusters)
func (m *ClientManager) printDBAllNodes() {
	totalNodes := m.config.GetTotalNodes()
	allNodeIDs := m.config.GetAllNodeIDs()

	fmt.Printf("\nPrintDB - Modified balances on all %d nodes:\n", totalNodes)

	type nodeDB struct {
		nodeID    int32
		clusterID int32
		balances  []*pb.BalanceEntry
		err       error
	}

	resultChan := make(chan nodeDB, totalNodes)

	// Query all nodes in parallel
	for _, nodeID := range allNodeIDs {
		go func(nid int32) {
			client, exists := m.nodeClients[nid]
			if !exists {
				resultChan <- nodeDB{nodeID: nid, err: fmt.Errorf("not connected")}
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			resp, err := client.PrintDB(ctx, &pb.PrintDBRequest{
				IncludeZeroBalance: false,
				Limit:              0,
			})

			if err != nil {
				resultChan <- nodeDB{nodeID: nid, err: err}
			} else {
				resultChan <- nodeDB{
					nodeID:    nid,
					clusterID: resp.ClusterId,
					balances:  resp.Balances,
					err:       nil,
				}
			}
		}(nodeID)
	}

	// Collect and sort results by node ID
	results := make([]nodeDB, 0, totalNodes)
	for i := 0; i < totalNodes; i++ {
		results = append(results, <-resultChan)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].nodeID < results[j].nodeID
	})

	// Display results grouped by cluster (supports configurable clusters)
	for _, clusterID := range m.config.GetAllClusterIDs() {
		fmt.Printf("\n--- Cluster %d ---\n", clusterID)
		clusterNodes := m.config.GetNodesInCluster(clusterID)
		clusterNodeSet := make(map[int32]bool)
		for _, nid := range clusterNodes {
			clusterNodeSet[int32(nid)] = true
		}

		for _, result := range results {
			if !clusterNodeSet[result.nodeID] {
				continue
			}
			if result.err != nil {
				fmt.Printf("  Node %d: ❌ Error: %v\n", result.nodeID, result.err)
			} else {
				if len(result.balances) == 0 {
					fmt.Printf("  Node %d: (No modified items)\n", result.nodeID)
				} else {
					fmt.Printf("  Node %d: ", result.nodeID)
					// Print all modified items on one line
					for i, entry := range result.balances {
						if i > 0 {
							fmt.Printf(", ")
						}
						fmt.Printf("%d=%d", entry.DataItem, entry.Balance)
					}
					fmt.Println()
				}
			}
		}
	}
	fmt.Println()
}

// printViewAllNodes implements PrintView - show NEW-VIEW messages from all nodes (supports configurable clusters)
func (m *ClientManager) printViewAllNodes() {
	totalNodes := m.config.GetTotalNodes()
	allNodeIDs := m.config.GetAllNodeIDs()

	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  PrintView - NEW-VIEW Messages                        ║\n")
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")

	// Query all nodes in parallel
	type nodeView struct {
		nodeID int32
		reply  *pb.PrintViewReply
		err    error
	}

	resultChan := make(chan nodeView, totalNodes)

	for _, nodeID := range allNodeIDs {
		go func(nid int32) {
			client, exists := m.nodeClients[nid]
			if !exists {
				resultChan <- nodeView{nodeID: nid, err: fmt.Errorf("not connected")}
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			resp, err := client.PrintView(ctx, &pb.PrintViewRequest{
				IncludeLog: true,
				LogEntries: 5,
			})

			resultChan <- nodeView{nodeID: nid, reply: resp, err: err}
		}(nodeID)
	}

	// Collect and sort results by node ID
	results := make([]nodeView, 0, totalNodes)
	for i := 0; i < totalNodes; i++ {
		results = append(results, <-resultChan)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].nodeID < results[j].nodeID
	})

	// Display results grouped by cluster (supports configurable clusters)
	for _, clusterID := range m.config.GetAllClusterIDs() {
		fmt.Printf("\n--- Cluster %d ---\n", clusterID)
		clusterNodes := m.config.GetNodesInCluster(clusterID)
		clusterNodeSet := make(map[int32]bool)
		for _, nid := range clusterNodes {
			clusterNodeSet[int32(nid)] = true
		}

		for _, result := range results {
			if !clusterNodeSet[result.nodeID] {
				continue
			}
			if result.err != nil {
				fmt.Printf("Node %d: ❌ Error: %v\n", result.nodeID, result.err)
			} else if result.reply != nil {
				// Print the full message which contains NEW-VIEW details
				fmt.Printf("%s", result.reply.Message)
			}
		}
	}
	fmt.Println()
}

// printReshard implements PrintReshard using simple graph partitioning
func (m *ClientManager) printReshard() {
	fmt.Printf("\n╔════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  PrintReshard - Client-Side Simple Graph Partitioning            ║\n")
	fmt.Printf("╚════════════════════════════════════════════════════════════════════╝\n")

	m.txnHistoryMu.Lock()
	txnCount := len(m.txnHistory)
	history := make([]TransactionRecord, txnCount)
	copy(history, m.txnHistory)
	m.txnHistoryMu.Unlock()

	if txnCount == 0 {
		fmt.Println("❌ No transaction history available for analysis")
		fmt.Println("   Run some transactions first, then call printreshard again")
		return
	}

	fmt.Printf("\n📊 Analyzing %d transactions...\n\n", txnCount)

	// Count cross-shard transactions in history
	crossCount := 0
	for _, txn := range history {
		if txn.IsCross {
			crossCount++
		}
	}
	fmt.Printf("Current cross-shard transactions: %d (%.1f%%)\n",
		crossCount, float64(crossCount)/float64(txnCount)*100)

	// Build simple graph from transaction history
	numClusters := int32(len(m.config.Clusters))
	graph := redistribution.NewSimpleGraph(numClusters, 0)

	// Count transactions between item pairs and item access counts
	edgeWeights := make(map[string]int64)
	itemCounts := make(map[int32]int64)

	for _, txn := range history {
		itemCounts[txn.Sender]++
		itemCounts[txn.Receiver]++

		// Create edge key (smaller ID first)
		var key string
		if txn.Sender < txn.Receiver {
			key = fmt.Sprintf("%d-%d", txn.Sender, txn.Receiver)
		} else {
			key = fmt.Sprintf("%d-%d", txn.Receiver, txn.Sender)
		}
		edgeWeights[key]++
	}

	// Add vertices
	for itemID, count := range itemCounts {
		partition := m.getClusterForDataItem(itemID)
		graph.AddVertex(itemID, count, partition)
	}

	// Add edges
	for key, weight := range edgeWeights {
		var from, to int32
		fmt.Sscanf(key, "%d-%d", &from, &to)
		graph.AddEdge(from, to, weight)
	}

	fmt.Printf("Graph: %d vertices, %d edges\n\n", len(graph.Vertices), len(graph.Edges))

	// Configure simple partitioner
	config := redistribution.DefaultSimplePartitionConfig()
	config.MinGain = 1
	config.Verbose = false // We'll print our own output

	// Run simple graph partitioning
	partitioner := redistribution.NewSimplePartitioner(graph, config)
	result, err := partitioner.Partition()
	if err != nil {
		fmt.Printf("❌ Partitioning failed: %v\n", err)
		return
	}

	// Display results
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n")
	fmt.Printf("                    RESHARDING RESULTS\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n")
	fmt.Printf("Initial edge cut (cross-shard txns): %d\n", result.InitialCut)
	fmt.Printf("Final edge cut (after resharding):   %d\n", result.FinalCut)
	fmt.Printf("Cross-shard reduction:               %.2f%%\n", result.CutReduction*100)
	fmt.Printf("Iterations:                          %d\n", result.Iterations)
	fmt.Printf("Items to move:                       %d\n\n", len(result.Moves))

	if len(result.Moves) == 0 {
		fmt.Println("✅ No items need to be moved - current partitioning is optimal!")
	} else {
		fmt.Printf("Migration Triplets (item_id, from_cluster, to_cluster):\n")
		fmt.Printf("───────────────────────────────────────────────────────\n")
		for _, move := range result.Moves {
			fmt.Printf("  (%d, c%d, c%d)  [gain: %d]\n",
				move.ItemID, move.FromCluster, move.ToCluster, move.Gain)
		}
	}

	// Show items analyzed per cluster (only items involved in transactions)
	fmt.Printf("\nItems analyzed from this test set (by current cluster):\n")
	for _, clusterID := range m.config.GetAllClusterIDs() {
		size := graph.PartitionSizes[int32(clusterID)]
		if size > 0 {
			fmt.Printf("  Cluster %d: %d items involved in transactions\n", clusterID, size)
		}
	}
	// Calculate items per cluster dynamically
	totalItems := m.config.GetTotalDataItems()
	itemsPerCluster := totalItems / int(numClusters)
	fmt.Printf("  (Note: Full clusters have ~%d items each; only accessed items are analyzed)\n", itemsPerCluster)

	fmt.Printf("═══════════════════════════════════════════════════════════════════\n\n")
}

// executeReshard performs actual data migration based on resharding analysis
func (m *ClientManager) executeReshard() {
	fmt.Println("\nExecuting Resharding (Data Migration)...")
	fmt.Println("─────────────────────────────────────────────────")

	// First, run the analysis to get the moves
	m.txnHistoryMu.Lock()
	txnCount := len(m.txnHistory)
	history := make([]TransactionRecord, txnCount)
	copy(history, m.txnHistory)
	m.txnHistoryMu.Unlock()

	if txnCount == 0 {
		fmt.Println("❌ No transaction history - run transactions first, then printreshard")
		return
	}

	// Build graph and run partitioning (same as printReshard)
	numClusters := int32(len(m.config.Clusters))
	graph := redistribution.NewSimpleGraph(numClusters, 0)

	edgeWeights := make(map[string]int64)
	itemCounts := make(map[int32]int64)

	for _, txn := range history {
		itemCounts[txn.Sender]++
		itemCounts[txn.Receiver]++
		var key string
		if txn.Sender < txn.Receiver {
			key = fmt.Sprintf("%d-%d", txn.Sender, txn.Receiver)
		} else {
			key = fmt.Sprintf("%d-%d", txn.Receiver, txn.Sender)
		}
		edgeWeights[key]++
	}

	for itemID, count := range itemCounts {
		partition := m.getClusterForDataItem(itemID)
		graph.AddVertex(itemID, count, partition)
	}

	for key, weight := range edgeWeights {
		var from, to int32
		fmt.Sscanf(key, "%d-%d", &from, &to)
		graph.AddEdge(from, to, weight)
	}

	config := redistribution.DefaultSimplePartitionConfig()
	config.MinGain = 1
	config.Verbose = false

	partitioner := redistribution.NewSimplePartitioner(graph, config)
	result, err := partitioner.Partition()
	if err != nil {
		fmt.Printf("❌ Partitioning failed: %v\n", err)
		return
	}

	if len(result.Moves) == 0 {
		fmt.Println("✅ No items need to be moved - current partitioning is optimal!")
		return
	}

	// Filter to only high-value moves (gain >= 2) to reduce unnecessary migrations
	const minGainThreshold = int64(2)
	highValueMoves := make([]redistribution.SimpleMove, 0)
	for _, move := range result.Moves {
		if move.Gain >= minGainThreshold {
			highValueMoves = append(highValueMoves, move)
		}
	}

	if len(highValueMoves) == 0 {
		fmt.Printf("ℹ️  %d moves suggested but none meet threshold (gain >= %d)\n", len(result.Moves), minGainThreshold)
		fmt.Println("   Low-gain moves are skipped to avoid unnecessary migrations")
		return
	}

	fmt.Printf("📦 Migrating %d high-value items (gain >= %d)...\n", len(highValueMoves), minGainThreshold)
	if len(highValueMoves) < len(result.Moves) {
		fmt.Printf("   (Skipping %d low-gain moves)\n", len(result.Moves)-len(highValueMoves))
	}
	fmt.Println()

	// Group moves by source cluster
	movesBySource := make(map[int32][]redistribution.SimpleMove)
	for _, move := range highValueMoves {
		movesBySource[move.FromCluster] = append(movesBySource[move.FromCluster], move)
	}

	migrationID := fmt.Sprintf("mig_%d", time.Now().UnixNano())
	successCount := 0
	failCount := 0

	// Execute migration for each source cluster
	for sourceCluster, moves := range movesBySource {
		fmt.Printf("--- Migrating from Cluster %d ---\n", sourceCluster)

		// Get source cluster leader
		sourceLeader := m.config.GetLeaderNodeForCluster(int(sourceCluster))
		sourceClient := m.nodeClients[sourceLeader]
		if sourceClient == nil {
			fmt.Printf("  ❌ Source leader (node %d) not available\n", sourceLeader)
			failCount += len(moves)
			continue
		}

		// Prepare items for migration on source
		itemIDs := make([]int32, len(moves))
		targetClusters := make(map[int32]int32)
		for i, move := range moves {
			itemIDs[i] = move.ItemID
			targetClusters[move.ItemID] = move.ToCluster
		}

		// Step 1: Prepare on source
		prepareReq := &pb.MigrationPrepareRequest{
			MigrationId:    migrationID,
			ClusterId:      sourceCluster,
			ItemIds:        itemIDs,
			TargetClusters: targetClusters,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		prepareResp, err := sourceClient.MigrationPrepare(ctx, prepareReq)
		cancel()

		if err != nil || !prepareResp.Success {
			errMsg := "unknown"
			if err != nil {
				errMsg = err.Error()
			} else {
				errMsg = prepareResp.Message
			}
			fmt.Printf("  ❌ Prepare failed: %s\n", errMsg)
			failCount += len(moves)
			continue
		}

		// Step 2: Get data from source
		getDataReq := &pb.MigrationGetDataRequest{
			MigrationId: migrationID,
			ItemIds:     itemIDs,
		}

		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		getDataResp, err := sourceClient.MigrationGetData(ctx, getDataReq)
		cancel()

		if err != nil || !getDataResp.Success {
			fmt.Printf("  ❌ GetData failed\n")
			failCount += len(moves)
			continue
		}

		// Group items by target cluster
		itemsByTarget := make(map[int32][]*pb.MigrationDataItem)
		for _, item := range getDataResp.Items {
			targetCluster := targetClusters[item.ItemId]
			itemsByTarget[targetCluster] = append(itemsByTarget[targetCluster], item)
		}

		// Step 3: Send data to all nodes in target cluster
		allTargetsOK := true
		for targetCluster, items := range itemsByTarget {
			targetNodes := m.config.GetNodesInCluster(int(targetCluster))

			// Send to all nodes in target cluster
			for _, targetNodeID := range targetNodes {
				targetClient := m.nodeClients[targetNodeID]
				if targetClient == nil {
					fmt.Printf("  ⚠️  Target node %d not available\n", targetNodeID)
					continue // Try other nodes
				}

				setDataReq := &pb.MigrationSetDataRequest{
					MigrationId:   migrationID,
					SourceCluster: sourceCluster,
					Items:         items,
				}

				ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
				setDataResp, err := targetClient.MigrationSetData(ctx, setDataReq)
				cancel()

				if err != nil || !setDataResp.Success {
					fmt.Printf("  ⚠️  SetData to node %d failed\n", targetNodeID)
					continue // Try other nodes
				}

				// Commit on this target node
				commitReq := &pb.MigrationCommitRequest{
					MigrationId: migrationID,
					ClusterId:   targetCluster,
				}

				ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
				_, err = targetClient.MigrationCommit(ctx, commitReq)
				cancel()

				if err != nil {
					fmt.Printf("  ⚠️  Commit on node %d failed\n", targetNodeID)
				}
			}
		}

		if !allTargetsOK {
			// Rollback on source
			rollbackReq := &pb.MigrationRollbackRequest{
				MigrationId: migrationID,
				ClusterId:   sourceCluster,
			}
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			sourceClient.MigrationRollback(ctx, rollbackReq)
			cancel()
			failCount += len(moves)
			continue
		}

		// Step 5: Commit on ALL nodes in source cluster (removes items from all replicas)
		sourceNodes := m.config.GetNodesInCluster(int(sourceCluster))
		for _, srcNodeID := range sourceNodes {
			srcClient := m.nodeClients[srcNodeID]
			if srcClient == nil {
				continue
			}

			// First prepare on this node
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			srcClient.MigrationPrepare(ctx, prepareReq)
			cancel()

			// Then commit
			commitReq := &pb.MigrationCommitRequest{
				MigrationId: migrationID,
				ClusterId:   sourceCluster,
			}
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			srcClient.MigrationCommit(ctx, commitReq)
			cancel()
		}

		// Print successful moves and update routing
		for _, move := range moves {
			fmt.Printf("  ✅ (%d, c%d, c%d) - migrated\n", move.ItemID, move.FromCluster, move.ToCluster)
			// Update client's routing for this item
			m.migratedItemsMu.Lock()
			if m.migratedItems == nil {
				m.migratedItems = make(map[int32]int32)
			}
			m.migratedItems[move.ItemID] = move.ToCluster
			m.migratedItemsMu.Unlock()
			successCount++
		}
	}

	fmt.Println("─────────────────────────────────────────────────")
	fmt.Printf("Migration complete: %d succeeded, %d failed\n\n", successCount, failCount)
	if successCount > 0 {
		fmt.Println("Client routing updated - future transactions will use new locations.")
	}
}

// flushAllNodes resets all node state
func (m *ClientManager) flushAllNodes(resetAccessTracker bool) {
	totalNodes := m.config.GetTotalNodes()
	allNodeIDs := m.config.GetAllNodeIDs()

	if resetAccessTracker {
		fmt.Print("Flushing all nodes (including resharding data)...")
	} else {
		fmt.Print("Flushing all nodes (preserving resharding data)...")
	}

	type flushResult struct {
		nodeID int32
		err    error
	}

	resultChan := make(chan flushResult, totalNodes)

	// Flush all nodes in parallel
	for _, nodeID := range allNodeIDs {
		go func(nid int32, resetAT bool) {
			client, exists := m.nodeClients[nid]
			if !exists {
				resultChan <- flushResult{nodeID: nid, err: fmt.Errorf("not connected")}
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			resp, err := client.FlushState(ctx, &pb.FlushStateRequest{
				ResetDatabase:      true,
				ResetLogs:          true,
				ResetBallot:        true,
				ResetAccessTracker: resetAT,
			})

			if err != nil {
				resultChan <- flushResult{nodeID: nid, err: err}
			} else if !resp.Success {
				resultChan <- flushResult{nodeID: nid, err: fmt.Errorf(resp.Message)}
			} else {
				resultChan <- flushResult{nodeID: nid, err: nil}
			}
		}(nodeID, resetAccessTracker)
	}

	// Collect results
	successCount := 0
	for i := 0; i < totalNodes; i++ {
		result := <-resultChan
		if result.err == nil {
			successCount++
		}
	}
	fmt.Printf(" %d/%d reset\n", successCount, totalNodes)

	// Reset client-side tracking
	m.mu.Lock()
	m.pendingQueue = make([]utils.Command, 0)
	// Reset cluster leaders to initial state
	for clusterID := range m.config.Clusters {
		nodes := m.config.GetNodesInCluster(clusterID)
		if len(nodes) > 0 {
			m.clusterLeaders[int32(clusterID)] = int32(nodes[0])
		}
	}
	m.mu.Unlock()

	if resetAccessTracker {
		m.migratedItemsMu.Lock()
		m.migratedItems = make(map[int32]int32)
		m.migratedItemsMu.Unlock()

		m.txnHistoryMu.Lock()
		m.txnHistory = nil
		m.txnHistoryMu.Unlock()

		fmt.Println("Migration tracking and transaction history reset")
	}

	fmt.Println("Waiting for nodes to stabilize...")
	time.Sleep(100 * time.Millisecond)
	fmt.Println()
}

// performanceAllNodes gets performance metrics - CLIENT-SIDE (per project spec)
func (m *ClientManager) performanceAllNodes() {
	fmt.Println("\nPerformance (Client-Side Measurements):")
	fmt.Println("─────────────────────────────────────────────────")

	// CLIENT-SIDE METRICS (per project spec: measured from client initiation to reply)
	m.latencyMu.Lock()
	totalTxns := m.totalTxns
	successTxns := m.successfulTxns
	failedTxns := m.failedTxns
	crossShard := m.crossShardTxns
	intraShard := m.intraShardTxns
	latenciesCopy := make([]time.Duration, len(m.latencies))
	copy(latenciesCopy, m.latencies)
	startTime := m.testSetStartTime
	m.latencyMu.Unlock()

	// Calculate throughput
	elapsed := time.Since(startTime)
	var throughput float64
	if elapsed.Seconds() > 0 && totalTxns > 0 {
		throughput = float64(totalTxns) / elapsed.Seconds()
	}

	// Calculate average latency
	var avgLatencyMs float64
	if len(latenciesCopy) > 0 {
		var sum time.Duration
		for _, l := range latenciesCopy {
			sum += l
		}
		avgLatencyMs = float64(sum.Microseconds()) / float64(len(latenciesCopy)) / 1000.0
	}

	// Calculate percentiles (p50, p99)
	var p50Ms, p99Ms float64
	if len(latenciesCopy) > 0 {
		sort.Slice(latenciesCopy, func(i, j int) bool {
			return latenciesCopy[i] < latenciesCopy[j]
		})
		p50Idx := len(latenciesCopy) / 2
		p99Idx := len(latenciesCopy) * 99 / 100
		p50Ms = float64(latenciesCopy[p50Idx].Microseconds()) / 1000.0
		p99Ms = float64(latenciesCopy[p99Idx].Microseconds()) / 1000.0
	}

	// Print client-side summary (REQUIRED by project spec)
	fmt.Printf("Throughput:       %.2f TPS\n", throughput)
	fmt.Printf("Avg Latency:      %.2f ms\n", avgLatencyMs)
	fmt.Printf("P50 Latency:      %.2f ms\n", p50Ms)
	fmt.Printf("P99 Latency:      %.2f ms\n", p99Ms)
	fmt.Println("─────────────────────────────────────────────────")
	fmt.Printf("Total Txns:       %d (Success: %d, Failed: %d)\n", totalTxns, successTxns, failedTxns)
	fmt.Printf("Cross-Shard:      %d (%.1f%%)\n", crossShard, safePct(crossShard, totalTxns))
	fmt.Printf("Intra-Shard:      %d (%.1f%%)\n", intraShard, safePct(intraShard, totalTxns))
	if totalTxns > 0 {
		fmt.Printf("Success Rate:     %.1f%%\n", safePct(successTxns, totalTxns))
	}
	fmt.Printf("Elapsed Time:     %.2f seconds\n", elapsed.Seconds())
	fmt.Println("─────────────────────────────────────────────────")
	fmt.Println()
}

// safePct calculates percentage safely
func safePct(num, denom int64) float64 {
	if denom == 0 {
		return 0
	}
	return float64(num) / float64(denom) * 100
}
