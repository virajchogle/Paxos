package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"paxos-banking/internal/config"
	"paxos-banking/internal/node"
)

// syncWriter wraps an io.Writer and syncs after every write
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
	f  *os.File
}

func (sw *syncWriter) Write(p []byte) (n int, err error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	n, err = sw.w.Write(p)
	if err != nil {
		return n, err
	}
	// Flush to disk immediately
	if sw.f != nil {
		_ = sw.f.Sync()
	}
	return n, nil
}

func main() {
	nodeID := flag.Int("id", 1, "Node ID (1-5)")
	configFile := flag.String("config", "config/nodes.yaml", "Config file")
	flag.Parse()

	if *nodeID < 1 || *nodeID > 5 {
		log.Fatal("Node ID must be between 1 and 5")
	}

	// Set up file logging for this node with immediate flushing
	logFileName := fmt.Sprintf("logs/node%d.log", *nodeID)
	logFile, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}
	defer logFile.Close()

	// Use a syncWriter that flushes after every write - write to BOTH file and stdout
	sw := &syncWriter{w: io.MultiWriter(logFile, os.Stdout), f: logFile}

	// Redirect standard logger to file AND stdout with immediate flushing
	log.SetOutput(sw)

	// Also redirect stderr to log file to capture panics
	os.Stderr = logFile
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	n, err := node.NewNode(int32(*nodeID), cfg)
	if err != nil {
		log.Fatalf("Failed to create node: %v", err)
	}

	if err := n.Start(); err != nil {
		log.Fatalf("Failed to start node: %v", err)
	}

	fmt.Printf("\n╔════════════════════════════════════╗\n")
	fmt.Printf("║   Node %d Started Successfully    ║\n", *nodeID)
	fmt.Printf("╚════════════════════════════════════╝\n")
	fmt.Printf("Address: %s\n\n", cfg.GetNodeAddress(*nodeID))
	fmt.Println("Commands:")
	fmt.Println("  printDB               - Show all balances")
	fmt.Println("  printLog              - Show transaction log")
	fmt.Println("  printStatus <seq>     - Show status of sequence number")
	fmt.Println("  printView             - Show NEW-VIEW history")
	fmt.Println("  status                - Show node status")
	fmt.Println("  quit                  - Stop node and exit")
	fmt.Println()

	// Handle interrupt for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start interactive CLI in goroutine
	go runInteractiveCLI(n)

	// Wait for interrupt
	<-sigChan
	fmt.Println("\nShutting down...")
	n.Stop()
	fmt.Println("Node stopped")
}

func runInteractiveCLI(n *node.Node) {
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("node> ")
		if !scanner.Scan() {
			break
		}

		cmd := strings.TrimSpace(scanner.Text())
		parts := strings.Fields(cmd)

		if len(parts) == 0 {
			continue
		}

		switch parts[0] {
		case "printDB":
			n.PrintDB()

		case "printLog":
			n.PrintLog()

		case "printStatus":
			if len(parts) < 2 {
				fmt.Println("Usage: printStatus <sequence_number>")
				continue
			}
			seqNum, err := strconv.Atoi(parts[1])
			if err != nil {
				fmt.Println("Invalid sequence number")
				continue
			}
			n.PrintStatus(int32(seqNum))

		case "printView":
			n.PrintView()

		case "status":
			n.ShowStatus()

		case "help":
			showHelp()

		case "quit", "exit":
			fmt.Println("Exiting...")
			os.Exit(0)

		default:
			fmt.Printf("Unknown command: %s (type 'help' for commands)\n", parts[0])
		}
	}
}

func showHelp() {
	fmt.Println("\nAvailable Commands:")
	fmt.Println("  printDB           - Display current balance of all clients")
	fmt.Println("  printLog          - Display all transactions in the log")
	fmt.Println("  printStatus <seq> - Display status of a specific sequence number")
	fmt.Println("                      Status codes: A=Accepted, C=Committed, E=Executed, X=None")
	fmt.Println("  printView         - Display all NEW-VIEW messages (leader changes)")
	fmt.Println("  status            - Display node's current Paxos status")
	fmt.Println("  help              - Show this help message")
	fmt.Println("  quit              - Stop the node and exit")
	fmt.Println()
}
