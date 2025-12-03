package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"paxos-banking/internal/config"
	"paxos-banking/internal/redistribution"
	"paxos-banking/internal/types"
	pb "paxos-banking/proto"
)

// Lock represents a lock on a data item
type Lock struct {
	clientID  string
	timestamp int64
	lockedAt  time.Time
}

type Node struct {
	pb.UnimplementedPaxosNodeServer

	mu sync.RWMutex

	// Identity
	id        int32
	clusterID int32
	address   string
	config    *config.Config

	// Paxos state
	isLeader       bool
	currentBallot  *types.Ballot
	promisedBallot *types.Ballot
	leaderID       int32

	// Sequence tracking
	nextSeqNum   int32
	lastExecuted int32

	// Logs
	log        map[int32]*types.LogEntry // Single log with status tracking (A/C/E)
	newViewLog []*pb.NewViewRequest

	// Client tracking
	clientLastReply map[string]*pb.TransactionReply
	clientLastTS    map[string]int64

	// Database - changed from client IDs to data item IDs
	balances map[int32]int32 // dataItemID -> balance

	// Locking mechanism (Phase 2)
	lockMu      sync.Mutex      // Separate mutex for lock operations
	locks       map[int32]*Lock // dataItemID -> Lock
	lockTimeout time.Duration   // How long to wait for a lock

	// Write-Ahead Log (Phase 5) - for 2PC rollback
	walMu   sync.RWMutex               // Separate mutex for WAL operations
	wal     map[string]*types.WALEntry // transactionID -> WALEntry
	walFile string                     // Path to WAL file for persistence

	// Communication
	grpcServer  *grpc.Server
	peerClients map[int32]pb.PaxosNodeClient // Peers within same cluster
	peerConns   map[int32]*grpc.ClientConn

	// Cross-cluster communication (for 2PC)
	allClusterClients map[int32]pb.PaxosNodeClient // All nodes across all clusters
	allClusterConns   map[int32]*grpc.ClientConn

	// Timers
	timerMu           sync.Mutex
	leaderTimer       *time.Timer
	timerDuration     time.Duration
	prepareCooldown   time.Duration
	lastPrepareTime   time.Time
	heartbeatInterval time.Duration
	heartbeatStop     chan struct{}

	// Control
	stopChan chan struct{}
	isActive bool

	// 💾 Persistence
	dbFilePath string

	// 🎯 System Initialization
	systemInitialized bool

	// 📊 Phase 7: Performance Counters
	perfMu                 sync.RWMutex
	totalTransactions      int64
	successfulTransactions int64
	failedTransactions     int64
	twoPCCoordinator       int64
	twoPCParticipant       int64
	twoPCCommits           int64
	twoPCAborts            int64
	electionsStarted       int64
	electionsWon           int64
	proposalsMade          int64
	proposalsAccepted      int64
	locksAcquired          int64
	locksTimeout           int64
	totalTransactionTimeMs int64
	totalTransactionCount  int64
	total2PCTimeMs         int64
	total2PCCount          int64
	startTime              time.Time

	// 🔄 Phase 9: Shard Redistribution
	accessTracker  *redistribution.AccessTracker
	migrationState *MigrationState
}

