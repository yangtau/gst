# SSH tunnel

使用 Go 和 `golang.org/x/crypto/ssh` 实现 SSH 隧道，不启动本机 `ssh` 子进程。支持两个方向的映射：远端转发（类似 `ssh -R`，服务器端口映射到本机）和本地转发（类似 `ssh -L`，本机端口映射到服务器）。程序本身不含平台相关逻辑，客户端和服务器均可为 Linux 或 macOS；后台运行在 Linux 上使用 systemd 用户服务，在 macOS 上使用 launchd 用户代理。

## 文件

- `main.go`：命令行入口，处理参数、信号和退出码。
- `internal/config`：读取并严格校验 `tunnel.json`。
- `internal/tunnel`：SSH 连接、保活、远端和本地监听、双向转发和远端清理（`cleanup.sh`）及其测试。
- `integration`：在容器中的真实 OpenSSH 上运行的集成测试，以及测试用的 sshd 镜像（`integration/sshd`）。
- `go.mod`、`go.sum`：Go 依赖及校验信息。
- `tunnel.example.json`：示例配置，含远端转发和本地转发各一条。
- `tunnel.json`：本机部署配置，由示例复制而来，不纳入 Git。
- `deploy/service.sh`：安装和控制后台服务，按系统选择 systemd 或 launchd。
- `deploy/reverse-ssh-tunnel.service`：Linux 的 systemd 用户服务。
- `deploy/local.reverse-ssh-tunnel.plist.in`：macOS 的 launchd 用户代理模板。
- `Makefile`：构建、测试、安装和运维命令。
- `bin/reverse-ssh-tunnel`：构建产物，不纳入 Git。

## 配置

从示例复制出 `tunnel.json` 再编辑：`cp tunnel.example.json tunnel.json`。除 `cleanup_listeners`（缺省为 `false`）外，所有字段都必须显式填写，未知字段会被拒绝；字段名匹配不区分大小写，重复字段以最后一个为准：

| 字段 | 含义 |
| --- | --- |
| `server` | SSH 服务器的 `主机:端口`，不读取 OpenSSH config 的 Host 别名 |
| `user` | 远端登录用户 |
| `identity_file` | 本机私钥文件，支持 `~/` 路径 |
| `known_hosts_file` | 已核验的服务器公钥记录，支持 `~/` 路径 |
| `connect_timeout_seconds` | TCP/SSH 握手、本机目标拨号及远端清理的超时秒数 |
| `keepalive_interval_seconds` | SSH 保活间隔秒数 |
| `keepalive_count_max` | 单个保活请求的响应超时为间隔 × 此数值，超时关闭连接 |
| `cleanup_listeners` | 连接前是否清理远端转发端口上的监听进程，缺省为 `false` |
| `forwards` | 至少一条映射，每条含 `direction`、`listen` 和 `target` |

每条映射的 `direction` 必填：

| `direction` | `listen` 所在 | `target` 由谁连接 |
| --- | --- | --- |
| `remote` | 服务器 | 本机（Go 程序所在机器） |
| `local` | 本机 | 服务器 |

映射示例：服务器的 `2222` 转发到本机的 `22`；本机的 `15432` 转发到服务器可访问的 `127.0.0.1:5432`。

```json
[
  {
    "direction": "remote",
    "listen": "0.0.0.0:2222",
    "target": "127.0.0.1:22"
  },
  {
    "direction": "local",
    "listen": "127.0.0.1:15432",
    "target": "127.0.0.1:5432"
  }
]
```

`listen` 使用 IP 地址和端口；IPv6 使用方括号，如 `[::1]:2222`。同一方向的 `listen` 不能重复，不同方向分别在两台机器上监听，可以相同。`target` 可用主机名，由连接它的一方解析：`local` 映射的 `127.0.0.1` 指服务器自身。远端转发的实际监听范围仍受 sshd 的 `GatewayPorts` 控制；`0.0.0.0` 不代表仅局域网可访问，应通过远端防火墙限制访问。本地转发监听 `0.0.0.0` 时同样会对本机所在网络开放。

