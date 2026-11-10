# AGENTS.md

gst links a user's machines into a private network over SSH: one background daemon per user and machine, installed as a user service (systemd on Linux, launchd on macOS), links to other machines' daemons over SSH, and carries WireGuard between them, directly over UDP when the network allows and through the SSH links otherwise. On iPhone and iPad the same daemon runs inside the packet tunnel of an app (`ios/`). `README.md` (English) and `README.zh-CN.md` (Simplified Chinese) are the user guide; keep them about using gst, keep the two in sync, and update both whenever CLI behavior changes. Development notes belong here.

架构合理、设计合理、使用简单是我的最高优先级，为此当前已经有的任何功能、设计都可以更改、兼容性也无需保持。

## Commands

- `make build`, `make test`, `make test-integration`, `make release V=X.Y.Z`, and, on a Mac, `make ios` (see `Makefile` and `ios/README.md`).
- `nix develop` provides Go, gopls, Make, and OpenSSH. `nix build` builds gst;
  `nix flake check` runs the unit tests and both vet checks as part of the build.
- After changing Go dependencies, update `vendorHash` in `flake.nix` using the
  hash reported by a build with `vendorHash = pkgs.lib.fakeHash;`.
- `go vet ./... && go vet -tags integration ./integration` before finishing; integration files only compile with the tag.

## Layout

