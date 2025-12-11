package node

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cockroachdb/pebble"

	"paxos-banking/internal/config"
	"paxos-banking/internal/redistribution"
	"paxos-banking/internal/types"
	pb "paxos-banking/proto"
)

type DataItemLock struct {
	clientID  string
	timestamp int64
	lockedAt  time.Time
}

type Node struct {
	pb.UnimplementedPaxosNodeServer

	paxosMu   sync.RWMutex
	logMu     sync.RWMutex
	balanceMu sync.RWMutex
	clientMu  sync.RWMutex
	execMu    sync.Mutex

	id        int32
	clusterID int32
	address   string
	config    *config.Config

	isLeader       bool
	currentBallot  *types.Ballot
	promisedBallot *types.Ballot
	leaderID       int32

	nextSeqNum   int32
	lastExecuted int32
	execNotify   chan struct{}

	log               map[int32]*types.LogEntry
	newViewLog        []*pb.NewViewRequest
	bootstrapComplete bool

	clientLastReply map[string]*pb.TransactionReply
	clientLastTS    map[string]int64

	balances map[int32]int32

	checkpointMu        sync.RWMutex
	lastCheckpointSeq   int32
	checkpointedBalance map[int32]int32
	checkpointInterval  int32
	modifiedItems       map[int32]bool

	lockMu      sync.Mutex
	locks       map[int32]*DataItemLock
	lockTimeout time.Duration

	walMu   sync.RWMutex
	wal     map[string]*types.WALEntry
	walFile string

	twoPCState TwoPCState
	twoPCWAL   map[string]map[int32]int32

	grpcServer  *grpc.Server
	peerClients map[int32]pb.PaxosNodeClient
	peerConns   map[int32]*grpc.ClientConn

	allClusterClients map[int32]pb.PaxosNodeClient
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
	dataDir    string
	dbFilePath string     // Legacy JSON file path
	pebbleDB   *pebble.DB // PebbleDB handle for fast persistence

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
	accessTracker    *redistribution.AccessTracker
	migrationState   *MigrationState
	migratedInItems  map[int32]bool // Items that were migrated INTO this node (persist across flush)
	migratedOutItems map[int32]bool // Items that were migrated OUT of this node (don't recreate on flush)
}

// getCheckpointInterval returns checkpoint interval from config or default (100)
func getCheckpointInterval(cfg *config.Config) int32 {
	if cfg.Data.CheckpointInterval > 0 {
		return cfg.Data.CheckpointInterval
	}
	return 100 // Default: checkpoint every 100 transactions
}

