# gst

English | [简体中文](README.zh-CN.md)

`gst` (go ssh tunnel) keeps SSH port forwards running in the background: tunnels start at login and reconnect after failures. It connects using your `~/.ssh/config`, and forwards use the same syntax as `ssh -R` / `ssh -L` / `ssh -D`. It runs on Linux and macOS.

- **tunnel**: a named SSH connection, such as `myhost`.
- **forward**: one port forward on a tunnel.
  - `-R [bind_address:]port[:host:hostport]`: listen on the server; this machine connects to `host:hostport`. Omit the target to run a reverse SOCKS5 proxy: this machine connects to each address that clients request.
  - `-L [bind_address:]port:host:hostport`: listen on this machine; the server connects to `host:hostport`.
  - `-D [bind_address:]port`: listen on this machine as a SOCKS5 proxy; the server connects to each address that clients request.
  - In fixed-target `-R` and `-L`, either side can be a Unix socket path instead of a port or `host:hostport`. SOCKS proxies use TCP listeners only, not Unix sockets.

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

Run `gst version` to print the installed version, for example `gst version 0.2.0`.

The background daemon remembers the path of gst. After upgrading or moving gst, run `gst restart --all` to restart the daemon with the new gst; any command that starts, stops, restarts, or changes tunnels does so too.

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

Every command explains itself with `--help`. In a terminal, help uses bold headings and cyan command and option names. Piped or redirected help is plain text; set `NO_COLOR=1` or `TERM=dumb` to disable styling in a terminal too. `gst completion bash|zsh|fish` generates shell completion scripts.

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
| `gst ui [--port N] [--no-open]` | Show tunnels, peer links, and a map of the mesh in a browser; start, stop, and restart tunnels there |
| `gst mesh enable [--name name]` / `gst mesh disable` | Rejoin (or rename in) or leave the mesh of linked machines |
| `gst mesh peers` | List peer links |
| `gst mesh nodes` | Show every machine in the mesh and the tunnels between them |
| `gst add <name> mesh:<machine> [-L spec]... [-D port]...` | Add a tunnel to a machine of the mesh, through other machines |
| `gst connect <machine> <port>` | Connect standard input and output to a port of a machine in the mesh, such as for `ProxyCommand` |
| `gst version` | Show the installed version |

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

# A reverse SOCKS5 proxy on the server's port 1080; connections leave from this machine
gst forward add myhost -R 1080
# Run curl on the server through the proxy; this machine resolves the domain
ssh myhost 'curl --proxy socks5h://127.0.0.1:1080 https://intranet.example.com'

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
- For `-R [bind_address:]port[:host:hostport]`, the target is optional: omit it for reverse SOCKS5, or include it for the existing fixed-target behavior. `-L` always needs a target; `-D` never takes one.
- A fixed target's `host` is resolved by the side that connects to it: in `-L`, `127.0.0.1` is the server itself; in `-R`, `localhost` is this machine.
- The server's sshd `GatewayPorts` setting can further limit where `-R` forwards listen. When listening on all interfaces, restrict access with a firewall.
- The server's sshd `AllowTcpForwarding` must allow the directions you use: `yes` for both, or `remote` for `-R` only, or `local` for `-L` and `-D` only.

## Unix sockets

- As in ssh, a field containing `/` is a Unix socket path: `-L /tmp/db.sock:127.0.0.1:5432`, `-L 15432:/run/postgresql/.s.PGSQL.5432`, `-R /home/user/agent.sock:/tmp/agent.sock`. A path cannot contain `:`.
- Paths on this machine may be relative or start with `~`; gst saves them as absolute paths. Paths on the server must be absolute.
- Sockets that gst creates on this machine are accessible to you only (mode `0600`). Before listening, gst replaces a socket file that nothing accepts on, as left behind by a crash; it never replaces other files or sockets in use.
- The server's sshd `AllowStreamLocalForwarding` must allow the directions you use; it does by default.
- **sshd leaves `-R` socket files on the server behind**, even when the tunnel stops cleanly, and refuses to listen on an existing file. Without one of the following, an `-R` socket forward fails with `listener start failed` from its first restart or reconnect on: set `StreamLocalBindUnlink yes` in the server's sshd configuration, or turn on `--cleanup-listeners` (see [Troubleshooting](#troubleshooting)).

