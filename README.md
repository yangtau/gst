# gst

English | [简体中文](README.zh-CN.md)

`gst` (go ssh tunnel) links your machines into a private network over SSH. It feels like Tailscale, and uses the SSH logins you already have: every machine gets a gst IP and the name `<machine>.gst`, for any program and protocol. Traffic is WireGuard, end to end. It goes directly over UDP when the network allows it, and is relayed along the SSH links otherwise. It runs on Linux and macOS, and needs root once on each machine.

Three words are all you need:

- A **machine** is a computer that runs gst. It has a name, such as `devbox`, and other machines reach it as `devbox.gst`.
- A **link** is an SSH connection that you make from one machine to another. Linked machines tell each other what they reach, so every machine reaches every other one: if `laptop` links to `homelab` and `devbox`, then `homelab` reaches `devbox` through `laptop`, without any credentials for `devbox`.
- The **network** is all the machines that you added to it with `gst add`. Each one has a **gst IP** in it. A machine stays in the network while it is offline, until `gst remove` takes it out.

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

Run `gst version` to print the installed version:

```
gst 0.5.0
commit: d65d699c4e3f2b1a0987654321fedcba01234567
committed: 2026-11-09T08:12:00Z
protocol: 4
helper: 1
```

That is the release, the commit it was built from (marked `(dirty)` when built with uncommitted changes) and that commit's time, the gst protocol, and the version of the network helper that `gst up` installs. Machines link only when they speak the same protocol. `gst status` shows each machine's release with the short commit, such as `gst 0.5.0+d65d699`.

