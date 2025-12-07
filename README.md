# Paxos Distributed Banking System

A distributed banking system implementing Paxos consensus with:
- Multi-Paxos consensus algorithm
- Sharded architecture (3 clusters, 9 nodes)
- 2-Phase Commit for cross-shard transactions
- Leader election and failure recovery
- Write-Ahead Logging (WAL)

## Build

```bash
go build -o bin/node cmd/node/main.go
go build -o bin/client cmd/client/main.go
```

## Run

Start all 9 nodes:
```bash
for i in 1 2 3 4 5 6 7 8 9; do
    ./bin/node -id $i -config config/nodes.yaml &
done
```

Run client:
```bash
./bin/client -config config/nodes.yaml
```

Or run with test file:
```bash
./bin/client -testfile testcases/official_tests_converted.csv
```

## Project Structure

- `cmd/node/` - Node server
- `cmd/client/` - Client application
- `internal/node/` - Paxos implementation
- `internal/config/` - Configuration management
- `proto/` - Protocol buffers
- `testcases/` - Test cases