func NewNode(id int32, cfg *config.Config) (*Node, error) {
	// jittered timeout for leader election
	rand.Seed(time.Now().UnixNano() + int64(id))
	baseTimeout := 100 * time.Millisecond                     // Was 500ms - now 100ms for performance
	jitter := time.Duration(rand.Intn(50)) * time.Millisecond // 0-50ms (was 0-200ms)
	timerDuration := baseTimeout + jitter

	// Get cluster ID for this node
	clusterID := int32(cfg.GetClusterForNode(int(id)))

	n := &Node{
		id:                 id,
		clusterID:          clusterID,
		address:            cfg.GetNodeAddress(int(id)),
		config:             cfg,
		isLeader:           false,
		currentBallot:      types.NewBallot(0, id),
		promisedBallot:     types.NewBallot(0, 0),
		leaderID:           -1,
		nextSeqNum:         1,
		lastExecuted:       0,
		log:                make(map[int32]*types.LogEntry),
		newViewLog:         make([]*pb.NewViewRequest, 0),
		clientLastReply:    make(map[string]*pb.TransactionReply),
		clientLastTS:       make(map[string]int64),
		balances:           make(map[int32]int32), // Changed to int32 -> int32
		checkpointInterval: getCheckpointInterval(cfg),
		modifiedItems:      make(map[int32]bool),                    // Track modified items
		locks:              make(map[int32]*DataItemLock),           // Initialize lock table
		lockTimeout:        100 * time.Millisecond,                  // Was 2s - now 100ms for performance
		wal:                make(map[string]*types.WALEntry),        // Initialize WAL (Phase 5)
		walFile:            fmt.Sprintf("data/node%d_wal.json", id), // WAL persistence file
		peerClients:        make(map[int32]pb.PaxosNodeClient),
		peerConns:          make(map[int32]*grpc.ClientConn),
		allClusterClients:  make(map[int32]pb.PaxosNodeClient),
		allClusterConns:    make(map[int32]*grpc.ClientConn),
		timerDuration:      timerDuration,
		dbFilePath:         fmt.Sprintf("data/node%d_db.json", id),
		prepareCooldown:    time.Duration(5+rand.Intn(10)) * time.Millisecond, // Was 50-100ms - now 5-15ms for performance
		heartbeatInterval:  10 * time.Millisecond,                             // Was 50ms - now 10ms for performance
		heartbeatStop:      nil,
		stopChan:           make(chan struct{}),
		isActive:           true,
		systemInitialized:  false,
		startTime:          time.Now(),                              // Phase 7: Track uptime
		accessTracker:      redistribution.NewAccessTracker(100000), // Phase 9: Access pattern tracking
		migrationState:     nil,                                     // Phase 9: Initialized on first migration
		migratedInItems:    make(map[int32]bool),                    // Phase 9: Items migrated into this node
		migratedOutItems:   make(map[int32]bool),                    // Phase 9: Items migrated out of this node
		twoPCState: TwoPCState{
			transactions: make(map[string]*TwoPCTransaction),
		},
		twoPCWAL:   make(map[string]map[int32]int32), // WAL for all nodes
		execNotify: make(chan struct{}, 1000),        // Buffered channel for execution signals
	}

	// Open PebbleDB for persistent storage
	pebbleDir := fmt.Sprintf("data/node%d_pebble", id)
	opts := &pebble.Options{
		// Optimize for write-heavy workload
		MemTableSize:             64 << 20, // 64 MB
		MaxConcurrentCompactions: func() int { return 2 },
		L0CompactionThreshold:    4,
		L0StopWritesThreshold:    8,
	}

	db, err := pebble.Open(pebbleDir, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to open PebbleDB: %w", err)
	}
	n.pebbleDB = db
	n.dataDir = "data"

	log.Printf("Node %d: 💾 Opened PebbleDB at %s", id, pebbleDir)

	// Try to load existing database from PebbleDB
	log.Printf("Node %d (Cluster %d): 🔄 Loading database from PebbleDB", id, clusterID)
	if err := n.loadDatabase(); err != nil {
		log.Printf("Node %d: No existing database, initializing fresh", id)

		// Initialize database with data items in this node's shard
		cluster := cfg.Clusters[int(clusterID)]
		for itemID := cluster.ShardStart; itemID <= cluster.ShardEnd; itemID++ {
			n.balances[itemID] = cfg.Data.InitialBalance
		}

		// Save initial state to PebbleDB
		if err := n.saveDatabase(); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to save initial database: %w", err)
		}

		log.Printf("Node %d (Cluster %d): 💾 Initialized %d data items (range %d-%d) with balance %d",
			id, clusterID, len(n.balances), cluster.ShardStart, cluster.ShardEnd, cfg.Data.InitialBalance)
	} else {
		log.Printf("Node %d: ✅ Loaded %d items from PebbleDB", id, len(n.balances))
	}

	// Load checkpoint if exists (restores balances and lastExecuted)
	if err := n.loadCheckpoint(); err != nil {
		log.Printf("Node %d: ⚠️ Failed to load checkpoint: %v", id, err)
	}

	// Load migration state - items stay in their migrated shards until flushall
	if err := n.loadMigratedInItems(); err != nil {
		log.Printf("Node %d: 📊 No migrated-in items to load (fresh start)", id)
	}
	if err := n.loadMigratedOutItems(); err != nil {
		log.Printf("Node %d: 📊 No migrated-out items to load (fresh start)", id)
	}
	// Access tracker starts fresh each session (only tracks current session's transactions)
	n.accessTracker = redistribution.NewAccessTracker(100000)
	log.Printf("Node %d: ✅ Migration state loaded - migrated items persist until flushall", id)

	log.Printf("Node %d (Cluster %d): Timer set to %v (base: 100ms + jitter: %v)",
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

	// Start background execution thread (sequential execution for linearizability)
	go n.backgroundExecutionThread()

	// Start the persistent leader timer goroutine
	n.startLeaderTimer()

	log.Printf("Node %d: Ready (standby mode - waiting for transaction)", n.id)
	return nil
}

