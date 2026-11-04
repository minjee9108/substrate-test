#!/bin/bash
# ssh ProxyCommand: ate-ssh-proxy.sh <atespace> <actor>
# Starts a kubectl-ate proxy on a free local port, relays this SSH connection
# over a WebSocket to the actor's sshd bridge, and tears the proxy down on exit.
set -u
ATESPACE="$1"; ACTOR="$2"
KUBECTL_ATE="${KUBECTL_ATE:-kubectl-ate}"
SSHPROXY="$(cd "$(dirname "$0")" && pwd)/sshproxy"
LOG=$(mktemp -t ate-ssh-proxy)
"$KUBECTL_ATE" --context "${ATE_KUBE_CONTEXT:-kind-kind}" proxy actor "$ACTOR" -a "$ATESPACE" :80 >"$LOG" 2>&1 &
PID=$!
disown $PID
trap 'kill $PID 2>/dev/null; rm -f "$LOG"' EXIT
PORT=""
for _ in $(seq 1 100); do
  PORT=$(sed -n 's/^Proxying 127.0.0.1:\([0-9]*\) .*/\1/p' "$LOG" | head -1)
  [ -n "$PORT" ] && break
  kill -0 $PID 2>/dev/null || { cat "$LOG" >&2; exit 1; }
  sleep 0.2
done
[ -n "$PORT" ] || { echo "ate-ssh-proxy: proxy did not start" >&2; cat "$LOG" >&2; exit 1; }
"$SSHPROXY" "ws://127.0.0.1:$PORT/ssh"