func NewNode(id int32, cfg *config.Config) (*Node, error) {
	// jittered timeout - use milliseconds for leader election
	// OPTIMIZED FOR HIGH THROUGHPUT (5000+ TPS target)
	rand.Seed(time.Now().UnixNano() + int64(id))
	baseTimeout := 100 * time.Millisecond                     // Was 500ms - now 100ms for performance
	jitter := time.Duration(rand.Intn(50)) * time.Millisecond // 0-50ms (was 0-200ms)
	timerDuration := baseTimeout + jitter

	// Get cluster ID for this node
	clusterID := int32(cfg.GetClusterForNode(int(id)))

	n := &Node{
		id:                id,
		clusterID:         clusterID,
		address:           cfg.GetNodeAddress(int(id)),
		config:            cfg,
		isLeader:          false,
		currentBallot:     types.NewBallot(0, id),
		promisedBallot:    types.NewBallot(0, 0),
		leaderID:          -1,
		nextSeqNum:        1,
		lastExecuted:      0,
		log:               make(map[int32]*types.LogEntry),
		newViewLog:        make([]*pb.NewViewRequest, 0),
		clientLastReply:   make(map[string]*pb.TransactionReply),
		clientLastTS:      make(map[string]int64),
		balances:          make(map[int32]int32),                   // Changed to int32 -> int32
		locks:             make(map[int32]*Lock),                   // Initialize lock table
		lockTimeout:       100 * time.Millisecond,                  // Was 2s - now 100ms for performance
		wal:               make(map[string]*types.WALEntry),        // Initialize WAL (Phase 5)
		walFile:           fmt.Sprintf("data/node%d_wal.json", id), // WAL persistence file
		peerClients:       make(map[int32]pb.PaxosNodeClient),
		peerConns:         make(map[int32]*grpc.ClientConn),
		allClusterClients: make(map[int32]pb.PaxosNodeClient),
		allClusterConns:   make(map[int32]*grpc.ClientConn),
		timerDuration:     timerDuration,
		dbFilePath:        fmt.Sprintf("data/node%d_db.json", id),
		prepareCooldown:   time.Duration(5+rand.Intn(10)) * time.Millisecond, // Was 50-100ms - now 5-15ms for performance
		heartbeatInterval: 10 * time.Millisecond,                             // Was 50ms - now 10ms for performance
		heartbeatStop:     nil,
		stopChan:          make(chan struct{}),
		isActive:          true,
		systemInitialized: false,
		startTime:         time.Now(),                              // Phase 7: Track uptime
		accessTracker:     redistribution.NewAccessTracker(100000), // Phase 9: Access pattern tracking
		migrationState:    nil,                                     // Phase 9: Initialized on first migration
	}

	// Initialize database with data items in this node's shard
	log.Printf("Node %d (Cluster %d): 🔄 Initializing fresh database for shard", id, clusterID)
	cluster := cfg.Clusters[int(clusterID)]
	for itemID := cluster.ShardStart; itemID <= cluster.ShardEnd; itemID++ {
		n.balances[itemID] = cfg.Data.InitialBalance
	}

	// Save initial state to disk
	if err := n.saveDatabase(); err != nil {
		log.Printf("Node %d: ⚠️  Warning - failed to save initial database: %v", id, err)
	} else {
		log.Printf("Node %d (Cluster %d): 💾 Initialized %d data items (range %d-%d) with balance %d",
			id, clusterID, len(n.balances), cluster.ShardStart, cluster.ShardEnd, cfg.Data.InitialBalance)
	}

	log.Printf("Node %d (Cluster %d): Timer set to %v (base: 500ms + jitter: %v)",
		id, clusterID, timerDuration, jitter)

	return n, nil
}

func (n *Node) Start() error {
	lis, err := net.Listen("tcp", n.address)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	n.grpcServer = grpc.NewServer()
	pb.RegisterPaxosNodeServer(n.grpcServer, n)

	go func() {
		log.Printf("Node %d: Starting on %s", n.id, n.address)
		if err := n.grpcServer.Serve(lis); err != nil {
			log.Fatalf("gRPC server error: %v", err)
		}
	}()

	// small pause to let server start then connect peers
	time.Sleep(200 * time.Millisecond)

	if err := n.connectToPeers(); err != nil {
		// log but continue - peers might come online later
		log.Printf("Node %d: connectToPeers warning: %v", n.id, err)
	}

	// Start background reconnection for failed peers
	go n.retryPeerConnections()

	// Start periodic gap detection
	go n.startGapDetection()

	// Don't start leader timer on startup - wait for first transaction
	// n.resetLeaderTimer()

	log.Printf("Node %d: Ready (standby mode - waiting for transaction)", n.id)
	return nil
}

