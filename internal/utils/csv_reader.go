package utils

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Transaction struct {
	Sender     int32
	Receiver   int32
	Amount     int32
	IsReadOnly bool
}

type Command struct {
	Type        string // "transaction", "balance", "fail", "recover"
	Transaction *Transaction
	NodeID      int32
}

type TestSet struct {
	SetNumber   int
	Commands    []Command
	ActiveNodes []int32
}

func ReadTestFile(filename string) ([]TestSet, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1 // Allow variable fields

	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	if len(records) < 2 {
		return nil, fmt.Errorf("empty file")
	}

	var testSets []TestSet
	var currentSet *TestSet

	// Skip header (row 0)
	for _, row := range records[1:] {
		if len(row) < 2 {
			continue
		}

		setNumStr := strings.TrimSpace(row[0])
		txnStr := strings.TrimSpace(row[1])
		nodesStr := ""
		if len(row) >= 3 {
			nodesStr = strings.TrimSpace(row[2])
		}

		// New test set
		if setNumStr != "" {
			if currentSet != nil {
				testSets = append(testSets, *currentSet)
			}

			setNum, err := strconv.Atoi(setNumStr)
			if err != nil {
				continue
			}

			currentSet = &TestSet{
				SetNumber:   setNum,
				Commands:    []Command{},
				ActiveNodes: parseNodes(nodesStr),
			}
		}

		if currentSet == nil || txnStr == "" {
			continue
		}

		cmd, err := parseCommand(txnStr)
		if err != nil {
			continue
		}
		currentSet.Commands = append(currentSet.Commands, cmd)
	}

	if currentSet != nil {
		testSets = append(testSets, *currentSet)
	}

	return testSets, nil
}

func parseCommand(s string) (Command, error) {
	s = strings.TrimSpace(s)

	// F(ni) - fail node
	if strings.HasPrefix(s, "F(") && strings.HasSuffix(s, ")") {
		nodeStr := strings.TrimSpace(s[2 : len(s)-1])
		if len(nodeStr) > 1 && nodeStr[0] == 'n' {
			nodeStr = nodeStr[1:]
		}
		nodeID, err := strconv.Atoi(nodeStr)
		if err != nil {
			return Command{}, err
		}
		return Command{Type: "fail", NodeID: int32(nodeID)}, nil
	}

	// R(ni) - recover node
	if strings.HasPrefix(s, "R(") && strings.HasSuffix(s, ")") {
		nodeStr := strings.TrimSpace(s[2 : len(s)-1])
		if len(nodeStr) > 1 && nodeStr[0] == 'n' {
			nodeStr = nodeStr[1:]
		}
		nodeID, err := strconv.Atoi(nodeStr)
		if err != nil {
			return Command{}, err
		}
		return Command{Type: "recover", NodeID: int32(nodeID)}, nil
	}

	// Transaction or balance query
	txn, err := parseTransaction(s)
	if err != nil {
		return Command{}, err
	}

	if txn.IsReadOnly {
		return Command{Type: "balance", Transaction: &txn}, nil
	}
	return Command{Type: "transaction", Transaction: &txn}, nil
}

func parseTransaction(s string) (Transaction, error) {
	s = strings.Trim(s, "()")
	s = strings.TrimSpace(s)

	if s == "" {
		return Transaction{}, fmt.Errorf("empty")
	}

	parts := strings.Split(s, ",")

	// Balance query (single item)
	if len(parts) == 1 {
		itemID, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return Transaction{}, err
		}
		return Transaction{Sender: int32(itemID), IsReadOnly: true}, nil
	}

	// Transaction (3 parts)
	if len(parts) != 3 {
		return Transaction{}, fmt.Errorf("invalid format")
	}

	sender, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return Transaction{}, err
	}
	receiver, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return Transaction{}, err
	}
	amount, err := strconv.Atoi(strings.TrimSpace(parts[2]))
	if err != nil {
		return Transaction{}, err
	}

	return Transaction{
		Sender:   int32(sender),
		Receiver: int32(receiver),
		Amount:   int32(amount),
	}, nil
}

func parseNodes(s string) []int32 {
	if s == "" {
		return []int32{1, 2, 3, 4, 5, 6, 7, 8, 9}
	}

	s = strings.Trim(s, "[]")
	s = strings.TrimSpace(s)
	if s == "" {
		return []int32{1, 2, 3, 4, 5, 6, 7, 8, 9}
	}

	parts := strings.Split(s, ",")
	var nodes []int32
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) < 2 || part[0] != 'n' {
			continue
		}
		num, err := strconv.Atoi(part[1:])
		if err != nil {
			continue
		}
		nodes = append(nodes, int32(num))
	}
	return nodes
}

func PrintTestSets(sets []TestSet) {
	for _, set := range sets {
		fmt.Printf("\n=== Test Set %d ===\n", set.SetNumber)
		fmt.Printf("Active Nodes: %v\n", set.ActiveNodes)
		fmt.Printf("Commands: %d\n", len(set.Commands))
		for i, cmd := range set.Commands {
			switch cmd.Type {
			case "transaction":
				fmt.Printf("  %d. (%d, %d, %d)\n", i+1, cmd.Transaction.Sender, cmd.Transaction.Receiver, cmd.Transaction.Amount)
			case "balance":
				fmt.Printf("  %d. Balance(%d)\n", i+1, cmd.Transaction.Sender)
			case "fail":
				fmt.Printf("  %d. F(n%d)\n", i+1, cmd.NodeID)
			case "recover":
				fmt.Printf("  %d. R(n%d)\n", i+1, cmd.NodeID)
			}
		}
	}
}
