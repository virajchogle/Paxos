package client

import (
	"context"
	"fmt"
	"log"
	"time"

	pb "paxos-banking/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	id            string
	timestamp     int64
	nodeClients   map[int32]pb.PaxosNodeClient
	nodeConns     map[int32]*grpc.ClientConn
	currentLeader int32
	retryTimeout  time.Duration
}

func NewClient(id string, nodeAddresses map[int32]string) (*Client, error) {
	nodeClients := make(map[int32]pb.PaxosNodeClient)
	nodeConns := make(map[int32]*grpc.ClientConn)

	// Connect to all nodes
	for nodeID, addr := range nodeAddresses {
		conn, err := grpc.NewClient(addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()))

		if err != nil {
			log.Printf("Client %s: Warning - couldn't connect to node %d: %v", id, nodeID, err)
			continue
		}

		nodeConns[nodeID] = conn
		nodeClients[nodeID] = pb.NewPaxosNodeClient(conn)
	}

	if len(nodeClients) == 0 {
		return nil, fmt.Errorf("failed to connect to any nodes")
	}

	return &Client{
		id:            id,
		timestamp:     0,
		nodeClients:   nodeClients,
		nodeConns:     nodeConns,
		currentLeader: 1, // Start with node 1
		retryTimeout:  5 * time.Second,
	}, nil
}

// NewClientSingleNode creates a client connected to a single node (for backwards compatibility)
func NewClientSingleNode(id string, nodeAddress string) (*Client, error) {
	conn, err := grpc.NewClient(nodeAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()))

	if err != nil {
		return nil, err
	}

	nodeClients := make(map[int32]pb.PaxosNodeClient)
	nodeConns := make(map[int32]*grpc.ClientConn)
	nodeClients[1] = pb.NewPaxosNodeClient(conn)
	nodeConns[1] = conn

	return &Client{
		id:            id,
		timestamp:     0,
		nodeClients:   nodeClients,
		nodeConns:     nodeConns,
		currentLeader: 1,
		retryTimeout:  5 * time.Second,
	}, nil
}

// SendTransaction sends a transaction with retry and broadcast mechanism
func (c *Client) SendTransaction(sender, receiver string, amount int32) error {
	c.timestamp++

	req := &pb.TransactionRequest{
		ClientId:  c.id,
		Timestamp: c.timestamp,
		Transaction: &pb.Transaction{
			Sender:   sender,
			Receiver: receiver,
			Amount:   amount,
		},
	}

	// Try 1: Send to current leader
	resp, err := c.sendToNode(c.currentLeader, req)
	if err == nil {
		c.logResult(sender, receiver, amount, resp)
		// Update leader based on response
		if resp.Ballot != nil {
			c.currentLeader = resp.Ballot.NodeId
		}
		return nil
	}

	log.Printf("Client %s: Failed to reach node %d: %v, broadcasting to all nodes...",
		c.id, c.currentLeader, err)

	// Try 2: Broadcast to all nodes on timeout
	for nodeID := range c.nodeClients {
		if nodeID == c.currentLeader {
			continue // Already tried this one
		}

		resp, err = c.sendToNode(nodeID, req)
		if err == nil {
			c.logResult(sender, receiver, amount, resp)
			// Update leader based on response
			if resp.Ballot != nil {
				c.currentLeader = resp.Ballot.NodeId
			} else {
				c.currentLeader = nodeID
			}
			log.Printf("Client %s: Successfully sent via node %d", c.id, nodeID)
			return nil
		}
	}

	return fmt.Errorf("failed to send transaction to any node after broadcast")
}

// sendToNode sends a transaction request to a specific node
func (c *Client) sendToNode(nodeID int32, req *pb.TransactionRequest) (*pb.TransactionReply, error) {
	client, exists := c.nodeClients[nodeID]
	if !exists {
		return nil, fmt.Errorf("node %d not connected", nodeID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.retryTimeout)
	defer cancel()

	resp, err := client.SubmitTransaction(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// logResult logs the transaction result
func (c *Client) logResult(sender, receiver string, amount int32, resp *pb.TransactionReply) {
	resultSymbol := "✅"
	if !resp.Success {
		resultSymbol = "❌"
	}

	log.Printf("Client %s: %s Transaction (%s->%s:%d) - %s - %s",
		c.id, resultSymbol, sender, receiver, amount, resp.Result, resp.Message)
}

func (c *Client) Close() {
	for _, conn := range c.nodeConns {
		if conn != nil {
			conn.Close()
		}
	}
}
