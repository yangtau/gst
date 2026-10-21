# gst

English | [简体中文](README.zh-CN.md)

`gst` (go ssh tunnel) keeps SSH port forwards running in the background: tunnels start at login and reconnect after failures. It connects using your `~/.ssh/config`, and forwards use the same syntax as `ssh -R` / `ssh -L` / `ssh -D`. It runs on Linux and macOS.

- **tunnel**: a named SSH connection, such as `myhost`.
- **forward**: one port forward on a tunnel.
  - `-R [bind_address:]port:host:hostport`: listen on the server; this machine connects to `host:hostport`.
  - `-L [bind_address:]port:host:hostport`: listen on this machine; the server connects to `host:hostport`.
  - `-D [bind_address:]port`: listen on this machine as a SOCKS5 proxy; the server connects to each address that clients request.
  - In `-R` and `-L`, either side can be a Unix socket path instead of a port or `host:hostport`.

## Install

```bash
go install github.com/yangtau/ssh-tunnel/cmd/gst@latest
```

With Nix (flakes enabled):

```bash
nix profile add github:yangtau/ssh-tunnel
```

From a checkout:

```bash
nix build                 # result/bin/gst
nix run . -- --help
nix profile add .         # install from the local checkout
```

The Nix package includes completion scripts for Bash, Zsh, and Fish.

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
| `gst add <name> [destination] [-R spec]... [-L spec]... [-D port]...` | Add a tunnel and start it in the background |
| `gst set <name> [flags]` | Change the destination or connection settings |
| `gst forward add <tunnel> (-R spec \| -L spec \| -D [bind:]port)...` | Add forwards |
| `gst forward remove <tunnel> (-R \| -L \| -D [bind:]port\|socket)...` | Remove forwards |
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

# Unix sockets: reach the server's PostgreSQL socket at a local socket,
# and the local ssh-agent at a socket on the server
gst forward add myhost -L ~/.gst/db.sock:/run/postgresql/.s.PGSQL.5432
gst forward add myhost -R /home/user/agent.sock:"$SSH_AUTH_SOCK" --no-restart
gst set myhost --cleanup-listeners

# A SOCKS5 proxy on local port 1080 whose connections leave from the server
gst forward add myhost -D 1080
curl --proxy socks5h://127.0.0.1:1080 https://intranet.example.com

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
- The server's sshd `AllowTcpForwarding` must allow the directions you use: `yes` for both, or `remote` for `-R` only, or `local` for `-L` and `-D` only.

## Unix sockets

- As in ssh, a field containing `/` is a Unix socket path: `-L /tmp/db.sock:127.0.0.1:5432`, `-L 15432:/run/postgresql/.s.PGSQL.5432`, `-R /home/user/agent.sock:/tmp/agent.sock`. A path cannot contain `:`.
- Paths on this machine may be relative or start with `~`; gst saves them as absolute paths. Paths on the server must be absolute.
- Sockets that gst creates on this machine are accessible to you only (mode `0600`). Before listening, gst replaces a socket file that nothing accepts on, as left behind by a crash; it never replaces other files or sockets in use.
- The server's sshd `AllowStreamLocalForwarding` must allow the directions you use; it does by default.
- **sshd leaves `-R` socket files on the server behind**, even when the tunnel stops cleanly, and refuses to listen on an existing file. Without one of the following, an `-R` socket forward fails with `listener start failed` from its first restart or reconnect on: set `StreamLocalBindUnlink yes` in the server's sshd configuration, or turn on `--cleanup-listeners` (see [Troubleshooting](#troubleshooting)).

## SOCKS proxy (`-D`)

