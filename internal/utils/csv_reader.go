package utils

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Transaction struct {
	Sender   string
	Receiver string
	Amount   int32
}

type TestSet struct {
	SetNumber      int
	Transactions   []Transaction
	ActiveNodes    []int32
	LeaderFailures []int // Indices where LF should occur (after which transaction)
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
				fmt.Printf("  -> Saving previous set %d with %d transactions\n",
					currentSet.SetNumber, len(currentSet.Transactions))
				testSets = append(testSets, *currentSet)
			}

			// Start new set
			setNum, err := strconv.Atoi(setNumStr)
			if err != nil {
				fmt.Printf("  -> Error parsing set number: %v\n", err)
				continue
			}

			currentSet = &TestSet{
				SetNumber:      setNum,
				Transactions:   []Transaction{},
				ActiveNodes:    parseNodes(nodesStr),
				LeaderFailures: []int{},
			}
			fmt.Printf("  -> Started new set %d\n", setNum)
		}

		// Make sure we have a current set to add to
		if currentSet == nil {
			fmt.Printf("  -> No current set, skipping\n")
			continue
		}

		// Parse transaction or LF
		if txnStr == "LF" {
			// LF occurs AFTER the current number of transactions
			currentSet.LeaderFailures = append(currentSet.LeaderFailures, len(currentSet.Transactions))
			fmt.Printf("  -> Added LF at index %d\n", len(currentSet.Transactions))
			continue
		}

		if txnStr == "" {
			fmt.Printf("  -> Empty transaction, skipping\n")
			continue
		}

		// Parse transaction: (A, B, 5)
		txn, err := parseTransaction(txnStr)
		if err != nil {
			fmt.Printf("  -> Failed to parse transaction '%s': %v\n", txnStr, err)
			continue
		}

		currentSet.Transactions = append(currentSet.Transactions, txn)
		fmt.Printf("  -> Added transaction: %s -> %s: %d (total: %d)\n",
			txn.Sender, txn.Receiver, txn.Amount, len(currentSet.Transactions))
	}

	// Add the last set
	if currentSet != nil {
		fmt.Printf("Saving final set %d with %d transactions\n",
			currentSet.SetNumber, len(currentSet.Transactions))
		testSets = append(testSets, *currentSet)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	fmt.Printf("\nTotal test sets loaded: %d\n", len(testSets))

	// CRITICAL DEBUG: Print summary
	fmt.Println("\n========== FINAL TEST SETS SUMMARY ==========")
	for _, ts := range testSets {
		fmt.Printf("Set %d: %d transactions, nodes=%v\n",
			ts.SetNumber, len(ts.Transactions), ts.ActiveNodes)
	}
	fmt.Println("=============================================")

	return testSets, nil
}

func parseTransaction(s string) (Transaction, error) {
	// Remove parentheses and spaces: "(A, B, 5)" -> "A,B,5"
	s = strings.Trim(s, "()")
	s = strings.TrimSpace(s)

	if s == "" {
		return Transaction{}, fmt.Errorf("empty transaction string")
	}

	parts := strings.Split(s, ",")

	if len(parts) != 3 {
		return Transaction{}, fmt.Errorf("invalid format (expected 3 parts): %s", s)
	}

	sender := strings.TrimSpace(parts[0])
	receiver := strings.TrimSpace(parts[1])
	amountStr := strings.TrimSpace(parts[2])

	amount, err := strconv.Atoi(amountStr)
	if err != nil {
		return Transaction{}, fmt.Errorf("invalid amount '%s': %v", amountStr, err)
	}

	return Transaction{
		Sender:   sender,
		Receiver: receiver,
		Amount:   int32(amount),
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
		fmt.Printf("Transactions: %d\n", len(set.Transactions))
		if len(set.LeaderFailures) > 0 {
			fmt.Printf("Leader Failures at indices: %v\n", set.LeaderFailures)
		}
		for i, txn := range set.Transactions {
			fmt.Printf("  %d. (%s, %s, %d)", i+1, txn.Sender, txn.Receiver, txn.Amount)
			for _, lfIdx := range set.LeaderFailures {
				if lfIdx == i {
					fmt.Print(" <-- LEADER FAILURE AFTER THIS")
				}
			}
			fmt.Println()
		}
	}
}
