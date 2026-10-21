# shellcheck shell=bash
# Free the listeners given as arguments, TCP ports or Unix socket paths,
# before the tunnel listens on them: send SIGTERM, then SIGKILL, to the
# login user's processes listening there, and remove the login user's socket
# files that nothing listens on. Exits non-zero if a listener stays busy, for
# example because another user's process holds it.
#
# lsof only shows the login user's own dumpable processes. On Linux an sshd
# session is not dumpable, so a stale tunnel session cannot be killed from
# here; the server's ClientAliveInterval has to reap it.
set -euo pipefail

command -v lsof >/dev/null
command -v ps >/dev/null

login_uid=$(id -u)
listeners=("$@")

# listener_pids prints the pids listening on port or socket path $1 that
# this user can see.
listener_pids() {
  case $1 in
    /*)
      if [ -r /proc/net/unix ]; then
        # lsof cannot look up sockets by path on Linux; match socket inodes.
        local inode fd
        for inode in $(unix_listener_inodes "$1"); do
          for fd in /proc/[0-9]*/fd/*; do
            if [ "$(readlink "$fd" 2>/dev/null)" = "socket:[$inode]" ]; then
              fd=${fd#/proc/}
              echo "${fd%%/*}"
            fi
          done
        done
      else
        lsof -t "$1" 2>/dev/null || true
      fi
      ;;
    *) lsof -t -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null || true ;;
  esac
}

# unix_listener_inodes prints the inodes of listening sockets bound to path $1.
unix_listener_inodes() {
  # Fields: Num RefCount Protocol Flags Type St Inode Path; the path may
  # contain spaces. Flags 00010000 marks a listening socket.
  awk -v path="$1" 'NR > 1 && $4 == "00010000" {
      p = $0
      for (i = 0; i < 7; i++) sub(/^[^ ]+ +/, "", p)
      if (p == path) print $7
    }' /proc/net/unix
}

kill_listeners() {
  local sig=$1 listener=$2 pids pid owner
  pids=$(listener_pids "$listener")
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

# socket_busy removes socket file $1 if this user owns it and nothing
# listens on it, then reports whether the path is still taken.
socket_busy() {
  local listening=
  if [ -r /proc/net/unix ]; then
    if [ -n "$(unix_listener_inodes "$1")" ]; then listening=1; fi
  elif [ -n "$(listener_pids "$1")" ]; then
    # Only this user's listeners are visible here, which suffices because
    # only this user's socket files are removed.
    listening=1
  fi
  if [ -z "$listening" ] && [ -S "$1" ] && [ -O "$1" ]; then
    rm -f -- "$1"
  fi
  [ -e "$1" ] || [ -L "$1" ]
}

busy() {
  local listener
  for listener in "${listeners[@]}"; do
    case $listener in
      /*) if socket_busy "$listener"; then return 0; fi ;;
      *) if port_listening "$listener"; then return 0; fi ;;
    esac
  done
  return 1
}

for listener in "${listeners[@]}"; do kill_listeners TERM "$listener"; done
for ((i = 0; i < 10; i++)); do
  if ! busy; then exit 0; fi
  sleep 0.3
done

for listener in "${listeners[@]}"; do kill_listeners KILL "$listener"; done
sleep 0.4
if busy; then exit 1; fi
