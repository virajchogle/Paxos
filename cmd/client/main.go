// cmd/client/main.go (ENHANCED VERSION)
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"paxos-banking/internal/config"
	"paxos-banking/internal/utils"
	pb "paxos-banking/proto"
)

type ClientManager struct {
	clients       map[string]*Client
	testSets      []utils.TestSet
	currentSet    int
	nodeClients   map[int32]pb.PaxosNodeClient
	currentLeader int32
	mu            sync.Mutex // Protect currentLeader
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

	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	testSets, err := utils.ReadTestFile(*testFile)
	if err != nil {
		log.Fatalf("Failed to read test file: %v", err)
	}

	// DEBUG: Print what we loaded
	fmt.Printf("\n[DEBUG] Loaded %d test sets\n", len(testSets))
	if len(testSets) > 0 {
		fmt.Printf("[DEBUG] Set 1 has %d transactions:\n", len(testSets[0].Transactions))
		for i, txn := range testSets[0].Transactions {
			fmt.Printf("[DEBUG]   %d. %s -> %s: %d\n", i+1, txn.Sender, txn.Receiver, txn.Amount)
		}
	}

	mgr := &ClientManager{
		clients:       make(map[string]*Client),
		testSets:      testSets,
		currentSet:    0,
		nodeClients:   make(map[int32]pb.PaxosNodeClient),
		currentLeader: 1, // Start with node 1
	}

	// Initialize clients with individual timers
	for _, clientID := range cfg.Clients.ClientIDs {
		mgr.clients[clientID] = &Client{
			id:            clientID,
			timestamp:     0,
			retryTimeout:  time.Duration(cfg.Clients.RetryTimeoutMs) * time.Millisecond,
			lastRequestTS: 0,
		}
	}

	// Connect to all nodes
	fmt.Println("Connecting to nodes...")
	for nodeID := 1; nodeID <= 5; nodeID++ {
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

func (m *ClientManager) runInteractive() {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("\n╔══════════════════════════════════════════════╗")
	fmt.Println("║   Paxos Banking System - Client Manager     ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println("\nCommands:")
	fmt.Println("  next           - Process next test set")
	fmt.Println("  status         - Show current status")
	fmt.Println("  leader         - Show current leader")
	fmt.Println("  send <s> <r> <amt> - Send single transaction")
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
		case "send":
			if len(parts) == 4 {
				m.sendSingleTransaction(parts[1], parts[2], parts[3])
			} else {
				fmt.Println("Usage: send <sender> <receiver> <amount>")
			}
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

	set := m.testSets[m.currentSet]
	m.currentSet++

	fmt.Printf("\n[DEBUG] processNextSet: set.SetNumber=%d, len(set.Transactions)=%d\n", set.SetNumber, len(set.Transactions))
	for i, txn := range set.Transactions {
		fmt.Printf("[DEBUG]   Txn %d: %s -> %s: %d\n", i+1, txn.Sender, txn.Receiver, txn.Amount)
	}

	fmt.Printf("\n╔════════════════════════════════════════╗\n")
	fmt.Printf("║  Processing Test Set %d               ║\n", set.SetNumber)
	fmt.Printf("╚════════════════════════════════════════╝\n")
	fmt.Printf("Active Nodes: %v\n", set.ActiveNodes)
	fmt.Printf("Transactions: %d\n", len(set.Transactions))
	if len(set.LeaderFailures) > 0 {
		fmt.Printf("Leader Failures scheduled at: %v\n", set.LeaderFailures)
	}
	fmt.Println()

	// Show inactive nodes warning
	activeMap := make(map[int32]bool)
	for _, nodeID := range set.ActiveNodes {
		activeMap[nodeID] = true
	}

	inactiveNodes := []int32{}
	for nodeID := int32(1); nodeID <= 5; nodeID++ {
		if !activeMap[nodeID] {
			inactiveNodes = append(inactiveNodes, nodeID)
		}
	}

	if len(inactiveNodes) > 0 {
		fmt.Printf("⚠️  Nodes %v should be INACTIVE\n", inactiveNodes)
		fmt.Println("Kill these nodes now, then press ENTER to continue...")
		bufio.NewReader(os.Stdin).ReadBytes('\n')
	}

	// Group transactions by client and preserve order
	clientTxns := make(map[string][]struct {
		txn utils.Transaction
		idx int
	})
	txnOrder := make([]utils.Transaction, 0, len(set.Transactions))

	for i, txn := range set.Transactions {
		clientTxns[txn.Sender] = append(clientTxns[txn.Sender], struct {
			txn utils.Transaction
			idx int
		}{txn: txn, idx: i})
		txnOrder = append(txnOrder, txn)
	}

	fmt.Printf("Processing %d transactions from %d clients in parallel...\n\n",
		len(set.Transactions), len(clientTxns))

	// Process each client's transactions in parallel
	var wg sync.WaitGroup
	resultCh := make(chan struct {
		txn utils.Transaction
		err error
		idx int
	}, len(set.Transactions))

	for clientID, txns := range clientTxns {
		wg.Add(1)
		go func(cid string, transactions []struct {
			txn utils.Transaction
			idx int
		}) {
			defer wg.Done()

			for i, item := range transactions {
				err := m.sendTransactionWithRetry(item.txn.Sender, item.txn.Receiver, item.txn.Amount)
				resultCh <- struct {
					txn utils.Transaction
					err error
					idx int
				}{txn: item.txn, err: err, idx: item.idx}

				// Small delay between requests from same client (closed-loop)
				if i < len(transactions)-1 {
					time.Sleep(100 * time.Millisecond)
				}
			}
		}(clientID, txns)
	}

	// Close result channel when all goroutines finish
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Collect and display results
	results := make(map[int]struct {
		txn utils.Transaction
		err error
	})

	for result := range resultCh {
		results[result.idx] = struct {
			txn utils.Transaction
			err error
		}{txn: result.txn, err: result.err}
	}

	// Display results in original order
	successCount := 0
	for i, txn := range txnOrder {
		result := results[i]
		if result.err == nil {
			fmt.Printf("✅ [%d/%d] %s → %s: %d units\n",
				i+1, len(txnOrder), txn.Sender, txn.Receiver, txn.Amount)
			successCount++
		} else {
			fmt.Printf("❌ [%d/%d] %s → %s: %d units - FAILED: %v\n",
				i+1, len(txnOrder), txn.Sender, txn.Receiver, txn.Amount, result.err)
		}
	}

	fmt.Printf("\n✅ Test Set %d completed! (%d/%d successful)\n",
		set.SetNumber, successCount, len(txnOrder))
}

// sendTransactionWithRetry sends transaction with individual client timer and retry logic
func (m *ClientManager) sendTransactionWithRetry(sender, receiver string, amount int32) error {
	client, exists := m.clients[sender]
	if !exists {
		return fmt.Errorf("client %s not found", sender)
	}

	client.mu.Lock()
	client.timestamp++
	currentTS := client.timestamp
	client.mu.Unlock()

	req := &pb.TransactionRequest{
		ClientId:  sender,
		Timestamp: currentTS,
		Transaction: &pb.Transaction{
			Sender:   sender,
			Receiver: receiver,
			Amount:   amount,
		},
	}

	// Start individual client timer
	client.mu.Lock()
	if client.timer != nil {
		client.timer.Stop()
	}
	client.timer = time.NewTimer(client.retryTimeout)
	client.mu.Unlock()

	// Try current leader first
	m.mu.Lock()
	currentLeader := m.currentLeader
	m.mu.Unlock()

	nodeClient, exists := m.nodeClients[currentLeader]
	if !exists || nodeClient == nil {
		// Try to find any available node
		for nodeID, nc := range m.nodeClients {
			nodeClient = nc
			m.mu.Lock()
			m.currentLeader = nodeID
			m.mu.Unlock()
			break
		}
	}

	if nodeClient == nil {
		return fmt.Errorf("no available nodes")
	}

	// Create context with client's individual timeout
	ctx, cancel := context.WithTimeout(context.Background(), client.retryTimeout)
	defer cancel()

	resp, err := nodeClient.SubmitTransaction(ctx, req)
	if err != nil {
		// Timeout occurred - broadcast to all nodes
		for nodeID, nc := range m.nodeClients {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
			resp, err = nc.SubmitTransaction(ctx2, req)
			cancel2()

			if err == nil {
				m.mu.Lock()
				m.currentLeader = nodeID
				m.mu.Unlock()
				break
			}
		}

		if err != nil {
			return err
		}
	}

	// Stop timer on successful response
	client.mu.Lock()
	if client.timer != nil {
		client.timer.Stop()
	}
	client.lastRequestTS = currentTS
	client.mu.Unlock()

	// Update leader based on response
	if resp.Ballot != nil {
		m.mu.Lock()
		m.currentLeader = resp.Ballot.NodeId
		m.mu.Unlock()
	}

	return nil
}

// sendTransaction is kept for backward compatibility and single transaction sends
func (m *ClientManager) sendTransaction(sender, receiver string, amount int32) error {
	return m.sendTransactionWithRetry(sender, receiver, amount)
}

func (m *ClientManager) sendSingleTransaction(sender, receiver, amountStr string) {
	var amount int32
	fmt.Sscanf(amountStr, "%d", &amount)

	fmt.Printf("Sending: %s → %s: %d units... ", sender, receiver, amount)
	err := m.sendTransaction(sender, receiver, amount)
	if err != nil {
		fmt.Printf("❌ FAILED: %v\n", err)
	} else {
		fmt.Printf("✅\n")
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

	if m.currentSet < len(m.testSets) {
		next := m.testSets[m.currentSet]
		fmt.Printf("\nNext Test Set: %d\n", next.SetNumber)
		fmt.Printf("  Transactions: %d\n", len(next.Transactions))
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
	fmt.Println()
	fmt.Println("  leader")
	fmt.Println("    Show current leader and query all nodes")
	fmt.Println()
	fmt.Println("  send <sender> <receiver> <amount>")
	fmt.Println("    Send a single transaction")
	fmt.Println("    Example: send A B 5")
	fmt.Println()
	fmt.Println("  help")
	fmt.Println("    Show this help message")
	fmt.Println()
	fmt.Println("  quit")
	fmt.Println("    Exit the client manager")
	fmt.Println()
	fmt.Println("After each test set, you can use node CLI:")
	fmt.Println("  ./scripts/node_cli.sh <node_id>")
	fmt.Println("  Then: printDB, printLog, printStatus <seq>, printView")
	fmt.Println()
}
