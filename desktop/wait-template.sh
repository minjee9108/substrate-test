#!/bin/bash
# Waits until the template row has more than 3 fields (a golden tag or an error appeared), up to about 8 minutes.
export PATH=$PATH:$HOME/go/bin
for i in $(seq 1 80); do
  LINE=$(kubectl ate --context kind-kind get actor-templates -a ate-spike-ssh 2>&1 | grep desktop-template-v3)
  echo "t+$((i*6))s $LINE"
  NF=$(echo "$LINE" | awk '{print NF}')
  [ "$NF" -gt 3 ] && exit 0
  sleep 6
done
exit 1
