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

	// Connect to all 9 nodes across 3 clusters
	fmt.Println("Connecting to nodes...")
	for nodeID := 1; nodeID <= 9; nodeID++ {
		address := cfg.GetNodeAddress(nodeID)

		conn, err := grpc.NewClient(address,
			grpc.WithTransportCredentials(insecure.NewCredentials()))

		if err != nil {
			log.Printf("Warning: Couldn't connect to node %d: %v", nodeID, err)
			continue
		}

		mgr.nodeClients[int32(nodeID)] = pb.NewPaxosNodeClient(conn)
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
	fmt.Println("  status         - Show current status")
	fmt.Println("  leader         - Show current leader")
	fmt.Println("  retry          - Retry queued transactions")
	fmt.Println("  send <s> <r> <amt> - Send single transaction (s,r=data item IDs 1-9000)")
	fmt.Println("  balance <id>   - Query balance of data item (read-only, no consensus)")
	fmt.Println("  printbalance <id> - PrintBalance function (all nodes in cluster)")
	fmt.Println("  printdb        - PrintDB function (all 9 nodes in parallel)")
	fmt.Println("  printview      - PrintView function (NEW-VIEW messages)")
	fmt.Println("  printreshard   - PrintReshard function (trigger resharding)")
	fmt.Println("  flush          - Flush system state (reset all nodes)")
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
		case "flush":
			m.flushAllNodes()
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

func (m *ClientManager) processNextSet() {
	if m.currentSet >= len(m.testSets) {
		fmt.Println("No more test sets")
		return
	}

	// Clear transaction history for this new test set
	// (printreshard should only analyze transactions from current test set)
	m.txnHistoryMu.Lock()
	m.txnHistory = nil
	m.txnHistoryMu.Unlock()

	// BOOTSTRAP: Treat every test set as a fresh start
	// This prevents split-brain issues and ensures clean leader elections

	// Step 1: Flush state if not first test set
	if m.currentSet > 0 {
		fmt.Println("\n🔄 Flushing system state before next test set...")
		m.flushAllNodes()
		time.Sleep(100 * time.Millisecond)
	}

	set := m.testSets[m.currentSet]
	m.currentSet++

	fmt.Printf("\n╔════════════════════════════════════════╗\n")
	fmt.Printf("║  Processing Test Set %d               ║\n", set.SetNumber)
	fmt.Printf("╚════════════════════════════════════════╝\n")
	fmt.Printf("Active Nodes: %v\n", set.ActiveNodes)
	fmt.Printf("Commands: %d\n", len(set.Commands))

	// Check if we have quorum
	hasQuorum := len(set.ActiveNodes) >= 3
	if !hasQuorum {
		fmt.Printf("⚠️  WARNING: Only %d active nodes - NO QUORUM! Transactions will be queued.\n", len(set.ActiveNodes))
	}
	fmt.Println()

	// Step 2: BOOTSTRAP - Set ALL nodes to INACTIVE first
	fmt.Printf("🔄 BOOTSTRAP: Setting all nodes to INACTIVE...\n")
	for nodeID := int32(1); nodeID <= 9; nodeID++ {
		if err := m.setNodeActive(nodeID, false); err != nil {
			// Ignore errors - node might already be inactive
		}
	}

	// Wait for all nodes to become inactive and clear leader state
	fmt.Printf("⏳ Waiting for all nodes to become inactive...\n")
	time.Sleep(500 * time.Millisecond)

	// Step 3: Activate ONLY the required nodes for this test set
	fmt.Printf("✅ Activating nodes %v...\n", set.ActiveNodes)
	for _, nodeID := range set.ActiveNodes {
		if err := m.setNodeActive(nodeID, true); err != nil {
			fmt.Printf("    ⚠️  Warning: Failed to activate node %d: %v\n", nodeID, err)
		}
	}

	// Step 4: Send initial request to expected leaders (n1, n4, n7) to trigger election
	// This ensures consistent leader election (these nodes are favored)
	fmt.Printf("⏳ Triggering leader election on expected leaders (n1, n4, n7)...\n")

	// Determine which expected leaders are active
	activeMap := make(map[int32]bool)
	for _, nodeID := range set.ActiveNodes {
		activeMap[nodeID] = true
	}

	// Send a no-op to each cluster's expected leader if they're active
	// Use cluster-appropriate data items to trigger elections
	expectedLeaders := map[int32]int32{
		1: 1,    // Node 1 → query item 1 (cluster 1)
		4: 3001, // Node 4 → query item 3001 (cluster 2)
		7: 6001, // Node 7 → query item 6001 (cluster 3)
	}
	for leaderID, dataItem := range expectedLeaders {
		if activeMap[leaderID] {
			// Trigger election by querying a balance from that node's cluster
			if nodeClient, exists := m.nodeClients[leaderID]; exists {
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				nodeClient.QueryBalance(ctx, &pb.BalanceQueryRequest{DataItemId: dataItem})
				cancel()
			}
		}
	}

	// Give nodes time to elect leader and stabilize
	fmt.Printf("⏳ Waiting for leader election to complete...\n")
	time.Sleep(3000 * time.Millisecond) // 3 seconds to allow elections to complete

	fmt.Printf("Processing %d commands...\n\n", len(set.Commands))

	// Process commands with configurable parallelism
	// Transactions can be parallel, but fail/recover/balance must be sequential
	const PARALLEL_TRANSACTIONS = 5 // Number of concurrent transactions (reduced from 10 to prevent overload)

	successCount := 0
	pendingTxns := []utils.Command{}

	for i, cmd := range set.Commands {
		switch cmd.Type {
		case "fail":
			// Before fail, flush any pending parallel transactions
			if len(pendingTxns) > 0 {
				successCount += m.sendParallel(pendingTxns, PARALLEL_TRANSACTIONS, hasQuorum)
				pendingTxns = nil
			}

			// F(ni) - Fail node
			fmt.Printf("💥 [%d/%d] F(n%d) - Failing node %d...\n", i+1, len(set.Commands), cmd.NodeID, cmd.NodeID)
			if err := m.setNodeActive(cmd.NodeID, false); err != nil {
				fmt.Printf("    ⚠️  Failed to deactivate node %d: %v\n", cmd.NodeID, err)
			} else {
				fmt.Printf("    ✅ Node %d deactivated\n", cmd.NodeID)
			}
			time.Sleep(500 * time.Millisecond) // Give time for failure to propagate

		case "recover":
			// Before recover, flush any pending parallel transactions
			if len(pendingTxns) > 0 {
				successCount += m.sendParallel(pendingTxns, PARALLEL_TRANSACTIONS, hasQuorum)
				pendingTxns = nil
			}

			// R(ni) - Recover node
			fmt.Printf("🔄 [%d/%d] R(n%d) - Recovering node %d...\n", i+1, len(set.Commands), cmd.NodeID, cmd.NodeID)
			if err := m.setNodeActive(cmd.NodeID, true); err != nil {
				fmt.Printf("    ⚠️  Failed to activate node %d: %v\n", cmd.NodeID, err)
			} else {
				fmt.Printf("    ✅ Node %d activated\n", cmd.NodeID)
			}
			time.Sleep(1 * time.Second) // Give time for recovery

		case "balance":
			// Before balance query, flush any pending parallel transactions
			if len(pendingTxns) > 0 {
				successCount += m.sendParallel(pendingTxns, PARALLEL_TRANSACTIONS, hasQuorum)
				pendingTxns = nil
			}

			// Balance query (read-only)
			fmt.Printf("📖 [%d/%d] Balance query for item %d...\n", i+1, len(set.Commands), cmd.Transaction.Sender)
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

	fmt.Printf("\n✅ Test Set %d execution completed! (%d successful, %d queued)\n",
		set.SetNumber, successCount, queueSize)

	// Immediately retry queued transactions in a loop until queue is empty or no progress
	if queueSize > 0 {
		fmt.Printf("\n🔄 Processing queued transactions...\n")
		totalRetried := m.retryQueuedTransactionsUntilDone()
		successCount += totalRetried

		m.mu.Lock()
		finalQueueSize := len(m.pendingQueue)
		m.mu.Unlock()

		if finalQueueSize > 0 {
			fmt.Printf("⚠️  %d transactions could not be completed\n", finalQueueSize)
		}
	}

	fmt.Printf("\n✅ Test Set %d final: %d successful\n", set.SetNumber, successCount)
}

// retryQueuedTransactionsUntilDone aggressively retries queued transactions until queue is empty or no progress
// Returns total number of transactions that succeeded
func (m *ClientManager) retryQueuedTransactionsUntilDone() int {
	totalSuccess := 0
	round := 0

	for {
		round++
		m.mu.Lock()
		queueSize := len(m.pendingQueue)
		if queueSize == 0 {
			m.mu.Unlock()
			if round > 1 {
				fmt.Printf("✅ All queued transactions completed after %d rounds\n", round-1)
			}
			return totalSuccess
		}

		// Copy queue and clear it
		queuedCmds := make([]utils.Command, len(m.pendingQueue))
		copy(queuedCmds, m.pendingQueue)
		m.pendingQueue = make([]utils.Command, 0)
		m.mu.Unlock()

		roundSuccess := 0
		for _, cmd := range queuedCmds {
			if cmd.Type != "transaction" {
				continue // Only retry transactions
			}

			txn := cmd.Transaction
			err := m.sendTransaction(txn.Sender, txn.Receiver, txn.Amount)
			if err == nil {
				// Success!
				senderCluster := m.getClusterForDataItem(txn.Sender)
				receiverCluster := m.getClusterForDataItem(txn.Receiver)
				if senderCluster == receiverCluster {
					fmt.Printf("   ✅ Retry: %d → %d: %d units (Cluster %d)\n",
						txn.Sender, txn.Receiver, txn.Amount, senderCluster)
				} else {
					fmt.Printf("   ✅ Retry: %d → %d: %d units (C%d→C%d cross-shard)\n",
						txn.Sender, txn.Receiver, txn.Amount, senderCluster, receiverCluster)
				}
				roundSuccess++
			} else {
				// Still failing - re-queue
				m.mu.Lock()
				m.pendingQueue = append(m.pendingQueue, cmd)
				m.mu.Unlock()
			}
		}

		totalSuccess += roundSuccess

		// If no progress this round, give up
		if roundSuccess == 0 {
			fmt.Printf("⚠️  No progress in round %d, stopping retry loop\n", round)
			return totalSuccess
		}

		// Otherwise continue to next round immediately
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
	for clusterID, clusterCfg := range m.config.Clusters {
		if dataItemID >= int32(clusterCfg.ShardStart) && dataItemID <= int32(clusterCfg.ShardEnd) {
			return int32(clusterID)
		}
	}
	// Default to cluster 1 if not found
	return 1
}

// getTargetNodeForTransaction determines which node should handle a transaction
// For intra-shard: returns leader of that shard's cluster
// For cross-shard: returns leader of sender's cluster (will be 2PC coordinator)
func (m *ClientManager) getTargetNodeForTransaction(sender, receiver int32) (int32, bool) {
	senderCluster := m.getClusterForDataItem(sender)
	receiverCluster := m.getClusterForDataItem(receiver)

	isCrossShard := senderCluster != receiverCluster

	// For both intra-shard and cross-shard, send to sender's cluster leader
	// (sender's cluster will be the 2PC coordinator for cross-shard)
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

	if isCrossShard {
		// Cross-shard transaction - will be handled by 2PC on server side
		log.Printf("🌐 Cross-shard transaction detected (%d→%d) - using 2PC", sender, receiver)
	}

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

	fmt.Printf("🚀 Sending %d transactions in parallel (max %d concurrent)...\n", len(cmds), concurrency)

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
				// Check if it's a business logic failure vs system error
				errMsg := err.Error()
				if !strings.Contains(errMsg, "transaction failed:") && !strings.Contains(errMsg, "Insufficient balance") {
					// Network/system error - should queue
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

		if result.Success {
			if senderCluster == receiverCluster {
				fmt.Printf("✅ [%d/%d] %d → %d: %d units (Cluster %d)\n",
					result.Index+1, len(cmds), txn.Sender, txn.Receiver, txn.Amount, senderCluster)
			} else {
				fmt.Printf("✅ [%d/%d] %d → %d: %d units (C%d→C%d cross-shard)\n",
					result.Index+1, len(cmds), txn.Sender, txn.Receiver, txn.Amount, senderCluster, receiverCluster)
			}
			successCount++
		} else if result.ShouldQueue {
			fmt.Printf("⏸️  [%d/%d] %d → %d: %d units - QUEUED (%v)\n",
				result.Index+1, len(cmds), txn.Sender, txn.Receiver, txn.Amount, result.Error)
			m.mu.Lock()
			m.pendingQueue = append(m.pendingQueue, result.Cmd)
			m.mu.Unlock()
		} else {
			// Business logic failure
			fmt.Printf("❌ [%d/%d] %d → %d: %d units - FAILED: %v\n",
				result.Index+1, len(cmds), txn.Sender, txn.Receiver, txn.Amount, result.Error)
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

// queryBalance queries the balance of a data item (Phase 3: Read-only transactions)
func (m *ClientManager) queryBalance(dataItemIDStr string) {
	var dataItemID int32
	if _, err := fmt.Sscanf(dataItemIDStr, "%d", &dataItemID); err != nil {
		fmt.Printf("❌ Invalid data item ID: %s\n", dataItemIDStr)
		return
	}

	// Determine which cluster owns this data item
	cluster := m.config.GetClusterForDataItem(dataItemID)

	// Get any node from that cluster (read can go to any replica)
	clusterNodes := m.config.GetNodesInCluster(cluster)
	if len(clusterNodes) == 0 {
		fmt.Printf("❌ No nodes found in cluster %d\n", cluster)
		return
	}

	// Try each node in the cluster until we get a response
	var resp *pb.BalanceQueryReply
	var err error

	for _, nodeID := range clusterNodes {
		client, exists := m.nodeClients[int32(nodeID)]
		if !exists || client == nil {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		resp, err = client.QueryBalance(ctx, &pb.BalanceQueryRequest{
			DataItemId: dataItemID,
		})
		cancel()

		if err == nil && resp != nil && resp.Success {
			break
		}
	}

	if err != nil {
		fmt.Printf("❌ Query failed: %v\n", err)
		return
	}

	if resp == nil || !resp.Success {
		msg := "unknown error"
		if resp != nil {
			msg = resp.Message
		}
		fmt.Printf("❌ Query failed: %s\n", msg)
		return
	}

	// Success - show balance
	fmt.Printf("📖 Balance of item %d: %d (from node %d, cluster %d)\n",
		dataItemID, resp.Balance, resp.NodeId, resp.ClusterId)
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
	fmt.Println("\n╔════════════════════════════════════════════════╗")
	fmt.Println("║   Client Manager Help                          ║")
	fmt.Println("╚════════════════════════════════════════════════╝")
	fmt.Println("\nCommands:")
	fmt.Println("  next")
	fmt.Println("    Process the next test set from CSV file")
	fmt.Println("    Transactions are sent sequentially")
	fmt.Println()
	fmt.Println("  status")
	fmt.Println("    Show current progress and test set info")
	fmt.Println("    Shows number of queued transactions")
	fmt.Println()
	fmt.Println("  leader")
	fmt.Println("    Show current leader and query all nodes")
	fmt.Println()
	fmt.Println("  retry")
	fmt.Println("    Manually retry all queued transactions")
	fmt.Println("    (Transactions are auto-retried when quorum restored)")
	fmt.Println()
	fmt.Println("  send <sender> <receiver> <amount>")
	fmt.Println("    Send a single transaction")
	fmt.Println("    Example: send 100 200 5")
	fmt.Println()
	fmt.Println("  balance <id>")
	fmt.Println("    Quick balance query (single node)")
	fmt.Println()
	fmt.Println("  printbalance <id>")
	fmt.Println("    PrintBalance(id) - Query all 3 nodes in cluster")
	fmt.Println("    Output format: n4 : 8, n5 : 8, n6 : 10")
	fmt.Println()
	fmt.Println("  printdb")
	fmt.Println("    PrintDB() - Query all 9 nodes in parallel")
	fmt.Println("    Shows modified balances")
	fmt.Println()
	fmt.Println("  printview")
	fmt.Println("    PrintView() - Show all NEW-VIEW messages")
	fmt.Println("    Check node logs for detailed output")
	fmt.Println()
	fmt.Println("  printreshard")
	fmt.Println("    PrintReshard() - Trigger resharding analysis")
	fmt.Println("    Outputs triplets: (item_id, c_from, c_to)")
	fmt.Println()
	fmt.Println("  flush")
	fmt.Println("    FLUSH system state (required between test sets)")
	fmt.Println("    Resets database, logs, ballots, locks, counters")
	fmt.Println()
	fmt.Println("  performance")
	fmt.Println("    Get performance metrics from all nodes")
	fmt.Println("    Shows throughput, latency, 2PC stats")
	fmt.Println()
	fmt.Println("  help")
	fmt.Println("    Show this help message")
	fmt.Println()
	fmt.Println("  quit")
	fmt.Println("    Exit the client manager")
	fmt.Println()
}

// printBalanceAllNodes implements PrintBalance(id) - query all 3 nodes in the cluster
func (m *ClientManager) printBalanceAllNodes(dataItemIDStr string) {
	var dataItemID int32
	if _, err := fmt.Sscanf(dataItemIDStr, "%d", &dataItemID); err != nil {
		fmt.Printf("❌ Invalid data item ID: %s\n", dataItemIDStr)
		return
	}

	// Determine which cluster owns this item
	cluster := m.config.GetClusterForDataItem(dataItemID)
	clusterNodes := m.config.GetNodesInCluster(int(cluster))

	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  PrintBalance(%d) - Cluster %d Nodes                  ║\n", dataItemID, cluster)
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")

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

	// Print in format: n4 : 8, n5 : 8, n6 : 10
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
			output += fmt.Sprintf("n%d : ERROR(%v)", nodeID, err)
		}
	}

	fmt.Printf("Output: %s\n\n", output)
}

// printDBAllNodes implements PrintDB - query all 9 nodes in parallel
func (m *ClientManager) printDBAllNodes() {
	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  PrintDB - All 9 Nodes                                ║\n")
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")

	type nodeDB struct {
		nodeID    int32
		clusterID int32
		balances  []*pb.BalanceEntry
		err       error
	}

	resultChan := make(chan nodeDB, 9)

	// Query all 9 nodes in parallel
	for nodeID := int32(1); nodeID <= 9; nodeID++ {
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

	// Collect and display results
	for i := 0; i < 9; i++ {
		result := <-resultChan
		if result.err != nil {
			fmt.Printf("Node %d: ❌ Error: %v\n", result.nodeID, result.err)
		} else {
			fmt.Printf("\n--- Node %d (Cluster %d) ---\n", result.nodeID, result.clusterID)
			if len(result.balances) == 0 {
				fmt.Println("  (No modified balances)")
			} else {
				for _, entry := range result.balances {
					if entry.Balance != 10 { // Only show modified items
						fmt.Printf("  Item %d: %d\n", entry.DataItem, entry.Balance)
					}
				}
			}
		}
	}
	fmt.Println()
}

// printViewAllNodes implements PrintView - show NEW-VIEW messages from all nodes
func (m *ClientManager) printViewAllNodes() {
	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  PrintView - NEW-VIEW Messages                        ║\n")
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")

	// Query all 9 nodes in parallel
	type nodeView struct {
		nodeID int32
		reply  *pb.PrintViewReply
		err    error
	}

	resultChan := make(chan nodeView, 9)

	for nodeID := int32(1); nodeID <= 9; nodeID++ {
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

	// Collect results
	for i := 0; i < 9; i++ {
		result := <-resultChan
		if result.err != nil {
			fmt.Printf("Node %d: ❌ Error: %v\n", result.nodeID, result.err)
		} else if result.reply != nil {
			fmt.Printf("Node %d: %s\n", result.nodeID, result.reply.Message)
		}
	}

	fmt.Println("\n(See node logs for detailed NEW-VIEW message contents)")
	fmt.Println()
}

// printReshard implements PrintReshard using CLIENT-SIDE SIMPLE GRAPH PARTITIONING
// This is a centralized approach where the client analyzes transaction history
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
	for part := int32(1); part <= numClusters; part++ {
		size := graph.PartitionSizes[part]
		if size > 0 {
			fmt.Printf("  Cluster %d: %d items involved in transactions\n", part, size)
		}
	}
	fmt.Printf("  (Note: Full clusters have 3000 items each; only accessed items are analyzed)\n")

	fmt.Printf("═══════════════════════════════════════════════════════════════════\n\n")
}

// flushAllNodes implements Flush - reset all node state
func (m *ClientManager) flushAllNodes() {
	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  Flushing System State - All Nodes                    ║\n")
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")
	fmt.Println("Resetting all nodes to initial state...")

	type flushResult struct {
		nodeID int32
		err    error
	}

	resultChan := make(chan flushResult, 9)

	// Flush all 9 nodes in parallel
	for nodeID := int32(1); nodeID <= 9; nodeID++ {
		go func(nid int32) {
			client, exists := m.nodeClients[nid]
			if !exists {
				resultChan <- flushResult{nodeID: nid, err: fmt.Errorf("not connected")}
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			resp, err := client.FlushState(ctx, &pb.FlushStateRequest{
				ResetDatabase: true,
				ResetLogs:     true,
				ResetBallot:   true,
			})

			if err != nil {
				resultChan <- flushResult{nodeID: nid, err: err}
			} else if !resp.Success {
				resultChan <- flushResult{nodeID: nid, err: fmt.Errorf(resp.Message)}
			} else {
				resultChan <- flushResult{nodeID: nid, err: nil}
			}
		}(nodeID)
	}

	// Collect results
	successCount := 0
	for i := 0; i < 9; i++ {
		result := <-resultChan
		if result.err != nil {
			fmt.Printf("  Node %d: ❌ Error: %v\n", result.nodeID, result.err)
		} else {
			fmt.Printf("  Node %d: ✅ Flushed\n", result.nodeID)
			successCount++
		}
	}

	fmt.Printf("\n✅ Flush complete: %d/9 nodes reset\n", successCount)

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

	fmt.Println("Waiting for nodes to stabilize...")
	time.Sleep(100 * time.Millisecond)
	fmt.Println()
}

// performanceAllNodes gets performance metrics from all nodes
func (m *ClientManager) performanceAllNodes() {
	fmt.Printf("\n╔════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  Performance Metrics - All Nodes                      ║\n")
	fmt.Printf("╚════════════════════════════════════════════════════════╝\n")

	type perfResult struct {
		nodeID int32
		reply  *pb.GetPerformanceReply
		err    error
	}

	resultChan := make(chan perfResult, 9)

	for nodeID := int32(1); nodeID <= 9; nodeID++ {
		go func(nid int32) {
			client, exists := m.nodeClients[nid]
			if !exists {
				resultChan <- perfResult{nodeID: nid, err: fmt.Errorf("not connected")}
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			resp, err := client.GetPerformance(ctx, &pb.GetPerformanceRequest{
				ResetCounters: false,
			})

			resultChan <- perfResult{nodeID: nid, reply: resp, err: err}
		}(nodeID)
	}

	// Collect and display results
	for i := 0; i < 9; i++ {
		result := <-resultChan
		if result.err != nil {
			fmt.Printf("Node %d: ❌ Error: %v\n", result.nodeID, result.err)
		} else if result.reply != nil {
			r := result.reply
			fmt.Printf("\n--- Node %d (Cluster %d) ---\n", r.NodeId, r.ClusterId)
			fmt.Printf("  Throughput: %s\n", r.Message)
			fmt.Printf("  Total Transactions: %d (Success: %d, Failed: %d)\n",
				r.TotalTransactions, r.SuccessfulTransactions, r.FailedTransactions)
			fmt.Printf("  Avg Latency: %.2f ms\n", r.AvgTransactionTimeMs)
			fmt.Printf("  2PC: Coordinator=%d, Participant=%d, Commits=%d, Aborts=%d\n",
				r.TwopcCoordinator, r.TwopcParticipant, r.TwopcCommits, r.TwopcAborts)
			if r.Avg_2PcTimeMs > 0 {
				fmt.Printf("  2PC Avg Latency: %.2f ms\n", r.Avg_2PcTimeMs)
			}
			fmt.Printf("  Elections: Started=%d, Won=%d\n",
				r.ElectionsStarted, r.ElectionsWon)
			fmt.Printf("  Locks: Acquired=%d, Timeout=%d\n",
				r.LocksAcquired, r.LocksTimeout)
			fmt.Printf("  Uptime: %d seconds\n", r.UptimeSeconds)
		}
	}
	fmt.Println()
}
