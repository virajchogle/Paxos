#!/bin/bash
set -e

mkdir -p logs

INITIAL_BALANCE=${INITIAL_BALANCE:-10}
echo "Starting 9 nodes (3 clusters) with INITIAL_BALANCE=$INITIAL_BALANCE"

NODE_BIN="./bin/node"
[[ -f "bin/node.exe" ]] && NODE_BIN="./bin/node.exe"
[[ ! -f "$NODE_BIN" ]] && echo "Error: No node binary. Run ./scripts/build.sh" && exit 1

pkill -f "bin/node" 2>/dev/null || true
sleep 1

CURRENT_DIR=$(pwd)

for i in {1..9}; do
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS: open Terminal window that closes when process exits
        osascript -e "tell application \"Terminal\" to do script \"cd '$CURRENT_DIR' && INITIAL_BALANCE=$INITIAL_BALANCE $NODE_BIN -id $i -config config/nodes.yaml; exit\"" &
    elif command -v gnome-terminal &>/dev/null; then
        gnome-terminal -- bash -c "cd '$CURRENT_DIR' && INITIAL_BALANCE=$INITIAL_BALANCE $NODE_BIN --id=$i --config=config/nodes.yaml" &
    else
        INITIAL_BALANCE=$INITIAL_BALANCE $NODE_BIN --id=$i --config=config/nodes.yaml > logs/node${i}.log 2>&1 &
    fi
    sleep 0.3
done

echo ""
echo "All 9 nodes started in separate windows!"
echo "  Cluster 1: nodes 1-3 (items 1-3000)"
echo "  Cluster 2: nodes 4-6 (items 3001-6000)"  
echo "  Cluster 3: nodes 7-9 (items 6001-9000)"
echo ""
echo "Stop: ./scripts/stop_all.sh"
