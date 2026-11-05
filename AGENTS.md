# AGENTS.md

gst links a user's machines into a mesh over SSH: one background daemon per user and machine, installed as a user service (systemd on Linux, launchd on macOS), links to other machines' daemons and relays TCP and Unix socket connections between them, directly over UDP when the network allows. `README.md` (English) and `README.zh-CN.md` (Simplified Chinese) are the user guide; keep them about using gst, keep the two in sync, and update both whenever CLI behavior changes. Development notes belong here.

架构合理、设计合理、使用简单是我的最高优先级，为此当前已经有的任何功能、设计都可以更改、兼容性也无需保持。

## Commands

- `make build`, `make test`, `make test-integration` (see `Makefile`).
- `nix develop` provides Go, gopls, Make, and OpenSSH. `nix build` builds gst;
  `nix flake check` runs the unit tests and both vet checks as part of the build.
- After changing Go dependencies, update `vendorHash` in `flake.nix` using the
  hash reported by a build with `vendorHash = pkgs.lib.fakeHash;`.
- `go vet ./... && go vet -tags integration ./integration` before finishing; integration files only compile with the tag.

## Layout

- `cmd/gst`: entry point.
- `internal/cli`: cobra commands, output, and the error/hint contract; `gst ui` and its embedded page (`internal/cli/ui`).
- `internal/config`: config file (locked, atomic writes; links, maps, routes, the proxy, this node), address parsing, and the conversion of version 2 files (`legacy.go`).
- `internal/sshconfig`: resolves a destination with `ssh -G`.
- `internal/sshconn`: SSH connections with keepalive, remote commands, and `Check`, the offline preflight behind `gst add`.
- `internal/daemon`: the daemon, which supervises the links, serves the proxy and the maps, and answers the control socket, and its client.
- `internal/mesh`: the node: links (`gst peer-bridge` plus yamux, or `internal/direct`, behind `session`), the link-state records they flood (`topology.sync`) into each node's view of the whole mesh, and relayed connections (`relay.open`) along source routes, with the exit check at the last hop.
- `internal/direct`: direct links: one try at a UDP path between two daemons (probes, QUIC, pinned certificates) and the connection it yields. Signaling is `mesh`'s.
- `internal/proxy`: the SOCKS5 and HTTP proxy.
- `internal/relay`: copies bytes between two connections.
- `internal/wire`: the gst protocol (frames, handshake, calls, and `Server`, which dispatches calls to registered methods) used by the control socket and links.
- `internal/logfile`: rotating log files.
- `internal/service`: installs the daemon as a systemd or launchd service.
- `integration`: tests against real OpenSSH servers in containers.

## Conventions

- **CLI contract.** Results go to stdout. Errors go to stderr as `error:` plus an optional `hint:` that names the next command to run. Usage errors also print `usage:` and exit 2; failed work exits 1. `--json` fields are stable. Commands never prompt. Positional arguments are validated with `a.args`, which names the missing argument from the `Use` line.
- **Resolution.** The config stores a link's destination and remote gst command only; every link connection resolves the destination with `ssh -G` (`-F $GST_SSH_CONFIG` when set), and settings the ssh config leaves unset take the defaults in `internal/cli/app.go`.
- **Log privacy.** The daemon log (which holds the link events), and the errors from `internal/sshconn`, `internal/mesh`, and config parsing that reach it, never include server addresses, users, key paths, targets, or what a remote gst reports. Interactive output may include them, so `Check`'s errors may name paths and addresses.
- **No personal data.** Code, tests, docs, and commits use placeholders such as `myhost` and `ssh.example.com`, never real hostnames, users, or emails.
- **Protocol.** `docs/design/mesh.md` specifies the design and the gst protocol. Extend the protocol with new methods, fields, and features; never change the frame format or the meaning of a field without raising `wire.Protocol`.
- **Unit tests stay local.** `internal/cli` tests inject a fake daemon backend and resolver; `internal/daemon` tests run daemons on temporary sockets with fake link connections; `internal/sshconn` tests use an in-process SSH server. Nothing installs a real service or contacts a real server.

## Integration tests

Read `integration/README.md` before running or changing them. They use a prepared sshd image and per-run node containers that run both sshd and gst and talk to each other by container IP (`integration/node_test.go`); nothing runs, listens, or connects on this machine. Network clients and servers inside the containers come from `integration/ithelper`. Tests run the CLI and daemons in the node containers (`startPair` for two, `start3` for three) with `GST_SSH_CONFIG=/dev/null`, `GST_DIRECT=off` unless `node.direct` is set, and the image's systemctl shim. Machine-specific settings such as `IT_APT_MIRROR` go in the git-ignored `integration/local.mk`, which the `Makefile` includes.
