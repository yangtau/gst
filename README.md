# gst

English | [简体中文](README.zh-CN.md)

`gst` (go ssh tunnel) keeps SSH port forwards running in the background: tunnels start at login and reconnect after failures. It connects using your `~/.ssh/config`, and forwards use the same syntax as `ssh -R` / `ssh -L`. It runs on Linux and macOS.

- **tunnel**: a named SSH connection, such as `myhost`.
- **forward**: one port forward on a tunnel.
  - `-R [bind_address:]port:host:hostport`: listen on the server; this machine connects to `host:hostport`.
  - `-L [bind_address:]port:host:hostport`: listen on this machine; the server connects to `host:hostport`.

## Install

```bash
go install github.com/yangtau/ssh-tunnel/cmd/gst@latest
```

Background tunnels remember the path of gst. After upgrading or moving gst, run `gst restart --all`.

## Before you start

gst runs in the background, where it cannot type a passphrase or confirm an unknown host key. You need:

1. **A private key without a passphrase**, authorized on the server. A dedicated key is best:

   ```bash
   ssh-keygen -t ed25519 -N '' -f ~/.ssh/gst_ed25519
   ssh-copy-id -i ~/.ssh/gst_ed25519 user@server.example.com
   ```

2. **A Host in `~/.ssh/config`**, for example:

   ```
   Host myhost
     HostName server.example.com
     User user
     IdentityFile ~/.ssh/gst_ed25519
   ```

3. **The server's host key in known_hosts**: connect once with `ssh myhost`, then verify and accept the host key.

## Quick start

```bash
# Add a tunnel and start it in the background: reach this machine's SSH from the server's port 2222
gst add myhost -R 2222:localhost:22

# Add another forward: reach the server's database on local port 15432
gst forward add myhost -L 15432:127.0.0.1:5432

gst list           # tunnels and their status
gst check myhost   # diagnose connection problems step by step
gst logs myhost -f # follow the log
gst stop myhost    # stop, keeping the configuration
gst start myhost   # start again
gst remove myhost  # stop and delete
```

Every command explains itself with `--help`. `gst completion bash|zsh|fish` generates shell completion scripts.

## Commands

| Command | Purpose |
| --- | --- |
| `gst add <name> [destination] [-R spec]... [-L spec]...` | Add a tunnel and start it in the background |
| `gst set <name> [flags]` | Change the destination or connection settings |
| `gst forward add <tunnel> (-R spec \| -L spec)...` | Add forwards |
| `gst forward remove <tunnel> (-R [bind:]port \| -L [bind:]port)...` | Remove forwards |
| `gst forward list <tunnel>` | List forwards |
| `gst remove <name>...` | Stop and delete tunnels |
| `gst start\|stop\|restart <name>... \| --all` | Start, stop, or restart tunnels |
| `gst list` | List tunnels and their status |
| `gst show <name>` | Show a tunnel's settings and what ssh resolves it to |
| `gst logs <name> [-f] [-n N]` | Print the log |
| `gst check <name>` | Diagnose: ssh config, identity, known_hosts, network, login |

When you change the forwards or settings of a running tunnel, gst restarts it; `--no-restart` skips that. A change that changes nothing does nothing.

## Common tasks

```bash
# Give the destination directly when it is not in your ssh config
gst add db admin@db.example.com -L 15432:127.0.0.1:5432
gst add web ssh://deploy@203.0.113.7:2200 -R 8080:localhost:3000

# Listen on all of the server's interfaces
gst forward add web -R '*:8080:localhost:3000'

# Remove a forward: the direction and listen address are enough
gst forward remove myhost -R 2222

# Override ssh config settings; '' or 0 goes back to the default
gst set myhost --keepalive-interval 30s --keepalive-count 5
gst set myhost --identity ''

# Turn remote port cleanup on or off (see below)
gst set myhost --cleanup-listeners
gst set myhost --cleanup-listeners=false
```

## Forward syntax

- Without `bind_address`, a forward listens on loopback (`127.0.0.1`) only; `*` or an empty `bind_address` (as in `:8080:localhost:3000`) listens on all interfaces. Put IPv6 addresses in brackets.
- `host` is resolved by the side that connects to it: in `-L`, `127.0.0.1` is the server itself; in `-R`, `localhost` is this machine.
- The server's sshd `GatewayPorts` setting can further limit where `-R` forwards listen. When listening on all interfaces, restrict access with a firewall.
- The server's sshd `AllowTcpForwarding` must allow the directions you use: `yes` for both, or `remote` for `-R` only, or `local` for `-L` only.