- gst supports SOCKS5 `CONNECT` without authentication, like `ssh -D`. UDP (`UDP ASSOCIATE`), `BIND`, and SOCKS4 are not supported, so DNS queries, QUIC, and other UDP traffic cannot go through it.
- Where names are resolved depends on the client. A client that sends the domain name, such as curl with `socks5h://`, has the server resolve it, so names that only resolve on the server's network work. A client that resolves names itself, such as curl with `socks5://`, sends only the IP address.
- Anyone who can reach the listen address can use the proxy. Keep the default loopback address unless you restrict access with a firewall.
- For programs without proxy support, a tool such as tun2socks can route their TCP traffic from a TUN device into the proxy; gst does not create TUN devices or routes. Their DNS queries are resolved on this machine unless you set up DNS separately.

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
- On macOS, a failed background process logs the error immediately but may stay alive until 30 seconds after its connection attempt began, to give the Local Network permission alert time to appear. This is best effort, not a guarantee; stopping the tunnel interrupts the wait. `running` means the process is alive, not that the tunnel is ready.
- Status in `gst list`: `running`; `down` means it failed and is waiting to restart, so check `gst logs` and `gst check`; `stopped`.
- Logs go to journald on Linux. On macOS they go to `~/Library/Logs/gst/<name>.log`; at 1 MiB gst moves it to `<name>.log.1`, replacing the older one, so a tunnel keeps at most about 2 MiB of logs. `gst remove` deletes them. For privacy, the log records no server addresses, users, key paths, or forward addresses. Forwards appear by number (such as `forward 2`), matching `gst forward list`.
- On Linux, to keep tunnels running after you log out and start them at boot, run `loginctl enable-linger "$USER"`; gst tells you when lingering is off. On macOS, tunnels start at login and stop at logout.

## Troubleshooting

Start with `gst check <name>`. It checks each step and suggests a fix for the one that fails. `add`, `set`, `start`, and `restart` also check the identity and known_hosts first, and save or start nothing if the check fails.

| Symptom | Fix |
| --- | --- |
| `no usable key`, or a key is `passphrase-protected` | Create a key without a passphrase as in [Before you start](#before-you-start), and select it with `--identity` or `IdentityFile` in your ssh config |
| `no host key for ...` | Connect once with `ssh <host>` to record the host key |
| Host key mismatch | Once you have confirmed the server's key really changed, run `ssh-keygen -R <host>` and connect once with `ssh` |
| A `-R` port is in use (`listener start failed`) | Usually a stale session from an earlier connection still holds it on the server; see below |
| A `-R` socket fails with `listener start failed` after a restart | The socket file from the previous session is still on the server; set `StreamLocalBindUnlink yes` in sshd or turn on `--cleanup-listeners` |
| macOS blocks access to a local network server | Allow gst in the Local Network alert or in System Settings → Privacy & Security → Local Network, then run `gst restart <name>`. Authorization for your terminal does not necessarily authorize the background service |

**Stale sessions holding server ports.** After a network failure, the old SSH session may keep a `-R` port on the server. Set `ClientAliveInterval 15` and `ClientAliveCountMax 3` in the server's sshd configuration so that sshd closes dead sessions quickly.

You can also turn on `--cleanup-listeners`: after each connect and before listening, gst kills the login user's processes on the server that listen on the `-R` ports and sockets, and removes the login user's socket files there that nothing listens on. These are **not only SSH processes**, so use it only for ports and sockets dedicated to the tunnel. On Linux it cannot clean up leftover sshd session processes. The server needs `bash`, `lsof`, `ps`, and `id` in `PATH`, plus readable `/proc/net/tcp` and `/proc/net/unix` on Linux or `netstat` elsewhere.

## Scripts and agents

- Results go to stdout. Errors go to stderr, start with `error:`, and usually come with a `hint:` line naming the next command to run.
- Exit status: `0` success, `1` the work failed (including a failed `check`), `2` invalid arguments.
- `list`, `show`, `check`, and `forward list` accept `--json`.
- No command prompts for input. Adding a forward that already exists changes nothing; a different forward on the same listen address is an error. `-L` and `-D` forwards both listen on this machine, so they cannot share an address.

## Configuration file

Tunnels are stored in `~/.config/gst/config.json` (`$XDG_CONFIG_HOME/gst/config.json` when `XDG_CONFIG_HOME` is set). `--config` or the `GST_CONFIG` environment variable selects another file, and background tunnels keep using it. gst maintains this file; change it with gst commands rather than by hand. Running several gst commands at once is safe.
