#!/usr/bin/env bash
# Installs and controls the tunnel as a per-user background service:
# a systemd user unit on Linux, a launchd agent on macOS.
# Usage: deploy/service.sh setup|start|stop|restart|status|logs|enable|disable
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
binary_link="$HOME/.local/bin/reverse-ssh-tunnel"
config_link="$HOME/.config/ssh-tunnel/tunnel.json"

unit=reverse-ssh-tunnel.service
unit_link="$HOME/.config/systemd/user/$unit"

label=local.reverse-ssh-tunnel
plist="$HOME/Library/LaunchAgents/$label.plist"
log_file="$HOME/Library/Logs/reverse-ssh-tunnel.log"

usage() {
  echo "usage: $0 setup|start|stop|restart|status|logs|enable|disable" >&2
  exit 2
}

install_links() {
  mkdir -p "$(dirname "$binary_link")" "$(dirname "$config_link")"
  ln -sfn "$repo/bin/reverse-ssh-tunnel" "$binary_link"
  ln -sfn "$repo/tunnel.json" "$config_link"
}

systemd_service() {
  case $1 in
  setup)
    install_links
    mkdir -p "$(dirname "$unit_link")"
    ln -sfn "$repo/deploy/$unit" "$unit_link"
    systemctl --user daemon-reload
    systemctl --user enable --now "$unit"
    ;;
  start) systemctl --user start "$unit" ;;
  stop) systemctl --user stop "$unit" ;;
  restart)
    systemctl --user daemon-reload
    systemctl --user restart "$unit"
    ;;
  status) systemctl --user status "$unit" --no-pager ;;
  logs) journalctl --user -u "$unit" -f ;;
  enable) systemctl --user enable --now "$unit" ;;
  disable) systemctl --user disable --now "$unit" ;;
  *) usage ;;
  esac
}

launchd_target() { echo "gui/$(id -u)/$label"; }

launchd_loaded() { launchctl print "$(launchd_target)" >/dev/null 2>&1; }

# launchd does not expand ~ or variables, so the plist carries absolute
# paths and is rendered from the template instead of being linked.
launchd_write_plist() {
  mkdir -p "$(dirname "$plist")" "$(dirname "$log_file")"
  sed -e "s|@LABEL@|$label|g" \
    -e "s|@BINARY@|$binary_link|g" \
    -e "s|@CONFIG@|$config_link|g" \
    -e "s|@LOG@|$log_file|g" \
    "$repo/deploy/$label.plist.in" >"$plist"
}

launchd_load() {
  if ! launchd_loaded; then
    launchctl bootstrap "gui/$(id -u)" "$plist"
  fi
}

launchd_unload() {
  launchd_loaded || return 0
  launchctl bootout "$(launchd_target)"
  # The job can linger briefly after bootout; bootstrapping it again
  # before it is gone fails.
  local i
  for ((i = 0; i < 40; i++)); do
    launchd_loaded || return 0
    sleep 0.5
  done
  echo "$label is still loaded" >&2
  return 1
}

launchd_service() {
  case $1 in
  setup)
    install_links
    launchd_write_plist
    launchctl enable "$(launchd_target)"
    launchd_load
    ;;
  start)
    launchd_load
    launchctl kickstart "$(launchd_target)"
    ;;
  stop) launchd_unload ;;
  restart)
    launchd_write_plist
    launchd_unload
    launchd_load
    ;;
  status) launchctl print "$(launchd_target)" ;;
  logs)
    touch "$log_file"
    tail -F "$log_file"
    ;;
  enable)
    launchctl enable "$(launchd_target)"
    launchd_load
    ;;
  disable)
    launchctl disable "$(launchd_target)"
    launchd_unload
    ;;
  *) usage ;;
  esac
}

[ "$#" -eq 1 ] || usage
case $(uname -s) in
Linux) systemd_service "$1" ;;
Darwin) launchd_service "$1" ;;
*)
  echo "unsupported system: $(uname -s)" >&2
  exit 1
  ;;
esac
