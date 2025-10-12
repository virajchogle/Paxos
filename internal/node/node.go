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

	// 🎯 System Initialization
	systemInitialized bool
}

func NewNode(id int32, cfg *config.Config) (*Node, error) {
	// jittered timeout - use milliseconds for leader election
	// Increased to prevent election storms
	rand.Seed(time.Now().UnixNano() + int64(id))
	baseTimeout := 500 * time.Millisecond
	jitter := time.Duration(rand.Intn(200)) * time.Millisecond // 0-200ms
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
		heartbeatInterval: 50 * time.Millisecond,                              // Fast heartbeats
		heartbeatStop:     nil,
		timerExpired:      false,
		prepareBuffer:     make(map[string]*pb.PrepareRequest),
		stopChan:          make(chan struct{}),
		isActive:          true,
		isRecovering:      false,
		recoverySeq:       0,
		systemInitialized: false,
	}

	// Initialize database with fresh balances from config (resets on every start, like logs)
	log.Printf("Node %d: 🔄 Initializing fresh database from config", id)
	for _, clientID := range cfg.Clients.ClientIDs {
		n.balances[clientID] = cfg.Clients.InitialBalance
	}

	// Save initial state to disk
	if err := n.saveDatabase(); err != nil {
		log.Printf("Node %d: ⚠️  Warning - failed to save initial database: %v", id, err)
	} else {
		log.Printf("Node %d: 💾 Initialized %s with balance %d for each client",
			id, n.dbFilePath, cfg.Clients.InitialBalance)
	}

	log.Printf("Node %d: Timer set to %v (base: 500ms + jitter: %v)",
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
	for seq := range n.acceptLog {
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
			if _, exists := n.acceptLog[seq]; !exists {
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
	maxSeq := lastExec
	for seq := range n.acceptLog {
		if seq > maxSeq {
			maxSeq = seq
		}
	}

	// Identify gaps
	missingSeqs := []int32{}
	for seq := lastExec + 1; seq <= maxSeq; seq++ {
		if _, exists := n.acceptLog[seq]; !exists {
			missingSeqs = append(missingSeqs, seq)
		}
	}

	if len(missingSeqs) == 0 {
		n.mu.RUnlock()
		return // No gaps, nothing to do
	}

	peers := make(map[int32]pb.PaxosNodeClient)
	for k, v := range n.peerClients {
		peers[k] = v
	}
	n.mu.RUnlock()

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
