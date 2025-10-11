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
	"paxos-banking/internal/types"
	pb "paxos-banking/proto"
)

type Node struct {
	pb.UnimplementedPaxosNodeServer

	mu sync.RWMutex

	// Identity
	id      int32
	address string
	config  *config.Config

	// Paxos state
	isLeader       bool
	currentBallot  *types.Ballot
	promisedBallot *types.Ballot
	leaderID       int32

	// Sequence tracking
	nextSeqNum   int32
	lastExecuted int32

	// Logs
	acceptLog    map[int32]*types.LogEntry
	committedLog map[int32]*types.LogEntry
	newViewLog   []*pb.NewViewRequest

	// Client tracking
	clientLastReply map[string]*pb.TransactionReply
	clientLastTS    map[string]int64

	// Database
	balances map[string]int32

	// Communication
	grpcServer  *grpc.Server
	peerClients map[int32]pb.PaxosNodeClient
	peerConns   map[int32]*grpc.ClientConn

	// Timers
	timerMu           sync.Mutex
	leaderTimer       *time.Timer
	timerDuration     time.Duration
	prepareCooldown   time.Duration
	lastPrepareTime   time.Time
	heartbeatInterval time.Duration
	heartbeatStop     chan struct{}
	timerExpired      bool // Track if leader timer has expired

	// Election state
	prepareBuffer map[string]*pb.PrepareRequest // Buffer prepare messages before timer expires

	// Control
	stopChan chan struct{}
	isActive bool

	// 🔥 Recovery state
	isRecovering bool
	recoverySeq  int32

	// 💾 Persistence
	dbFilePath string

	// 📸 Checkpointing
	checkpointSeq    int32            // Sequence number of last checkpoint
	checkpointState  map[string]int32 // State at checkpoint
	checkpointPeriod int32            // Create checkpoint every N commits
	commitsSinceCP   int32            // Commits since last checkpoint
}

func NewNode(id int32, cfg *config.Config) (*Node, error) {
	// jittered timeout - use milliseconds for fast leader election
	rand.Seed(time.Now().UnixNano() + int64(id))
	baseTimeout := 150 * time.Millisecond
	jitter := time.Duration(rand.Intn(150)) * time.Millisecond // 0-150ms
	timerDuration := baseTimeout + jitter

	n := &Node{
		id:                id,
		address:           cfg.GetNodeAddress(int(id)),
		config:            cfg,
		isLeader:          false,
		currentBallot:     types.NewBallot(0, id),
		promisedBallot:    types.NewBallot(0, 0),
		leaderID:          -1,
		nextSeqNum:        1,
		lastExecuted:      0,
		acceptLog:         make(map[int32]*types.LogEntry),
		committedLog:      make(map[int32]*types.LogEntry),
		newViewLog:        make([]*pb.NewViewRequest, 0),
		clientLastReply:   make(map[string]*pb.TransactionReply),
		clientLastTS:      make(map[string]int64),
		balances:          make(map[string]int32),
		peerClients:       make(map[int32]pb.PaxosNodeClient),
		peerConns:         make(map[int32]*grpc.ClientConn),
		timerDuration:     timerDuration,
		dbFilePath:        fmt.Sprintf("data/node%d_db.json", id),
		prepareCooldown:   time.Duration(50+rand.Intn(50)) * time.Millisecond, // 50-100ms
		checkpointSeq:     0,
		checkpointState:   make(map[string]int32),
		checkpointPeriod:  10, // Checkpoint every 10 commits (can be configurable)
		commitsSinceCP:    0,
		heartbeatInterval: 50 * time.Millisecond, // Fast heartbeats
		heartbeatStop:     nil,
		timerExpired:      false,
		prepareBuffer:     make(map[string]*pb.PrepareRequest),
		stopChan:          make(chan struct{}),
		isActive:          true,
		isRecovering:      false,
		recoverySeq:       0,
	}

	// Try to load existing database from file
	if err := n.loadDatabase(); err != nil {
		// If no saved state, initialize with default balances
		log.Printf("Node %d: No saved database found, initializing with default balances", id)
		for _, clientID := range cfg.Clients.ClientIDs {
			n.balances[clientID] = cfg.Clients.InitialBalance
		}
		// Save initial state
		if err := n.saveDatabase(); err != nil {
			log.Printf("Node %d: Warning - failed to save initial database: %v", id, err)
		}
	} else {
		log.Printf("Node %d: 💾 Loaded database from %s", id, n.dbFilePath)
	}

	log.Printf("Node %d: Timer set to %v (base: 150ms + jitter: %v)",
		id, timerDuration, jitter)

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

	// 🔥 Start recovery phase
	go func() {
		if err := n.StartRecovery(); err != nil {
			log.Printf("Node %d: Recovery failed: %v", n.id, err)
		}
	}()

	n.resetLeaderTimer()

	log.Printf("Node %d: Ready", n.id)
	return nil
}

