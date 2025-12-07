#!/bin/bash

# Simple script to run the Paxos system

case "$1" in
    build)
        echo "Building..."
        go build -o bin/node cmd/node/main.go
        go build -o bin/client cmd/client/main.go
        echo "✅ Build complete"
        ;;
    
    start)
        echo "Starting 9 nodes..."
        mkdir -p logs
        for i in 1 2 3 4 5 6 7 8 9; do
            mkdir -p data/node$i
            nohup ./bin/node -id $i -config config/nodes.yaml > logs/node$i.log 2>&1 &
        done
        sleep 2
        echo "✅ All nodes started"
        ;;
    
    stop)
        echo "Stopping all nodes..."
        pkill -9 -f "bin/node"
        echo "✅ All nodes stopped"
        ;;
    
    clean)
        echo "Cleaning data and logs..."
        rm -rf data logs
        echo "✅ Cleaned"
        ;;
    
    test)
        echo "Running client with test file..."
        ./bin/client -testfile testcases/official_tests_converted.csv
        ;;
    
    *)
        echo "Usage: ./run.sh {build|start|stop|clean|test}"
        echo ""
        echo "  build  - Compile node and client"
        echo "  start  - Start all 9 nodes"
        echo "  stop   - Stop all nodes"
        echo "  clean  - Remove data and logs"
        echo "  test   - Run client with test cases"
        ;;
esac