func (n *Node) connectToPeers() error {
	// No lock needed - connections are set up once during initialization
	// (peerClients and allClusterClients are effectively read-only after this)

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
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
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

	// Check if client already exists (no lock - allClusterClients effectively read-only after setup)
	client, exists := n.allClusterClients[nodeID]

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

	// Save the connection for future use (no lock - connections set up once)
	n.allClusterConns[nodeID] = conn
	n.allClusterClients[nodeID] = client

	log.Printf("Node %d: ✅ Established cross-cluster connection to node %d", n.id, nodeID)
	return client, nil
}

// isExpectedLeader checks if this node should be leader (first node in cluster)
func (n *Node) isExpectedLeader() bool {
	expectedLeader := n.config.GetLeaderNodeForCluster(int(n.clusterID))
	return n.id == expectedLeader
}

func (n *Node) Stop() {
	// Check and update active status (use paxosMu)
	n.paxosMu.Lock()
	if !n.isActive {
		n.paxosMu.Unlock()
		return
	}
	n.isActive = false
	n.paxosMu.Unlock()

	// Stop timers & heartbeats
	n.timerMu.Lock()
	if n.leaderTimer != nil {
		n.leaderTimer.Stop()
	}
	n.timerMu.Unlock()

	n.stopHeartbeat()

	// Close peer conns (no lock - done during shutdown)
	for _, c := range n.peerConns {
		_ = c.Close()
	}
	n.peerConns = map[int32]*grpc.ClientConn{}
	n.peerClients = map[int32]pb.PaxosNodeClient{}

	// stop server
	if n.grpcServer != nil {
		n.grpcServer.GracefulStop()
	}

	close(n.stopChan)
}

