# gst

English | [简体中文](README.zh-CN.md)

`gst` (go ssh tunnel) links your machines into a private network over SSH. It feels like Tailscale, and uses the SSH logins you already have: every machine gets an IP address and the name `<machine>.gst` on a WireGuard network, for any program and protocol. Packets go directly over UDP when the network allows it, and through the SSH links otherwise. It runs on Linux and macOS, and needs root once on each machine.

- Every machine runs gst (0.4 or later, on all machines) and a small background daemon.
- A **link** is an SSH connection from one machine to another that also runs gst. Linked machines tell each other what they know, so every machine can reach every other one through the links between them: if `laptop` links to `homelab` and `devbox`, then `homelab` reaches `devbox` through `laptop`, without any credentials for `devbox`.
- The links only find the machines and carry what they say to each other. Everything your programs send is WireGuard, end to end, over one of two ways: a UDP path between the two machines, found through the links, or the SSH links when no such path exists.

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

Run `gst version` to print the installed version, for example `gst version 0.4.0`.

Install gst on every machine of the mesh, and make sure it is in the `PATH` of the login shell that ssh starts there (see `--remote-gst` if it is not). The background daemon remembers the path of gst. After upgrading or moving gst, run `gst up` to restart the daemon with the new gst; any command that changes the configuration does so too.

## Before you start

gst runs in the background, where it cannot type a passphrase or confirm an unknown host key. For each machine you link to, you need:

1. **A private key without a passphrase**, authorized on that machine. A dedicated key is best:

   ```bash
   ssh-keygen -t ed25519 -N '' -f ~/.ssh/gst_ed25519
   ssh-copy-id -i ~/.ssh/gst_ed25519 user@devbox.example.com
   ```

