#!/bin/bash
echo "=== EXTREME MANUAL STRESS TEST ==="
echo ""

echo "### TEST 1: Concurrent cross-shard storm"
(sleep 2; echo "flush"; sleep 8
 for i in {1..8}; do echo "send $((i*100)) $((5000+i*100)) 2"; done
 sleep 15
 for item in 100 200 300 400 500 600 700 800; do echo "printbalance $item"; sleep 1; done
 for item in 5100 5200 5300 5400 5500 5600 5700 5800; do echo "printbalance $item"; sleep 1; done
 echo "quit") | ./bin/client 2>&1 | grep "n[0-9] :"

echo ""
echo "### TEST 2: All leaders fail + transactions"
(sleep 2; echo "flush"; sleep 8
 echo "send 100 200 5"; sleep 3
 echo "fail 1"; echo "fail 4"; echo "fail 7"; sleep 2
 echo "send 300 400 3"; sleep 8
 echo "send 500 6000 2"; sleep 8
 echo "printbalance 100"; sleep 2
 echo "printbalance 300"; sleep 2
 echo "printbalance 500"; sleep 2
 echo "quit") | ./bin/client 2>&1 | grep -E "n[0-9] :|FAILED"

echo ""
echo "### TEST 3: Rapid fail/recover during 2PC"
(sleep 2; echo "flush"; sleep 8
 echo "send 1000 5000 3"; sleep 1
 echo "fail 2"; echo "fail 5"; sleep 2
 echo "send 1100 5100 3"; sleep 1
 echo "recover 2"; echo "recover 5"; sleep 8
 echo "send 1200 5200 3"; sleep 8
 echo "printbalance 1000"; sleep 2
 echo "printbalance 1100"; sleep 2
 echo "printbalance 1200"; sleep 2
 echo "quit") | ./bin/client 2>&1 | grep "n[0-9] :"

echo ""
echo "=== ALL TESTS COMPLETE ==="
