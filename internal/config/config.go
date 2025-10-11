package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Nodes   map[int]NodeConfig `yaml:"nodes"`
	Clients ClientConfig       `yaml:"clients"`
}

type NodeConfig struct {
	ID      int    `yaml:"id"`
	Address string `yaml:"address"`
	Port    int    `yaml:"port"`
}

type ClientConfig struct {
	Count          int      `yaml:"count"`
	InitialBalance int32    `yaml:"initial_balance"`
	ClientIDs      []string `yaml:"client_ids"`
	RetryTimeoutMs int      `yaml:"retry_timeout_ms"`
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