func (n *Node) connectToPeers() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	totalNodes := len(n.config.Nodes)
	if totalNodes == 0 {
		return fmt.Errorf("no nodes in config")
	}

	for nodeID := range n.config.Nodes {
		nid := int32(nodeID)
		if nid == n.id {
			continue
		}
		addr := n.config.GetNodeAddress(nodeID)
		// dial with short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancel()
		if err != nil {
			log.Printf("Node %d: Warning - couldn't connect to node %d (%s): %v", n.id, nid, addr, err)
			continue
		}
		n.peerConns[nid] = conn
		n.peerClients[nid] = pb.NewPaxosNodeClient(conn)
		log.Printf("Node %d: Connected to node %d (%s)", n.id, nid, addr)
	}

	log.Printf("Node %d: Started with %d peer connections (configured cluster size: %d)", n.id, len(n.peerClients), totalNodes)
	return nil
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
	// quorum is floor(N/2) + 1 where N is configured cluster size
	n.mu.RLock()
	defer n.mu.RUnlock()
	total := len(n.config.Nodes)
	if total <= 0 {
		// safe default
		return 3
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

	// Reset timer expiry flag
	n.mu.Lock()
	n.timerExpired = false
	n.prepareBuffer = make(map[string]*pb.PrepareRequest) // Clear buffer on reset
	n.mu.Unlock()

	// create new timer
	n.leaderTimer = time.NewTimer(n.timerDuration)

	go func() {
		<-n.leaderTimer.C
		n.mu.Lock()
		isLeader := n.isLeader
		active := n.isActive
		n.timerExpired = true // Mark timer as expired

		// Process any buffered prepare messages
		bufferedPrepares := make([]*pb.PrepareRequest, 0, len(n.prepareBuffer))
		for _, prep := range n.prepareBuffer {
			bufferedPrepares = append(bufferedPrepares, prep)
		}
		n.prepareBuffer = make(map[string]*pb.PrepareRequest) // Clear buffer
		n.mu.Unlock()

		if !isLeader && active {
			log.Printf("Node %d: ⏰ Leader timeout - starting election", n.id)

			// If there are buffered prepares, accept the highest one
			if len(bufferedPrepares) > 0 {
				var highestPrepare *pb.PrepareRequest
				var highestBallot *types.Ballot

				for _, prep := range bufferedPrepares {
					ballot := types.BallotFromProto(prep.Ballot)
					if highestBallot == nil || ballot.GreaterThan(highestBallot) {
						highestBallot = ballot
						highestPrepare = prep
					}
				}

				if highestPrepare != nil {
					log.Printf("Node %d: Processing buffered PREPARE %s from node %d",
						n.id, highestBallot.String(), highestPrepare.NodeId)
					// Process the highest prepare message
					go n.respondToBufferedPrepare(highestPrepare)
					return // Don't start own election if responding to buffered prepare
				}
			}

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

			// Find nodes we should be connected to but aren't
			missingPeers := []struct {
				id   int32
				addr string
			}{}
			for nodeID := range n.config.Nodes {
				nid := int32(nodeID)
				if nid == n.id {
					continue
				}
				if _, exists := n.peerClients[nid]; !exists {
					missingPeers = append(missingPeers, struct {
						id   int32
						addr string
					}{id: nid, addr: n.config.GetNodeAddress(nodeID)})
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
					n.mu.Unlock()
					log.Printf("Node %d: Connected to node %d (%s)", n.id, peer.id, peer.addr)
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
	balancesCopy := make(map[string]int32, len(n.balances))
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

// loadDatabase loads the balance state from a JSON file
func (n *Node) loadDatabase() error {
	data, err := os.ReadFile(n.dbFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("database file does not exist")
		}
		return fmt.Errorf("failed to read database file: %w", err)
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if err := json.Unmarshal(data, &n.balances); err != nil {
		return fmt.Errorf("failed to unmarshal balances: %w", err)
	}

	return nil
}

// 📸 Checkpointing functions

// createCheckpoint creates and broadcasts a checkpoint (leader only)
func (n *Node) createCheckpoint(seq int32) {
	n.mu.Lock()

	// Copy current state
	state := make(map[string]int32, len(n.balances))
	for k, v := range n.balances {
		state[k] = v
	}

	n.checkpointSeq = seq
	n.checkpointState = state
	n.commitsSinceCP = 0

	log.Printf("Node %d: 📸 Creating checkpoint at seq=%d", n.id, seq)
	n.mu.Unlock()

	// Broadcast checkpoint to all nodes
	req := &pb.CheckpointRequest{
		SequenceNumber: seq,
		State:          state,
		LeaderId:       n.id,
	}

	for nodeID, client := range n.peerClients {
		if nodeID == n.id {
			continue
		}

		go func(nid int32, c pb.PaxosNodeClient) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, err := c.Checkpoint(ctx, req)
			if err != nil {
				log.Printf("Node %d: Failed to send checkpoint to node %d: %v", n.id, nid, err)
			}
		}(nodeID, client)
	}
}

// Checkpoint RPC handler - receives checkpoint from leader
func (n *Node) Checkpoint(ctx context.Context, req *pb.CheckpointRequest) (*pb.CheckpointReply, error) {
	log.Printf("Node %d: 📸 Received checkpoint from leader %d at seq=%d", n.id, req.LeaderId, req.SequenceNumber)

	n.mu.Lock()
	defer n.mu.Unlock()

	// Update checkpoint
	n.checkpointSeq = req.SequenceNumber
	n.checkpointState = make(map[string]int32, len(req.State))
	for k, v := range req.State {
		n.checkpointState[k] = v
	}

	// Clean up old log entries (seq <= checkpoint)
	for seq := range n.acceptLog {
		if seq <= req.SequenceNumber {
			delete(n.acceptLog, seq)
		}
	}
	for seq := range n.committedLog {
		if seq <= req.SequenceNumber {
			delete(n.committedLog, seq)
		}
	}

	// Discard old new-view messages (keep only the latest one if any)
	if len(n.newViewLog) > 1 {
		n.newViewLog = n.newViewLog[len(n.newViewLog)-1:]
	}

	log.Printf("Node %d: Cleaned up logs up to seq=%d", n.id, req.SequenceNumber)

	return &pb.CheckpointReply{Success: true, NodeId: n.id}, nil
}

// GetCheckpoint RPC handler - allows nodes to fetch checkpoint from peers
func (n *Node) GetCheckpoint(ctx context.Context, req *pb.GetCheckpointRequest) (*pb.GetCheckpointReply, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	if n.checkpointSeq == 0 {
		return &pb.GetCheckpointReply{Success: false}, nil
	}

	// Copy checkpoint state
	state := make(map[string]int32, len(n.checkpointState))
	for k, v := range n.checkpointState {
		state[k] = v
	}

	log.Printf("Node %d: Sending checkpoint (seq=%d) to node %d", n.id, n.checkpointSeq, req.NodeId)

	return &pb.GetCheckpointReply{
		Success:        true,
		SequenceNumber: n.checkpointSeq,
		State:          state,
	}, nil
}
