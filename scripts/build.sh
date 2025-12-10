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

echo "Build complete!"
