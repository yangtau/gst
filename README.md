# gst

English | [简体中文](README.zh-CN.md)

`gst` (go ssh tunnel) links your machines into a private mesh over SSH. It feels like Tailscale, but works at layer 4: it carries TCP connections and Unix sockets only, needs no root, and uses the SSH logins you already have. It runs on Linux and macOS.

- Every machine runs gst (0.3 or later, on both ends of a link) and a small background daemon.
- A **link** is an SSH connection from one machine to another that also runs gst. Linked machines tell each other what they know, so every machine can reach every other one through the links between them: if `laptop` links to `mini` and `devcn`, then `mini` reaches `devcn` through `laptop`, without any credentials for `devcn`.
- You reach a machine with `gst connect`, through a SOCKS5/HTTP proxy where `<machine>.gst` is that machine, or through a local port or Unix socket that leads to it (a map).

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

The Nix package includes completion scripts for Bash, Zsh, and Fish; `gst completion bash|zsh|fish` generates them otherwise.

Run `gst version` to print the installed version, for example `gst version 0.3.0`.

Install gst on every machine of the mesh, and make sure it is in the `PATH` of the login shell that ssh starts there (see `--remote-gst` if it is not). The background daemon remembers the path of gst. After upgrading or moving gst, run `gst up` to restart the daemon with the new gst; any command that changes the configuration does so too.

## Before you start

gst runs in the background, where it cannot type a passphrase or confirm an unknown host key. For each machine you link to, you need:

1. **A private key without a passphrase**, authorized on that machine. A dedicated key is best:

   ```bash
   ssh-keygen -t ed25519 -N '' -f ~/.ssh/gst_ed25519
   ssh-copy-id -i ~/.ssh/gst_ed25519 user@devcn.example.com
   ```

