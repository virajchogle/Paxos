package utils

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Transaction struct {
	Sender     int32 // Data item ID (1-9000)
	Receiver   int32 // Data item ID (1-9000)
	Amount     int32
	IsReadOnly bool // True for balance queries (s)
}

type Command struct {
	Type        string // "transaction", "balance", "fail", "recover"
	Transaction *Transaction
	NodeID      int32 // For F(ni) and R(ni)
}

type TestSet struct {
	SetNumber   int
	Commands    []Command // Changed from Transactions to Commands
	ActiveNodes []int32
}

func ReadTestFile(filename string) ([]TestSet, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var testSets []TestSet
	scanner := bufio.NewScanner(file)

	// Skip header line
	if !scanner.Scan() {
		return nil, fmt.Errorf("empty file")
	}

	var currentSet *TestSet
	lineNum := 1 // Track line number for debugging

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// Skip empty lines
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Debug: Print the line being processed
		fmt.Printf("DEBUG Line %d: %q\n", lineNum, line)

		// Split by tab - handle multiple consecutive tabs
		parts := strings.Split(line, "\t")

		// Debug: Show how many parts we got
		fmt.Printf("  -> Split into %d parts\n", len(parts))

		if len(parts) < 2 {
			fmt.Printf("  -> Skipping: not enough parts\n")
			continue
		}

		setNumStr := strings.TrimSpace(parts[0])
		txnStr := strings.TrimSpace(parts[1])
		nodesStr := ""
		if len(parts) >= 3 {
			nodesStr = strings.TrimSpace(parts[2])
		}

		fmt.Printf("  -> SetNum='%s', Txn='%s', Nodes='%s'\n", setNumStr, txnStr, nodesStr)

		// Check if this is a new set (has a set number)
		if setNumStr != "" {
			// Save previous set if exists
			if currentSet != nil {
				fmt.Printf("  -> Saving previous set %d with %d commands\n",
					currentSet.SetNumber, len(currentSet.Commands))
				testSets = append(testSets, *currentSet)
			}

			// Start new set
			setNum, err := strconv.Atoi(setNumStr)
			if err != nil {
				fmt.Printf("  -> Error parsing set number: %v\n", err)
				continue
			}

			currentSet = &TestSet{
				SetNumber:   setNum,
				Commands:    []Command{},
				ActiveNodes: parseNodes(nodesStr),
			}
			fmt.Printf("  -> Started new set %d\n", setNum)
		}

		// Make sure we have a current set to add to
		if currentSet == nil {
			fmt.Printf("  -> No current set, skipping\n")
			continue
		}

		if txnStr == "" {
			fmt.Printf("  -> Empty command, skipping\n")
			continue
		}

		// Parse command: transaction, balance query, F(ni), or R(ni)
		cmd, err := parseCommand(txnStr)
		if err != nil {
			fmt.Printf("  -> Failed to parse command '%s': %v\n", txnStr, err)
			continue
		}

		currentSet.Commands = append(currentSet.Commands, cmd)
		
		switch cmd.Type {
		case "transaction":
			fmt.Printf("  -> Added transaction: %d -> %d: %d (total: %d)\n",
				cmd.Transaction.Sender, cmd.Transaction.Receiver, cmd.Transaction.Amount, len(currentSet.Commands))
		case "balance":
			fmt.Printf("  -> Added balance query: %d (total: %d)\n",
				cmd.Transaction.Sender, len(currentSet.Commands))
		case "fail":
			fmt.Printf("  -> Added F(n%d) - fail node %d (total: %d)\n",
				cmd.NodeID, cmd.NodeID, len(currentSet.Commands))
		case "recover":
			fmt.Printf("  -> Added R(n%d) - recover node %d (total: %d)\n",
				cmd.NodeID, cmd.NodeID, len(currentSet.Commands))
		}
	}

	// Add the last set
	if currentSet != nil {
		fmt.Printf("Saving final set %d with %d commands\n",
			currentSet.SetNumber, len(currentSet.Commands))
		testSets = append(testSets, *currentSet)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	fmt.Printf("\nTotal test sets loaded: %d\n", len(testSets))

	// CRITICAL DEBUG: Print summary
	fmt.Println("\n========== FINAL TEST SETS SUMMARY ==========")
	for _, ts := range testSets {
		fmt.Printf("Set %d: %d commands, nodes=%v\n",
			ts.SetNumber, len(ts.Commands), ts.ActiveNodes)
	}
	fmt.Println("=============================================")

	return testSets, nil
}