程序使用私钥认证并严格校验 known_hosts，不自动接受未知主机密钥。不读取 ssh-agent 或 OpenSSH 配置；需要交互输入口令的加密私钥不受支持。连接参数显式配置在 JSON 中。

**清理注意事项：** `cleanup_listeners=true` 时，通过 SSH session 在远端执行 Bash 清理逻辑，尝试终止 `remote` 映射端口上属于实际登录用户的监听进程，先 SIGTERM、必要时 SIGKILL，**不限于 SSH 进程**。这些端口必须专用于隧道。不独占端口时应关闭清理；端口冲突会导致启动失败，由服务管理器重试。`local` 映射的本机端口不做清理，被占用时同样启动失败。

清理只是兜底手段，主要用于 macOS 服务器。清理通过 `lsof` 查找进程，只能看到登录用户自己的普通进程。Linux 上 sshd 会话进程不可转储，旧隧道遗留的 sshd 会话因此无法被清理；其他用户的进程也不会被清理。端口仍被占用时，清理以 `remote listener cleanup failed` 失败，由服务管理器重试。macOS 上的 sshd 不受此限制，但未在真实 macOS 上验证。清理脚本兼容 macOS 自带的 Bash 3.2。无论哪种系统，都应在远端 sshd 配置中设置 `ClientAliveInterval`（例如 `15`）和 `ClientAliveCountMax`（例如 `3`），让 sshd 及时断开客户端已失联的会话、释放端口。

## 安装

前置条件：

- 本机安装与 `go.mod` 相容的 Go 工具链、GNU Make；Linux 上需有可用的 systemd 用户实例，macOS 上使用自带的 launchd。
- 私钥可非交互登录远端；已核验服务器主机密钥并写入指定 known_hosts 文件。
- 远端 sshd 允许所用方向的端口转发：`AllowTcpForwarding` 为 `yes`，或只用远端转发时为 `remote`、只用本地转发时为 `local`。sshd 拒绝本地转发时，每个连接都以 `target connection failed` 失败。
- 启用清理时，远端需在 SSH 非交互会话的 `PATH` 中具备 `bash`（3.2 及以上）、lsof、ps、id 等命令；Linux 上还需可读的 `/proc/net/tcp`，其他系统需 `netstat`。
- 远端 sshd 建议设置 `ClientAliveInterval` 和 `ClientAliveCountMax`，见上文清理注意事项。
- `remote` 映射的本机目标、`local` 映射的服务器侧目标正在监听相应端口。

```bash
cd ~/ssh-tunnel
cp tunnel.example.json tunnel.json  # 然后按实际环境编辑
make test
make setup
```

`make setup` 构建程序，安装后台服务，启用自启并启动。**已经运行的服务不会重启**；应用更新需执行 `make restart`。两个平台都会把以下路径软链接到仓库，替换已有文件；不要移动或删除仓库：

- `~/.local/bin/reverse-ssh-tunnel`
- `~/.config/ssh-tunnel/tunnel.json`

### Linux：systemd 用户服务

服务名 `reverse-ssh-tunnel.service`，unit 文件软链接到 `~/.config/systemd/user/reverse-ssh-tunnel.service`。退出后每 5 秒重启，无重启频率限制；停止时发送 SIGTERM，15 秒后强杀。日志进入 journald。

退出登录后保持运行及开机启动用户服务需要开启 linger（可能需要管理员权限）：

```bash
loginctl enable-linger "$USER"
```

### macOS：launchd 用户代理

Label 为 `local.reverse-ssh-tunnel`。launchd 不展开 `~` 和变量，所以 `make setup` 和 `make restart` 用 `deploy/local.reverse-ssh-tunnel.plist.in` 生成含绝对路径的 `~/Library/LaunchAgents/local.reverse-ssh-tunnel.plist`，而不是软链接。修改模板后执行 `make restart` 生效。