2. **A Host in `~/.ssh/config`**, for example:

   ```
   Host devcn
     HostName ssh.example.com
     User user
     IdentityFile ~/.ssh/gst_ed25519
   ```

   Or skip the entry and give the destination to `gst add`, such as `user@ssh.example.com`. All SSH settings (address, user, key, known_hosts, timeouts) come from `~/.ssh/config`; see [ssh configuration](#ssh-configuration).

3. **The machine's host key in known_hosts**: connect once with `ssh devcn`, then verify and accept the host key.

4. **gst 0.3 or later on that machine.** It needs no links, configuration, or running daemon of its own: being linked to is enough, and gst starts there on the first link.

Only the machine that creates a link needs these; the machine that is linked to just needs gst and an sshd that runs commands (gst uses a login session, not port forwarding, so sshd's `AllowTcpForwarding` does not matter). A server that forces a fixed command on every session cannot be linked.

## Quick start

On `laptop`, link to `devcn` and `mini`:

```bash
gst add devcn             # the "devcn" entry of ~/.ssh/config
gst add mini
gst status                # every machine in the mesh, one per line
gst ping devcn            # check that devcn answers through the mesh
gst status devcn          # devcn's own links, proxy, maps, and routes
```

`gst status` prints something like (the first machine is this one, shown as `-`):

```
4f1c9a27  laptop  gst 0.3.0  -
9db2b03b  devcn   gst 0.3.0  online
c07e5d11  mini    gst 0.3.0  online
```

Links connect in the background; machines show `offline` until they do. Below the list, a `# Problems:` section names this machine's links that are not running and its failed proxy or maps. `gst ping devcn` prints `pong from devcn (9db2b03b) directly in 12.3 ms`.

Reach a machine in any of these ways:

```bash
# One connection on standard input and output: a bare port means localhost on devcn
gst connect devcn 22

# The proxy is off until you turn it on: SOCKS5 and HTTP, where devcn.gst is devcn
gst proxy 127.0.0.1:1080  # turns it on and prints how to use it
export ALL_PROXY=socks5h://127.0.0.1:1080 http_proxy=http://127.0.0.1:1080 https_proxy=http://127.0.0.1:1080
curl http://devcn.gst:8080/   # port 8080 on devcn

# A map: local port 15432 leads to devcn's port 5432
gst map add 15432 devcn:5432
psql -h 127.0.0.1 -p 15432

# Unix sockets work too, on either side
gst map add 18082 devcn:/run/user/1000/app.sock
gst map add /tmp/devcn-docker.sock devcn:/var/run/docker.sock
```

Machine `mini` now sees the same mesh: on `mini`, `gst status` lists `laptop` and `devcn`, and `gst connect devcn 22` goes through `laptop`.

Every command explains itself with `--help`. In a terminal, help uses bold headings and cyan command and option names. Piped or redirected help is plain text; set `NO_COLOR=1` or `TERM=dumb` to disable styling in a terminal too.

## Concepts

| Concept | What it is | Commands |
| --- | --- | --- |
| **node** | A machine that runs gst. It has a random 128-bit id and a name (default: the host name) | `gst name`, `gst status` |
| **link** | An SSH connection from this machine to another gst machine, kept up in the background; the two daemons join each other's mesh over it | `gst add`, `gst remove` |
| **proxy** | A SOCKS5 and HTTP proxy served by this machine's daemon: `<machine>.gst` and the hosts that routes match go through the mesh | `gst proxy` |
| **map** | A local port or Unix socket whose connections go to a port or socket of a machine; for programs that cannot use a proxy | `gst map …` |
| **route** | A rule that sends the proxy's connections to some hosts out through another machine | `gst route …` |
| **exit** | Allows other machines to connect through this one to addresses beyond this machine | `gst exit` |

### Nodes and links

- A node's name is for display and addressing, and need not be unique; its id is unique. Change the name with `gst name laptop`. Names, like link names, are 1-32 lowercase letters, digits, or `-`.
- A link is bidirectional: traffic can pass through it either way, so `devcn` needs no link of its own to reach `laptop`. A link has a name (`gst add devcn`), which also names the machine at the other end until it reports its own name.
- A started link runs in the background, starts at login, and reconnects after failures. Linked machines pass on what they know, so `gst status` on any machine shows the whole mesh; a machine that drops out disappears within 30 minutes. A route goes on past a machine only over running links.

### Machine addressing

Every command and the proxy name a machine with `<machine>`. It is resolved in this order, and a step applies only if it matches exactly one machine:

1. Remove a trailing `.gst`.
2. A node id.
3. A node name.
4. A link name, which means the machine at the other end of that link. Links of any machine in the mesh count, so `devcn` works everywhere, whether it is a link name on `laptop` or the name of `devcn` itself.
5. An id prefix of at least 4 characters.

When names repeat, `gst status` tells the machines apart by id; use the id or an id prefix.

### Proxy

The daemon serves SOCKS5 and HTTP on one port. The proxy is off by default; `gst proxy <address>` turns it on. Each connection goes:

1. To `<machine>.gst` and port `p`: to `localhost:p` on that machine.
2. If a route matches the host: through that route's machine, which resolves domain names itself and must be an exit.
3. Otherwise: directly from this machine.

```bash
gst proxy 127.0.0.1:1080  # turn it on, or move it (a TCP address, port, or bind:port)
gst proxy                 # show it, with the export line for ALL_PROXY, http_proxy, and https_proxy
gst proxy off             # turn it off
```

- SOCKS5 supports `CONNECT` without authentication; names are passed on unresolved, as with `socks5h://`. UDP, `BIND`, and SOCKS4 are not supported, so UDP DNS queries and QUIC cannot go through it.
- HTTP supports `CONNECT host:port` and plain `GET http://host/...` requests.
- Anyone who can reach the proxy address can use it. Keep a loopback address unless a firewall limits access.
- For programs that ignore proxies, use a map, or a tool such as tun2socks to route their TCP traffic into the proxy; gst creates no TUN devices or routes.

### Map

```bash
gst map add <listen> <machine>:<target>
```

- `<listen>` is a port (listens on `127.0.0.1`), `address:port` (`0.0.0.0:8080` or `*:8080` listen on all interfaces; put IPv6 addresses in brackets), or a Unix socket path, which may be relative or start with `~` and is saved as an absolute path.
- `<target>` is a port (`localhost` on that machine), `host:port`, or an absolute Unix socket path there. A path cannot contain `:`.
- Sockets that gst creates are accessible to you only (mode `0600`). Before listening, gst replaces a socket file that nothing accepts on, as left behind by a crash; it never replaces other files or sockets in use.
- A map keeps listening whether or not the machine is reachable; each connection fails on its own when there is no route. `gst map list` shows whether each listener works.

### Route

```bash
gst route add corp.example.com devcn   # the domain and its subdomains
gst route add 10.20.0.0/16 devcn       # an IP prefix (or a single IP)
gst route add default devsg           # everything else
```

- Routes apply to the proxy's connections. The most specific route wins: longer domains and longer prefixes first, `default` last. Adding a route with the same match replaces it.
- Domain routes match by name. IP and prefix routes match IP addresses only: gst does not resolve a name to pick a route, so `curl http://intranet.example.com/` does not match `10.20.0.0/16`, even if the name resolves into it.
- The `via` machine must be an exit. `gst route add` prints a note when the mesh shows that it is not.
- Hosts that no route matches are connected to from this machine.

### Exit

By default, a machine connects, for the others, only to addresses on itself: `localhost`, loopback addresses, and Unix sockets. Only an exit also connects to other addresses, such as its local network or the internet. Turn that on with `gst exit on` on the machine that should be the exit (`gst exit off` undoes it, and `gst exit` shows it). The machine that makes the final connection checks this, whatever the route.

### Trust

Machines in the mesh trust each other. A link logs in as your user, and the daemon accepts it only through its own socket. Any machine in the mesh can ask any machine to connect to that machine's own `localhost` ports and Unix sockets (a local database, a Docker socket, an ssh-agent). Exits go further: any machine can connect anywhere an exit can. Link only machines you trust, and turn exit on only where you want it. `gst down` takes this machine out of the mesh.

## Replacing ssh forwards

There are no `-L`, `-R`, or `-D` forwards; the mesh covers the same cases, and either machine can be the one that listens.

| With ssh | With gst |
| --- | --- |
| `ssh -L 15432:127.0.0.1:5432 devcn` | `gst map add 15432 devcn:5432` |
| `ssh -L 15432:db.example.com:5432 devcn` | `gst map add 15432 devcn:db.example.com:5432`, and `gst exit on` on devcn |
| `ssh -L ~/db.sock:/run/postgresql/.s.PGSQL.5432 devcn` | `gst map add ~/db.sock devcn:/run/postgresql/.s.PGSQL.5432` |
| `ssh -R 2222:localhost:22 devcn` | On devcn: `gst map add 2222 laptop:22` |
| `ssh -R /home/user/agent.sock:$SSH_AUTH_SOCK devcn` | On devcn: `gst map add /home/user/agent.sock laptop:"$SSH_AUTH_SOCK"` (an absolute path) |
| `ssh -D 1080 devcn` | `gst route add default devcn` (or just some domains or prefixes), and `gst exit on` on devcn; turn the proxy on with `gst proxy 127.0.0.1:1080` |
| A reverse proxy: devcn's programs use laptop's network | On devcn: `gst route add default laptop`, and `gst exit on` on laptop |

A `-R`-style map on `devcn` works because every machine can reach every other one through the mesh, so `laptop` needs no listener or open port of its own. No stale `-R` ports or socket files remain on the server after a crash, and `GatewayPorts` does not apply.

## Commands

Commands follow the groups of `gst --help`. Names of machines and links are described in [Concepts](#concepts).

### Build the mesh

| Command | Purpose |
| --- | --- |
| `gst add <name> [destination] [--remote-gst cmd]` | Add a link and start it in the background |
| `gst remove <name>...` (`rm`) | Stop links and delete them |
| `gst status` | List every machine of the mesh as this machine knows it: short id, name, gst version, and state; then this machine's problems |
| `gst status --all` | Also list the running links between all the machines |
| `gst status <machine>` | Show a machine's links (with state, destination, and peer), proxy, maps, and routes, asked through the mesh |
| `gst ping <machine> [-c N]` | Ask a machine to answer through the mesh once a second (3 times by default), and print the path and time of each answer |

`destination` is `host`, `user@host`, or `ssh://user@host:port`, and defaults to the link name. `--remote-gst <command>` names the gst command on the machine, when gst is not in the `PATH` of its login shell, such as `~/.nix-profile/bin/gst`. Everything else about the SSH connection comes from your ssh configuration (see [ssh configuration](#ssh-configuration)). To change a link, remove it and add it again. `gst add` checks first that a usable key without a passphrase exists and that known_hosts records the server, and saves nothing if not.

`gst status` prints one line per machine, this machine first:

| State | Meaning |
| --- | --- |
| `-` | This machine |
| `online` | A running link leads to it directly |
| `online; via devcn` | Connections to it pass through the machines named |
| `offline` | No running links lead to it |

`; exit` follows the state of an exit. A `# Problems:` section lists this machine's links that are not running and its proxy or maps that failed. A link is `running` (the two daemons are linked), `connecting`, or `down` (failed and retrying; the last error is shown, including the first line of the other gst's error). A failed link retries after 5 seconds, then 10, doubling up to every 5 minutes, and starts over once it runs.

`gst ping` prints, for example, `pong from devsg (1a2b3c4d) via devcn in 41.3 ms`. The time includes setting up the connection through each machine on the way. It exits with status 1 if no answer came.

### Reach machines

| Command | Purpose |
| --- | --- |
| `gst connect <machine> <port \| host:port \| socket>` | Connect standard input and output to a port or socket of a machine, such as for ssh's `ProxyCommand` |
| `gst proxy [address \| off]` | Show the proxy, turn it on at an address, or turn it off (it is off by default) |
| `gst map add <listen> <machine>:<target>` | Map a local port or socket to a machine |
| `gst map remove <listen>...` (`rm`) | Remove maps |
| `gst map list` (`ls`) | List maps and whether they listen |
| `gst route add <match> <machine>` | Send connections to matching hosts through a machine |
| `gst route remove <match>...` (`rm`) | Remove routes |
| `gst route list` (`ls`) | List routes |

`<match>` is `default`, a domain, an IP address, or a CIDR prefix. A `host:port` that is not the machine's own needs the machine to be an exit.

### This machine

| Command | Purpose |
| --- | --- |
| `gst name [name]` | Show or change this machine's name and show its id |
| `gst exit [on \| off]` | Show or change whether this machine is an exit |
| `gst up` | Run the daemon now and at login |
| `gst down` | Stop the daemon and keep it from starting; other machines cannot link to this one |
| `gst ui [--port N] [--no-open]` | Show this machine's status, the mesh, and any machine's links, proxy, maps, and routes, in a browser |
| `gst version` | Show the gst version |

Every command accepts `--config <file>`. `status`, `ping`, `proxy`, `map list`, and `route list` accept `--json`; `gst status --json` prints `{"self": <this machine's status>, "mesh": <the machines and links>}`.

## ssh configuration

A link has no SSH settings of its own. Every time it connects, gst resolves its destination again with `ssh -G`, so it follows `~/.ssh/config`. After editing the file, run `gst remove <name>` and `gst add <name>` again to reconnect now; otherwise the change applies the next time the link connects.

| ssh setting | Used for | Default when unset |
| --- | --- | --- |
| `HostName`, `Port`, `User` | Address and login user | ssh's defaults |
| `IdentityFile` | Private keys, tried in order; missing or passphrase-protected ones are skipped | ssh's default key list |
| `UserKnownHostsFile`, `GlobalKnownHostsFile` | Host key verification | ssh's default files |
| `ConnectTimeout` | Connect and login timeout | 15 seconds |
| `ServerAliveInterval` | Keepalive interval | 15 seconds |
| `ServerAliveCountMax` | Unanswered intervals before reconnecting | 3 |

Limitations:

- Only private keys without a passphrase work; ssh-agent is not used.
- known_hosts is checked strictly; unknown hosts are never accepted.
- `ProxyJump`, `ProxyCommand`, and `HostKeyAlias` are not supported for links. To reach a machine behind another, run gst on both and link through the one in front.

## Running in the background

- One background process per user, the gst daemon, runs every started link, the proxy, and the maps, and accepts links from other machines. It is a user service: a systemd user service `gst.service` on Linux, a launchd agent `local.gst` on macOS. gst installs it with the first change to its configuration, or when another machine first links to this one; you do not manage it yourself. `gst down` stops it and removes the service, and `gst up` brings it back.
- gst commands talk to the daemon over a socket that only you can access: `~/.local/state/gst/gst.sock` on Linux (under `$XDG_STATE_HOME` when set), `~/Library/Application Support/gst/gst.sock` on macOS.
- If the daemon itself exits, its service restarts it after 5 seconds.
- The daemon's log, which also holds link events (connected, failed, reconnecting), goes to journald on Linux (`journalctl --user -u gst`) and to `~/Library/Logs/gst-daemon.log` on macOS.
- For privacy, logs record no server addresses, users, key paths, or target addresses, and not the errors that another gst reports. Those appear in interactive output only: `gst status`, `gst ping`, and `gst ui`. To see why a link fails in full, run `ssh -v <destination>`.
- On Linux, to keep links running after you log out and start them at boot, run `loginctl enable-linger "$USER"`; gst tells you when lingering is off. On macOS, gst starts at login and stops at logout.
- The service runs `gst daemon`, and links run `gst peer-bridge` on the other machine over SSH; neither is a command you run yourself.

### Web page

`gst ui` shows the mesh in a browser page that updates itself every few seconds: every machine with its state and problems, a map of the links between them, and, for the machine you pick, its links, the links from other machines, its proxy, maps, and routes (as `gst status <machine>` does). Press J and K to move between machines. It says so when the daemon on this machine is not running. It serves the page on `127.0.0.1` until you press Ctrl-C, prints the address, and opens it in your browser. The page only shows; changes take the commands above.

```bash
gst ui
# On a machine without a browser: pick a port and forward it
gst ui --port 8370 --no-open
ssh -L 8370:localhost:8370 devcn   # then open the printed address on your own machine
```

The address ends with a random secret after `#`, which the page needs; anyone with the full address can use the page while `gst ui` runs. Each run makes a new secret.

## Upgrading from 0.2

gst 0.3 replaces tunnels and forwards with the mesh model above. Its configuration converts itself: the first gst command that reads a 0.2 configuration rewrites it, and nothing needs doing.

- Each tunnel becomes a link of the same name; a tunnel to `mesh:<machine>` has no link of its own.
- Each `-L` forward becomes a map to the tunnel's machine (or to `<machine>` for a `mesh:` tunnel). If its target is not on that machine, run `gst exit on` there.
- `-R` and `-D` forwards are dropped. Each one is listed on stderr, such as `note: dropped tunnel myhost: -R 2222:localhost:22; recreate it with gst map on the other machine, or with gst route`. Recreate a `-R` as a map on the other machine, and a `-D` as the proxy with a route, as in [Replacing ssh forwards](#replacing-ssh-forwards).
- The other machines need gst 0.3 as well. The gst protocol is now version 2, and gst refuses to talk to an older one: it reports `speaks an older gst protocol; upgrade gst there to 0.3 or later`. Upgrade every machine you link to.
- `gst mesh …`, `gst list`, and the other 0.2 commands are gone, and `gst add` now adds a link; see [Commands](#commands). `gst up` replaces `gst restart --all` for restarting the daemon after an upgrade.

## Troubleshooting

Work from the outside in: `gst status` shows whether the machine is `online` and lists this machine's links that are not running, each with its last error; `gst ping <machine>` checks that the path to it works; the daemon log has the link events (`journalctl --user -u gst` on Linux, `~/Library/Logs/gst-daemon.log` on macOS); and `ssh -v <destination>` shows what ssh itself says about the login. `gst add` also checks the key and known_hosts first, and saves nothing if the check fails.

| Symptom | Fix |
| --- | --- |
| `no usable key`, or a key is `passphrase-protected` | Create a key without a passphrase as in [Before you start](#before-you-start), and name it with `IdentityFile` in `~/.ssh/config` |
| `no host key for ...` | Connect once with `ssh <host>` to record the host key |
| Host key mismatch | Once you have confirmed the machine's key really changed, run `ssh-keygen -R <host>` and connect once with `ssh` |
| A link is `down` with an error from the other gst, such as command not found | gst is not in the `PATH` of the login shell that ssh starts there. Install it, or give its path: `gst remove devcn`, then `gst add devcn --remote-gst ~/.nix-profile/bin/gst` |
| A link is `down` with `gst is down on this machine; run gst up there` | Run `gst up` on the other machine |
| `speaks an older gst protocol; upgrade gst there to 0.3 or later` | Upgrade gst on the machine it names (0.3 on both ends of every link) |
| `... is not an exit, so it connects only to addresses on its own machine` | The target is not on the machine you are connecting through. Run `gst exit on` there, or aim at its own `localhost` |
| `no route to ...: no running links lead there` | No running links connect this machine to that one. Check `gst status` (a machine that is `offline` or missing, and links that are not running) |
| `cannot listen on ...` for the proxy or a map | Another program uses the address: choose another with `gst proxy <address>` or `gst map`, or free it, then run `gst up` |
| `gst status <machine>` or `gst ping <machine>` fails | The machine must be reachable and run gst 0.3 or later with a running daemon; see `gst status` for the way to it |
| `the gst daemon does not answer` | Run `gst up`; see the daemon log (`journalctl --user -u gst`, or `~/Library/Logs/gst-daemon.log`) |
| A machine name matches several machines | Use its id or an id prefix from `gst status` |
| macOS blocks access to a local network server | Allow gst in the Local Network alert or in System Settings → Privacy & Security → Local Network, then remove and add the link again. Authorization for your terminal does not necessarily authorize the background service |

## Scripts and agents

- Results go to stdout. Errors go to stderr, start with `error:`, and usually come with a `hint:` line naming the next command to run. Notes that are not errors start with `note:`.
- Exit status: `0` success, `1` the work failed (including a `gst ping` with no answer), `2` invalid arguments.
- `--json` fields are stable on `status`, `ping`, `proxy`, `map list`, and `route list`.
- No command prompts for input. Adding a route with an existing match replaces it; adding a map for an address that is already mapped is an error. The proxy and the maps cannot share a listen address.

## Configuration file

The configuration is stored in `~/.config/gst/config.json` (`$XDG_CONFIG_HOME/gst/config.json` when `XDG_CONFIG_HOME` is set). It holds this machine's node (id, name, exit), the proxy address, links, maps, and routes. `--config` or the `GST_CONFIG` environment variable selects another file, and the daemon keeps using it. There is one daemon per user, so changing another file switches the daemon to that file. gst maintains this file; change it with gst commands rather than by hand. Running several gst commands at once is safe.