// parseCommand parses a command which can be:
// - Transaction: (100, 200, 5)
// - Balance query: (100)
// - Fail node: F(n3)
// - Recover node: R(n3)
func parseCommand(s string) (Command, error) {
	s = strings.TrimSpace(s)

	// Check for F(ni) - fail node
	if strings.HasPrefix(s, "F(") && strings.HasSuffix(s, ")") {
		nodeStr := s[2 : len(s)-1] // Extract "ni" or "n3"
		nodeStr = strings.TrimSpace(nodeStr)
		
		// Remove 'n' prefix
		if len(nodeStr) > 1 && nodeStr[0] == 'n' {
			nodeStr = nodeStr[1:]
		}
		
		nodeID, err := strconv.Atoi(nodeStr)
		if err != nil {
			return Command{}, fmt.Errorf("invalid node ID in F(%s): %v", nodeStr, err)
		}
		
		return Command{
			Type:   "fail",
			NodeID: int32(nodeID),
		}, nil
	}

	// Check for R(ni) - recover node
	if strings.HasPrefix(s, "R(") && strings.HasSuffix(s, ")") {
		nodeStr := s[2 : len(s)-1]
		nodeStr = strings.TrimSpace(nodeStr)
		
		// Remove 'n' prefix
		if len(nodeStr) > 1 && nodeStr[0] == 'n' {
			nodeStr = nodeStr[1:]
		}
		
		nodeID, err := strconv.Atoi(nodeStr)
		if err != nil {
			return Command{}, fmt.Errorf("invalid node ID in R(%s): %v", nodeStr, err)
		}
		
		return Command{
			Type:   "recover",
			NodeID: int32(nodeID),
		}, nil
	}

	// Parse transaction or balance query
	txn, err := parseTransaction(s)
	if err != nil {
		return Command{}, err
	}

	if txn.IsReadOnly {
		return Command{
			Type:        "balance",
			Transaction: &txn,
		}, nil
	}

	return Command{
		Type:        "transaction",
		Transaction: &txn,
	}, nil
}

func parseTransaction(s string) (Transaction, error) {
	// Remove parentheses and spaces: "(100, 200, 5)" -> "100,200,5"
	s = strings.Trim(s, "()")
	s = strings.TrimSpace(s)

	if s == "" {
		return Transaction{}, fmt.Errorf("empty transaction string")
	}

	parts := strings.Split(s, ",")

	// Check if it's a balance query (single item)
	if len(parts) == 1 {
		itemStr := strings.TrimSpace(parts[0])
		itemID, err := strconv.Atoi(itemStr)
		if err != nil {
			return Transaction{}, fmt.Errorf("invalid item ID '%s': %v", itemStr, err)
		}

		return Transaction{
			Sender:     int32(itemID),
			Receiver:   0,
			Amount:     0,
			IsReadOnly: true,
		}, nil
	}

	// Full transaction (3 parts)
	if len(parts) != 3 {
		return Transaction{}, fmt.Errorf("invalid format (expected 1 or 3 parts): %s", s)
	}

	senderStr := strings.TrimSpace(parts[0])
	receiverStr := strings.TrimSpace(parts[1])
	amountStr := strings.TrimSpace(parts[2])

	// Parse sender as int32 data item ID
	sender, err := strconv.Atoi(senderStr)
	if err != nil {
		return Transaction{}, fmt.Errorf("invalid sender ID '%s': %v", senderStr, err)
	}

	// Parse receiver as int32 data item ID
	receiver, err := strconv.Atoi(receiverStr)
	if err != nil {
		return Transaction{}, fmt.Errorf("invalid receiver ID '%s': %v", receiverStr, err)
	}

	// Parse amount
	amount, err := strconv.Atoi(amountStr)
	if err != nil {
		return Transaction{}, fmt.Errorf("invalid amount '%s': %v", amountStr, err)
	}

	return Transaction{
		Sender:     int32(sender),
		Receiver:   int32(receiver),
		Amount:     int32(amount),
		IsReadOnly: false,
	}, nil
}

func parseNodes(s string) []int32 {
	if s == "" {
		return []int32{1, 2, 3, 4, 5} // Default all nodes
	}

	// Remove brackets: "[n1, n2, n3]" -> "n1, n2, n3"
	s = strings.Trim(s, "[]")
	s = strings.TrimSpace(s)

	if s == "" {
		return []int32{1, 2, 3, 4, 5}
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
				fmt.Printf("  %d. Transaction: (%d, %d, %d)\n",
					i+1, cmd.Transaction.Sender, cmd.Transaction.Receiver, cmd.Transaction.Amount)
			case "balance":
				fmt.Printf("  %d. Balance query: (%d)\n", i+1, cmd.Transaction.Sender)
			case "fail":
				fmt.Printf("  %d. F(n%d) - Fail node %d\n", i+1, cmd.NodeID, cmd.NodeID)
			case "recover":
				fmt.Printf("  %d. R(n%d) - Recover node %d\n", i+1, cmd.NodeID, cmd.NodeID)
			}
		}
	}
}