func (n *Node) connectToPeers() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Connect to peers within the same cluster
	clusterNodes := n.config.GetNodesInCluster(int(n.clusterID))
	peersInCluster := 0

	for _, nid := range clusterNodes {
		if nid == n.id {
			continue
		}
		addr := n.config.GetNodeAddress(int(nid))
		// dial with short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancel()
		if err != nil {
			log.Printf("Node %d (Cluster %d): Warning - couldn't connect to peer node %d (%s): %v", n.id, n.clusterID, nid, addr, err)
			continue
		}
		n.peerConns[nid] = conn
		n.peerClients[nid] = pb.NewPaxosNodeClient(conn)
		log.Printf("Node %d (Cluster %d): Connected to peer node %d (%s)", n.id, n.clusterID, nid, addr)
		peersInCluster++
	}

	// Also connect to ALL nodes across all clusters for cross-cluster communication (2PC)
	// Note: We'll use lazy connection establishment - connect on first use with retry
	totalCrossCluster := 0
	for nodeID := range n.config.Nodes {
		nid := int32(nodeID)
		if nid == n.id {
			// Add self-reference for 2PC (when coordinator is also a participant)
			// Use a special marker that will route to local handlers
			continue
		}
		// Skip if already connected as peer
		if _, exists := n.peerClients[nid]; exists {
			n.allClusterClients[nid] = n.peerClients[nid]
			n.allClusterConns[nid] = n.peerConns[nid]
			totalCrossCluster++
			continue
		}

		addr := n.config.GetNodeAddress(nodeID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // Increased from 500ms
		conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancel()
		if err != nil {
			log.Printf("Node %d (Cluster %d): Warning - couldn't connect to cross-cluster node %d (%s): %v (will retry on-demand)", n.id, n.clusterID, nid, addr, err)
			// Don't fail - will retry later when needed
			continue
		}
		n.allClusterConns[nid] = conn
		n.allClusterClients[nid] = pb.NewPaxosNodeClient(conn)
		totalCrossCluster++
	}

	log.Printf("Node %d (Cluster %d): Connected to %d peers in cluster, %d cross-cluster nodes",
		n.id, n.clusterID, peersInCluster, totalCrossCluster)
	return nil
}

// getCrossClusterClient gets a client for cross-cluster communication with retry
// Returns the client or nil if unable to connect after retries
func (n *Node) getCrossClusterClient(nodeID int32) (pb.PaxosNodeClient, error) {
	// Check if it's a request to self
	if nodeID == n.id {
		// Return a special marker - caller will handle local dispatch
		return nil, fmt.Errorf("self-reference: use local handler")
	}

	n.mu.RLock()
	client, exists := n.allClusterClients[nodeID]
	n.mu.RUnlock()

	if exists {
		return client, nil
	}

	// Client doesn't exist - try to establish connection
	log.Printf("Node %d: Attempting to establish cross-cluster connection to node %d", n.id, nodeID)

	addr := n.config.GetNodeAddress(int(nodeID))
	if addr == "" {
		return nil, fmt.Errorf("no address found for node %d", nodeID)
	}

	// Try to connect with reasonable timeout
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node %d at %s: %v", nodeID, addr, err)
	}

	client = pb.NewPaxosNodeClient(conn)

	// Save the connection for future use
	n.mu.Lock()
	n.allClusterConns[nodeID] = conn
	n.allClusterClients[nodeID] = client
	n.mu.Unlock()

	log.Printf("Node %d: ✅ Established cross-cluster connection to node %d", n.id, nodeID)
	return client, nil
}

func (n *Node) Stop() {
	n.mu.Lock()
	if !n.isActive {
		n.mu.Unlock()
		return
	}
	n.isActive = false
	n.mu.Unlock()

	// stop timers & heartbeats
	n.timerMu.Lock()
	if n.leaderTimer != nil {
		n.leaderTimer.Stop()
	}
	n.timerMu.Unlock()

	n.stopHeartbeat()

	// close peer conns
	n.mu.Lock()
	for _, c := range n.peerConns {
		_ = c.Close()
	}
	n.peerConns = map[int32]*grpc.ClientConn{}
	n.peerClients = map[int32]pb.PaxosNodeClient{}
	n.mu.Unlock()

	// stop server
	if n.grpcServer != nil {
		n.grpcServer.GracefulStop()
	}

	close(n.stopChan)
}

func (n *Node) quorumSize() int {
	// quorum is floor(N/2) + 1 where N is the cluster size (not total nodes)
	n.mu.RLock()
	defer n.mu.RUnlock()
	clusterNodes := n.config.GetNodesInCluster(int(n.clusterID))
	total := len(clusterNodes)
	if total <= 0 {
		// safe default
		return 2
	}
	return (total / 2) + 1
}

func (n *Node) hasQuorum(count int) bool {
	return count >= n.quorumSize()
}

//
// Timers & heartbeat helpers
//