## ssh configuration

`destination` is `host`, `user@host`, or `ssh://user@host:port`, and defaults to the tunnel name. Every time a tunnel starts, gst reads your ssh configuration again with `ssh -G`, so after editing `~/.ssh/config`, run `gst restart <name>`.

| ssh setting | Used for | gst override | Default when neither is set |
| --- | --- | --- | --- |
| `HostName`, `Port`, `User` | Server address and login user | `gst set --destination` | ssh's defaults |
| `IdentityFile` | Private keys, tried in order; missing or passphrase-protected ones are skipped | `--identity` | ssh's default key list |
| `UserKnownHostsFile`, `GlobalKnownHostsFile` | Host key verification | `--known-hosts` | ssh's default files |
| `ConnectTimeout` | Connect and login timeout | `--connect-timeout` | 15 seconds |
| `ServerAliveInterval` | Keepalive interval | `--keepalive-interval` | 15 seconds |
| `ServerAliveCountMax` | Unanswered intervals before reconnecting | `--keepalive-count` | 3 |

Limitations:

- Only private keys without a passphrase work; ssh-agent is not used.
- known_hosts is checked strictly; unknown hosts are never accepted.
- `ProxyJump`, `ProxyCommand`, and `HostKeyAlias` are not supported.

## Running in the background

- Each started tunnel is a user service: a systemd user service `gst-<name>.service` on Linux, a launchd agent `local.gst.<name>` on macOS. gst creates and deletes them; you never manage them yourself.
- A tunnel that exits restarts after 5 seconds, without a limit.
- Status in `gst list`: `running`; `down` means it failed and is waiting to restart, so check `gst logs` and `gst check`; `stopped`.
- Logs go to journald on Linux and to `~/Library/Logs/gst/<name>.log` on macOS. For privacy, the log records no server addresses, users, key paths, or forward addresses. Forwards appear by number (such as `forward 2`), matching `gst forward list`.
- On Linux, to keep tunnels running after you log out and start them at boot, run `loginctl enable-linger "$USER"`; gst tells you when lingering is off. On macOS, tunnels start at login and stop at logout.

## Troubleshooting

Start with `gst check <name>`. It checks each step and suggests a fix for the one that fails. `add`, `set`, `start`, and `restart` also check the identity and known_hosts first, and save or start nothing if the check fails.

| Symptom | Fix |
| --- | --- |
| `no usable key`, or a key is `passphrase-protected` | Create a key without a passphrase as in [Before you start](#before-you-start), and select it with `--identity` or `IdentityFile` in your ssh config |
| `no host key for ...` | Connect once with `ssh <host>` to record the host key |
| Host key mismatch | Once you have confirmed the server's key really changed, run `ssh-keygen -R <host>` and connect once with `ssh` |
| A `-R` port is in use (`listener start failed`) | Usually a stale session from an earlier connection still holds it on the server; see below |

**Stale sessions holding server ports.** After a network failure, the old SSH session may keep a `-R` port on the server. Set `ClientAliveInterval 15` and `ClientAliveCountMax 3` in the server's sshd configuration so that sshd closes dead sessions quickly.

You can also turn on `--cleanup-listeners`: after each connect and before listening, gst kills the login user's processes on the server that listen on the `-R` ports. These are **not only SSH processes**, so use it only for ports dedicated to the tunnel. On Linux it cannot clean up leftover sshd session processes. The server needs `bash`, `lsof`, `ps`, and `id` in `PATH`, plus a readable `/proc/net/tcp` on Linux or `netstat` elsewhere.

## Scripts and agents

- Results go to stdout. Errors go to stderr, start with `error:`, and usually come with a `hint:` line naming the next command to run.
- Exit status: `0` success, `1` the work failed (including a failed `check`), `2` invalid arguments.
- `list`, `show`, `check`, and `forward list` accept `--json`.
- No command prompts for input. Adding a forward that already exists changes nothing; a different target on the same listen address is an error.

## Configuration file

Tunnels are stored in `~/.config/gst/config.json` (`$XDG_CONFIG_HOME/gst/config.json` when `XDG_CONFIG_HOME` is set). `--config` or the `GST_CONFIG` environment variable selects another file, and background tunnels keep using it. gst maintains this file; change it with gst commands rather than by hand. Running several gst commands at once is safe.
