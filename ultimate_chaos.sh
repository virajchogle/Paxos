#!/bin/bash
echo "=== ULTIMATE CHAOS TEST ==="
echo "Testing: 20 concurrent cross-shard + cascading failures + rapid recovery"
(sleep 2; echo "flush"; sleep 8
 
 # Send 20 cross-shard transactions rapidly
 for i in {1..20}; do
   sender=$((100 + i*10))
   receiver=$((5000 + i*10))
   echo "send $sender $receiver 1"
 done
 
 sleep 5
 
 # Cascade failures across all clusters
 echo "fail 1"; echo "fail 2"
 sleep 2
 echo "fail 4"; echo "fail 5"
 sleep 2
 echo "fail 7"; echo "fail 8"
 
 sleep 5
 
 # Send more transactions during failures
 for i in {21..30}; do
   sender=$((100 + i*10))
   receiver=$((5000 + i*10))
   echo "send $sender $receiver 1"
 done
 
 sleep 5
 
 # Rapid recovery
 echo "recover 1"; echo "recover 2"
 sleep 1
 echo "recover 4"; echo "recover 5"
 sleep 1
 echo "recover 7"; echo "recover 8"
 
 sleep 10
 
 # Final transactions
 for i in {31..40}; do
   sender=$((100 + i*10))
   receiver=$((5000 + i*10))
   echo "send $sender $receiver 1"
 done
 
 sleep 15
 
 # Check consistency on sample items
 echo "printbalance 110"
 sleep 2
 echo "printbalance 310"
 sleep 2
 echo "printbalance 5110"
 sleep 2
 echo "printbalance 5310"
 sleep 2
 
 echo "quit"
) | ./bin/client 2>&1 | grep -E "n[0-9] :"

echo ""
echo "✅ ULTIMATE CHAOS TEST COMPLETE"
