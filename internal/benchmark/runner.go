package benchmark

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	pb "paxos-banking/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// BenchmarkRunner executes benchmarks
type BenchmarkRunner struct {
	config    *BenchmarkConfig
	workload  *WorkloadGenerator
	clients   []pb.PaxosNodeClient
	stats     *Statistics
	startTime time.Time
	endTime   time.Time

	// Synchronization
	wg         sync.WaitGroup
	stopChan   chan struct{}
	txnChan    chan *Transaction
	resultChan chan *Result

	// Rate limiting
	rateLimiter *RateLimiter
}

// Result represents the result of a single transaction
type Result struct {
	Success bool
	Latency time.Duration
	Type    TransactionType
	Error   error
}

// NewBenchmarkRunner creates a new benchmark runner
func NewBenchmarkRunner(config *BenchmarkConfig, nodeAddresses []string) (*BenchmarkRunner, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	// Connect to nodes
	clients := make([]pb.PaxosNodeClient, 0, len(nodeAddresses))
	for _, addr := range nodeAddresses {
		conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
		}
		clients = append(clients, pb.NewPaxosNodeClient(conn))
	}

	if len(clients) == 0 {
		return nil, fmt.Errorf("no clients available")
	}

	// Create workload generator
	workload := NewWorkloadGenerator(config, 9000) // 9000 total data items

	// Create rate limiter
	var rateLimiter *RateLimiter
	if config.TargetTPS > 0 {
		rateLimiter = NewRateLimiter(config.TargetTPS)
	}

	return &BenchmarkRunner{
		config:      config,
		workload:    workload,
		clients:     clients,
		stats:       NewStatistics(),
		stopChan:    make(chan struct{}),
		txnChan:     make(chan *Transaction, config.NumClients*10),
		resultChan:  make(chan *Result, config.NumClients*10),
		rateLimiter: rateLimiter,
	}, nil
}

// Run executes the benchmark
func (br *BenchmarkRunner) Run() error {
	log.Println("\n" + br.config.String())
	log.Println("\n╔════════════════════════════════════════════════════════╗")
	log.Println("║                Benchmark Starting                    ║")
	log.Println("╚════════════════════════════════════════════════════════╝\n")

	// Start result collector
	go br.collectResults()

	// Warmup phase
	if br.config.WarmupSeconds > 0 {
		log.Printf("🔥 Warmup phase: %d seconds...\n", br.config.WarmupSeconds)
		br.warmup()
		log.Println("✅ Warmup complete\n")
	}

	// Start measurement
	log.Println("📊 Starting measurement...")
	br.startTime = time.Now()
	br.stats.ResetCounters()

	// Start worker clients
	for i := 0; i < br.config.NumClients; i++ {
		br.wg.Add(1)
		go br.worker(i)
	}

	// Start progress reporter
	if br.config.ReportInterval > 0 {
		go br.progressReporter()
	}

	// Generate workload
	br.generateWorkload()

	// Wait for all workers to complete
	close(br.txnChan)
	br.wg.Wait()
	close(br.resultChan)
	close(br.stopChan)

	br.endTime = time.Now()

	// Print final report
	br.printFinalReport()

	// Export CSV if requested
	if br.config.ExportCSV {
		br.exportCSV()
	}

	return nil
}

// warmup runs transactions without measuring
func (br *BenchmarkRunner) warmup() {
	warmupTxns := br.config.TargetTPS * br.config.WarmupSeconds
	if warmupTxns == 0 {
		warmupTxns = 1000 // Default warmup
	}

	warmupChan := make(chan *Transaction, br.config.NumClients*10)
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < br.config.NumClients; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			client := br.clients[clientID%len(br.clients)]
			for txn := range warmupChan {
				br.executeTransaction(client, txn)
			}
		}(i)
	}

	// Generate warmup transactions
	for i := 0; i < warmupTxns; i++ {
		warmupChan <- br.workload.GenerateTransaction()
	}
	close(warmupChan)
	wg.Wait()
}

// generateWorkload generates transactions and sends to workers
func (br *BenchmarkRunner) generateWorkload() {
	if br.config.Duration > 0 {
		// Duration-based
		deadline := time.Now().Add(time.Duration(br.config.Duration) * time.Second)
		for time.Now().Before(deadline) {
			if br.rateLimiter != nil {
				br.rateLimiter.Wait()
			}
			br.txnChan <- br.workload.GenerateTransaction()
		}
	} else {
		// Count-based
		for i := 0; i < br.config.TotalTransactions; i++ {
			if br.rateLimiter != nil {
				br.rateLimiter.Wait()
			}
			br.txnChan <- br.workload.GenerateTransaction()
		}
	}
}

