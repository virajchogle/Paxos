#!/bin/bash

# Simple script to demonstrate checkpointing

echo "========================================="
echo "   Checkpointing Test"
echo "========================================="
echo ""

# Check if nodes are built
if [ ! -f "bin/node.exe" ] && [ ! -f "bin/node" ]; then
    echo "❌ Node binary not found. Building..."
    ./scripts/build.sh
fi

echo "📋 This script will:"
echo "  1. Show you checkpoint messages in real-time"
echo "  2. Verify checkpoints are created every 10 commits"
echo "  3. Show log cleanup happening"
echo ""
echo "⚠️  Make sure 5 nodes are already running!"
echo "   (Use ./scripts/start_nodes.sh in another terminal)"
echo ""
read -p "Press ENTER when nodes are running..."

echo ""
echo "🔍 Starting to monitor logs for checkpoint activity..."
echo "   (Press Ctrl+C to stop monitoring)"
echo ""

# Monitor checkpoint activity in real-time
tail -f logs/node*.log | grep --line-buffered -i "checkpoint\|cleaned up logs"