func (n *Node) quorumSize() int {
	// quorum is floor(N/2) + 1 where N is the cluster size (not total nodes)
	n.paxosMu.RLock()
	defer n.paxosMu.RUnlock()
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

// startLeaderTimer starts the leader timeout timer goroutine
func (n *Node) startLeaderTimer() {
	n.timerMu.Lock()
	n.leaderTimer = time.NewTimer(n.timerDuration)
	n.timerMu.Unlock()

	go func() {
		for {
			<-n.leaderTimer.C

			// Check if we should start election
			n.paxosMu.RLock()
			isLeader := n.isLeader
			active := n.isActive
			knownLeader := n.leaderID
			n.paxosMu.RUnlock()

			if !isLeader && active {
				// only expected leaders (n1, n4, n7) can start elections from timer
				if n.isExpectedLeader() && knownLeader <= 0 {
					log.Printf("Node %d: ⏰ Leader timeout - starting election (expected leader, no known leader)",
						n.id)
					go n.StartLeaderElection()
				}
			}

			n.timerMu.Lock()
			if n.leaderTimer != nil {
				n.leaderTimer.Reset(n.timerDuration)
			}
			n.timerMu.Unlock()
		}
	}()
}

// resetLeaderTimer resets the leader timeout timer
func (n *Node) resetLeaderTimer() {
	n.timerMu.Lock()
	defer n.timerMu.Unlock()

	if n.leaderTimer != nil {
		// Stop and drain the timer
		if !n.leaderTimer.Stop() {
			// Timer already fired, drain the channel
			select {
			case <-n.leaderTimer.C:
			default:
			}
		}
		// Reset the timer - this is the idiomatic Go way
		n.leaderTimer.Reset(n.timerDuration)
	}
}

func (n *Node) startHeartbeat() {
	// start only once
	n.paxosMu.Lock()
	if n.heartbeatStop != nil {
		n.paxosMu.Unlock()
		return
	}
	n.heartbeatStop = make(chan struct{})
	n.paxosMu.Unlock()

	go func() {
		ticker := time.NewTicker(n.heartbeatInterval)
		defer ticker.Stop()

		// Ensure we clean up heartbeatStop when exiting
		defer func() {
			n.paxosMu.Lock()
			n.heartbeatStop = nil
			n.paxosMu.Unlock()
		}()

		for {
			select {
			case <-ticker.C:
				n.paxosMu.RLock()
				if !n.isLeader {
					n.paxosMu.RUnlock()
					log.Printf("Node %d: ❌ Heartbeat goroutine exiting - no longer leader", n.id)
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
				n.paxosMu.RUnlock()

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
	n.paxosMu.Lock()
	defer n.paxosMu.Unlock()
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
			n.paxosMu.Lock()
			if !n.isActive {
				n.paxosMu.Unlock()
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
			n.paxosMu.Unlock()

			// Try to connect to missing peers
			for _, peer := range missingPeers {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				conn, err := grpc.DialContext(ctx, peer.addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
				cancel()

				if err == nil {
					n.paxosMu.Lock()
					n.peerConns[peer.id] = conn
					n.peerClients[peer.id] = pb.NewPaxosNodeClient(conn)
					n.allClusterClients[peer.id] = n.peerClients[peer.id]
					n.allClusterConns[peer.id] = n.peerConns[peer.id]
					n.paxosMu.Unlock()
					log.Printf("Node %d (Cluster %d): Reconnected to peer node %d (%s)", n.id, n.clusterID, peer.id, peer.addr)
				}
			}

		case <-n.stopChan:
			return
		}
	}
}

// saveDatabase persists the current balance state to a JSON file
// saveBalance persists a single balance update to PebbleDB (fast!)
// This is called after every transaction
// Note: Using NoSync for simulation performance (OS will flush within 5-30s)
func (n *Node) saveBalance(itemID int32, balance int32) error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	key := []byte(fmt.Sprintf("balance:%d", itemID))
	value := []byte(fmt.Sprintf("%d", balance))

	// Write single item without fsync for speed (safe in controlled simulation)
	if err := n.pebbleDB.Set(key, value, pebble.NoSync); err != nil {
		return fmt.Errorf("failed to save balance for item %d: %w", itemID, err)
	}

	return nil
}

// saveDatabase persists the entire database to PebbleDB (used for initialization)
func (n *Node) saveDatabase() error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	// Use balanceMu for balance access
	n.balanceMu.RLock()
	defer n.balanceMu.RUnlock()

	// Write all balances to PebbleDB in a batch for efficiency
	batch := n.pebbleDB.NewBatch()
	defer batch.Close()

	for itemID, balance := range n.balances {
		key := []byte(fmt.Sprintf("balance:%d", itemID))
		value := []byte(fmt.Sprintf("%d", balance))

		if err := batch.Set(key, value, pebble.NoSync); err != nil {
			return fmt.Errorf("failed to set balance for item %d: %w", itemID, err)
		}
	}

	// Commit the batch (this is where fsync happens for durability)
	if err := batch.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("failed to commit batch: %w", err)
	}

	return nil
}

// loadDatabase loads the database from PebbleDB on startup
func (n *Node) loadDatabase() error {
	if n.pebbleDB == nil {
		return fmt.Errorf("PebbleDB not initialized")
	}

	// Use balanceMu for balance access
	n.balanceMu.Lock()
	defer n.balanceMu.Unlock()

	// Iterate over all balance keys
	iter, err := n.pebbleDB.NewIter(&pebble.IterOptions{
		LowerBound: []byte("balance:"),
		UpperBound: []byte("balance;"), // Next character after ':' in ASCII
	})
	if err != nil {
		return fmt.Errorf("failed to create iterator: %w", err)
	}
	defer iter.Close()

	count := 0
	for iter.First(); iter.Valid(); iter.Next() {
		key := string(iter.Key())
		value := string(iter.Value())

		var itemID, balance int32
		if _, err := fmt.Sscanf(key, "balance:%d", &itemID); err != nil {
			log.Printf("Node %d: ⚠️  Warning - failed to parse key %s: %v", n.id, key, err)
			continue
		}
		if _, err := fmt.Sscanf(value, "%d", &balance); err != nil {
			log.Printf("Node %d: ⚠️  Warning - failed to parse value for key %s: %v", n.id, key, err)
			continue
		}

		n.balances[itemID] = balance
		count++
	}

	if err := iter.Error(); err != nil {
		return fmt.Errorf("iterator error: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("no data found")
	}

	return nil
}

// Close shuts down the node and closes PebbleDB
func (n *Node) Close() error {
	log.Printf("Node %d: Closing...", n.id)

	// Stop all background goroutines
	close(n.stopChan)

	// Close gRPC server
	if n.grpcServer != nil {
		n.grpcServer.GracefulStop()
	}

	// Close PebbleDB
	if n.pebbleDB != nil {
		log.Printf("Node %d: Closing PebbleDB", n.id)
		if err := n.pebbleDB.Close(); err != nil {
			return fmt.Errorf("failed to close PebbleDB: %w", err)
		}
	}

	log.Printf("Node %d: ✅ Closed successfully", n.id)
	return nil
}

func (n *Node) startGapDetection() {
	ticker := time.NewTicker(1 * time.Second)
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
	n.paxosMu.RLock()
	if !n.isActive {
		n.paxosMu.RUnlock()
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
	n.paxosMu.RUnlock()

	// 🔥 FIX: If we have gaps and we're NOT the leader, request the leader to trigger NEW-VIEW
	// This breaks the deadlock where nodes with gaps wait forever for data
	if maxSeq > lastExec+1 {
		// Check if we have actual gaps (not just uncommitted sequences)
		n.paxosMu.RLock()
		hasGaps := false
		for seq := lastExec + 1; seq < maxSeq; seq++ {
			if _, exists := n.log[seq]; !exists {
				hasGaps = true
				break
			}
		}
		n.paxosMu.RUnlock()

		if hasGaps && !isLeader && leaderID > 0 {
			// We have gaps and there's a leader - send a special request to trigger NEW-VIEW
			log.Printf("Node %d: 🔍 Detected persistent gaps (lastExec=%d, maxSeq=%d) - requesting recovery from leader %d",
				n.id, lastExec, maxSeq, leaderID)
			go n.requestRecoveryFromLeader(leaderID)
		} else if hasGaps && !isLeader && leaderID <= 0 {
			// We have gaps but no known leader - only expected leaders should trigger election
			if n.isExpectedLeader() {
				log.Printf("Node %d: 🔍 Detected persistent gaps with no leader (lastExec=%d, maxSeq=%d) - triggering election",
					n.id, lastExec, maxSeq)
				go n.StartLeaderElection()
			}
		}
	}
}

// requestRecoveryFromLeader actively requests missing sequences from peers
// This breaks the deadlock where nodes with gaps can't recover
func (n *Node) requestRecoveryFromLeader(leaderID int32) {
	// Find which sequences we're missing
	n.paxosMu.RLock()
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
	n.paxosMu.RUnlock()

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
		n.paxosMu.RLock()
		_, exists := n.log[seq]
		n.paxosMu.RUnlock()
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

				// Directly insert into log bypassing ballot check
				// This is safe because the entry is already committed on other nodes
				ballot := types.BallotFromProto(resp.Entry.Ballot)
				ent := types.NewLogEntry(ballot, resp.Entry.SequenceNumber, resp.Entry.Request, resp.Entry.IsNoop)
				ent.Status = "A" // Mark as accepted

				n.logMu.Lock()
				if existing, ok := n.log[resp.Entry.SequenceNumber]; ok {
					// Only replace if the recovered entry has a higher ballot
					existingBallot := existing.Ballot
					if ballot.GreaterThan(existingBallot) || ballot.Equal(existingBallot) {
						n.log[resp.Entry.SequenceNumber] = ent
						log.Printf("Node %d: 📝 Filled gap seq=%d with ballot=%s", n.id, resp.Entry.SequenceNumber, ballot.String())
					}
				} else {
					n.log[resp.Entry.SequenceNumber] = ent
					log.Printf("Node %d: 📝 Filled gap seq=%d with ballot=%s", n.id, resp.Entry.SequenceNumber, ballot.String())
				}
				n.logMu.Unlock()

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
			n.locks[dataItemID] = &DataItemLock{
				clientID:  clientID,
				timestamp: timestamp,
				lockedAt:  time.Now(),
			}
			return true
		}

		return false
	}

	// No lock exists, grant it
	n.locks[dataItemID] = &DataItemLock{
		clientID:  clientID,
		timestamp: timestamp,
		lockedAt:  time.Now(),
	}
	return true
}

// releaseLock releases a lock on a data item
func (n *Node) releaseLock(dataItemID int32, clientID string, timestamp int64) {
	n.lockMu.Lock()
	defer n.lockMu.Unlock()

	// Only release if held by this client
	if lock, exists := n.locks[dataItemID]; exists {
		if lock.clientID == clientID && lock.timestamp == timestamp {
			delete(n.locks, dataItemID)
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
			// Failed - release all previously acquired locks
			for _, relItem := range acquired {
				n.releaseLock(relItem, clientID, timestamp)
			}
			return false, acquired
		}
	}

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

	n.paxosMu.Lock()
	defer n.paxosMu.Unlock()

	// Reset database to initial state
	if req.ResetDatabase {
		cluster := n.config.Clusters[int(n.clusterID)]

		// Save migrated-in items before reset (they should persist)
		migratedInItems := make(map[int32]int32)
		if !req.ResetAccessTracker && n.migratedInItems != nil {
			// Preserve migrated-in items with their current balances
			for itemID := range n.migratedInItems {
				if balance, exists := n.balances[itemID]; exists {
					migratedInItems[itemID] = balance
				}
			}
		}

		// Reset to initial state (but skip migrated-out items)
		n.balances = make(map[int32]int32)
		for itemID := cluster.ShardStart; itemID <= cluster.ShardEnd; itemID++ {
			// Skip items that were migrated OUT (unless doing full reset)
			if !req.ResetAccessTracker && n.migratedOutItems != nil && n.migratedOutItems[int32(itemID)] {
				continue // Don't recreate migrated-out items
			}
			n.balances[int32(itemID)] = n.config.Data.InitialBalance
		}

		// Restore migrated-in items (if not doing full reset)
		if !req.ResetAccessTracker {
			for itemID, balance := range migratedInItems {
				n.balances[itemID] = balance
			}
			log.Printf("Node %d: Database reset (preserved %d migrated-in items, excluded %d migrated-out items)",
				n.id, len(migratedInItems), len(n.migratedOutItems))
		} else {
			log.Printf("Node %d: Database fully reset to initial state (%d items with balance %d)",
				n.id, len(n.balances), n.config.Data.InitialBalance)
		}

		// Save to disk
		n.paxosMu.Unlock()
		if err := n.saveDatabase(); err != nil {
			log.Printf("Node %d: Warning - failed to save database: %v", n.id, err)
		}
		n.paxosMu.Lock()
	}

	// Reset Paxos logs
	if req.ResetLogs {
		n.log = make(map[int32]*types.LogEntry)
		n.newViewLog = make([]*pb.NewViewRequest, 0)
		n.bootstrapComplete = false // Reset so bootstrap NEW-VIEWs aren't logged
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
		n.leaderID = -1 // -1 indicates no known leader (fresh start)
		n.systemInitialized = false
		log.Printf("Node %d: Ballot and leader state reset (leaderID → -1, ballots → 0)", n.id)
	}

	// Clear locks
	n.lockMu.Lock()
	n.locks = make(map[int32]*DataItemLock)
	n.lockMu.Unlock()

	// Clear WAL
	n.walMu.Lock()
	n.wal = make(map[string]*types.WALEntry)
	n.walMu.Unlock()

	// Reset access tracker and migrated items ONLY if explicitly requested (flushall)
	// By default (reset_access_tracker=false), resharding data persists across test cases
	if req.ResetAccessTracker {
		if n.accessTracker != nil {
			n.accessTracker.Reset()
		}
		// Clear access tracker from persistent storage
		if err := n.clearAccessTrackerFromDB(); err != nil {
			log.Printf("Node %d: Warning - failed to clear access tracker from DB: %v", n.id, err)
		}
		// Clear migrated-in items tracking
		if err := n.clearMigratedInItems(); err != nil {
			log.Printf("Node %d: Warning - failed to clear migratedInItems: %v", n.id, err)
		}
		// Clear migrated-out items tracking
		if err := n.clearMigratedOutItems(); err != nil {
			log.Printf("Node %d: Warning - failed to clear migratedOutItems: %v", n.id, err)
		}
		log.Printf("Node %d: Access tracker and migration tracking reset (resharding data cleared)", n.id)
	} else {
		log.Printf("Node %d: Resharding data preserved (access tracker + migration tracking persist)", n.id)
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

	// Stop leader timer during flush
	// This prevents elections during the flush->SetActive->first-transaction window
	n.timerMu.Lock()
	if n.leaderTimer != nil {
		n.leaderTimer.Stop()
		// Drain the channel if timer already fired
		select {
		case <-n.leaderTimer.C:
		default:
		}
	}
	n.timerMu.Unlock()

	// Stop the leader timer during flush
	n.timerMu.Lock()
	if n.leaderTimer != nil {
		n.leaderTimer.Stop()
		// Drain the channel if timer already fired
		select {
		case <-n.leaderTimer.C:
		default:
		}
	}
	n.timerMu.Unlock()

	log.Printf("Node %d: ✅ State flushed successfully", n.id)

	// DON'T trigger election here - let first transaction trigger it
	// This ensures n1/n4/n7 become leaders as designed
	// First transaction will call resetLeaderTimer() to restart the timer

	return &pb.FlushStateReply{
		Success: true,
		Message: fmt.Sprintf("Node %d state flushed", n.id),
	}, nil
}

// ============================================================================
// PRINT FUNCTIONS - Required by project specification
// ============================================================================

// PrintDB returns all modified balances on this node
func (n *Node) PrintDB(ctx context.Context, req *pb.PrintDBRequest) (*pb.PrintDBReply, error) {
	n.balanceMu.RLock()
	defer n.balanceMu.RUnlock()

	initialBalance := n.config.Data.InitialBalance
	var balances []*pb.BalanceEntry

	// Get cluster info
	cluster := n.config.Clusters[int(n.clusterID)]

	for itemID, balance := range n.balances {
		// Include if different from initial balance OR if requested to include zero balance
		if balance != initialBalance || req.IncludeZeroBalance {
			balances = append(balances, &pb.BalanceEntry{
				DataItem: itemID,
				Balance:  balance,
			})
		}
	}

	// Apply limit if specified
	if req.Limit > 0 && int32(len(balances)) > req.Limit {
		balances = balances[:req.Limit]
	}

	return &pb.PrintDBReply{
		Success:    true,
		Balances:   balances,
		TotalItems: int32(len(n.balances)),
		ShardStart: cluster.ShardStart,
		ShardEnd:   cluster.ShardEnd,
		NodeId:     n.id,
		ClusterId:  n.clusterID,
		Message:    fmt.Sprintf("Node %d: %d modified items", n.id, len(balances)),
	}, nil
}

// PrintView returns NEW-VIEW messages with all parameters (required by project spec)
func (n *Node) PrintView(ctx context.Context, req *pb.PrintViewRequest) (*pb.PrintViewReply, error) {
	// Collect Paxos state
	n.paxosMu.RLock()
	isLeader := n.isLeader
	leaderID := n.leaderID
	ballotNum := n.currentBallot.Number
	ballotNode := n.currentBallot.NodeID
	isActive := n.isActive
	n.paxosMu.RUnlock()

	n.logMu.RLock()
	nextSeq := n.nextSeqNum
	lastExec := n.lastExecuted
	newViewCount := len(n.newViewLog)
	// Copy NEW-VIEW log for safe access
	newViewCopy := make([]*pb.NewViewRequest, len(n.newViewLog))
	copy(newViewCopy, n.newViewLog)
	n.logMu.RUnlock()

	// Build message with NEW-VIEW details (required by project spec)
	var message string
	if newViewCount == 0 {
		message = fmt.Sprintf("Node %d: No NEW-VIEW messages (no leader elections since test start)", n.id)
	} else {
		message = fmt.Sprintf("Node %d: %d NEW-VIEW message(s):\n", n.id, newViewCount)
		for i, nv := range newViewCopy {
			ballot := types.BallotFromProto(nv.Ballot)
			message += fmt.Sprintf("  [%d] NEW-VIEW Ballot=(%d,%d) LeaderNode=%d AcceptMsgs=%d\n",
				i+1, ballot.Number, ballot.NodeID, ballot.NodeID, len(nv.AcceptMessages))

			// Include accept message details (all parameters)
			for j, acc := range nv.AcceptMessages {
				if acc.IsNoop {
					message += fmt.Sprintf("      %d. Seq=%d NO-OP\n", j+1, acc.SequenceNumber)
				} else if acc.Request != nil && acc.Request.Transaction != nil {
					tx := acc.Request.Transaction
					message += fmt.Sprintf("      %d. Seq=%d Txn=(%d->%d:%d)\n",
						j+1, acc.SequenceNumber, tx.Sender, tx.Receiver, tx.Amount)
				}
			}
		}
	}

	// Include recent log entries if requested
	var recentLog []*pb.LogEntrySummary
	if req.IncludeLog {
		n.logMu.RLock()
		logEntries := req.LogEntries
		if logEntries <= 0 {
			logEntries = 10 // Default
		}

		// Get last N entries
		startSeq := n.lastExecuted - logEntries + 1
		if startSeq < 1 {
			startSeq = 1
		}

		for seq := startSeq; seq <= n.lastExecuted; seq++ {
			if entry, ok := n.log[seq]; ok {
				summary := &pb.LogEntrySummary{
					Sequence: seq,
					Executed: entry.Status == "E",
				}
				if !entry.IsNoOp && entry.Request != nil && entry.Request.Transaction != nil {
					tx := entry.Request.Transaction
					summary.Sender = tx.Sender
					summary.Receiver = tx.Receiver
					summary.Amount = tx.Amount
					summary.ClientId = entry.Request.ClientId
				}
				recentLog = append(recentLog, summary)
			}
		}
		n.logMu.RUnlock()
	}

	return &pb.PrintViewReply{
		Success:      true,
		NodeId:       n.id,
		ClusterId:    n.clusterID,
		IsLeader:     isLeader,
		LeaderId:     leaderID,
		BallotNumber: ballotNum,
		BallotNodeId: ballotNode,
		NextSequence: nextSeq,
		LastExecuted: lastExec,
		RecentLog:    recentLog,
		IsActive:     isActive,
		Message:      message,
	}, nil
}
