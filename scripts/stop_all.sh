#!/bin/bash

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

rm -f logs/*.pid
echo "Done!"
