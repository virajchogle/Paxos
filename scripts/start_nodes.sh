#!/bin/bash
# Configurable node startup script
# Supports dynamic cluster configurations
set -e

CONFIG_FILE="${CONFIG_FILE:-config/nodes.yaml}"
INITIAL_BALANCE="${INITIAL_BALANCE:-10}"

mkdir -p logs

# Parse configuration to get node and cluster info
if command -v yq &> /dev/null; then
    # Use yq if available for proper YAML parsing
    NUM_NODES=$(yq '.nodes | length' "$CONFIG_FILE")
    NUM_CLUSTERS=$(yq '.clusters | length' "$CONFIG_FILE")
else
    # Fallback: count nodes by looking for "port:" entries under nodes section
    # Each node has exactly one port entry
    NUM_NODES=$(grep -c "port:" "$CONFIG_FILE" 2>/dev/null || echo "9")
    
    # Count clusters by looking for "shard_start:" entries
    NUM_CLUSTERS=$(grep -c "shard_start:" "$CONFIG_FILE" 2>/dev/null || echo "3")
    
    # Validate - if counts seem wrong, use defaults
    if [ -z "$NUM_NODES" ] || [ "$NUM_NODES" -eq 0 ]; then
        NUM_NODES=9
    fi
    if [ -z "$NUM_CLUSTERS" ] || [ "$NUM_CLUSTERS" -eq 0 ]; then
        NUM_CLUSTERS=3
    fi
fi

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  Paxos Banking System - Node Startup                        ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""
echo "Configuration: $CONFIG_FILE"
echo "Clusters: $NUM_CLUSTERS"
echo "Total Nodes: $NUM_NODES"
echo "Initial Balance: $INITIAL_BALANCE"
echo ""

NODE_BIN="./bin/node"
[[ -f "bin/node.exe" ]] && NODE_BIN="./bin/node.exe"
[[ ! -f "$NODE_BIN" ]] && echo "Error: No node binary. Run ./scripts/build.sh" && exit 1

# Stop any existing nodes
pkill -f "bin/node" 2>/dev/null || true
sleep 1

CURRENT_DIR=$(pwd)

echo "Starting $NUM_NODES nodes..."

for i in $(seq 1 $NUM_NODES); do
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS: open Terminal window that closes when process exits
        osascript -e "tell application \"Terminal\" to do script \"cd '$CURRENT_DIR' && INITIAL_BALANCE=$INITIAL_BALANCE $NODE_BIN -id $i -config $CONFIG_FILE; exit\"" &
    elif command -v gnome-terminal &>/dev/null; then
        gnome-terminal -- bash -c "cd '$CURRENT_DIR' && INITIAL_BALANCE=$INITIAL_BALANCE $NODE_BIN --id=$i --config=$CONFIG_FILE" &
    else
        INITIAL_BALANCE=$INITIAL_BALANCE $NODE_BIN --id=$i --config=$CONFIG_FILE > logs/node${i}.log 2>&1 &
    fi
    sleep 0.3
done

echo ""
echo "✅ All $NUM_NODES nodes started!"
echo ""

# Print cluster information dynamically
if command -v yq &> /dev/null; then
    for c in $(seq 1 $NUM_CLUSTERS); do
        SHARD_START=$(yq ".clusters.$c.shard_start" "$CONFIG_FILE")
        SHARD_END=$(yq ".clusters.$c.shard_end" "$CONFIG_FILE")
        NODES=$(yq ".clusters.$c.nodes | join(\", \")" "$CONFIG_FILE")
        echo "  Cluster $c: nodes [$NODES] (items $SHARD_START-$SHARD_END)"
    done
else
    echo "  (Install 'yq' for detailed cluster info, or check $CONFIG_FILE)"
fi

echo ""
echo "Stop: ./scripts/stop_all.sh"
echo ""
echo "To use custom configuration:"
echo "  1. Generate: ./bin/configgen -clusters=4 -nodes-per-cluster=5"
echo "  2. Start: CONFIG_FILE=config/nodes.yaml ./scripts/start_nodes.sh"
