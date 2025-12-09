#!/bin/bash

# Paxos Banking System - Benchmark Runner Script
# This script builds and runs benchmarks with various configurations

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Project root
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$( cd "$SCRIPT_DIR/.." && pwd )"
cd "$PROJECT_ROOT"

# Build the benchmark binary
echo -e "${BLUE}🔨 Building benchmark binary...${NC}"
go build -o bin/benchmark cmd/benchmark/main.go
echo -e "${GREEN}✅ Build complete${NC}\n"

# Check if nodes are specified
NODES="localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055,localhost:50056,localhost:50057,localhost:50058,localhost:50059"
if [ ! -z "$BENCHMARK_NODES" ]; then
    NODES="$BENCHMARK_NODES"
fi

# Function to run a benchmark
run_benchmark() {
    local name=$1
    shift
    echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${YELLOW}Running: $name${NC}"
    echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}\n"
    
    ./bin/benchmark -nodes "$NODES" "$@"
    
    echo -e "\n${GREEN}✅ $name complete${NC}\n"
}

# Parse arguments
case "$1" in
    quick)
        # Quick test - 1000 transactions
        run_benchmark "Quick Test" \
            -transactions 1000 \
            -clients 10 \
            -cross-shard 20 \
            -read-only 10 \
            -distribution uniform \
            -detailed
        ;;
        
    default)
        # Default benchmark
        run_benchmark "Default Benchmark" \
            -preset default \
            -detailed
        ;;
        
    high-throughput|ht)
        # Maximum throughput test
        run_benchmark "High Throughput Test" \
            -preset high-throughput \
            -detailed \
            -csv \
            -output results/high_throughput.csv
        ;;
        
    cross-shard|cs)
        # Cross-shard heavy workload
        run_benchmark "Cross-Shard Heavy Test" \
            -preset cross-shard \
            -detailed \
            -csv \
            -output results/cross_shard.csv
        ;;
        
    stress)
        # Stress test
        run_benchmark "Stress Test" \
            -preset stress \
            -report 10 \
            -detailed \
            -csv \
            -output results/stress_test.csv
        ;;
        
    zipf)
        # Zipf distribution test
        run_benchmark "Zipf Distribution Test" \
            -transactions 10000 \
            -clients 20 \
            -cross-shard 30 \
            -read-only 10 \
            -distribution zipf \
            -detailed \
            -csv \
            -output results/zipf_test.csv
        ;;
        
    hotspot)
        # Hotspot test
        run_benchmark "Hotspot Distribution Test" \
            -transactions 10000 \
            -clients 20 \
            -cross-shard 30 \
            -read-only 10 \
            -distribution hotspot \
            -detailed \
            -csv \
            -output results/hotspot_test.csv
        ;;
        
    all)
        # Run all presets
        mkdir -p results
        echo -e "${BLUE}Running all benchmark presets...${NC}\n"
        
        run_benchmark "1. Quick Test" \
            -transactions 1000 -clients 10 -detailed
        
        run_benchmark "2. Default Benchmark" \
            -preset default -detailed -csv -output results/default.csv
        
        run_benchmark "3. High Throughput" \
            -preset high-throughput -detailed -csv -output results/high_throughput.csv
        
        run_benchmark "4. Cross-Shard Heavy" \
            -preset cross-shard -detailed -csv -output results/cross_shard.csv
        
        run_benchmark "5. Zipf Distribution" \
            -transactions 10000 -clients 20 -cross-shard 30 -read-only 10 \
            -distribution zipf -detailed -csv -output results/zipf.csv
        
        run_benchmark "6. Hotspot Distribution" \
            -transactions 10000 -clients 20 -cross-shard 30 -read-only 10 \
            -distribution hotspot -detailed -csv -output results/hotspot.csv
        
        echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        echo -e "${GREEN}✅ All benchmarks complete!${NC}"
        echo -e "${GREEN}Results saved to: results/${NC}\n"
        ;;
        
    custom)
        # Custom benchmark - pass all remaining args
        shift
        run_benchmark "Custom Benchmark" "$@"
        ;;
        
    *)
        echo -e "${BLUE}╔════════════════════════════════════════════════════════╗${NC}"
        echo -e "${BLUE}║     Paxos Banking - Benchmark Runner                ║${NC}"
        echo -e "${BLUE}╚════════════════════════════════════════════════════════╝${NC}\n"
        
        echo "Usage: $0 [preset|command]"
        echo ""
        echo "Presets:"
        echo "  quick           Quick test (1000 txns, 10 clients)"
        echo "  default         Default benchmark (10K txns)"
        echo "  high-throughput Maximum throughput test (50K txns, unlimited TPS)"
        echo "  cross-shard     Cross-shard heavy (80% cross-shard)"
        echo "  stress          Stress test (100K txns, 5000 TPS target)"
        echo "  zipf            Zipf distribution test"
        echo "  hotspot         Hotspot distribution test"
        echo "  all             Run all presets sequentially"
        echo ""
        echo "Commands:"
        echo "  custom [args]   Run custom benchmark with specified args"
        echo ""
        echo "Examples:"
        echo "  $0 quick"
        echo "  $0 high-throughput"
        echo "  $0 custom -transactions 50000 -clients 50 -cross-shard 50"
        echo ""
        echo "Environment Variables:"
        echo "  BENCHMARK_NODES  Comma-separated node addresses (default: localhost:50051-50059)"
        echo ""
        exit 1
        ;;
esac