- `cmd/gst`: entry point.
- `internal/version`: the release and the commit, for the command and the iOS app.
- `internal/cli`: cobra commands, output, and the error/hint contract; `gst ui` and its embedded page (`internal/cli/ui`).
- `internal/config`: config file (locked, atomic writes; links, the network's subnet, this node).
- `internal/sshconfig`: resolves a destination with `ssh -G`, or, where there is no ssh, reads what it says by itself (`Literal`).
- `internal/sshconn`: SSH connections, remote commands, `Link`, which runs `gst peer-bridge` on a server, and `Check`, the offline preflight behind `gst add`. For machines without ssh: `NewIdentity`, `Try`, a login that tells an unknown host key from an unauthorized key, and `Trust`.
- `internal/daemon`: the daemon, which supervises the links, runs the virtual network, and answers the control socket (`Serve`), and its client, whose one `Call` takes any control method; the CLI's `backend` has the same shape, so a new control method needs only its handler and its callers. `Run` is the same daemon without a socket, and `Daemon.Call` answers the control methods in its process, for iOS.
- `internal/mesh`: the node, on three layers that each use only the ones below: `link` (a link: `gst peer-bridge` carrying typed frames, with its handshake, `alive`, and `bye`), `state` (the table every node keeps of the network, `node/<id>`, `removed/<id>`, and `subnet` entries, flooded over the links and kept in `state.json`), and `route` (messages to any node, hop by hop along routes computed from the table: calls and packets). `Node` (`mesh.go`) joins them and holds the rules of joining and leaving the network (generation, removal, admit; `standsRemoved`), the topology (`topology.go`), the way packets take, a UDP path or the route (`packets.go`), and signaling for UDP paths (`direct.go`).
- `internal/direct`: UDP paths: `Transport`, one wildcard socket per IP family on one port that all paths share (on macOS bound to the interface below any VPN, `bind_darwin.go`), and `Path`, one per node, which probes the node's addresses, sends to the fastest, and carries raw packets. No encryption: that is WireGuard's. Signaling is `mesh`'s.
- `internal/vnet`: the virtual network, in the daemon, and the only way data goes: wireguard-go over the mesh (`bind`), the choice of subnet and addresses, packets to this machine's own address (`loopback`), and DNS for `.gst`. `mesh` carries its packets (`packets.go`). Where the daemon's sockets cannot reach the interface, as on iOS, `inside` answers the DNS and `gst ping` at the packets of the interface.
- `internal/tun`: the root helper that creates the interface and sets addresses, routes, and name resolution, and the daemon's client of it. Commands that take root stay here; `internal/service` installs it. `Inherited` finds the interface that iOS makes for a packet tunnel.
- `internal/expose`: the exposed ports, bound to this machine's gst IP, which the virtual network applies; it copies bytes between the connections.
- `internal/wire`: the gst protocol: the handshake, calls, and `Server`, which dispatches calls to registered methods, for the control socket, and the types that links carry.
- `internal/logfile`: rotating log files.
- `internal/service`: installs the daemon as a systemd or launchd service, and the network helper as a system one.
- `internal/mobile`: gst on a phone, in two processes that share a directory: the app's part (the `app.…` methods: the identity, a key of its own, trusted host keys, the links), which does what the CLI does elsewhere, and the packet tunnel's (`Start`, `Stop`), which runs the daemon with `inside` and has the system set the interface up (`Settings`).
- `ios`: the iOS app. `gstkit` is `internal/mobile` as a C library (`gstkit.h`; it builds wherever cgo does, so `go vet ./...` covers it), `App` the SwiftUI app, `Tunnel` the packet tunnel extension, `Shared` the Swift face of `gstkit`, `Tests` and `UITests` its tests. See `ios/README.md`.
- `integration`: tests against real OpenSSH servers in containers.

## Conventions

- **CLI contract.** Results go to stdout. Errors go to stderr as `error:` plus an optional `hint:` that names the next command to run. Usage errors also print `usage:` and exit 2; failed work exits 1. Output is for people: there is no `--json`; integration tests read structured state through `gst-it call`, which calls the daemon's control methods. Commands never prompt, with one exception: before running anything as root, `gst up` and `gst down` say what `sudo` will run and what it changes, and ask on a terminal, because `sudo` may not ask for a password (`--yes` skips only the question; without a terminal they run nothing). `gst down` needs that only when the helper does not answer: it normally asks the helper to remove itself. gst never reads the password, and never sets up another machine: `gst peer-bridge` refuses until someone ran `gst up` there. Positional arguments are validated with `a.args`, which names the missing argument from the `Use` line.
- **Words.** One word for each concept. People see the user's word, in output, help, errors, hints, the README, `gst ui`, and the iOS app; identifiers, comments, logs, `-v` output, and `docs/design` use the code's word. A concept with no user word stays out of what people see.

  | User's word | Code's word | Meaning |
  | --- | --- | --- |
  | machine | node | gst on a computer, known by its id and name (`wire.NodeInfo`). In code, *machine* means only the computer itself: its interfaces, routes, SSH server, and services. |
  | network | network | the nodes that were added and not removed. A node is *in the network*; there is no "member". |
  | — | mesh | what joins the nodes: links, the table, and routes (`internal/mesh`) |
  | link | link | an SSH connection from one node to another, named by its destination; it has no name of its own |
  | — | peer | the node at the other end of a link, or the other node of a call between nodes (`wire.RolePeer`) |
  | add, remove, removed | admit, removal, generation | joining the network, and leaving it for good until added again |
  | name, `<name>.gst` | name | a node's name |
  | id | id | a node's id |
  | gst IP | address | a node's address in the virtual network |
  | — | virtual network, `net`, `vnet` | the interface, WireGuard, addresses, and DNS (`internal/vnet`) |
  | subnet | subnet | the virtual network's prefix; the table's `subnet` entry |
  | — | used prefixes | the IPv4 prefixes in use on a machine, which the subnet avoids |
  | direct, relay | way (`WayDirect`, `WayRelay`) | how packets reach a node: over its UDP path, or along the links |
  | relay via X | route | the nodes along the links from one node to another (`TopoNode.Route`) |
  | — | UDP path | a UDP shortcut to one node (`internal/direct`); in code, *path* means nothing else |
  | offline | — | a node in the network that no way reaches now |
  | — | topology | a node's view of the network: its nodes and links (`wire.Topology`) |
  | up, down, "gst is not running" | daemon | gst's background process |
  | network helper | helper | the root service that sets up the interface |
  | expose | expose | a port bound to this machine's gst IP |

  Table, entry, generation, handshake, probe, bridge, and MTU are code's words too. Commands are flat verbs, in the order of `gst --help`: `up`, `add`, `status`, `ping`, `expose`, `unexpose`, `unlink`, `remove`, `name`, `subnet`, `ui`, `down`, `version`; no aliases. `gst <noun> [value]` shows or sets one value; `-v` is the only switch for more detail. Text quotes commands in backticks; a success message is a short statement plus, when useful, the next step.
- **Resolution.** The config stores a link's destination and the ssh arguments given after `--` (besides the machine it reached and the admit mark); every link connection resolves them with `ssh -G <args> -- <destination>` (`-F $GST_SSH_CONFIG` when set), runs the fixed remote command of `sshconn.Link` (the gst that `gst up` linked at `~/.local/state/gst/bin/gst`, else the one in `PATH`), and settings the ssh config leaves unset take the defaults in `internal/sshconn`. A phone has no ssh: `internal/mobile` reads the destination with `sshconfig.Literal`, and uses its own key and known_hosts file.
- **Log privacy.** The daemon log (which holds the link events and names links by `config.LinkID`, never by destination), and the errors from `internal/sshconn`, `internal/mesh`, and config parsing that reach it, never include server addresses, users, key paths, or what a remote gst reports. Interactive output may include them, so `Check`'s errors may name paths and addresses.
- **No personal data.** Code, tests, docs, and commits use placeholders such as `myhost` and `ssh.example.com`, never real hostnames, users, or emails.
- **Protocol.** `docs/design/mesh.md` specifies the design and the gst protocol. Extend the protocol with new methods, fields, and table entries; never change the frame format or the meaning of a field without raising `wire.Protocol`.
- **Unit tests stay local.** `internal/cli` tests inject a fake daemon backend and resolver; `internal/daemon` tests run daemons on temporary sockets with fake link connections; `internal/sshconn` tests use an in-process SSH server; `internal/vnet` tests run real WireGuard over a fake mesh and interface; `internal/tun` tests use a fake system; `internal/mobile` tests join a phone to a network through an in-process SSH server, a daemon on a temporary socket, and a fake interface. Nothing installs a real service, contacts a real server, or touches a real interface. macOS's `utun`, routes, socket binding, and `/etc/resolver` are only exercised on a real Mac, and the iOS app's VPN only on a device. The app itself builds and runs its tests in CI on a Mac (`.github/workflows/ios.yml`): `ios/Tests` runs the Go code through the Swift bridge, and `ios/UITests` opens every screen on a made-up network (`ios/App/Demo.swift`) and keeps a picture of it.

## Versions

Each version changes for its own reason, and only it:

- **Release**: `const Release` in `internal/version/version.go`, the single source (`flake.nix` and `ios/build-core.sh` read it). `make release V=X.Y.Z` sets it, runs the unit tests and vet, commits, and tags `vX.Y.Z`; push yourself. Raise the minor version with `wire.Protocol` or `config.Version`. README and design-doc examples are examples: leave their versions alone.
- **Commit**: for people only. `gst version` prints it in full with its time; `software` (`gst status`, the handshake) adds it to the release as `+a2ffc87[.dirty]`. It comes from Go's VCS stamp, or in nix, whose source has no `.git`, from `-X internal/version.commit` and `.committed`.
- **Build**: `daemon.Build`, the executable's SHA-256, is how a gst command decides the daemon runs another gst and restarts it.
- **Protocol**: `wire.Protocol`, raised per the Protocol convention; nodes of different protocols refuse each other, naming both `software`.
- **Helper**: `tun.HelperVersion`, raised only when the helper's code or the daemon's requests of it change, since updating the helper takes root.

## Integration tests

Read `integration/README.md` before running or changing them. They use a prepared sshd image and per-run node containers that run both sshd and gst and talk to each other by container IP (`integration/node_test.go`); nothing runs, listens, or connects on this machine. Network clients and servers inside the containers come from `integration/ithelper`. Tests run the CLI and daemons in the node containers (`startPair` for two, `start3` for three) with `GST_SSH_CONFIG=/dev/null`, `GST_DIRECT=off` unless `node.direct` is set, and the image's systemctl shim, which also stands in for the system's systemctl for the virtual network's helper. Machine-specific settings such as `IT_APT_MIRROR` go in the git-ignored `integration/local.mk`, which the `Makefile` includes.