## SOCKS proxies (`-D` and targetless `-R`)

- `-D` listens on this machine and connects from the SSH server; targetless `-R` listens on the SSH server and connects from the local gst machine. Both require TCP listeners; dynamic Unix sockets are not supported.
- Both support SOCKS5 `CONNECT` without authentication, like `ssh -D`. UDP (`UDP ASSOCIATE`), `BIND`, and SOCKS4 are not supported, so UDP DNS queries, QUIC, and other UDP traffic cannot go through them.
- Where names are resolved depends on the client. With `socks5h://`, curl sends the domain name: the SSH server resolves it for `-D`, and the local gst machine resolves it for targetless `-R`. Names available only on that side's network work. With `socks5://`, curl resolves names where curl runs and sends only the IP address.
- Anyone who can reach the listen address can use the proxy and access networks reachable from the connecting side—including your local network with reverse SOCKS. Keep the default loopback address unless you restrict access with a firewall.
- For programs without proxy support, a tool such as tun2socks can route their TCP traffic from a TUN device into the proxy; gst does not create TUN devices or routes. Their DNS queries are resolved where those programs run unless you set up DNS separately.

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

- One background process, the gst daemon, runs every started tunnel. It is a user service: a systemd user service `gst.service` on Linux, a launchd agent `local.gst` on macOS. gst installs it when you start the first tunnel and removes it when no tunnel is started; you never manage it yourself.
- gst commands talk to the daemon over a socket that only you can access: `~/.local/state/gst/gst.sock` on Linux (under `$XDG_STATE_HOME` when set), `~/Library/Application Support/gst/gst.sock` on macOS.
- A tunnel that fails reconnects after 5 seconds, without a limit. If the daemon itself exits, its service restarts it after 5 seconds.
- Status in `gst list`: `running` means connected with every forward listening; `connecting`; `down` means it failed and is waiting to reconnect, so check `gst logs` and `gst check`; `stopped`. `gst show` and `gst list --json` include the last error of a tunnel that is not running.
- Each tunnel has its own log: `~/.local/state/gst/logs/<name>.log` on Linux, `~/Library/Logs/gst/<name>.log` on macOS. At 1 MiB gst moves it to `<name>.log.1`, replacing the older one, so a tunnel keeps at most about 2 MiB of logs. `gst remove` deletes them. For privacy, the log records no server addresses, users, key paths, or forward addresses. Forwards appear by number (such as `forward 2`), matching `gst forward list`.
- The daemon's own log goes to journald on Linux (`journalctl --user -u gst`) and to `~/Library/Logs/gst-daemon.log` on macOS.
- Upgrading from gst 0.1, where each tunnel was a service of its own (`gst-<name>.service`, `local.gst.<name>`): the first gst command that reads the configuration moves the tunnels whose service exists to the daemon, removes those services, and marks the other tunnels stopped.
- On Linux, to keep tunnels running after you log out and start them at boot, run `loginctl enable-linger "$USER"`; gst tells you when lingering is off. On macOS, tunnels start at login and stop at logout.

## Linking machines (mesh)

gst on several machines links with each other by itself, without any setup: a machine with a tunnel joins the mesh, every tunnel links this machine's gst with the gst on its server over the tunnel's SSH connection, and the server's gst joins when it is first linked to. Each then sees the other in `gst mesh peers`. Linked machines pass on what they know, so `gst mesh nodes` on any of them shows the whole network: when laptop has tunnels to `mini` and `devcn`, `gst mesh nodes` on mini lists laptop and devcn too, and mini can connect to devcn through laptop.

```bash
gst mesh peers                  # this machine's links
gst mesh nodes                  # every machine and the tunnels between them
gst mesh enable --name laptop   # optional: change the node name (default: the host name)
```

