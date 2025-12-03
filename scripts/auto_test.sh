#!/bin/bash

# Automated Edge Case Testing Script
# Tests edge cases and captures results

TEST_FILE=$1

if [ -z "$TEST_FILE" ]; then
    echo "Usage: $0 <test_file>"
    exit 1
fi

echo "Testing: $TEST_FILE"
echo ""

# Create commands file
cat > /tmp/test_commands.txt <<EOF
flush
next
printdb
performance
quit
EOF

# Run client with commands
./bin/client -testfile "$TEST_FILE" < /tmp/test_commands.txt

rm /tmp/test_commands.txt
