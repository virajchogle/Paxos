#!/bin/bash

echo "Stopping all Paxos nodes and clients..."

# Detect OS and use appropriate kill command
if [[ "$OSTYPE" == "msys" ]] || [[ "$OSTYPE" == "cygwin" ]]; then
    # Windows - use taskkill
    
    # Stop node processes
    if taskkill //F //IM node.exe 2>/dev/null; then
        echo "✅ Stopped node processes"
    else
        echo "No node processes found running"
    fi
    
    # Stop client processes
    if taskkill //F //IM client.exe 2>/dev/null; then
        echo "✅ Stopped client processes"
    else
        echo "No client processes found running"
    fi
    
    # Close mintty terminal windows (Git Bash terminals)
    if taskkill //F //IM mintty.exe 2>/dev/null; then
        echo "✅ Closed terminal windows"
    else
        echo "No terminal windows found"
    fi
else
    # Linux/Mac - use pkill
    
    # Stop node processes
    if pkill -f "bin/node" 2>/dev/null; then
        echo "✅ Stopped node processes"
    else
        echo "No node processes found running"
    fi
    
    # Stop client processes
    if pkill -f "bin/client" 2>/dev/null; then
        echo "✅ Stopped client processes"
    else
        echo "No client processes found running"
    fi
    
    # Close terminal windows
    if pkill -f "mintty.*Paxos Node" 2>/dev/null; then
        echo "✅ Closed terminal windows"
    else
        echo "No terminal windows found"
    fi
fi

# Clean up PID files
rm -f logs/*.pid

echo ""
echo "All processes stopped!"