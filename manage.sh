#!/usr/bin/env bash
# ── h4kmally Server Management ──
set -euo pipefail
cd "$(dirname "$0")"

# Source .env first so PORT and other vars are available to all commands
if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi

PORT="${PORT:-3001}"
BIN="./server-bin"
NAME="h4kmally-server"

usage() {
  echo "Usage: $0 {start|stop|restart|build|status|logs}"
  echo
  echo "  start    Build (if needed) and start the server"
  echo "  stop     Stop the running server"
  echo "  restart  Stop then start"
  echo "  build    Compile the Go binary"
  echo "  status   Check if the server is running"
  echo "  logs     Tail stdout (if running via nohup)"
  exit 1
}

get_pid() {
  lsof -ti :"$PORT" -sTCP:LISTEN 2>/dev/null | head -1 || true
}

do_build() {
  echo "[$NAME] Building..."
  go build -o "$BIN" ./cmd/server 2>&1
  echo "[$NAME] Build OK"
}

do_start() {
  pid=$(get_pid)
  if [[ -n "$pid" ]]; then
    echo "[$NAME] Already running (PID $pid) on port $PORT"
    return 0
  fi
  if [[ ! -x "$BIN" ]]; then
    do_build
  fi
  echo "[$NAME] Starting on port $PORT..."
  nohup "$BIN" > server.log 2>&1 &
  sleep 1
  pid=$(get_pid)
  if [[ -n "$pid" ]]; then
    echo "[$NAME] Running (PID $pid)"
  else
    echo "[$NAME] Failed to start. Check server.log"
    return 1
  fi
}

do_stop() {
  pid=$(get_pid)
  if [[ -z "$pid" ]]; then
    echo "[$NAME] Not running"
    return 0
  fi
  echo "[$NAME] Stopping PID $pid..."
  kill "$pid" 2>/dev/null || true
  sleep 1
  # Force kill if still alive
  pid=$(get_pid)
  if [[ -n "$pid" ]]; then
    kill -9 "$pid" 2>/dev/null || true
  fi
  echo "[$NAME] Stopped"
}

do_status() {
  pid=$(get_pid)
  if [[ -n "$pid" ]]; then
    echo "[$NAME] Running (PID $pid) on port $PORT"
  else
    echo "[$NAME] Not running"
  fi
}

do_logs() {
  if [[ -f server.log ]]; then
    tail -f server.log
  else
    echo "[$NAME] No log file found"
  fi
}

case "${1:-}" in
  start)   do_start ;;
  stop)    do_stop ;;
  restart) do_stop; sleep 1; do_start ;;
  build)   do_build ;;
  status)  do_status ;;
  logs)    do_logs ;;
  *)       usage ;;
esac