// worker processes transactions
func (br *BenchmarkRunner) worker(clientID int) {
	defer br.wg.Done()

	client := br.clients[clientID%len(br.clients)]

	for txn := range br.txnChan {
		start := time.Now()
		success, err := br.executeTransaction(client, txn)
		latency := time.Since(start)

		br.resultChan <- &Result{
			Success: success,
			Latency: latency,
			Type:    txn.Type,
			Error:   err,
		}
	}
}

// executeTransaction executes a single transaction
func (br *BenchmarkRunner) executeTransaction(client pb.PaxosNodeClient, txn *Transaction) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch txn.Type {
	case TxnTypeReadOnly:
		// Balance query
		req := &pb.BalanceQueryRequest{
			DataItemId: txn.Sender,
		}
		reply, err := client.QueryBalance(ctx, req)
		if err != nil {
			return false, err
		}
		return reply.Success, nil

	default:
		// Write transaction (intra or cross-shard)
		req := &pb.TransactionRequest{
			ClientId:  fmt.Sprintf("bench_%d", txn.Sender),
			Timestamp: time.Now().UnixNano(),
			Transaction: &pb.Transaction{
				Sender:   txn.Sender,
				Receiver: txn.Receiver,
				Amount:   txn.Amount,
			},
		}
		reply, err := client.SubmitTransaction(ctx, req)
		if err != nil {
			return false, err
		}
		return reply.Success, nil
	}
}

// collectResults collects and aggregates results
func (br *BenchmarkRunner) collectResults() {
	for result := range br.resultChan {
		br.stats.RecordResult(result)
	}
}

// progressReporter prints progress periodically
func (br *BenchmarkRunner) progressReporter() {
	ticker := time.NewTicker(time.Duration(br.config.ReportInterval) * time.Second)
	defer ticker.Stop()

	lastTotal := int64(0)
	lastTime := br.startTime

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			total := br.stats.GetTotal()
			success := br.stats.GetSuccessful()

			interval := now.Sub(lastTime).Seconds()
			intervalTxns := total - lastTotal
			intervalTPS := float64(intervalTxns) / interval

			elapsed := now.Sub(br.startTime).Seconds()
			overallTPS := float64(total) / elapsed
			successRate := float64(success) / float64(total) * 100

			log.Printf("📊 [%.1fs] Total: %d | Success: %d (%.1f%%) | Interval TPS: %.0f | Overall TPS: %.0f",
				elapsed, total, success, successRate, intervalTPS, overallTPS)

			lastTotal = total
			lastTime = now

		case <-br.stopChan:
			return
		}
	}
}

// printFinalReport prints the final benchmark report
func (br *BenchmarkRunner) printFinalReport() {
	duration := br.endTime.Sub(br.startTime)

	log.Println("\n╔════════════════════════════════════════════════════════╗")
	log.Println("║              Benchmark Results                       ║")
	log.Println("╚════════════════════════════════════════════════════════╝\n")

	// Overall statistics
	total := br.stats.GetTotal()
	successful := br.stats.GetSuccessful()
	failed := br.stats.GetFailed()
	successRate := float64(successful) / float64(total) * 100
	actualTPS := float64(total) / duration.Seconds()

	log.Printf("Duration:           %.2f seconds\n", duration.Seconds())
	log.Printf("Total Transactions: %d\n", total)
	log.Printf("Successful:         %d (%.2f%%)\n", successful, successRate)
	log.Printf("Failed:             %d (%.2f%%)\n", failed, 100-successRate)
	log.Printf("Throughput:         %.2f TPS\n", actualTPS)

	if br.config.TargetTPS > 0 {
		efficiency := (actualTPS / float64(br.config.TargetTPS)) * 100
		log.Printf("Target TPS:         %d\n", br.config.TargetTPS)
		log.Printf("Efficiency:         %.2f%%\n", efficiency)
	}

	// Latency statistics
	log.Println("\n--- Latency Statistics ---")
	p50, p95, p99, p999 := br.stats.GetPercentiles()
	avg := br.stats.GetAverageLatency()
	min := br.stats.GetMinLatency()
	max := br.stats.GetMaxLatency()

	log.Printf("Average:            %.2f ms\n", float64(avg.Microseconds())/1000.0)
	log.Printf("Min:                %.2f ms\n", float64(min.Microseconds())/1000.0)
	log.Printf("Max:                %.2f ms\n", float64(max.Microseconds())/1000.0)

	if br.config.DetailedStats {
		log.Printf("p50:                %.2f ms\n", float64(p50.Microseconds())/1000.0)
		log.Printf("p95:                %.2f ms\n", float64(p95.Microseconds())/1000.0)
		log.Printf("p99:                %.2f ms\n", float64(p99.Microseconds())/1000.0)
		log.Printf("p99.9:              %.2f ms\n", float64(p999.Microseconds())/1000.0)
	}

	// Per-type statistics
	log.Println("\n--- Per-Type Statistics ---")
	for _, txnType := range []TransactionType{TxnTypeIntraShard, TxnTypeCrossShard, TxnTypeReadOnly} {
		count := br.stats.GetCountByType(txnType)
		if count == 0 {
			continue
		}
		avgLatency := br.stats.GetAverageLatencyByType(txnType)
		log.Printf("%s: %d transactions, avg latency: %.2f ms\n",
			txnType.String(), count, float64(avgLatency.Microseconds())/1000.0)
	}

	log.Println("\n✅ Benchmark complete!\n")
}

