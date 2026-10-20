# shellcheck shell=bash
# Free the TCP ports given as arguments before the tunnel listens on them:
# send SIGTERM, then SIGKILL, to the login user's processes listening there.
# Exits non-zero if a port stays busy, for example because another user's
# process holds it.
#
# lsof only shows the login user's own dumpable processes. On Linux an sshd
# session is not dumpable, so a stale tunnel session cannot be killed from
# here; the server's ClientAliveInterval has to reap it.
set -euo pipefail

command -v lsof >/dev/null
command -v ps >/dev/null

login_uid=$(id -u)
ports=("$@")

kill_listeners() {
  local sig=$1 port=$2 pids pid owner
  pids=$(lsof -t -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)
  for pid in $pids; do
    owner=$(ps -o uid= -p "$pid" 2>/dev/null | tr -d '[:space:]') || continue
    if [ "$owner" = "$login_uid" ]; then
      kill "-$sig" "$pid" 2>/dev/null || true
    fi
  done
}

# port_listening reports whether any process of any user listens on port $1.
# Unlike lsof, the kernel socket tables also show sockets that lsof hides.
port_listening() {
  if [ -r /proc/net/tcp ]; then
    local files=(/proc/net/tcp) suffix
    if [ -r /proc/net/tcp6 ]; then files+=(/proc/net/tcp6); fi
    suffix=$(printf ':%04X' "$1")
    # Column 2 is the local address as HEXIP:HEXPORT; state 0A is LISTEN.
    awk -v suffix="$suffix" '$4 == "0A" && substr($2, length($2) - 4) == suffix { found = 1 }
      END { exit !found }' "${files[@]}"
  else
    # macOS and BSD: the local address column ends in ".PORT".
    netstat -an -p tcp 2>/dev/null |
      awk -v port="$1" '$NF == "LISTEN" && $4 ~ ("[.:]" port "$") { found = 1 }
        END { exit !found }'
  fi
}

busy() {
  local port
  for port in "${ports[@]}"; do
    if port_listening "$port"; then
      return 0
    fi
  done
  return 1
}

for port in "${ports[@]}"; do kill_listeners TERM "$port"; done
for ((i = 0; i < 10; i++)); do
  if ! busy; then exit 0; fi
  sleep 0.3
done

for port in "${ports[@]}"; do kill_listeners KILL "$port"; done
sleep 0.4
if busy; then exit 1; fi
