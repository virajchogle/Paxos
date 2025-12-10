package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Clusters map[int]ClusterConfig `yaml:"clusters"`
	Nodes    map[int]NodeConfig    `yaml:"nodes"`
	Data     DataConfig            `yaml:"data"`
}

type ClusterConfig struct {
	ID         int   `yaml:"id"`
	ShardStart int32 `yaml:"shard_start"`
	ShardEnd   int32 `yaml:"shard_end"`
	Nodes      []int `yaml:"nodes"`
}

type NodeConfig struct {
	ID      int    `yaml:"id"`
	Cluster int    `yaml:"cluster"`
	Address string `yaml:"address"`
	Port    int    `yaml:"port"`
}

type DataConfig struct {
	TotalItems         int   `yaml:"total_items"`
	InitialBalance     int32 `yaml:"initial_balance"`
	CheckpointInterval int32 `yaml:"checkpoint_interval"` // Checkpoint every N transactions (default: 100)
}

func LoadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) GetNodeAddress(nodeID int) string {
	if node, exists := c.Nodes[nodeID]; exists {
		return fmt.Sprintf("%s:%d", node.Address, node.Port)
	}
	return ""
}

func (c *Config) GetAllNodeAddresses() map[int32]string {
	addrs := make(map[int32]string)
	for id, node := range c.Nodes {
		addrs[int32(id)] = fmt.Sprintf("%s:%d", node.Address, node.Port)
	}
	return addrs
}

// GetClusterForNode returns the cluster ID for a given node
func (c *Config) GetClusterForNode(nodeID int) int {
	if node, exists := c.Nodes[nodeID]; exists {
		return node.Cluster
	}
	return 0
}

// GetClusterForDataItem returns the cluster ID that manages a given data item
func (c *Config) GetClusterForDataItem(itemID int32) int {
	for _, cluster := range c.Clusters {
		if itemID >= cluster.ShardStart && itemID <= cluster.ShardEnd {
			return cluster.ID
		}
	}
	return 0
}

// GetNodesInCluster returns all node IDs in a given cluster
func (c *Config) GetNodesInCluster(clusterID int) []int32 {
	if cluster, exists := c.Clusters[clusterID]; exists {
		nodes := make([]int32, len(cluster.Nodes))
		for i, nodeID := range cluster.Nodes {
			nodes[i] = int32(nodeID)
		}
		return nodes
	}
	return nil
}

// GetPeerNodesInCluster returns all peer node IDs in the same cluster (excluding self)
func (c *Config) GetPeerNodesInCluster(nodeID int, clusterID int) []int32 {
	allNodes := c.GetNodesInCluster(clusterID)
	peers := make([]int32, 0, len(allNodes)-1)
	for _, nid := range allNodes {
		if int(nid) != nodeID {
			peers = append(peers, nid)
		}
	}
	return peers
}

// GetLeaderNodeForCluster returns the first node ID in a cluster (expected leader at test start)
func (c *Config) GetLeaderNodeForCluster(clusterID int) int32 {
	if cluster, exists := c.Clusters[clusterID]; exists && len(cluster.Nodes) > 0 {
		return int32(cluster.Nodes[0])
	}
	return 0
}
