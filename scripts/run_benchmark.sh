#!/bin/bash
set -e

cd "$(dirname "$0")/.."

go build -o bin/benchmark cmd/benchmark/main.go

NODES="localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055,localhost:50056,localhost:50057,localhost:50058,localhost:50059"
[[ -n "$BENCHMARK_NODES" ]] && NODES="$BENCHMARK_NODES"

run() {
    echo "Running: $1"
    shift
    ./bin/benchmark -nodes "$NODES" "$@"
    echo ""
}

case "$1" in
    quick)
        run "Quick Test" -transactions 1000 -clients 10 -cross-shard 20 -read-only 10 -distribution uniform -detailed
        ;;
    default)
        run "Default Benchmark" -preset default -detailed
        ;;
    high-throughput|ht)
        mkdir -p results
        run "High Throughput" -preset high-throughput -detailed -csv -output results/high_throughput.csv
        ;;
    cross-shard|cs)
        mkdir -p results
        run "Cross-Shard Heavy" -preset cross-shard -detailed -csv -output results/cross_shard.csv
        ;;
    stress)
        mkdir -p results
        run "Stress Test" -preset stress -report 10 -detailed -csv -output results/stress_test.csv
        ;;
    zipf)
        mkdir -p results
        run "Zipf Distribution" -transactions 10000 -clients 20 -cross-shard 30 -read-only 10 -distribution zipf -detailed -csv -output results/zipf.csv
        ;;
    hotspot)
        mkdir -p results
        run "Hotspot Distribution" -transactions 10000 -clients 20 -cross-shard 30 -read-only 10 -distribution hotspot -detailed -csv -output results/hotspot.csv
        ;;
    all)
        mkdir -p results
        run "Quick Test" -transactions 1000 -clients 10 -detailed
        run "Default" -preset default -detailed -csv -output results/default.csv
        run "High Throughput" -preset high-throughput -detailed -csv -output results/high_throughput.csv
        run "Cross-Shard" -preset cross-shard -detailed -csv -output results/cross_shard.csv
        run "Zipf" -transactions 10000 -clients 20 -cross-shard 30 -read-only 10 -distribution zipf -detailed -csv -output results/zipf.csv
        run "Hotspot" -transactions 10000 -clients 20 -cross-shard 30 -read-only 10 -distribution hotspot -detailed -csv -output results/hotspot.csv
        echo "All benchmarks complete! Results in results/"
        ;;
    custom)
        shift
        run "Custom Benchmark" "$@"
        ;;
    *)
        echo "Paxos Banking Benchmark Runner"
        echo ""
        echo "Usage: $0 [preset]"
        echo ""
        echo "Presets:"
        echo "  quick        1000 txns, 10 clients"
        echo "  default      10K txns"
        echo "  ht           High throughput (50K txns)"
        echo "  cs           Cross-shard heavy (80%)"
        echo "  stress       100K txns stress test"
        echo "  zipf         Zipf distribution"
        echo "  hotspot      Hotspot distribution"
        echo "  all          Run all presets"
        echo "  custom       Custom args (e.g., custom -transactions 5000)"
        echo ""
        echo "Env: BENCHMARK_NODES=host1:port1,host2:port2,..."
        exit 1
        ;;
esac
