#!/bin/bash

echo "=== Quick Edge Case Test Suite ==="

# Test simple cross-shard with concurrent transactions
echo -e "\n### Test 1: Multiple concurrent cross-shard transactions"
(sleep 2; echo "flush"; sleep 8
 for i in 1 2 3; do echo "send 1000 5000 2"; done
 sleep 12; echo "printbalance 1000"; sleep 2; echo "printbalance 5000"; sleep 2; echo "quit") | \
  ./bin/client 2>&1 | grep -E "n[0-9] :" | tail -6

# Test with failures
echo -e "\n### Test 2: Transaction during node failure"  
(sleep 2; echo "flush"; sleep 8
 echo "send 100 200 5"; sleep 3
 echo "fail 2"; sleep 3
 echo "send 300 400 3"; sleep 8
 echo "printbalance 100"; sleep 2; echo "printbalance 300"; sleep 2; echo "quit") | \
  ./bin/client 2>&1 | grep -E "n[0-9] :|FAILED" | tail -10

echo -e "\n### Test Summary: Basic tests complete"
