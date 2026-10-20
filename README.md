# gst

`gst`（go ssh tunnel）让 SSH 端口转发在后台持续运行。它用 Go 和 `golang.org/x/crypto/ssh` 建立连接，不依赖 `ssh` 进程保持隧道；只在解析配置时调用 `ssh -G`。支持 Linux 和 macOS。

- **tunnel**：一个有名字的 SSH 连接。目标地址按你的 ssh 配置（`~/.ssh/config`）解析。
- **forward**：tunnel 上的一条转发，写法与 ssh 相同。
  - `-R [bind_address:]port:host:hostport`：在服务器上监听，由本机连接 `host:hostport`。
  - `-L [bind_address:]port:host:hostport`：在本机监听，由服务器连接 `host:hostport`。

运行中的 tunnel 是一个后台服务（Linux 上是 systemd 用户服务，macOS 上是 launchd 用户代理），登录后自动启动，断线后自动重连。服务由 gst 创建和删除，用户不需要直接管理。

## 安装

```bash
go install github.com/yangtau/ssh-tunnel/cmd/gst@latest
```

后台服务会记录 gst 的绝对路径。移动或升级 gst 后执行 `gst tunnel restart --all`。

## 快速开始

```bash
# 使用 ~/.ssh/config 里的 Host myhost；在服务器的 2222 端口访问本机的 SSH
gst tunnel add myhost -R 2222:localhost:22

# 在本机的 15432 端口访问服务器上的数据库
gst forward add myhost -L 15432:127.0.0.1:5432

gst tunnel list            # 查看所有 tunnel 及状态
gst tunnel check myhost    # 逐步诊断连接问题
gst tunnel logs myhost -f  # 查看日志
gst tunnel stop myhost     # 停止并删除后台服务，保留配置
gst tunnel start myhost    # 重新创建后台服务并启动
gst tunnel remove myhost   # 停止并删除 tunnel
```

每个命令都可以用 `--help` 查看详细说明和示例。`gst completion bash|zsh|fish` 生成 shell 补全脚本。

## 命令

| 命令 | 作用 |
| --- | --- |
| `gst tunnel add <name> [destination] [-R spec]... [-L spec]...` | 添加 tunnel 并在后台启动 |
| `gst tunnel set <name> [flags]` | 修改目标地址或覆盖 ssh 配置的设置 |
| `gst tunnel start\|stop\|restart <name>... \| --all` | 启动、停止、重启 |
| `gst tunnel remove <name>...` | 停止并删除 |
| `gst tunnel list` / `show <name>` | 查看 tunnel、状态和 ssh 解析结果 |
| `gst tunnel logs <name> [-f] [-n N]` | 查看日志 |
| `gst tunnel check <name>` | 诊断：ssh 配置、私钥、known_hosts、网络、登录 |
| `gst forward add <tunnel> (-R spec \| -L spec)...` | 添加 forward |
| `gst forward remove <tunnel> (-R [bind:]port \| -L [bind:]port)...` | 删除 forward |
| `gst forward list <tunnel>` | 列出 forward |

约定：

- 结果输出到 stdout，错误和提示输出到 stderr；失败时通常附带 `hint:`，给出下一步该执行的命令。
- 退出码：`0` 成功，`1` 执行失败（包括 `check` 未通过），`2` 参数无效。
- `list`、`show`、`check`、`forward list` 支持 `--json`，字段结构固定。
- 所有命令都不需要交互输入。重复添加完全相同的 forward 不会报错，也不会做任何改动；监听地址相同但 target 不同则报错。
- 修改运行中 tunnel 的 forward 或设置后，gst 会自动重启该 tunnel，并在输出中说明；加 `--no-restart` 可以跳过。
- `tunnel list` 中的状态：`running`（运行中）、`down`（服务存在，进程失败后正在等待重启，可用 `logs` 和 `check` 排查）、`stopped`（没有后台服务）。

## ssh 配置