2. **A Host in `~/.ssh/config`**, for example:

   ```
   Host devbox
     HostName ssh.example.com
     User user
     IdentityFile ~/.ssh/gst_ed25519
   ```

   Or skip the entry and give the destination to `gst add`, such as `user@ssh.example.com`. All SSH settings (address, user, key, known_hosts, timeouts) come from `~/.ssh/config`; see [ssh configuration](#ssh-configuration).

3. **The machine's host key in known_hosts**: connect once with `ssh devbox`, then verify and accept the host key.

4. **gst 0.4 or later on that machine, and `gst up` run there once.** It needs no links of its own. gst never sets up another machine: if `gst up` has not run there, `gst add` says so and gives the command, and the link connects by itself once it has.

Only the machine that creates a link needs these; the machine that is linked to just needs gst, `gst up`, and an sshd that runs commands (gst uses a login session, not port forwarding, so sshd's `AllowTcpForwarding` does not matter). A server that forces a fixed command on every session cannot be linked.

## Quick start

Run `gst up` once on each machine: `laptop`, `devbox`, and `homelab`. Then, on `laptop`, link to the other two:

```bash
gst up                     # once on each machine
gst add devbox             # the "devbox" entry of ~/.ssh/config
gst add homelab
gst status                 # the machines you can reach, one per line
```

`gst status` prints something like (the first machine is this one, shown as `-`):

```
4f1c9a27  laptop   gst 0.4.0  10.87.26.1   -
3f2a9c10  devbox   gst 0.4.0  10.87.61.7   online; direct
c07e5d11  homelab  gst 0.4.0  10.87.140.3  online
```

Then any program reaches any machine, with any protocol:

```bash
ping devbox.gst
ssh devbox.gst
psql -h devbox.gst
curl http://homelab.gst:8080/
```

Links connect in the background, and machines appear in the list when they do. Below the list, a `# Problems:` section names this machine's links that are not running, and what keeps it off the network. `gst ping devbox` checks the network and prints `reply from devbox (10.87.61.7) via 203.0.113.7:41642 in 12.3 ms`.

Machine `homelab` now sees the same mesh: on `homelab`, `gst status` lists `laptop` and `devbox`, and `ping devbox.gst` works without a link to `devbox`.

Every command explains itself with `--help`. In a terminal, help uses bold headings and cyan command and option names. Piped or redirected help is plain text; set `NO_COLOR=1` or `TERM=dumb` to disable styling in a terminal too.

## Concepts

| Concept | What it is | Commands |
| --- | --- | --- |
| **node** | A machine that runs gst. It has a random 128-bit id and a name (default: the host name) | `gst name`, `gst status` |
| **link** | An SSH connection from this machine to another gst machine, kept up in the background; the two daemons join each other's mesh over it | `gst add`, `gst remove` |
| **virtual network** | A private subnet where each machine has an IP address and the name `<machine>.gst`, for every program and protocol | `gst up`, `gst subnet` |

### Nodes and links

- A node's name is for display and addressing, and need not be unique; its id is unique. Change the name with `gst name laptop`. Names, like link names, are 1-32 lowercase letters, digits, or `-`.
- A link is bidirectional: traffic can pass through it either way, so `devbox` needs no link of its own to reach `laptop`. A link has a name (`gst add devbox`), which also names the machine at the other end until it reports its own name.
- A started link runs in the background, starts at login, and reconnects after failures. Linked machines pass on what they know, so `gst status` on any machine shows the whole mesh; a machine that nothing leads to any more leaves the list at once. A path goes on past a machine only over running links.

### Machine addressing

Every command names a machine with `<machine>`, and `<machine>.gst` resolves the same way. It is resolved in this order, and a step applies only if it matches exactly one machine:

1. Remove a trailing `.gst`.
2. A node id.
3. A node name.
4. A link name, which means the machine at the other end of that link. Links of any machine in the mesh count, so `devbox` works everywhere, whether it is a link name on `laptop` or the name of `devbox` itself.
5. An id prefix of at least 4 characters.

When names repeat, `gst status` tells the machines apart by id; use the id or an id prefix.

### Virtual network

The virtual network gives every machine an IP address in one private subnet and the name `<machine>.gst`, so that any program can reach it with any protocol: TCP, UDP, and ping, such as a game stream or a screen sharing session. Programs need no setup.

- **Root.** Creating an interface and a route takes root, and the daemon runs as your user, so a small system service, the helper, does only that for your daemon, with a copy of gst that root owns. When the helper is missing or out of date, `gst up` shows the `sudo` command it would run and what that changes on this machine, and asks first, because `sudo` may not ask for a password. `sudo` then asks for your password itself; gst never sees it. Without a terminal `gst up` does not ask and runs nothing; `gst up --yes` goes ahead without asking and still shows the command. Running `gst up` on a machine that is up changes nothing, and after upgrading gst it needs root again only if the helper changed. The machine joins by itself; until then, `gst status` says what is missing.
- **Services.** Other machines reach the ports that a program opens on all addresses or on this machine's `.gst` address. A program that listens only on `127.0.0.1`, or on a Unix socket, is not reachable from the network; change where it listens, or log in with `ssh <machine>.gst` and use it there.
- **Subnet.** The machines share one subnet. The first machine chooses a `/16` that overlaps nothing on the machines that answer, from their interfaces and routes. `gst subnet 10.99.0.0/16` sets another for the whole network: the latest choice wins, and all machines move to it, changing their addresses. It must be private (inside `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, or `100.64.0.0/10`). If the subnet overlaps something on a machine, that machine reports it in `gst status`.
- **Addresses.** A machine keeps its address across restarts. Only IPv4 is carried.
- **Names.** `<machine>.gst` resolves for all programs, by the machine's name, the name of a link to it, or an id prefix. On macOS and with systemd-resolved, only `.gst` goes to the daemon. Elsewhere on Linux, the names go in `/etc/hosts` for as long as the network runs.
- **Paths.** WireGuard encrypts each packet from end to end, so that machines on the way see only ciphertext. Packets start through the SSH links, and that traffic makes the two machines look for a UDP path, over the links: nothing needs configuring, and no outside server is involved. It works when UDP gets through and an address of one machine, such as a public IPv6 address or one on a shared network, can be reached from the other; two machines behind NAT with no such address may stay on SSH, though each learns its public address from any machine it has a UDP path to, which often gets them through. All paths share one UDP port, 15355 unless it is taken; packets follow the system's routes, through another VPN if that is where they lead. gst keeps measuring the addresses of a machine and moves to a clearly faster one without interrupting anything. `gst status` shows `direct` next to the machine, and `gst status --all` shows the way of each. If UDP stops working, packets move to SSH within seconds, and programs keep their connections. A UDP path does not need the links once it runs, so it carries on when SSH drops or a machine between the two sleeps, for 20 to 30 minutes; after that the machine counts as gone and the path ends. `gst remove` ends it at once if no links then lead to the machine. Through SSH, TCP runs inside TCP, which works but slows down on lossy networks.
- **MTU.** The interface has an MTU of 1168 bytes, which fits every UDP path. Programs that choose a packet size themselves should stay below it, or their packets are fragmented.
- `gst down` undoes `gst up`: it removes the daemon's service, the interface, the route, the names, and the helper, which removes itself, so it takes no password, and `gst up` asks for one again. Only the configuration and logs stay, with the machine's address and links. If the daemon just stops, the interface, route, and names go too, and the helper stays.

### Trust

Machines in the mesh trust each other. A link logs in as your user, and the daemon accepts it only through its own socket. Every port that a machine opens on all its addresses is open to the other machines of the network, and the machines learn each other's WireGuard keys through the links, so they trust what the links trust. Link only machines you trust. `gst down` takes this machine out of the mesh.

## Commands

Commands follow the groups of `gst --help`. Names of machines and links are described in [Concepts](#concepts).

### Build the mesh

| Command | Purpose |
| --- | --- |
| `gst add <name> [destination] [--remote-gst cmd]` | Add a link and start it in the background |
| `gst remove <name>...` (`rm`) | Stop links and delete them |
| `gst status` | List every machine of the mesh as this machine knows it: short id, name, gst version, address, and state; then this machine's problems |
| `gst status --all` | Also list the running links between all the machines, and how packets go to each machine (`ssh`, or `direct` and the `ip:port`), with their last handshake and the bytes received and sent |
| `gst status <machine>` | Show a machine's address and links (with state and destination), asked through the mesh |
| `gst ping <machine> [-c N] [-v]` | Send a packet to a machine and back over the virtual network once a second (3 times by default), and print the way it went and the time; `-v` also shows the UDP path to it, or why there is none, and what both machines did in their latest try at one |

`destination` is `host`, `user@host`, or `ssh://user@host:port`, and defaults to the link name. `--remote-gst <command>` names the gst command on the machine, when gst is not in the `PATH` of its login shell, such as `~/.nix-profile/bin/gst`. Everything else about the SSH connection comes from your ssh configuration (see [ssh configuration](#ssh-configuration)). To change a link, remove it and add it again. `gst add` checks first that a usable key without a passphrase exists and that known_hosts records the server, and saves nothing if not.

`gst status` prints one line per machine, this machine first:

| State | Meaning |
| --- | --- |
| `-` | This machine |
| `online` | A running link leads to it directly |
| `online; via devbox` | The links to it pass through the machines named |
| `online; direct` | Packets to it go directly over UDP |

Links show as the machines at their ends, such as `laptop->gateway`; when a link's name differs from the machine it leads to, the name that `gst remove` takes follows, as in `laptop->prod (db)`. A `# Problems:` section lists this machine's links that are not running and what keeps it off the network. A link is `running` (the two daemons are linked), `connecting`, or `down` (failed and retrying; the last error is shown, including the first line of the other gst's error). A failed link retries after 5 seconds, then 10, doubling up to every 5 minutes, and starts over once it runs.

`gst ping` prints, for example, `reply from devbox (10.87.61.7) via 203.0.113.7:41642 in 12.3 ms`, or `reply from gateway (10.87.2.9) via SSH (devbox) in 41.3 ms`. The packet goes the way of every program's packets, through the interface and WireGuard, so an `ip:port` means that the UDP path to that address works, and the time is about what `ping <machine>.gst` shows. Traffic is what starts a UDP path, so the first replies may come over SSH. `-v` also checks the SSH links, which carry the control of the network: `SSH links: gateway answered via devbox in 41.3 ms`. That time is higher, as it includes setting up the way through each machine. The command exits with status 1 if no reply came.

### Virtual network

| Command | Purpose |
| --- | --- |
| `gst subnet [cidr]` | Show the subnet of the virtual network, or change it for all machines |

### This machine

| Command | Purpose |
| --- | --- |
| `gst name [name]` | Show or change this machine's name and show its id |
| `gst up [--yes]` | Run the daemon now and at login; when the helper is missing or out of date, say what `sudo` will run, ask, and install it |
| `gst down` | Undo `gst up`: stop the daemon, remove its service and the helper; other machines cannot link to this one |
| `gst ui [--port N] [--no-open]` | Show this machine's status, the mesh, and any machine's links, in a browser |
| `gst version` | Show the gst version |

Every command accepts `--config <file>`. `status` and `ping` accept `--json`; `gst status --json` prints `{"self": <this machine's status>, "mesh": <the machines and links>}`.

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

- One background process per user, the gst daemon, runs every started link and the virtual network, and accepts links from other machines. It is a user service: a systemd user service `gst.service` on Linux, a launchd agent `local.gst` on macOS. gst installs it with `gst up` or the first change to its configuration; you do not manage it yourself. Another machine that links to this one never installs it. `gst down` stops it and removes the service, and `gst up` brings it back.
- gst commands talk to the daemon over a socket that only you can access: `~/.local/state/gst/gst.sock` on Linux (under `$XDG_STATE_HOME` when set), `~/Library/Application Support/gst/gst.sock` on macOS.
- If the daemon itself exits, its service restarts it after 5 seconds.
- The daemon's log, which also holds link events (connected, failed, reconnecting), goes to journald on Linux (`journalctl --user -u gst`) and to `~/Library/Logs/gst-daemon.log` on macOS.
- For privacy, logs record no server addresses, users, key paths, or target addresses, and not the errors that another gst reports. Those appear in interactive output only: `gst status`, `gst ping`, and `gst ui`. The addresses of UDP paths stay out of the logs too. To see why a link fails in full, run `ssh -v <destination>`.
- On Linux, to keep links running after you log out and start them at boot, run `loginctl enable-linger "$USER"`; gst tells you when lingering is off. On macOS, gst starts at login and stops at logout.
- The service runs `gst daemon`, and links run `gst peer-bridge` on the other machine over SSH; neither is a command you run yourself.

### Web page

`gst ui` shows the mesh in a browser page that updates itself every few seconds: every machine with its state and problems, a map of the links between them, and, for the machine you pick, its address, its links, and the links from other machines (as `gst status <machine>` does). Press J and K to move between machines. It says so when the daemon on this machine is not running. It serves the page on `127.0.0.1` until you press Ctrl-C, prints the address, and opens it in your browser. The page only shows; changes take the commands above.

```bash
gst ui
# On a machine without a browser: pick a port and forward it
gst ui --port 8370 --no-open
ssh -L 8370:localhost:8370 devbox   # then open the printed address on your own machine
```

The address ends with a random secret after `#`, which the page needs; anyone with the full address can use the page while `gst ui` runs. Each run makes a new secret.

## Upgrading from 0.3

gst 0.4 reaches machines only over the virtual network: `gst connect`, the proxy, maps, routes, and exits are gone, and so are direct links as a way to carry connections. It does not read a 0.3 configuration, and starts over on each machine:

```sh
rm ~/.config/gst/config.json
gst up
gst add devbox               # on the machines that had links
```

- `gst up` replaces the 0.3 daemon. Each machine gets a new identity; `gst name` sets its name again.
- Reach a machine as `devbox.gst` where you had a map or the proxy; see [Services](#virtual-network) for programs that listen only on loopback or a Unix socket.
- Upgrade every machine you link to together. The gst protocol is now version 3, and gst refuses to talk to an older one: it reports `speaks an older gst protocol; upgrade gst there to 0.4 or later`.
- `gst net on`, `gst net off`, and `gst net status` are gone: the network is always on. `gst subnet` replaces `--subnet`, `gst status --all` shows the peers, and `gst up` replaces `sudo gst net install`.

## Troubleshooting

Work from the outside in: `gst status` shows whether the machine can be reached and lists this machine's links that are not running, each with its last error, and what keeps it off the network; `gst ping <machine>` checks that the path to it works, and `gst ping <machine> -v` shows why a UDP path does or does not come up; the daemon log has the link events (`journalctl --user -u gst` on Linux, `~/Library/Logs/gst-daemon.log` on macOS); and `ssh -v <destination>` shows what ssh itself says about the login. `gst add` also checks the key and known_hosts first, and saves nothing if the check fails.

| Symptom | Fix |
| --- | --- |
| `no usable key`, or a key is `passphrase-protected` | Create a key without a passphrase as in [Before you start](#before-you-start), and name it with `IdentityFile` in `~/.ssh/config` |
| `no host key for ...` | Connect once with `ssh <host>` to record the host key |
| Host key mismatch | Once you have confirmed the machine's key really changed, run `ssh-keygen -R <host>` and connect once with `ssh` |
| A link is `down` with an error from the other gst, such as command not found | gst is not in the `PATH` of the login shell that ssh starts there. Install it, or give its path: `gst remove devbox`, then `gst add devbox --remote-gst ~/.nix-profile/bin/gst` |
| A link is `down` with `gst is not up on this machine; run gst up there`, or `gst add` says so | Run `gst up` on the other machine, for example with `ssh -t <destination> gst up`; the link connects by itself |
| `speaks an older gst protocol; upgrade gst there to 0.4 or later` | Upgrade gst on the machine it names (0.4 on both ends of every link) |
| `config version 3 is too old` | Remove the file it names and add the links again ([Upgrading from 0.3](#upgrading-from-03)) |
| `no route to ...: no running links lead there` | No running links connect this machine to that one. Check `gst status` (a machine that is missing, and links that are not running) |
| `gst status <machine>` or `gst ping <machine>` fails | The machine must be reachable and run gst 0.4 or later with a running daemon; see `gst status` for the way to it |
| `the gst daemon does not answer` | Run `gst up`; see the daemon log (`journalctl --user -u gst`, or `~/Library/Logs/gst-daemon.log`) |
| A machine name matches several machines | Use its id or an id prefix from `gst status` |
| `the helper of the virtual network does not run` | Run `gst up`; the machine then joins by itself |
| `the subnet ... overlaps ... on this machine` | Choose another subnet with `gst subnet <cidr>`, on any machine of the network |
| `HANDSHAKE` stays `never` in `gst status --all` | Traffic has to start it: try `ping <machine>.gst`. If it stays, check that `gst status` on the other machine shows no problem |
| A program on a machine does not answer on its `.gst` name | It may listen only on `127.0.0.1` or a Unix socket; see [Services](#virtual-network) |
| macOS blocks access to a local network server | Allow gst in the Local Network alert or in System Settings → Privacy & Security → Local Network, then remove and add the link again. Authorization for your terminal does not necessarily authorize the background service |

## Scripts and agents

- Results go to stdout. Errors go to stderr, start with `error:`, and usually come with a `hint:` line naming the next command to run. Notes that are not errors start with `note:`.
- Exit status: `0` success, `1` the work failed (including a `gst ping` with no reply), `2` invalid arguments.
- `--json` fields are stable on `status` and `ping`.
- No command prompts for input.

## Configuration file

The configuration is stored in `~/.config/gst/config.json` (`$XDG_CONFIG_HOME/gst/config.json` when `XDG_CONFIG_HOME` is set). It holds this machine's node (id and name), its links, and the subnet if you chose one. `--config` or the `GST_CONFIG` environment variable selects another file, and the daemon keeps using it. There is one daemon per user, so changing another file switches the daemon to that file. gst maintains this file; change it with gst commands rather than by hand. Running several gst commands at once is safe.
