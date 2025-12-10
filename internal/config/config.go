package config

import (
	"fmt"
	"os"
	"strconv"

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

	// Override initial_balance from environment variable if set
	// Use INITIAL_BALANCE=100000 for benchmarking, default 10 for tests
	if envBalance := os.Getenv("INITIAL_BALANCE"); envBalance != "" {
		if balance, err := strconv.ParseInt(envBalance, 10, 32); err == nil {
			cfg.Data.InitialBalance = int32(balance)
		}
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

// ============================================================================
// CONFIGURABLE CLUSTERS SUPPORT (Bonus Feature)
// ============================================================================

// GetNumClusters returns the total number of clusters
func (c *Config) GetNumClusters() int {
	return len(c.Clusters)
}

// GetTotalNodes returns the total number of nodes across all clusters
func (c *Config) GetTotalNodes() int {
	return len(c.Nodes)
}

// GetNodesPerCluster returns the number of nodes in a specific cluster
func (c *Config) GetNodesPerCluster(clusterID int) int {
	if cluster, exists := c.Clusters[clusterID]; exists {
		return len(cluster.Nodes)
	}
	return 0
}

// GetAllClusterIDs returns all cluster IDs in sorted order
func (c *Config) GetAllClusterIDs() []int {
	ids := make([]int, 0, len(c.Clusters))
	for id := range c.Clusters {
		ids = append(ids, id)
	}
	// Sort for consistent ordering
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[i] > ids[j] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	return ids
}

// GetAllNodeIDs returns all node IDs in sorted order
func (c *Config) GetAllNodeIDs() []int32 {
	ids := make([]int32, 0, len(c.Nodes))
	for id := range c.Nodes {
		ids = append(ids, int32(id))
	}
	// Sort for consistent ordering
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[i] > ids[j] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	return ids
}

// GetClusterRange returns the shard range for a cluster
func (c *Config) GetClusterRange(clusterID int) (start, end int32) {
	if cluster, exists := c.Clusters[clusterID]; exists {
		return cluster.ShardStart, cluster.ShardEnd
	}
	return 0, 0
}

// GetExpectedLeaders returns a map of clusterID -> expected leader nodeID
func (c *Config) GetExpectedLeaders() map[int32]int32 {
	leaders := make(map[int32]int32)
	for clusterID, cluster := range c.Clusters {
		if len(cluster.Nodes) > 0 {
			leaders[int32(clusterID)] = int32(cluster.Nodes[0])
		}
	}
	return leaders
}

// GetMinNodeID returns the minimum node ID
func (c *Config) GetMinNodeID() int32 {
	minID := int32(0)
	first := true
	for id := range c.Nodes {
		if first || int32(id) < minID {
			minID = int32(id)
			first = false
		}
	}
	return minID
}

// GetMaxNodeID returns the maximum node ID
func (c *Config) GetMaxNodeID() int32 {
	maxID := int32(0)
	for id := range c.Nodes {
		if int32(id) > maxID {
			maxID = int32(id)
		}
	}
	return maxID
}

// GetTotalDataItems returns the total number of data items
func (c *Config) GetTotalDataItems() int {
	return c.Data.TotalItems
}

// IsValidNodeID checks if a node ID exists in the configuration
func (c *Config) IsValidNodeID(nodeID int) bool {
	_, exists := c.Nodes[nodeID]
	return exists
}
