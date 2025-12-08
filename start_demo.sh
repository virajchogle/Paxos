#!/bin/bash

# Demo startup script - Opens 9 visible terminal windows (one per node)

echo "🧹 Cleaning old data and processes..."
pkill -9 -f "bin/node" 2>/dev/null
rm -rf data logs
mkdir -p logs

echo "🚀 Starting 9 nodes in separate terminal windows..."

# For macOS Terminal - each node gets its own window
for i in 1 2 3 4 5 6 7 8 9; do
    mkdir -p data/node$i
    osascript -e "tell app \"Terminal\" to do script \"cd $(pwd) && ./bin/node -id $i -config config/nodes.yaml\""
    sleep 0.3
done

echo ""
echo "✅ All 9 nodes starting in separate terminal windows!"
echo "⏳ Wait ~5 seconds for nodes to initialize, then run:"
echo "   ./bin/client -testfile testcases/official_tests_converted.csv"