- 退出后至少间隔 5 秒重启（`KeepAlive`、`ThrottleInterval`）；停止时发送 SIGTERM，15 秒后强杀（`ExitTimeOut`）。
- 日志写入 `~/Library/Logs/reverse-ssh-tunnel.log`，不自动轮转；程序只在启动失败和断线时写日志，体积很小。
- 代理运行在登录用户的 `gui/<uid>` 域：用户登录后自动启动，注销后停止。需要不登录就运行时，应改为 `/Library/LaunchDaemons` 下带 `UserName` 的守护进程（需要管理员权限），本仓库未提供。
- `make stop` 卸载代理，下次登录仍会自动加载；`make disable` 在 launchd 中持久禁用，之后 `make start` 会失败，需先 `make enable`。

launchd 流程仅在 Linux 上用替身 `launchctl` 验证过命令顺序和生成的 plist，未在真实 macOS 上运行。

## 运维

```bash
make help     # 默认命令
make build    # 构建到 bin/
make test     # 运行本地测试，不连接真实远端
make test-integration  # 在容器中的真实 sshd 上运行集成测试
make status   # 查看服务状态
make logs     # 实时日志，Ctrl-C 退出
make restart  # 构建、重新加载服务定义并重启
make stop     # 停止，保留自启
make start    # 启动
make disable  # 禁用自启并停止
make enable   # 启用自启并启动
```

服务命令由 `deploy/service.sh` 按 `uname -s` 选择 systemd 或 launchd。可使用 `make -C ~/ssh-tunnel <命令>`。代码、配置或服务定义修改后执行 `make restart` 生效。服务管理器负责自动重启；Go 程序负责 SSH 连接、保活、远端和本地监听、双向转发和退出时释放连接。

也可以前台运行（不要与服务同时占用相同端口）：

```bash
./bin/reverse-ssh-tunnel ./tunnel.json
```

## 集成测试

`make test-integration` 编译程序，并让它连接容器中的真实 OpenSSH sshd。每个用例启动一个全新的容器，用完自动删除；密钥、主机密钥和端口都随机生成，不使用本机的 SSH 配置。sshd 镜像按 `integration/sshd` 的内容打标签，内容不变时复用已有镜像。

覆盖的场景：

- 转发：多条映射上的并发大流量与半关闭、目标拒绝连接、SIGTERM 正常退出并释放远端端口、日志不泄露地址和路径；远端转发与本地转发同时工作，本地转发经服务器连到容器内的 sshd。
- 保活：正常连接长时间不误判；sshd 会话被 SIGSTOP 冻结（TCP 仍在）时因保活超时退出；sshd 会话被杀或容器被杀时退出。上述异常都以退出码 1 退出，由服务管理器重启。
- 清理：杀掉登录用户自己的占用进程；不杀其他用户的进程并报错；关闭清理时端口被占即报错；客户端失联的旧会话由 sshd 的 `ClientAliveInterval` 回收后，新隧道能接管端口。
- 启动：主机密钥不匹配、服务器不可达。

需要 podman 或 docker（自动选择，也可用 `CONTAINER_RUNTIME` 指定）。可选环境变量：

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

在 cgroup v1 的机器上，非 root 运行的 podman 可能还需要 `~/.config/containers/containers.conf` 中设置 `cgroups = "disabled"` 和 `cgroup_manager = "cgroupfs"`，镜像构建需要 `--isolation=chroot`。

## 隐私边界

进程参数只包含程序和配置文件路径，不包含具体远端地址、用户、密钥路径或转发参数。常规日志不输出这些配置及底层错误中的地址；这也意味着排障信息较简略。

这不防御本机管理员读取文件、内存或观察网络连接。`tunnel.json`、密钥内容和 known_hosts 都不纳入 Git。不要把口令、私钥等秘密写入仓库。
