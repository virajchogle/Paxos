#!/bin/bash
set -e

echo "Building Paxos Banking System..."

cd proto
protoc --go_out=. --go_opt=paths=source_relative \
    --go-grpc_out=. --go-grpc_opt=paths=source_relative \
    paxos.proto
cd ..

mkdir -p bin logs
go build -o bin/node cmd/node/main.go
go build -o bin/client cmd/client/main.go
go build -o bin/benchmark cmd/benchmark/main.go
go build -o bin/configgen cmd/configgen/main.go

echo "Build complete!"
echo ""
echo "Binaries built:"
echo "  bin/node       - Node server"
echo "  bin/client     - Client CLI"
echo "  bin/benchmark  - Benchmark runner"
echo "  bin/configgen  - Configuration generator (for configurable clusters)"
echo ""
echo "Usage for configurable clusters:"
echo "  ./bin/configgen -clusters=4 -nodes-per-cluster=5 -items=12000"
echo "  CONFIG_FILE=config/nodes.yaml ./scripts/start_nodes.sh"