- A link needs gst 0.2 or later on the server. gst starts it over SSH with `gst peer-bridge` in the login shell; when gst is not in that shell's `PATH`, give its path with `--remote-gst`, such as `gst set myhost --remote-gst ~/.nix-profile/bin/gst`. A server without gst, or after `gst mesh disable` there, still takes part as a machine its tunnels reach; its link stays `down` and retries less and less often, up to every 5 minutes.
- `gst set <name> --no-mesh` keeps a tunnel from running gst on its server, for example when the server runs a forced command for every session; `--no-mesh=false` undoes it.
- In the mesh, the daemon keeps running even when no tunnel is started, so that it can accept links.
- A link is `connecting`, `up`, or `down`. `gst mesh peers` shows why a link is down. A failed link does not affect the tunnel's forwards.
- `gst mesh nodes` lists gst nodes by name and SSH servers without gst by the name of the tunnel to them. A server whose gst joins the mesh later shows up as that gst node. A machine is reachable when this machine can get to it through running tunnels, going on past a machine only over a peer link that is up. Machines that drop out disappear within 30 minutes.
- Peers are trusted: a link logs in as your user on the server, and the server's daemon accepts it only through its own socket. So any server with gst that your tunnels reach can, through the mesh, reach wherever your machines can; use `--no-mesh` for a server you do not fully trust, or `gst mesh disable` on this machine.
- `gst mesh disable` takes this machine out of the mesh: it closes every link, stops joining by itself, and stops the daemon when no tunnel is started. `gst mesh enable` joins again with a new node id.

### Connecting through the mesh

A tunnel whose destination is `mesh:<machine>` reaches a machine of `gst mesh nodes` through the machines that link to it, without an SSH login of its own: the gst on the last machine before it connects, using its own tunnel when the machine has no gst. Targets such as `localhost` are that machine's. `gst connect` does the same for a single connection on standard input and output.

```bash
# On mini: port 15432 reaches port 5432 of devcn, through laptop
gst add devcn-db mesh:devcn -L 15432:localhost:5432
gst check devcn-db          # shows the route: mini → laptop → devcn

# A SOCKS5 proxy whose connections leave from devcn
gst add devcn-proxy mesh:devcn -D 1080

# ssh to devcn through the mesh, with your own login to devcn
ssh -o ProxyCommand='gst connect %n 22' devcn
```

- `<machine>` is a name from `gst mesh nodes`, or its id when names repeat. A bare port in `gst connect` means localhost there; `host:port` and absolute Unix socket paths work too.
- These tunnels have only `-L` and `-D` forwards and, of the SSH settings, only `--connect-timeout`. They run as long as they are started, whether or not the mesh currently has a route; each connection takes the shortest route at that moment and fails on its own when there is none. `gst check` shows the route.
- A route goes on past a machine only over a peer link that is up. Its last step can be any running tunnel without `--no-mesh`, also to a server without gst.
- Every gst in the mesh can connect anywhere its machine and its tunnels reach, on behalf of the others; join only machines you trust with that.

### Web page

`gst ui` shows this machine's tunnels, its peer links, and a map of the mesh in a browser page that updates itself every few seconds. It serves the page on `127.0.0.1` until you press Ctrl-C, prints the address, and opens it in your browser. Its buttons start, stop, and restart this machine's tunnels, as `gst start`, `stop`, and `restart` do; other changes still take the commands above.

```bash
gst ui
# On a server without a browser: pick a port and forward it
gst ui --port 8370 --no-open
ssh -L 8370:localhost:8370 myhost   # then open the printed address on this machine
```

- The address ends with a random secret after `#`, which the page needs; anyone with the full address can use the page while `gst ui` runs. Each run makes a new secret.
- The map places machines by their distance from this one. Blue arrows are tunnels with a peer link up, grey ones running tunnels, and dashed red ones tunnels that are not running; arrows point from the machine that runs the tunnel to its server. Machines this one cannot reach are faded.

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
- `list`, `show`, `check`, `forward list`, `mesh peers`, and `mesh nodes` accept `--json`.
- No command prompts for input. Adding a forward that already exists changes nothing; a different forward on the same listen address is an error. `-L` and `-D` forwards both listen on this machine, so they cannot share an address. Fixed-target and dynamic (SOCKS) `-R` forwards both listen on the server and cannot share a listen address either.

## Configuration file

Tunnels are stored in `~/.config/gst/config.json` (`$XDG_CONFIG_HOME/gst/config.json` when `XDG_CONFIG_HOME` is set). `--config` or the `GST_CONFIG` environment variable selects another file, and the daemon keeps using it. There is one daemon per user, so starting or changing tunnels in another file switches the daemon to that file. gst maintains this file; change it with gst commands rather than by hand. Running several gst commands at once is safe. Reverse SOCKS needs no configuration schema change: it is stored as a `remote` forward with an empty target.
