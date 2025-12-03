#!/bin/bash
echo "=== EXTREME STRESS TEST START ==="
(
  sleep 3
  for i in {1..10}; do
    echo "next"
    sleep 15  # Give each set time to complete
  done
  echo "printDB"
  sleep 5
  echo "quit"
) | timeout 300 ./bin/client testcases/extreme_stress_test.csv 2>&1 | \
  grep -E "Set [0-9]|Processing Test Set|completed|SUCCESS|FAILED|n[0-9] :" | tail -80