func (n *Node) resetLeaderTimer() {
	n.timerMu.Lock()
	defer n.timerMu.Unlock()

	// stop existing timer safely
	if n.leaderTimer != nil {
		if !n.leaderTimer.Stop() {
			select {
			case <-n.leaderTimer.C:
			default:
			}
		}
	}

	// create new timer
	n.leaderTimer = time.NewTimer(n.timerDuration)

	go func() {
		<-n.leaderTimer.C
		n.mu.RLock()
		isLeader := n.isLeader
		active := n.isActive
		n.mu.RUnlock()

		if !isLeader && active {
			log.Printf("Node %d: ⏰ Leader timeout - starting election", n.id)
			go n.StartLeaderElection()
		}
	}()
}

func (n *Node) startHeartbeat() {
	// start only once
	n.mu.Lock()
	if n.heartbeatStop != nil {
		n.mu.Unlock()
		return
	}
	n.heartbeatStop = make(chan struct{})
	n.mu.Unlock()

	go func() {
		ticker := time.NewTicker(n.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				n.mu.RLock()
				if !n.isLeader {
					n.mu.RUnlock()
					return
				}
				// snapshot peer clients
				peers := make(map[int32]pb.PaxosNodeClient, len(n.peerClients))
				for k, v := range n.peerClients {
					peers[k] = v
				}
				var ballotProto *pb.Ballot
				if n.currentBallot != nil {
					ballotProto = n.currentBallot.ToProto()
				} else {
					ballotProto = types.NewBallot(0, n.id).ToProto()
				}
				n.mu.RUnlock()

				// send best-effort no-op Accept messages (heartbeat)
				for pid, cli := range peers {
					go func(peerID int32, c pb.PaxosNodeClient) {
						ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
						defer cancel()
						req := &pb.AcceptRequest{
							Ballot:         ballotProto,
							SequenceNumber: 0,
							Request:        nil,
							IsNoop:         true,
						}
						_, _ = c.Accept(ctx, req) // ignore result - heartbeat only
						_ = peerID
					}(pid, cli)
				}

			case <-n.heartbeatStop:
				return
			case <-n.stopChan:
				return
			}
		}
	}()
}

func (n *Node) stopHeartbeat() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.heartbeatStop != nil {
		select {
		default:
			close(n.heartbeatStop)
		case <-n.heartbeatStop:
			// already closed
		}
		n.heartbeatStop = nil
	}
}

// retryPeerConnections keeps trying to connect to peers that failed initially
func (n *Node) retryPeerConnections() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			n.mu.Lock()
			if !n.isActive {
				n.mu.Unlock()
				return
			}

			// Find cluster peers we should be connected to but aren't
			missingPeers := []struct {
				id   int32
				addr string
			}{}
			clusterNodes := n.config.GetNodesInCluster(int(n.clusterID))
			for _, nid := range clusterNodes {
				if nid == n.id {
					continue
				}
				if _, exists := n.peerClients[nid]; !exists {
					missingPeers = append(missingPeers, struct {
						id   int32
						addr string
					}{id: nid, addr: n.config.GetNodeAddress(int(nid))})
				}
			}
			n.mu.Unlock()

			// Try to connect to missing peers
			for _, peer := range missingPeers {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				conn, err := grpc.DialContext(ctx, peer.addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
				cancel()

				if err == nil {
					n.mu.Lock()
					n.peerConns[peer.id] = conn
					n.peerClients[peer.id] = pb.NewPaxosNodeClient(conn)
					n.allClusterClients[peer.id] = n.peerClients[peer.id]
					n.allClusterConns[peer.id] = n.peerConns[peer.id]
					n.mu.Unlock()
					log.Printf("Node %d (Cluster %d): Reconnected to peer node %d (%s)", n.id, n.clusterID, peer.id, peer.addr)
				}
			}

		case <-n.stopChan:
			return
		}
	}
}

// saveDatabase persists the current balance state to a JSON file
func (n *Node) saveDatabase() error {
	n.mu.RLock()
	balancesCopy := make(map[int32]int32, len(n.balances))
	for k, v := range n.balances {
		balancesCopy[k] = v
	}
	n.mu.RUnlock()

	data, err := json.MarshalIndent(balancesCopy, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal balances: %w", err)
	}

	if err := os.WriteFile(n.dbFilePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write database file: %w", err)
	}

	return nil
}

func (n *Node) startGapDetection() {
	ticker := time.NewTicker(2 * time.Second) // ✅ Reduced from 10s to 2s
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			n.checkForGaps()
		case <-n.stopChan:
			return
		}
	}
}