`destination` 使用 ssh 的写法：`host`、`user@host` 或 `ssh://user@host:port`，省略时等于 tunnel 名。每次启动 tunnel 时，gst 都会执行 `ssh -G <destination>`，读取以下配置：

| ssh 配置 | 用途 | 覆盖参数 | 都未设置时 |
| --- | --- | --- | --- |
| `HostName`、`Port`、`User` | 服务器地址和登录用户 | `tunnel set --destination` | ssh 的默认值 |
| `IdentityFile` | 私钥，依次尝试，跳过不存在或有口令的 | `--identity` | ssh 的默认私钥列表 |
| `UserKnownHostsFile`、`GlobalKnownHostsFile` | 主机密钥校验 | `--known-hosts` | ssh 的默认文件 |
| `ConnectTimeout` | 连接和登录超时 | `--connect-timeout` | 15 秒 |
| `ServerAliveInterval` | 保活间隔 | `--keepalive-interval` | 15 秒 |
| `ServerAliveCountMax` | 保活请求无响应超过 间隔 × 此值 后断开重连 | `--keepalive-count` | 3 |

限制：

- 只使用没有口令的私钥，不读取 ssh-agent。
- 严格校验 known_hosts，不接受未知主机。首次使用前先用 `ssh` 连接一次并核验主机密钥。
- 不支持 `ProxyJump`、`ProxyCommand`、`HostKeyAlias`，配置了会报错。

`tunnel add`、`set`、`start` 和 `restart` 会先离线检查私钥和 known_hosts，不通过时不会保存配置，也不会启动服务。

## forward

- 省略 `bind_address` 时只监听 loopback（`127.0.0.1`）；`*` 或空的 `bind_address`（如 `:8080:localhost:3000`）监听所有接口；IPv6 地址加方括号。
- `host` 可以是主机名，由连接它的一方解析：`-L` 中的 `127.0.0.1` 指服务器自己。
- 服务器上实际的监听范围还受 sshd 的 `GatewayPorts` 控制。监听所有接口时，应通过防火墙限制访问来源。
- sshd 的 `AllowTcpForwarding` 需要允许所用的方向：同时使用两个方向时设为 `yes`，只用 `-R` 时可设为 `remote`，只用 `-L` 时可设为 `local`。sshd 拒绝 `-L` 时，日志中每个连接都会记录 `target connection failed`。

**清理远端监听（`--cleanup-listeners`）：** 每次建立连接后、开始监听前，在服务器上用 Bash 终止占用 `-R` 端口、属于登录用户的监听进程，先发 SIGTERM，必要时再发 SIGKILL。被终止的**不限于 SSH 进程**，所以这些端口必须专用于这个 tunnel。这个功能只是兜底：Linux 上残留的 sshd 会话进程无法被清理，其他用户的进程也不会被清理，端口仍被占用时会以 `remote listener cleanup failed` 失败并重试。服务器需要在非交互 SSH 会话的 `PATH` 中提供 `bash`（3.2 及以上）、`lsof`、`ps`、`id`；Linux 上还需要可读的 `/proc/net/tcp`，其他系统需要 `netstat`。无论是否开启清理，都建议在服务器的 sshd 配置中设置 `ClientAliveInterval 15` 和 `ClientAliveCountMax 3`，让 sshd 及时回收失联的会话，释放端口。

## 后台服务

| | Linux | macOS |
| --- | --- | --- |
| 服务 | `~/.config/systemd/user/gst-<name>.service` | `~/Library/LaunchAgents/local.gst.<name>.plist` |
| 日志 | journald | `~/Library/Logs/gst/<name>.log`（不轮转，日志量很小） |
| 生命周期 | 每次退出后 5 秒重启，没有频率限制 | 每次退出后至少间隔 5 秒重启 |

- 两个平台上，停止时都先发送 SIGTERM，15 秒后强制结束。
- Linux 上，要让 tunnel 在退出登录后继续运行、并在开机时启动，需要开启 linger：`loginctl enable-linger "$USER"`。gst 检测到未开启时会给出提示。
- macOS 上，用户代理在登录后启动、注销后停止。

