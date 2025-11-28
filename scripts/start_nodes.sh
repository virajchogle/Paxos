#!/bin/bash
set -e

echo "Starting all 5 nodes in separate windows..."
mkdir -p logs

# Determine which binary to use
if [[ -f "bin/node.exe" ]]; then
    NODE_BIN="./bin/node.exe"
elif [[ -f "bin/node" ]]; then
    NODE_BIN="./bin/node"
else
    echo "ERROR: No node binary found in bin/"
    echo "Please run: ./scripts/build.sh"
    exit 1
fi

echo "Using binary: $NODE_BIN"

# Stop any existing nodes
echo "Stopping any existing nodes..."
if [[ "$OSTYPE" == "msys" ]] || [[ "$OSTYPE" == "cygwin" ]]; then
    # Windows - use taskkill
    taskkill //F //IM node.exe 2>/dev/null || true
    taskkill //F //IM node 2>/dev/null || true
else
    # Linux/Mac - use pkill
    pkill -f "bin/node" || true
fi
sleep 1

# Get the current directory
CURRENT_DIR=$(pwd)

# Convert to Windows path if needed
if [[ "$OSTYPE" == "msys" ]] || [[ "$OSTYPE" == "cygwin" ]]; then
    # Convert Git Bash path to Windows path
    WINDOWS_DIR=$(cygpath -w "$CURRENT_DIR" 2>/dev/null || echo "$CURRENT_DIR")
else
    WINDOWS_DIR="$CURRENT_DIR"
fi

# Start each node in a separate terminal window
for i in {1..5}; do
    echo "Starting node $i in new window..."
    
    # For Git Bash on Windows or WSL
    if [[ "$OSTYPE" == "msys" ]] || [[ "$OSTYPE" == "cygwin" ]] || [[ -n "$WSLENV" ]]; then
        # Windows environment - use mintty (Git Bash terminal)
        mintty -t "Paxos Node $i" -h always /bin/bash -c "cd '$CURRENT_DIR' && echo '═══════════════════════════════════════' && echo '   Paxos Node $i' && echo '═══════════════════════════════════════' && echo '' && $NODE_BIN --id=$i --config=config/nodes.yaml; exec bash" &
    else
        # macOS - use Terminal.app
        if [[ "$OSTYPE" == "darwin"* ]]; then
            osascript -e "tell application \"Terminal\" to do script \"cd '$CURRENT_DIR' && echo '═══════════════════════════════════════' && echo '   Paxos Node $i' && echo '═══════════════════════════════════════' && echo '' && $NODE_BIN -id $i -config config/nodes.yaml\"" &
        # Linux - use gnome-terminal or xterm
        elif command -v gnome-terminal &> /dev/null; then
            gnome-terminal -- bash -c "cd '$CURRENT_DIR' && echo '═══════════════════════════════════════' && echo '   Paxos Node $i' && echo '═══════════════════════════════════════' && echo '' && $NODE_BIN --id=$i --config=config/nodes.yaml; exec bash" &
        elif command -v xterm &> /dev/null; then
            xterm -T "Paxos Node $i" -e bash -c "cd '$CURRENT_DIR' && echo '═══════════════════════════════════════' && echo '   Paxos Node $i' && echo '═══════════════════════════════════════' && echo '' && $NODE_BIN --id=$i --config=config/nodes.yaml; exec bash" &
        else
            echo "Warning: No terminal emulator found. Running in background..."
            $NODE_BIN --id=$i --config=config/nodes.yaml > logs/node${i}.log 2>&1 &
        fi
    fi
    
    sleep 0.5
done

echo ""
echo "✅ All 5 nodes started in separate windows!"
echo "   - Each node runs in its own window"
echo "   - Logs are also saved to logs/nodeX.log"
echo "   - Close windows or press Ctrl+C in each to stop"
echo ""
echo "To stop all nodes: ./scripts/stop_all.sh"