// checkForGaps checks if there are gaps in the log and triggers recovery if needed
func (n *Node) checkForGaps() {
	n.mu.RLock()
	if !n.isActive {
		n.mu.RUnlock()
		return
	}

	lastExec := n.lastExecuted
	maxSeq := lastExec
	for seq := range n.log {
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	leaderID := n.leaderID
	isLeader := n.isLeader
	n.mu.RUnlock()

	// 🔥 FIX: If we have gaps and we're NOT the leader, request the leader to trigger NEW-VIEW
	// This breaks the deadlock where nodes with gaps wait forever for data
	if maxSeq > lastExec+1 {
		// Check if we have actual gaps (not just uncommitted sequences)
		n.mu.RLock()
		hasGaps := false
		for seq := lastExec + 1; seq < maxSeq; seq++ {
			if _, exists := n.log[seq]; !exists {
				hasGaps = true
				break
			}
		}
		n.mu.RUnlock()

		if hasGaps && !isLeader && leaderID > 0 {
			// We have gaps and there's a leader - send a special request to trigger NEW-VIEW
			log.Printf("Node %d: 🔍 Detected persistent gaps (lastExec=%d, maxSeq=%d) - requesting recovery from leader %d",
				n.id, lastExec, maxSeq, leaderID)
			go n.requestRecoveryFromLeader(leaderID)
		} else if hasGaps && !isLeader && leaderID <= 0 {
			// We have gaps but no known leader - trigger election so someone else can become leader
			log.Printf("Node %d: 🔍 Detected persistent gaps with no leader (lastExec=%d, maxSeq=%d) - triggering election (won't become leader ourselves)",
				n.id, lastExec, maxSeq)
			go n.StartLeaderElection()
		}
	}
}

// requestRecoveryFromLeader actively requests missing sequences from peers
// This breaks the deadlock where nodes with gaps can't recover
func (n *Node) requestRecoveryFromLeader(leaderID int32) {
	// Find which sequences we're missing
	n.mu.RLock()
	lastExec := n.lastExecuted
	maxSeqLocal := lastExec
	for seq := range n.log {
		if seq > maxSeqLocal {
			maxSeqLocal = seq
		}
	}

	peers := make(map[int32]pb.PaxosNodeClient)
	for k, v := range n.peerClients {
		peers[k] = v
	}
	n.mu.RUnlock()

	// 🔥 FIX: Query peers to find the REAL maximum sequence in the cluster
	maxSeqCluster := maxSeqLocal
	for peerID, peerClient := range peers {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		statusResp, err := peerClient.GetStatus(ctx, &pb.StatusRequest{NodeId: peerID})
		cancel()

		if err == nil && statusResp.NextSequenceNumber-1 > maxSeqCluster {
			maxSeqCluster = statusResp.NextSequenceNumber - 1
			log.Printf("Node %d: Peer %d has advanced to seq %d", n.id, peerID, maxSeqCluster)
		}
	}

	// Use the cluster-wide max sequence
	maxSeq := maxSeqCluster

	// Identify ALL missing sequences (including ones we didn't know about)
	missingSeqs := []int32{}
	for seq := lastExec + 1; seq <= maxSeq; seq++ {
		n.mu.RLock()
		_, exists := n.log[seq]
		n.mu.RUnlock()
		if !exists {
			missingSeqs = append(missingSeqs, seq)
		}
	}

	if len(missingSeqs) == 0 {
		return // No gaps, nothing to do
	}

	log.Printf("Node %d: 🔄 Actively requesting missing sequences %v from peers", n.id, missingSeqs)

	// Request each missing sequence from all peers until we get it
	for _, seq := range missingSeqs {
		// Try each peer for this sequence
		for peerID, peerClient := range peers {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			resp, err := peerClient.GetLogEntry(ctx, &pb.GetLogEntryRequest{
				NodeId:         n.id,
				SequenceNumber: seq,
			})
			cancel()

			if err == nil && resp != nil && resp.Entry != nil {
				// Got the entry! Add it to our log
				log.Printf("Node %d: ✅ Received missing seq %d from node %d", n.id, seq, peerID)

				// Convert to AcceptRequest and process it
				acceptReq := &pb.AcceptRequest{
					Ballot:         resp.Entry.Ballot,
					SequenceNumber: resp.Entry.SequenceNumber,
					Request:        resp.Entry.Request,
					IsNoop:         resp.Entry.IsNoop,
				}

				n.Accept(context.Background(), acceptReq)

				// Also commit and try to execute it
				commitReq := &pb.CommitRequest{
					Ballot:         resp.Entry.Ballot,
					SequenceNumber: resp.Entry.SequenceNumber,
					Request:        resp.Entry.Request,
					IsNoop:         resp.Entry.IsNoop,
				}
				n.Commit(context.Background(), commitReq)

				break // Got this sequence, move to next
			}
		}
	}

	log.Printf("Node %d: Finished requesting missing sequences, will retry on next check if gaps remain", n.id)
}

// ============================================================================
// LOCKING MECHANISM (Phase 2)
// ============================================================================

// acquireLock attempts to acquire a lock on a data item
// Returns true if lock is acquired, false otherwise
func (n *Node) acquireLock(dataItemID int32, clientID string, timestamp int64) bool {
	n.lockMu.Lock()
	defer n.lockMu.Unlock()

	// Check if lock exists
	if lock, exists := n.locks[dataItemID]; exists {
		// Check if lock is held by same client (re-entrant)
		if lock.clientID == clientID && lock.timestamp == timestamp {
			// Same client, same transaction - allow
			return true
		}

		// Check if lock has timed out
		if time.Since(lock.lockedAt) > n.lockTimeout {
			log.Printf("Node %d: Lock on item %d expired (held by %s), granting to %s",
				n.id, dataItemID, lock.clientID, clientID)
			// Lock expired, can grant to new client
			n.locks[dataItemID] = &Lock{
				clientID:  clientID,
				timestamp: timestamp,
				lockedAt:  time.Now(),
			}
			return true
		}

		// Lock is held by someone else and hasn't timed out
		log.Printf("Node %d: Lock on item %d DENIED - held by %s (age: %v)",
			n.id, dataItemID, lock.clientID, time.Since(lock.lockedAt))
		return false
	}

	// No lock exists, grant it
	n.locks[dataItemID] = &Lock{
		clientID:  clientID,
		timestamp: timestamp,
		lockedAt:  time.Now(),
	}
	log.Printf("Node %d: Lock on item %d GRANTED to %s", n.id, dataItemID, clientID)
	return true
}

// releaseLock releases a lock on a data item
func (n *Node) releaseLock(dataItemID int32, clientID string, timestamp int64) {
	n.lockMu.Lock()
	defer n.lockMu.Unlock()

	// Check if lock exists
	if lock, exists := n.locks[dataItemID]; exists {
		// Only release if held by this client
		if lock.clientID == clientID && lock.timestamp == timestamp {
			delete(n.locks, dataItemID)
			log.Printf("Node %d: Lock on item %d RELEASED by %s", n.id, dataItemID, clientID)
		} else {
			log.Printf("Node %d: Lock release DENIED - item %d not held by %s (held by %s)",
				n.id, dataItemID, clientID, lock.clientID)
		}
	}
}

// acquireLocks acquires locks on multiple data items in sorted order (deadlock prevention)
// Returns true if all locks acquired, false otherwise
// If false, also returns list of items that were locked (need to be released)
func (n *Node) acquireLocks(items []int32, clientID string, timestamp int64) (bool, []int32) {
	// Sort items to prevent deadlocks (always acquire in same order)
	sortedItems := make([]int32, len(items))
	copy(sortedItems, items)

	// Simple bubble sort for small arrays
	for i := 0; i < len(sortedItems); i++ {
		for j := i + 1; j < len(sortedItems); j++ {
			if sortedItems[i] > sortedItems[j] {
				sortedItems[i], sortedItems[j] = sortedItems[j], sortedItems[i]
			}
		}
	}

	// Remove duplicates
	uniqueItems := make([]int32, 0, len(sortedItems))
	seen := make(map[int32]bool)
	for _, item := range sortedItems {
		if !seen[item] {
			uniqueItems = append(uniqueItems, item)
			seen[item] = true
		}
	}

	// Try to acquire all locks
	acquired := make([]int32, 0, len(uniqueItems))
	for _, item := range uniqueItems {
		if n.acquireLock(item, clientID, timestamp) {
			acquired = append(acquired, item)
		} else {
			// Failed to acquire this lock, release all previously acquired locks
			log.Printf("Node %d: Failed to acquire lock on item %d, releasing %d locks",
				n.id, item, len(acquired))
			for _, relItem := range acquired {
				n.releaseLock(relItem, clientID, timestamp)
			}
			return false, acquired
		}
	}

	log.Printf("Node %d: Successfully acquired %d locks for %s", n.id, len(acquired), clientID)
	return true, acquired
}

// releaseLocks releases locks on multiple data items
func (n *Node) releaseLocks(items []int32, clientID string, timestamp int64) {
	for _, item := range items {
		n.releaseLock(item, clientID, timestamp)
	}
}

// ============================================================================
// FLUSH STATE - Reset node state between test sets
// ============================================================================

// FlushState resets the node state to initial conditions
func (n *Node) FlushState(ctx context.Context, req *pb.FlushStateRequest) (*pb.FlushStateReply, error) {
	log.Printf("Node %d: 🔄 FlushState requested (db=%v, logs=%v, ballot=%v)",
		n.id, req.ResetDatabase, req.ResetLogs, req.ResetBallot)

	n.mu.Lock()
	defer n.mu.Unlock()

	// Reset database to initial state
	if req.ResetDatabase {
		cluster := n.config.Clusters[int(n.clusterID)]
		n.balances = make(map[int32]int32)
		for itemID := cluster.ShardStart; itemID <= cluster.ShardEnd; itemID++ {
			n.balances[int32(itemID)] = n.config.Data.InitialBalance
		}
		log.Printf("Node %d: Database reset to initial state (%d items with balance %d)",
			n.id, len(n.balances), n.config.Data.InitialBalance)

		// Save to disk
		n.mu.Unlock()
		if err := n.saveDatabase(); err != nil {
			log.Printf("Node %d: Warning - failed to save database: %v", n.id, err)
		}
		n.mu.Lock()
	}

	// Reset Paxos logs
	if req.ResetLogs {
		n.log = make(map[int32]*types.LogEntry)
		n.newViewLog = make([]*pb.NewViewRequest, 0)
		n.clientLastReply = make(map[string]*pb.TransactionReply)
		n.clientLastTS = make(map[string]int64)
		n.nextSeqNum = 1
		n.lastExecuted = 0
		log.Printf("Node %d: Paxos logs cleared", n.id)
	}

	// Reset ballot and leader state
	if req.ResetBallot {
		n.currentBallot = types.NewBallot(0, n.id)
		n.promisedBallot = types.NewBallot(0, 0)
		n.isLeader = false
		n.leaderID = -1
		n.systemInitialized = false
		log.Printf("Node %d: Ballot and leader state reset", n.id)
	}

	// Clear locks
	n.lockMu.Lock()
	n.locks = make(map[int32]*Lock)
	n.lockMu.Unlock()

	// Clear WAL
	n.walMu.Lock()
	n.wal = make(map[string]*types.WALEntry)
	n.walMu.Unlock()

	// Reset access tracker
	if n.accessTracker != nil {
		n.accessTracker.Reset()
	}

	// Reset performance counters
	n.perfMu.Lock()
	n.totalTransactions = 0
	n.successfulTransactions = 0
	n.failedTransactions = 0
	n.twoPCCoordinator = 0
	n.twoPCParticipant = 0
	n.twoPCCommits = 0
	n.twoPCAborts = 0
	n.electionsStarted = 0
	n.electionsWon = 0
	n.proposalsMade = 0
	n.proposalsAccepted = 0
	n.locksAcquired = 0
	n.locksTimeout = 0
	n.totalTransactionTimeMs = 0
	n.totalTransactionCount = 0
	n.total2PCTimeMs = 0
	n.total2PCCount = 0
	n.startTime = time.Now()
	n.perfMu.Unlock()

	log.Printf("Node %d: ✅ State flushed successfully", n.id)

	// Reset leader timeout timer to prevent race conditions during election
	if n.leaderTimer != nil {
		n.leaderTimer.Stop()
		// Give longer timeout after FLUSH to allow clean election
		n.leaderTimer = time.NewTimer(2 * time.Second)
	}

	// Trigger leader election after flush to ensure system is ready
	// Only trigger if this is the expected leader for the cluster
	expectedLeader := n.config.GetLeaderNodeForCluster(int(n.clusterID))
	if expectedLeader == n.id {
		log.Printf("Node %d: Triggering leader election after flush (expected leader for cluster %d)", n.id, n.clusterID)
		go func() {
			time.Sleep(100 * time.Millisecond) // Brief delay
			n.StartLeaderElection()
		}()
	}

	return &pb.FlushStateReply{
		Success: true,
		Message: fmt.Sprintf("Node %d state flushed", n.id),
	}, nil
}
