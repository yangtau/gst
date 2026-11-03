# AGENTS.md

gst keeps SSH port forwards running in one background daemon per user, installed as a user service (systemd on Linux, launchd on macOS). `README.md` (English) and `README.zh-CN.md` (Simplified Chinese) are the user guide; keep them about using gst, keep the two in sync, and update both whenever CLI behavior changes. Development notes belong here.

## Commands

- `make build`, `make test`, `make test-integration` (see `Makefile`).
- `nix develop` provides Go, gopls, Make, and OpenSSH. `nix build` builds gst;
  `nix flake check` runs the unit tests and both vet checks as part of the build.
- After changing Go dependencies, update `vendorHash` in `flake.nix` using the
  hash reported by a build with `vendorHash = pkgs.lib.fakeHash;`.
- `go vet ./... && go vet -tags integration ./integration` before finishing; integration files only compile with the tag.

## Layout

- `cmd/gst`: entry point.
- `internal/cli`: cobra commands, output, and the error/hint contract.
- `internal/config`: config file (locked, atomic writes) and forward spec parsing.
- `internal/sshconfig`: resolves a destination with `ssh -G`.
- `internal/tunnel`: SSH connection, keepalive, remote, local, and dynamic (SOCKS5) forwards over TCP or Unix sockets, remote cleanup (`cleanup.sh`), and `Diagnose` behind `gst check`.
- `internal/daemon`: the daemon, which supervises every started tunnel and serves the control socket, and its client.
- `internal/mesh`: peer links between daemons over a tunnel's SSH connection (`gst peer-bridge` plus yamux), and the link-state records they flood (`topology.sync`) into each node's view of the whole mesh.
- `internal/wire`: the gst protocol (frames, handshake, calls) used by the control socket and, later, peer links.
- `internal/logfile`: rotating log files.
- `internal/service`: installs the daemon as a systemd or launchd service and removes gst 0.1's per-tunnel services.
- `integration`: tests against a real OpenSSH sshd in a container.

## Conventions

- **CLI contract.** Results go to stdout. Errors go to stderr as `error:` plus an optional `hint:` that names the next command to run. Usage errors also print `usage:` and exit 2; failed work exits 1. `--json` fields are stable. Commands never prompt. Positional arguments are validated with `a.args`, which names the missing argument from the `Use` line.
- **Resolution.** The config stores the destination alias and gst overrides only; every tunnel start resolves the destination with `ssh -G` (`-F $GST_SSH_CONFIG` when set). Precedence: gst flag, then ssh config, then the defaults in `internal/cli/app.go`.
- **Log privacy.** The background `gst run` log, and the errors from `internal/tunnel` and config parsing that reach it, never include server addresses, users, key paths, or forward addresses; forwards are referred to by number. `Diagnose` and other interactive output may include them.
- **No personal data.** Code, tests, docs, and commits use placeholders such as `myhost` and `ssh.example.com`, never real hostnames, users, or emails.
- **Protocol.** `docs/design/mesh.md` specifies the gst protocol and the mesh plan. Extend the protocol with new methods, fields, and features; never change the frame format or the meaning of a field without raising `wire.Protocol`.
- **Unit tests stay local.** `internal/cli` tests inject a fake daemon backend and resolver; `internal/daemon` tests run the daemon on a temporary socket with fake tunnels; `internal/tunnel` tests use an in-process SSH server. Nothing installs a real service or contacts a real server.

## Integration tests

Read `integration/README.md` before running or changing them. They use a prepared sshd image and per-run node containers that run both sshd and gst and talk to each other by container IP (`integration/node_test.go`); nothing runs, listens, or connects on this machine. Network clients and servers inside the containers come from `integration/ithelper`. Foreground tests run `gst run` with `GST_SSH_CONFIG=/dev/null` in a client container; daemon tests, and later peer-link tests, run the CLI and daemon there with the image's systemctl shim. Machine-specific settings such as `IT_APT_MIRROR` go in the git-ignored `integration/local.mk`, which the `Makefile` includes.