// exportCSV exports results to CSV file
func (br *BenchmarkRunner) exportCSV() {
	log.Printf("📄 Exporting results to %s...\n", br.config.OutputFile)
	// TODO: Implement CSV export
	log.Println("(CSV export not yet implemented)")
}

// ============================================================================
// Rate Limiter
// ============================================================================

type RateLimiter struct {
	targetTPS int
	interval  time.Duration
	ticker    *time.Ticker
}

func NewRateLimiter(targetTPS int) *RateLimiter {
	interval := time.Second / time.Duration(targetTPS)
	return &RateLimiter{
		targetTPS: targetTPS,
		interval:  interval,
		ticker:    time.NewTicker(interval),
	}
}

func (rl *RateLimiter) Wait() {
	<-rl.ticker.C
}

// ============================================================================
// Statistics
// ============================================================================

type Statistics struct {
	mu              sync.RWMutex
	total           int64
	successful      int64
	failed          int64
	latencies       []time.Duration
	latenciesByType map[TransactionType][]time.Duration
	countByType     map[TransactionType]int64
}

func NewStatistics() *Statistics {
	return &Statistics{
		latencies:       make([]time.Duration, 0, 100000),
		latenciesByType: make(map[TransactionType][]time.Duration),
		countByType:     make(map[TransactionType]int64),
	}
}

func (s *Statistics) RecordResult(result *Result) {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddInt64(&s.total, 1)
	if result.Success {
		atomic.AddInt64(&s.successful, 1)
	} else {
		atomic.AddInt64(&s.failed, 1)
	}

	s.latencies = append(s.latencies, result.Latency)
	s.latenciesByType[result.Type] = append(s.latenciesByType[result.Type], result.Latency)
	s.countByType[result.Type]++
}

func (s *Statistics) ResetCounters() {
	s.mu.Lock()
	defer s.mu.Unlock()
	atomic.StoreInt64(&s.total, 0)
	atomic.StoreInt64(&s.successful, 0)
	atomic.StoreInt64(&s.failed, 0)
	s.latencies = make([]time.Duration, 0, 100000)
	s.latenciesByType = make(map[TransactionType][]time.Duration)
	s.countByType = make(map[TransactionType]int64)
}

func (s *Statistics) GetTotal() int64 {
	return atomic.LoadInt64(&s.total)
}

func (s *Statistics) GetSuccessful() int64 {
	return atomic.LoadInt64(&s.successful)
}

func (s *Statistics) GetFailed() int64 {
	return atomic.LoadInt64(&s.failed)
}

func (s *Statistics) GetPercentiles() (p50, p95, p99, p999 time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.latencies) == 0 {
		return 0, 0, 0, 0
	}

	sorted := make([]time.Duration, len(s.latencies))
	copy(sorted, s.latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	p50 = sorted[len(sorted)*50/100]
	p95 = sorted[len(sorted)*95/100]
	p99 = sorted[len(sorted)*99/100]
	p999 = sorted[len(sorted)*999/1000]

	return
}

func (s *Statistics) GetAverageLatency() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.latencies) == 0 {
		return 0
	}

	var total time.Duration
	for _, l := range s.latencies {
		total += l
	}
	return total / time.Duration(len(s.latencies))
}

func (s *Statistics) GetMinLatency() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.latencies) == 0 {
		return 0
	}

	min := s.latencies[0]
	for _, l := range s.latencies {
		if l < min {
			min = l
		}
	}
	return min
}

func (s *Statistics) GetMaxLatency() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.latencies) == 0 {
		return 0
	}

	max := s.latencies[0]
	for _, l := range s.latencies {
		if l > max {
			max = l
		}
	}
	return max
}

func (s *Statistics) GetCountByType(txnType TransactionType) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.countByType[txnType]
}

func (s *Statistics) GetAverageLatencyByType(txnType TransactionType) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	latencies := s.latenciesByType[txnType]
	if len(latencies) == 0 {
		return 0
	}

	var total time.Duration
	for _, l := range latencies {
		total += l
	}
	return total / time.Duration(len(latencies))
}

