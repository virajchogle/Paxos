#!/bin/bash

CLEAN_DATA="${CLEAN_DATA:-true}"

echo "Stopping all Paxos processes..."

pkill -f "bin/node" 2>/dev/null && echo "Stopped nodes" || echo "No nodes running"
pkill -f "bin/client" 2>/dev/null && echo "Stopped clients" || echo "No clients running"
pkill -f "bin/benchmark" 2>/dev/null && echo "Stopped benchmarks" || echo "No benchmarks running"

# Close finished Terminal windows on macOS
if [[ "$OSTYPE" == "darwin"* ]]; then
    sleep 0.5
    osascript <<'EOF'
tell application "Terminal"
    repeat with w in windows
        try
            if busy of (tab 1 of w) is false then
                close w saving no
            end if
        end try
    end repeat
end tell
EOF
fi

# Clean up data and logs for fresh start
if [ "$CLEAN_DATA" = "true" ]; then
    echo "Cleaning up logs and data..."
    rm -rf logs/*.log logs/*.pid 2>/dev/null
    rm -rf data/node*_pebble 2>/dev/null
    rm -rf data/node*_db.json data/node*_wal.json 2>/dev/null
    echo "✅ Cleaned logs/ and data/ directories"
else
    rm -f logs/*.pid
fi

echo "Done!"
echo ""
echo "To preserve data on stop, use: CLEAN_DATA=false ./scripts/stop_all.sh"