Install gst on every machine. `gst up` puts a link to it at `~/.local/state/gst/bin/gst`, which the links from other machines run, so gst need not be in the `PATH` of the login shell that ssh starts there. After upgrading or moving gst, run `gst up` to restart it and update that link; any command that changes the configuration restarts it too.

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

   Or skip the entry and give the destination to `gst add`, such as `user@ssh.example.com`, with ssh options after `--`, such as `gst add ssh.example.com -- -p 2222 -i ~/.ssh/gst_ed25519`. All SSH settings (address, user, key, known_hosts, timeouts) come from `~/.ssh/config` and those options; see [ssh configuration](#ssh-configuration).

3. **The machine's host key in known_hosts**: connect once with `ssh devbox`, then verify and accept the host key.

4. **gst of the same protocol on that machine, and `gst up` run there once.** It needs no links of its own. gst never sets up another machine: if `gst up` has not run there, `gst add` says so and gives the command, and the link connects by itself once it has.

Only the machine that makes a link needs these; the machine at the other end just needs gst, `gst up`, and an sshd that runs commands (gst uses a login session, not port forwarding, so sshd's `AllowTcpForwarding` does not matter). A server that forces a fixed command on every session cannot be linked to.

## Quick start

Run `gst up` once on each machine: `laptop`, `devbox`, and `homelab`. Then, on `laptop`, add the other two:

```bash
gst up                     # once on each machine
gst add devbox             # the "devbox" host of ~/.ssh/config
gst add homelab
gst status                 # the machines of the network, one per line
```

`gst status` prints something like this; the first machine is this one, shown as `-`:

```
4f1c9a27  laptop   gst 0.4.0  10.87.26.1   -
3f2a9c10  devbox   gst 0.4.0  10.87.61.7   direct
c07e5d11  homelab  gst 0.4.0  10.87.140.3  relay
```

Then any program reaches any machine, with any protocol:

```bash
ping devbox.gst
ssh devbox.gst
psql -h devbox.gst
curl http://homelab.gst:8080/
```

Links connect in the background, and machines appear in the list when they do. A machine that no link reaches any more stays in the list as `offline, seen 3h ago`. Below the list, a `Problems:` section names this machine's links that are not running, and anything that keeps it off the network. `gst ping devbox` checks the way to a machine and prints `reply from devbox (10.87.61.7): direct 203.0.113.7:41642, 12.3 ms`.

`homelab` now sees the same network: on `homelab`, `gst status` lists `laptop` and `devbox`, and `ping devbox.gst` works without a link to `devbox`.

Every command explains itself with `--help`. In a terminal, help uses bold headings and cyan command and option names. Piped or redirected help is plain text; set `NO_COLOR=1` or `TERM=dumb` to disable styling in a terminal too.

## Concepts

| Word | What it is | Commands |
| --- | --- | --- |
| **machine** | A computer that runs gst. It has a name (default: the host name) and a random id | `gst name`, `gst status` |
| **link** | An SSH connection from this machine to another one, kept up in the background | `gst add`, `gst unlink` |
| **network** | All the machines added to it, each with a gst IP and the name `<machine>.gst` | `gst add`, `gst remove`, `gst subnet` |
| **direct** / **relay** / **offline** | How packets go to a machine: directly over UDP, relayed along the SSH links, or not at all for now | `gst status`, `gst ping` |

### Machines and links

- A machine's name is for people and for `<machine>.gst`, and need not be unique; its id is unique. Change the name with `gst name laptop`. Names are 1-32 lowercase letters, digits, or `-`.
- `gst add devbox` adds the machine at that destination to the network, and links this machine to it. If `devbox` is in another network, the two networks become one. If it is in this network already, this machine links to it too. Adding the destination again replaces its ssh options.
- A link is named by its destination, the ssh destination you gave `gst add`. Traffic passes through a link both ways, so `devbox` needs no link of its own to reach `laptop`.
- A link runs in the background, starts at login, and reconnects after failures. Linked machines tell each other what they know of the network, so `gst status` on any machine shows the whole network.
- A machine stays in the network while it is offline: it keeps its name and gst IP, and comes back by itself when a link reaches it again. `gst status` shows it as `offline, seen 3h ago`.
- A link belongs to the machine that made it: `gst unlink <destination>` on that machine removes it, and changes nothing else. The machine at the other end stays in the network.
- `gst remove <machine>`, on any machine of the network, takes a machine out for all of them: they drop their links to it and forget it, and it drops its links and forgets them as soon as it hears of it. `gst remove` of this machine leaves the network. A removed machine comes back only with `gst add`, on it or on a machine of the network.

### Naming a machine

Every command names a machine with `<machine>`, and `<machine>.gst` resolves the same way. It is resolved in this order, and a step applies only if it matches exactly one machine:

1. Remove a trailing `.gst`.
2. The machine's id.
3. The machine's name.
4. An id prefix of at least 4 characters.

When names repeat, `gst status` tells the machines apart by id; use the id or an id prefix. `gst unlink` takes the destination of a link instead.

### The network

The network gives every machine a gst IP in one private subnet and the name `<machine>.gst`, so that any program can reach it with any protocol: TCP, UDP, and ping, such as a game stream or a screen sharing session. Programs need no setup.

- **Root.** Creating an interface and a route takes root, and gst runs as your user, so a small system service, the **network helper**, does only that for gst, with a copy of gst that root owns. When the network helper is missing or out of date, `gst up` shows the `sudo` command it would run and what that changes on this machine, and asks first, because `sudo` may not ask for a password. `sudo` then asks for your password itself; gst never sees it. Without a terminal `gst up` does not ask and runs nothing; `gst up --yes` goes ahead without asking and still shows the command. Running `gst up` on a machine that is up changes nothing, and after upgrading gst it needs root again only if the network helper changed. The machine joins by itself; until then, `gst status` says what is missing.
- **Services.** Other machines reach the ports that a program opens on all addresses or on this machine's gst IP. Use `gst expose` to make a loopback port, Unix stream socket, or another address reachable by this machine available on its gst IP.
- **Subnet.** The machines share one subnet. The first machine chooses a `/16` that the machines that answer do not use, judging from their interfaces and routes. When two networks become one, the older network's choice stays. `gst subnet 10.99.0.0/16` sets another for the whole network: a person's choice beats gst's, the latest one wins, and all machines move to it, changing their gst IPs, the offline ones when they come back. It must be private (inside `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, or `100.64.0.0/10`). If a machine uses part of the subnet for something else, an interface's network or a route inside it, that machine reports it in `gst status`. A wider route does not count, such as a VPN's routes for all addresses, because the subnet's own route is narrower and wins.
- **gst IPs.** A machine keeps its gst IP across restarts and while it is offline. Only IPv4 is carried.
- **Names.** `<machine>.gst` resolves for all programs, by the machine's name or an id prefix, offline machines included. On macOS and with systemd-resolved, only `.gst` goes to gst. Elsewhere on Linux, the names go in `/etc/hosts` for as long as the network runs.
- **Direct and relay.** WireGuard encrypts each packet from end to end, so that machines on the way see only ciphertext. Packets start out relayed along the SSH links, and that traffic makes the two machines look for a direct path over UDP, through the links: nothing needs configuring, and no outside server is involved. It works when UDP gets through and an address of one machine, such as a public IPv6 address or one on a shared network, can be reached from the other; two machines behind NAT with no such address may stay relayed, though each learns its public address from any machine it has a direct path to, which often gets them through. All direct paths share one UDP port, 15355 unless it is taken; packets follow the system's routes, through another VPN if that is where they lead. On macOS, when a VPN or proxy takes the routes, they go through the network interface below it instead, because a proxy sends them again from another port, even when its rules say direct, and firewalls then drop them; an address that only that VPN reaches then stays relayed. gst keeps measuring the addresses of a machine and moves to a clearly faster one without interrupting anything; an address on a private network, such as the local one, wins when it is about as fast, because those packets stay off the internet. `gst status` shows `direct` or `relay` next to each machine, and `gst status -v` shows the address of each direct path. A direct path that carries nothing for 45 seconds closes, so idle machines send no probes; the next packets are relayed while the machines find a path again, as they did the first time. If UDP stops working, packets move to the relay within seconds, and programs keep their connections. When a machine moves to another network, gst tries again at once, without waiting for earlier failures to wear off, and a running path learns the new addresses. A direct path does not need the links once it runs, so it carries on when SSH drops or a machine between the two sleeps, and closes only when it stops working or one of the two machines is removed from the network. Relayed, TCP runs inside TCP, which works but slows down on lossy networks.
- **MTU.** The interface has an MTU of 1168 bytes, which fits every direct path. Programs that choose a packet size themselves should stay below it, or their packets are fragmented.
- `gst down` undoes `gst up`: it stops gst and removes its service, the interface, the route, the names, and the network helper, which removes itself, so it takes no password, and `gst up` asks for one again. Only the configuration and logs stay, with the machine's gst IP, links, and exposed ports. If gst just stops, the interface, route, and names go too, and the network helper stays.

### Expose a service

Run these on the machine that can reach the target:

```sh
gst expose 8080 --port 5999                  # TCP: gst IP:5999 -> 127.0.0.1:8080
gst expose unix:///tmp/service.sock --port 6000
gst expose 5353 --udp                       # UDP: gst IP:5353 -> 127.0.0.1:5353
gst expose udp://service.example.com:5353 --port 5999
gst expose                                 # list the exposed ports
gst unexpose 5999                          # TCP only
gst unexpose 5999 --udp                    # UDP only
```

If the machine is named `myhost`, connect to `myhost.gst:5999` using the matching protocol. Bare ports mean loopback; explicit TCP targets use `tcp://host:port`. The exposed port defaults to the target port; Unix stream sockets require `--port`. TCP and UDP can share a port. Repeating a rule is harmless; to change its target, unexpose it first.

Rules run in gst and survive terminal exit and restart. They bind **only this machine's gst IP**, never a wildcard or physical interface address. `waiting` means the network is not ready; `failed` reports a bind error, retried automatically; `running` means the listener is open, not that the target is healthy. `stopped` means gst is not reporting the rule. Targets may start later; each new connection tries again. `gst status <machine>` and `gst ui` also show its exposed ports and their state.

Unexposing a port or stopping gst closes its connections. A change of the gst IP closes the old connections and rebinds the rules. `gst down` keeps the rules for the next `gst up`. Exposing a port never asks for root; a port that requires privileges reports an error.

All machines of the network can reach exposed ports. Targets see a connection from gst, not from the original client; Unix socket permissions apply to gst's user. Forwarding preserves bytes and UDP datagrams without HTTP rewriting or TLS termination. WireGuard protects the way between machines; the way to the target uses the target's protocol. UDP mappings expire after two idle minutes and are limited to 256 clients per rule; excess traffic is dropped. Unix datagram sockets, broadcast and multicast forwarding are not supported. Binding a gst IP restricts the destination address, not the interface on which a deliberately routed packet arrives.

### Trust

Machines of the network trust each other. A link logs in as your user, and gst there accepts it only through its own socket. Every port that a machine opens on all its addresses is open to the other machines of the network, and the machines learn each other's WireGuard keys through the links, so they trust what the links trust. Add only machines you trust. Any machine of the network can remove any other with `gst remove`; to keep a removed machine out for good, also take its key out of the other machines' `authorized_keys`, since anyone who can log in to a machine of the network can add a machine to it. `gst down` stops this machine without taking it out of the network.

## Commands

The commands, in the order of `gst --help`. How to name a machine is described in [Concepts](#naming-a-machine).

| Command | Purpose |
| --- | --- |
| `gst up [--yes]` | Start gst now and at login; when the network helper is missing or out of date, say what `sudo` will run, ask, and install it |
| `gst add <destination> [-- <ssh option>...]` | Add the machine at an SSH destination to the network, and link this machine to it, in the background |
| `gst status` | List every machine of the network: short id, name, gst version, gst IP, and `direct`, `relay`, or `offline`; then this machine's problems |
| `gst status -v` | Also list the links between all the machines (`from -> to`, destination, and state; this machine's down links too, the others' running ones only), and the traffic with each machine: the way (`direct` and the `ip:port`, or `relay`), the last handshake, and the bytes received and sent |
| `gst status <machine>` | Show a machine's gst IP, exposed ports, and every link from or to it (`from -> to`, destination, and state), asked through the network |
| `gst ping <machine> [-c N] [-s BYTES] [-v]` | Send a packet to a machine and back once a second (3 times by default), print how it went and the time, and at the end how many were lost; `-s` sets the size of the packets, which past the interface's MTU go in fragments; `-v` also checks the SSH links and shows the direct path, or why there is none, what both machines did in their latest try at one, and where this machine lost packets |
| `gst expose [target] [--port port] [--udp]` | Without a target, list this machine's exposed ports; with one, expose it on this machine's gst IP |
| `gst unexpose <port> [--udp]` | Stop exposing a port and close its connections |
| `gst unlink <destination>...` | Remove this machine's links to the destinations given; the machines stay in the network |
| `gst remove <machine>` | Remove a machine from the network, for every machine of it; this machine leaves the network |
| `gst name [name]` | Show or change this machine's name, and show its id |
| `gst subnet [cidr]` | Show the subnet of the network, or change it for all machines |
| `gst ui [--port N]` | Show the network in a web page |
| `gst down [--yes]` | Undo `gst up`: stop gst and remove its service and the network helper; other machines cannot link to this one |
| `gst version` | Show the gst release, commit, protocol, and network helper version |

`destination` is `host`, `user@host`, or `ssh://user@host:port`. ssh options after `--` go with it, such as `-p 2222` or `-i ~/.ssh/gst_ed25519`; everything else about the SSH connection comes from your ssh configuration (see [ssh configuration](#ssh-configuration)). A machine links to a destination once; adding it again replaces its ssh options. `gst add` checks first that a usable key without a passphrase exists and that known_hosts records the server, and saves nothing if not. It then says what happened: `Added devbox to the network: 10.87.61.7, devbox.gst.`, `devbox is already in the network; this machine now also links to it.`, or `Changed this machine's link to devbox.`

`gst remove` says which machines reached the network only through the removed one; they stay offline until they link to another machine. On a removed machine, `gst status` says who removed it and when.

`gst status` prints one line per machine, this machine first; the last column says how packets go to it:

| Way | Meaning |
| --- | --- |
| `-` | This machine |
| `direct` | Directly over UDP |
| `relay` | Relayed along the SSH link between the two machines |
| `relay via devbox` | Relayed along the SSH links, through the machines named |
| `offline, seen 3h ago` | No way leads to it now; it last said anything of itself that long ago |

With `-v`, links show as the machines at their ends, such as `laptop -> gateway`. A `Problems:` section lists this machine's links that are not running, and what keeps it off the network. A link is `running`, `connecting`, or `down` (failed and retrying; the last error is shown, including the first line of the other gst's error). A failed link retries after 5 seconds, then 10, doubling up to every 5 minutes, and starts over once it runs.

`gst ping` prints, for example, `reply from devbox (10.87.61.7): direct 203.0.113.7:41642, 12.3 ms`, or `reply from gateway (10.87.2.9): relay via devbox, 41.3 ms`. The packet goes the way of every program's packets, through the interface and WireGuard, so `direct` and an `ip:port` mean that the direct path to that address works, and the time is about what `ping <machine>.gst` shows. Traffic is what starts a direct path, so the first replies may be relayed. `-v` also checks the SSH links, which carry the control of the network: `SSH links: gateway answered via devbox in 41.3 ms`. That time is higher, as it includes setting up the way through each machine. The command exits with status 1 if no reply came.

Every command accepts `--config <file>`.

## ssh configuration

A link has no SSH settings of its own beyond the options given to `gst add` after `--`. Every time it connects, gst resolves its destination again with `ssh -G <options> -- <destination>`, so it follows `~/.ssh/config`, and the options win over it. A change to the file applies the next time the link connects.

| ssh setting | Used for | Default when unset |
| --- | --- | --- |
| `HostName`, `Port`, `User` | Address and login user | ssh's defaults |
| `IdentityFile` | Private keys, tried in order; missing or passphrase-protected ones are skipped | ssh's default key list |
| `UserKnownHostsFile`, `GlobalKnownHostsFile` | Host key verification | ssh's default files |
| `ConnectTimeout` | Connect and login timeout | 15 seconds |

gst checks a running link itself: each side sends something every 15 seconds, and a link that hears nothing for 45 seconds reconnects. `ServerAliveInterval` and `ServerAliveCountMax` are not used.

Limitations:

- Only private keys without a passphrase work; ssh-agent is not used.
- known_hosts is checked strictly; unknown hosts are never accepted.
- `ProxyJump`, `ProxyCommand`, and `HostKeyAlias` are not supported for links. To reach a machine behind another, run gst on both and link through the one in front.

## Running in the background

- One background process per user, the gst daemon, runs the links and the network, and accepts links from other machines. It is a user service: a systemd user service `gst.service` on Linux, a launchd agent `local.gst` on macOS. gst installs it with `gst up` or the first change to its configuration; you do not manage it yourself. Another machine that links to this one never installs it. `gst down` stops it and removes the service, and `gst up` brings it back.
- gst commands talk to it over a socket that only you can access: `~/.local/state/gst/gst.sock` on Linux (under `$XDG_STATE_HOME` when set), `~/Library/Application Support/gst/gst.sock` on macOS.
- If it exits, its service restarts it after 5 seconds.
- Its log, which also holds link events (connected, failed, reconnecting), goes to journald on Linux (`journalctl --user -u gst`) and to `~/Library/Logs/gst-daemon.log` on macOS. The log names a link by a short id, not by its destination.
- For privacy, logs record no server addresses, users, key paths, or target addresses, and not the errors that another gst reports. Those appear in interactive output only: `gst status`, `gst ping`, and `gst ui`. The addresses of direct paths stay out of the logs too. To see why a link fails in full, run `ssh -v <destination>`.
- On Linux, to keep links running after you log out and start them at boot, run `loginctl enable-linger "$USER"`; gst tells you when lingering is off. On macOS, gst starts at login and stops at logout.
- The service runs `gst daemon`, and links run `~/.local/state/gst/bin/gst peer-bridge` on the other machine over SSH, or `gst peer-bridge` from the `PATH` when `gst up` has not put gst there; neither is a command you run yourself.
- The daemon keeps what it knows of the network, every machine of it and the removed ones, in `state.json` next to its socket, so it knows the network before any link connects.

### Web page

`gst ui` shows the network in a web page that updates itself every few seconds: this machine at the top with a map of the links between all machines, then every machine with its gst IP, how packets go to it, and its problems. Open a machine to see its exposed ports and every link that starts or ends at it, written `from → to` as in `gst status -v`; a link belongs to the machine it starts from, with the ssh destination it was made with. It says so when gst is not running on this machine. The page only shows; changes take the commands above.

`gst ui` serves the page on this machine's gst IP until you press Ctrl-C, so that any machine of the network, this one included, opens it at `<name>.gst`; it does not listen on `localhost` or any other address. It needs the network up, prints the address, and opens no browser.

```bash
gst ui
gst ui --port 8370   # on devbox; then open http://devbox.gst:8370/#... on any machine of the network
```

The address ends with a random secret after `#`, which the page needs; anyone with the full address can use the page while `gst ui` runs. Each run makes a new secret.

## Upgrading

### From 0.6

A machine added to the network now stays in it while offline, until `gst remove` takes it out. gst reads a 0.6 configuration and keeps its links. Commands changed:

| 0.6 | Now |
| --- | --- |
| `gst link <destination>` | `gst add <destination>` |
| `gst link <destination> --remote-gst <command>` | `gst up` on the other machine, which puts gst where links find it |
| `gst unlink <machine>` | `gst unlink <destination>` removes only this machine's link; `gst remove <machine>` takes a machine out of the network |

The subnet you chose with `gst subnet` is not kept; the network chooses one again, and `gst subnet <cidr>` on any machine sets it for all of them. Run `gst up` once on every machine, so that links find gst there. Upgrade every machine together: gst refuses to talk to a gst of another protocol, and names both.

### From 0.5 and earlier

This gst does not read a configuration of gst 0.5 or earlier, and starts over on each machine:

```sh
rm ~/.config/gst/config.json
gst up
gst add devbox               # on the machines that had links
```

Each machine gets a new identity; `gst name` sets its name again. Links have no names of their own, and a machine is reached by its own name, such as `devbox.gst`; `gst connect`, the proxy, maps, routes, exits, and `--json` are gone. See [Services](#the-network) for programs that listen only on loopback or a Unix socket.

## Troubleshooting

Work from the outside in: `gst status` shows whether the machine can be reached and lists this machine's links that are not running, each with its last error, and what keeps it off the network; `gst ping <machine>` checks the way to it, and `gst ping <machine> -v` shows why a direct path does or does not come up; the log has the link events (`journalctl --user -u gst` on Linux, `~/Library/Logs/gst-daemon.log` on macOS); and `ssh -v <destination>` shows what ssh itself says about the login. `gst add` also checks the key and known_hosts first, and saves nothing if the check fails.

| Symptom | Fix |
| --- | --- |
| `no usable key`, or a key is `passphrase-protected` | Create a key without a passphrase as in [Before you start](#before-you-start), and name it with `IdentityFile` in `~/.ssh/config` |
| `no host key for ...` | Connect once with `ssh <host>` to record the host key |
| Host key mismatch | Once you have confirmed the machine's key really changed, run `ssh-keygen -R <host>` and connect once with `ssh` |
| A link is `down` with an error from the other gst, such as command not found | `gst up` has not run there with the gst you installed. Run it there, for example `ssh -t <destination> ~/.nix-profile/bin/gst up`; the link connects by itself |
| A link is `down` with `gst is not up on this machine; run gst up there`, or `gst add` says so | Run `gst up` on the other machine, for example with `ssh -t <destination> gst up`; the link connects by itself |
| `This machine was removed from the network by ...` in `gst status` | Run `gst add <destination>` here, or `gst add` of this machine on a machine of the network |
| A link is `down` with `... was removed from the network` | The machine at its end was removed. Bring it back with `gst add <destination>` |
| `speaks gst protocol 4 and … speaks 5; upgrade gst on the older side` | Upgrade gst on the machine with the older protocol; both ends of every link need the same one |
| `config version 4 is too old` | Remove the file it names and add the machines again ([From 0.5 and earlier](#from-05-and-earlier)) |
| `... is offline` | No running links connect this machine to that one. Check `gst status` (an offline machine, and links that are not running) |
| `gst status <machine>` or `gst ping <machine>` fails | The machine must be reachable and run gst of the same protocol; see `gst status` for the way to it |
| `gst is not running on this machine` | Run `gst up`; see the log (`journalctl --user -u gst`, or `~/Library/Logs/gst-daemon.log`) |
| A machine name matches several machines | Use its id or an id prefix from `gst status` |
| `the network helper is not running` | Run `gst up`; the machine then joins by itself |
| `the subnet ... overlaps ... on this machine` | Choose another subnet with `gst subnet <cidr>`, on any machine of the network |
| `HANDSHAKE` stays `never` in `gst status -v` | Traffic has to start it: try `ping <machine>.gst`. If it stays, check that `gst status` on the other machine shows no problem |
| A program on a machine does not answer on its `.gst` name | It may listen only on `127.0.0.1` or a Unix socket; see [Services](#the-network) |
| macOS blocks access to a local network server | Allow gst in the Local Network alert or in System Settings → Privacy & Security → Local Network, then run `gst up` to restart gst. Authorization for your terminal does not necessarily authorize the background service |

## Scripts and agents

- Results go to stdout, written for people. Errors go to stderr, start with `error:`, and usually come with a `hint:` line naming the next command to run. Notes that are not errors start with `note:`.
- Exit status: `0` success, `1` the work failed (including a `gst ping` with no reply), `2` invalid arguments.
- No command prompts for input, except `gst up` and `gst down` before they run `sudo` on a terminal; `--yes` skips the question.

## Configuration file

The configuration is stored in `~/.config/gst/config.json` (`$XDG_CONFIG_HOME/gst/config.json` when `XDG_CONFIG_HOME` is set). It holds this machine's name and id, its links with their ssh options, and exposed ports. The subnet belongs to the network, and the daemon keeps it with the rest of the network in `state.json`. `--config` or the `GST_CONFIG` environment variable selects another file, and gst keeps using it in the background. There is one gst daemon per user, so changing another file switches it to that file. gst maintains this file; change it with gst commands rather than by hand. Running several gst commands at once is safe.
