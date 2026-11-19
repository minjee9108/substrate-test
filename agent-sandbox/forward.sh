#!/bin/bash
# Keeps the gateway port-forward alive. kubectl port-forward exits when its tunnel breaks,
# for example after the laptop sleeps, so restart it in a loop.
CTX=${CTX:-kind-agent-sandbox}
while true; do
  kubectl --context "$CTX" -n sandbox-gateway port-forward svc/ssh-gateway 2226:22
  echo "$(date) port-forward exited, restarting" >&2
  sleep 1
done