## 配置文件

tunnel 保存在 `~/.config/gst/config.json`（设置了 `XDG_CONFIG_HOME` 时为 `$XDG_CONFIG_HOME/gst/config.json`）。可以用 `--config` 或环境变量 `GST_CONFIG` 指定其他路径，后台服务会沿用同一个路径。

这个文件由 gst 维护，不需要手动编辑。写入时加锁，并通过"临时文件 + rename"原子替换，多个 gst 命令并发执行也不会互相覆盖。文件权限为 `0600`。

## 隐私

- 后台服务的日志不记录服务器地址、用户、密钥路径和转发地址，转发只按编号记录，例如 `forward 2`；编号与 `gst forward list` 的顺序一致。因此日志中的错误信息比较简略，详细原因用 `gst tunnel check` 查看。
- 服务进程的参数只包含 gst 路径、配置文件路径和 tunnel 名。解析配置时临时运行的 `ssh -G` 会在参数中带上 destination。
- 这些措施不防御本机管理员读取文件、内存或观察网络连接。

## 开发

```bash
make build             # 构建 bin/gst
make test              # 单元测试，不连接真实服务器
make test-integration  # 在容器中的真实 OpenSSH sshd 上运行集成测试
```

代码结构：

- `cmd/gst`：程序入口。
- `internal/cli`：命令行。
- `internal/config`：配置文件和 forward 的写法。
- `internal/sshconfig`：调用 `ssh -G` 解析 destination。
- `internal/tunnel`：SSH 连接、保活、两个方向的转发、远端清理（`cleanup.sh`）和诊断。
- `internal/service`：systemd 和 launchd 服务。
- `integration`：集成测试及其 sshd 镜像。

集成测试编译 gst，并用 `gst run` 连接容器中的 sshd。每个用例使用一个全新的容器，用完自动删除；密钥、主机密钥和端口都随机生成。测试通过 `GST_SSH_CONFIG=/dev/null` 忽略本机的 ssh 配置，这个变量会作为 `ssh -F` 传给 `ssh -G`。sshd 镜像按 `integration/sshd` 的内容打标签，内容不变时复用已有镜像。

覆盖的场景：

- 转发：多条 forward 并发大流量与半关闭、目标拒绝连接、SIGTERM 正常退出并释放端口、日志不泄露地址和路径、两个方向同时工作。
- 保活：正常连接长时间运行不误判断线；sshd 会话被 SIGSTOP 冻结时因保活超时退出；sshd 会话或容器被杀时退出。
- 清理：终止登录用户自己的占用进程；不终止其他用户的进程并报错；未开启清理时端口被占用会报错；失联的旧会话被 sshd 回收后，新连接能接管端口。
- 启动和诊断：主机密钥不匹配、服务器不可达、`gst tunnel check` 通过。

需要 podman 或 docker，自动选择，也可以用 `CONTAINER_RUNTIME` 指定。可选的环境变量：

| 变量 | 含义 |
| --- | --- |
| `IT_BASE_IMAGE` | 基础镜像，默认 `docker.io/library/debian:bookworm-slim` |
| `IT_APT_MIRROR` | 构建镜像时使用的 Debian 镜像站，如 `http://mirrors.example.com` |
| `IT_BUILD_FLAGS` | 额外的镜像构建参数，如 `--isolation=chroot` |

本机专用的设置写在 `integration/local.mk`（不纳入 Git），例如：

```make
IT_APT_MIRROR := http://mirrors.example.com
IT_BUILD_FLAGS := --isolation=chroot
```

在 cgroup v1 的机器上，非 root 运行的 podman 可能需要在 `~/.config/containers/containers.conf` 中设置 `cgroups = "disabled"` 和 `cgroup_manager = "cgroupfs"`，构建镜像时还需要 `--isolation=chroot`。
