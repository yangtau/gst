# AGENTS.md

gst links a user's machines into a private network over SSH: one background daemon per user and machine, installed as a user service (systemd on Linux, launchd on macOS), links to other machines' daemons over SSH, and carries WireGuard between them, directly over UDP when the network allows and through the SSH links otherwise. `README.md` (English) and `README.zh-CN.md` (Simplified Chinese) are the user guide; keep them about using gst, keep the two in sync, and update both whenever CLI behavior changes. Development notes belong here.

架构合理、设计合理、使用简单是我的最高优先级，为此当前已经有的任何功能、设计都可以更改、兼容性也无需保持。

## Commands

- `make build`, `make test`, `make test-integration`, `make release V=X.Y.Z` (see `Makefile`).
- `nix develop` provides Go, gopls, Make, and OpenSSH. `nix build` builds gst;
  `nix flake check` runs the unit tests and both vet checks as part of the build.
- After changing Go dependencies, update `vendorHash` in `flake.nix` using the
  hash reported by a build with `vendorHash = pkgs.lib.fakeHash;`.
- `go vet ./... && go vet -tags integration ./integration` before finishing; integration files only compile with the tag.

## Layout

- `cmd/gst`: entry point.
- `internal/cli`: cobra commands, output, and the error/hint contract; `gst ui` and its embedded page (`internal/cli/ui`).
- `internal/config`: config file (locked, atomic writes; links, the network's subnet, this node).
- `internal/sshconfig`: resolves a destination with `ssh -G`.
- `internal/sshconn`: SSH connections with keepalive, remote commands, and `Check`, the offline preflight behind `gst add`.
- `internal/daemon`: the daemon, which supervises the links, runs the virtual network, and answers the control socket, and its client.
- `internal/mesh`: the node: links (`gst peer-bridge` plus yamux, behind `session`), the link-state records they flood (`topology.sync`) into each node's view of the whole mesh, calls to any node along source routes (`relay.open`), and the two ways of packets: a UDP path, or a stream along the links (`packets.go`). Signaling for UDP paths is here (`direct.go`).
- `internal/direct`: UDP paths: `Transport`, one wildcard socket per IP family on one port that all paths share (on macOS bound to the interface below any VPN, `bind_darwin.go`), and `Path`, one per node, which probes the node's addresses, sends to the fastest, and carries raw packets. No encryption: that is WireGuard's. Signaling is `mesh`'s.
- `internal/vnet`: the virtual network, in the daemon, and the only data path: wireguard-go over the mesh (`bind`), the choice of subnet and addresses, packets to this machine's own address (`loopback`), and DNS for `.gst`. `mesh` carries its packets (`packets.go`).
- `internal/tun`: the root helper that creates the interface and sets addresses, routes, and name resolution, and the daemon's client of it. Commands that take root stay here; `internal/service` installs it.
- `internal/relay`: copies bytes between two connections, for `relay.open`.
- `internal/wire`: the gst protocol (frames, handshake, calls, and `Server`, which dispatches calls to registered methods) used by the control socket and links.
- `internal/logfile`: rotating log files.
- `internal/service`: installs the daemon as a systemd or launchd service, and the helper of the virtual network as a system one.
- `integration`: tests against real OpenSSH servers in containers.

## Conventions

- **CLI contract.** Results go to stdout. Errors go to stderr as `error:` plus an optional `hint:` that names the next command to run. Usage errors also print `usage:` and exit 2; failed work exits 1. `--json` fields are stable. Commands never prompt, with one exception: before running anything as root, `gst up` and `gst down` say what `sudo` will run and what it changes, and ask on a terminal, because `sudo` may not ask for a password (`--yes` skips only the question; without a terminal they run nothing). `gst down` needs that only when the helper does not answer: it normally asks the helper to remove itself. gst never reads the password, and never sets up another machine: `gst peer-bridge` refuses until someone ran `gst up` there. Positional arguments are validated with `a.args`, which names the missing argument from the `Use` line.
- **Resolution.** The config stores a link's destination and remote gst command only; every link connection resolves the destination with `ssh -G` (`-F $GST_SSH_CONFIG` when set), and settings the ssh config leaves unset take the defaults in `internal/cli/app.go`.
- **Log privacy.** The daemon log (which holds the link events), and the errors from `internal/sshconn`, `internal/mesh`, and config parsing that reach it, never include server addresses, users, key paths, or what a remote gst reports. Interactive output may include them, so `Check`'s errors may name paths and addresses.
- **No personal data.** Code, tests, docs, and commits use placeholders such as `myhost` and `ssh.example.com`, never real hostnames, users, or emails.
- **Protocol.** `docs/design/mesh.md` specifies the design and the gst protocol. Extend the protocol with new methods, fields, and features; never change the frame format or the meaning of a field without raising `wire.Protocol`.
- **Unit tests stay local.** `internal/cli` tests inject a fake daemon backend and resolver; `internal/daemon` tests run daemons on temporary sockets with fake link connections; `internal/sshconn` tests use an in-process SSH server; `internal/vnet` tests run real WireGuard over a fake mesh and interface; `internal/tun` tests use a fake system. Nothing installs a real service, contacts a real server, or touches a real interface. macOS's `utun`, routes, socket binding, and `/etc/resolver` are only exercised on a real Mac.

## Versions

Each version changes for its own reason, and only it:

- **Release**: `const version` in `internal/cli/root.go`, the single source (`flake.nix` reads it). `make release V=X.Y.Z` sets it, runs the unit tests and vet, commits, and tags `vX.Y.Z`; push yourself. Raise the minor version with `wire.Protocol` or `config.Version`. README and design-doc examples are examples: leave their versions alone.
- **Commit**: added to the release as `+a2ffc87[.dirty]` in `software`, for people only: Go's VCS stamp, or `-X internal/cli.commit` in nix, whose source has no `.git`.
- **Build**: `daemon.Build`, the executable's SHA-256, is how a gst command decides the daemon runs another gst and restarts it.
- **Protocol**: `wire.Protocol`, raised per the Protocol convention; nodes of different protocols refuse each other, naming both `software`.
- **Helper**: `tun.HelperVersion`, raised only when the helper's code or the daemon's requests of it change, since updating the helper takes root.

## Integration tests

Read `integration/README.md` before running or changing them. They use a prepared sshd image and per-run node containers that run both sshd and gst and talk to each other by container IP (`integration/node_test.go`); nothing runs, listens, or connects on this machine. Network clients and servers inside the containers come from `integration/ithelper`. Tests run the CLI and daemons in the node containers (`startPair` for two, `start3` for three) with `GST_SSH_CONFIG=/dev/null`, `GST_DIRECT=off` unless `node.direct` is set, and the image's systemctl shim, which also stands in for the system's systemctl for the virtual network's helper. Machine-specific settings such as `IT_APT_MIRROR` go in the git-ignored `integration/local.mk`, which the `Makefile` includes.
