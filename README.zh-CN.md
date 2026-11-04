# gst

[English](README.md) | 简体中文

`gst`（go ssh tunnel）让 SSH 端口转发在后台持续运行：登录后自动启动，断线后自动重连。它按你的 `~/.ssh/config` 连接服务器，转发的写法与 `ssh -R` / `ssh -L` / `ssh -D` 相同。支持 Linux 和 macOS。

- **tunnel**：一个有名字的 SSH 连接，例如 `myhost`。
- **forward**：tunnel 上的一条端口转发。
  - `-R [bind_address:]port[:host:hostport]`：在服务器上监听，由本机连接 `host:hostport`。省略目标即为反向 SOCKS5 代理，由本机连接客户端请求的地址。
  - `-L [bind_address:]port:host:hostport`：在本机监听，由服务器连接 `host:hostport`。
  - `-D [bind_address:]port`：在本机监听一个 SOCKS5 代理，由服务器连接客户端请求的地址。
  - 固定目标的 `-R` 和 `-L` 的任意一端都可以用 Unix socket 路径代替端口或 `host:hostport`。SOCKS 代理只支持 TCP 监听，不支持 Unix socket。

## 安装

```bash
go install github.com/yangtau/ssh-tunnel/cmd/gst@latest
```

使用 Nix（需启用 flakes）：

```bash
nix profile add github:yangtau/ssh-tunnel
```

也可以在项目目录中执行：

```bash
nix build                 # result/bin/gst
nix run . -- --help
nix profile add .         # 从本地项目安装
```

Nix 包附带 Bash、Zsh 和 Fish 的补全脚本。

运行 `gst version` 查看已安装的版本，例如输出 `gst version 0.2.0`。

后台 daemon 会记住 gst 的路径。升级或移动 gst 后，执行 `gst restart --all`，用新的 gst 重启 daemon；任何启动、停止、重启或修改 tunnel 的命令也会这样做。

## 使用前准备

gst 在后台运行，不能输入口令，也不能确认未知的主机密钥，所以需要：

1. **一个没有口令的私钥**，并已授权到服务器上。建议单独创建一个：

   ```bash
   ssh-keygen -t ed25519 -N '' -f ~/.ssh/gst_ed25519
   ssh-copy-id -i ~/.ssh/gst_ed25519 user@server.example.com
   ```

2. **`~/.ssh/config` 里的 Host**，例如：

   ```
   Host myhost
     HostName server.example.com
     User user
     IdentityFile ~/.ssh/gst_ed25519
   ```

3. **known_hosts 里有服务器的主机密钥**：先用 `ssh myhost` 连接一次，核对并接受主机密钥。

## 快速开始

```bash
# 添加 tunnel 并在后台启动：在服务器的 2222 端口访问本机的 SSH
gst add myhost -R 2222:localhost:22

# 再加一条 forward：在本机的 15432 端口访问服务器上的数据库
gst forward add myhost -L 15432:127.0.0.1:5432

gst list           # 查看所有 tunnel 及状态
gst check myhost   # 逐步诊断连接问题
gst logs myhost -f # 查看日志
gst stop myhost    # 停止，保留配置
gst start myhost   # 重新启动
gst remove myhost  # 停止并删除
```

每个命令都可以用 `--help` 查看说明和示例。在终端中，帮助的标题加粗，命令名和选项名显示为青色。管道或重定向输出保持纯文本；设置 `NO_COLOR=1` 或 `TERM=dumb` 也可以关闭终端中的配色。`gst completion bash|zsh|fish` 生成 shell 补全脚本。

## 命令

| 命令 | 作用 |
| --- | --- |
| `gst add <name> [destination] [-R spec]... [-L spec]... [-D port]...` | 添加 tunnel 并在后台启动 |
| `gst set <name> [flags]` | 修改目标地址或连接设置 |
| `gst forward add <tunnel> (-R spec \| -L spec \| -D [bind:]port)...` | 添加 forward |
| `gst forward remove <tunnel> (-R \| -L \| -D [bind:]port\|socket)...` | 删除 forward |
| `gst forward list <tunnel>` | 列出 forward |
| `gst remove <name>...` | 停止并删除 tunnel |
| `gst start\|stop\|restart <name>... \| --all` | 启动、停止、重启 |
| `gst list` | 列出 tunnel 及状态 |
| `gst show <name>` | 查看 tunnel 的设置和 ssh 解析结果 |
| `gst logs <name> [-f] [-n N]` | 查看日志 |
| `gst check <name>` | 诊断：ssh 配置、私钥、known_hosts、网络、登录 |
| `gst ui [--port N] [--no-open]` | 在浏览器中查看 tunnel、peer link 和 mesh 拓扑图，并可启动、停止、重启 tunnel |
| `gst mesh enable [--name name]` / `gst mesh disable` | 重新加入（或改名）、退出由多台机器组成的 mesh |
| `gst mesh peers` | 列出 peer link |
| `gst mesh nodes` | 显示 mesh 中的所有机器以及它们之间的 tunnel |
| `gst add <name> mesh:<machine> [-L spec]... [-D port]...` | 添加一条经由其他机器到达 mesh 中某台机器的 tunnel |
| `gst connect <machine> <port>` | 把标准输入输出连到 mesh 中某台机器的端口，可用作 `ProxyCommand` |
| `gst version` | 查看已安装的版本 |

修改运行中 tunnel 的 forward 或设置后，gst 会自动重启它；加 `--no-restart` 可以跳过。设置没有变化时什么也不做。

## 常见用法

```bash
# 目标地址不在 ssh 配置里时，直接写出来
gst add db admin@db.example.com -L 15432:127.0.0.1:5432
gst add web ssh://deploy@203.0.113.7:2200 -R 8080:localhost:3000

# Unix socket：在本机 socket 上访问服务器的 PostgreSQL socket，
# 在服务器的 socket 上访问本机的 ssh-agent
gst forward add myhost -L ~/.gst/db.sock:/run/postgresql/.s.PGSQL.5432
gst forward add myhost -R /home/user/agent.sock:"$SSH_AUTH_SOCK" --no-restart
gst set myhost --cleanup-listeners

# 在本机 1080 端口开一个 SOCKS5 代理，连接从服务器发出
gst forward add myhost -D 1080
curl --proxy socks5h://127.0.0.1:1080 https://intranet.example.com

# 在服务器的 1080 端口开一个反向 SOCKS5 代理，连接从本机发出
gst forward add myhost -R 1080
# 在服务器上执行 curl，通过代理访问；域名由本机解析
ssh myhost 'curl --proxy socks5h://127.0.0.1:1080 https://intranet.example.com'

# 在服务器的所有网卡上监听
gst forward add web -R '*:8080:localhost:3000'

# 删除 forward：只需要方向和监听地址
gst forward remove myhost -R 2222

# 覆盖 ssh 配置的设置；'' 或 0 表示恢复默认
gst set myhost --keepalive-interval 30s --keepalive-count 5
gst set myhost --identity ''

# 开关远端端口清理（见下文）
gst set myhost --cleanup-listeners
gst set myhost --cleanup-listeners=false
```

## forward 的写法

- 省略 `bind_address` 时只监听 loopback（`127.0.0.1`）；`*` 或空的 `bind_address`（如 `:8080:localhost:3000`）监听所有网卡；IPv6 地址加方括号。
- `-R [bind_address:]port[:host:hostport]` 的目标可选：省略时为反向 SOCKS5，提供时保持原有的固定目标转发行为。`-L` 必须有目标，`-D` 不接受目标。
- 固定目标的 `host` 由连接它的一方解析：`-L` 中的 `127.0.0.1` 指服务器自己，`-R` 中的 `localhost` 指本机。
- 服务器上的实际监听范围还受 sshd 的 `GatewayPorts` 控制。监听所有网卡时，记得用防火墙限制访问来源。
- sshd 的 `AllowTcpForwarding` 需要允许所用的方向：两个方向都用时设为 `yes`，只用 `-R` 时可以是 `remote`，只用 `-L` 和 `-D` 时可以是 `local`。

## Unix socket

- 与 ssh 一样，含 `/` 的字段是 Unix socket 路径：`-L /tmp/db.sock:127.0.0.1:5432`、`-L 15432:/run/postgresql/.s.PGSQL.5432`、`-R /home/user/agent.sock:/tmp/agent.sock`。路径中不能有 `:`。
- 本机路径可以是相对路径或以 `~` 开头，gst 会保存为绝对路径；服务器上的路径必须是绝对路径。
- gst 在本机创建的 socket 只有你自己能访问（权限 `0600`）。开始监听前，gst 会替换没有进程在接受连接的 socket 文件（例如崩溃后留下的）；不会替换其他文件或正在使用的 socket。
- 服务器 sshd 的 `AllowStreamLocalForwarding` 需要允许所用的方向，默认允许。
- **sshd 会把服务器上的 `-R` socket 文件留下**，即使 tunnel 正常停止也是如此，而且不会在已存在的文件上监听。如果不做以下任一项，`-R` socket forward 从第一次重启或重连起就会报 `listener start failed`：在服务器的 sshd 配置中设置 `StreamLocalBindUnlink yes`，或开启 `--cleanup-listeners`（见[排查问题](#排查问题)）。

## SOCKS 代理（`-D` 和不带目标的 `-R`）

- `-D` 在本机监听，由 SSH 服务器发起连接；不带目标的 `-R` 在 SSH 服务器上监听，由运行 gst 的本机发起连接。两者都只支持 TCP 监听，不支持动态 Unix socket。
- 两者都与 `ssh -D` 一样，支持无认证的 SOCKS5 `CONNECT`。不支持 UDP（`UDP ASSOCIATE`）、`BIND` 和 SOCKS4，所以 UDP DNS 查询、QUIC 等 UDP 流量无法通过代理。
- 域名在哪里解析取决于客户端。使用 `socks5h://` 时，curl 发送域名：`-D` 由 SSH 服务器解析，不带目标的 `-R` 由运行 gst 的本机解析，因此只在相应一侧网络中才能解析的域名也能访问。使用 `socks5://` 时，curl 在自身运行的机器上解析域名，只发送 IP 地址。
- 能访问监听地址的人都能使用代理，访问发起连接一侧可达的网络——反向 SOCKS 会开放对本机所在网络的访问。除非用防火墙限制访问来源，否则请保持默认的 loopback 地址。
- 不支持代理的程序，可以用 tun2socks 等工具把 TUN 设备上的 TCP 流量导入代理；gst 不创建 TUN 设备，也不配置路由。这些程序的 DNS 查询仍在程序运行的机器上解析，除非另外配置 DNS。

## ssh 配置

`destination` 可以是 `host`、`user@host` 或 `ssh://user@host:port`，省略时等于 tunnel 名。每次启动 tunnel 时，gst 都会用 `ssh -G` 重新读取 ssh 配置，所以修改 `~/.ssh/config` 后执行 `gst restart <name>` 即可生效。

| ssh 配置 | 用途 | gst 的覆盖参数 | 都未设置时 |
| --- | --- | --- | --- |
| `HostName`、`Port`、`User` | 服务器地址和登录用户 | `gst set --destination` | ssh 的默认值 |
| `IdentityFile` | 私钥，依次尝试，跳过不存在或有口令的 | `--identity` | ssh 的默认私钥列表 |
| `UserKnownHostsFile`、`GlobalKnownHostsFile` | 主机密钥校验 | `--known-hosts` | ssh 的默认文件 |
| `ConnectTimeout` | 连接和登录超时 | `--connect-timeout` | 15 秒 |
| `ServerAliveInterval` | 保活间隔 | `--keepalive-interval` | 15 秒 |
| `ServerAliveCountMax` | 连续多少个间隔无响应后断开重连 | `--keepalive-count` | 3 |

限制：

- 只使用没有口令的私钥，不使用 ssh-agent。
- 严格校验 known_hosts，不接受未知主机。
- 不支持 `ProxyJump`、`ProxyCommand`、`HostKeyAlias`。

## 后台运行

- 所有启动的 tunnel 都由一个后台进程（gst daemon）运行。它是一个用户级服务：Linux 上是 systemd 用户服务 `gst.service`，macOS 上是 launchd 代理 `local.gst`。启动第一个 tunnel 时 gst 安装它，没有任何已启动的 tunnel 时删除它，不需要手动管理。
- gst 命令通过只有你能访问的 socket 与 daemon 通信：Linux 上是 `~/.local/state/gst/gst.sock`（设置了 `$XDG_STATE_HOME` 时在其下），macOS 上是 `~/Library/Application Support/gst/gst.sock`。
- tunnel 失败后 5 秒自动重连，没有次数限制。daemon 本身退出后，服务会在 5 秒后重启它。
- `gst list` 中的状态：`running` 已连接且所有 forward 都在监听；`connecting` 正在连接；`down` 失败后正在等待重连，用 `gst logs` 和 `gst check` 排查；`stopped` 已停止。对未在运行的 tunnel，`gst show` 和 `gst list --json` 会给出最近一次错误。
- 每个 tunnel 有自己的日志：Linux 上在 `~/.local/state/gst/logs/<name>.log`，macOS 上在 `~/Library/Logs/gst/<name>.log`。超过 1 MiB 时 gst 会把它改名为 `<name>.log.1`（覆盖更早的一份），所以每个 tunnel 的日志最多约 2 MiB；`gst remove` 会删除它们。为保护隐私，日志不记录服务器地址、用户、密钥路径和转发地址，forward 按编号记录（如 `forward 2`），编号与 `gst forward list` 一致。
- daemon 自身的日志：Linux 上在 journald（`journalctl --user -u gst`），macOS 上在 `~/Library/Logs/gst-daemon.log`。
- 从 gst 0.1 升级：0.1 中每个 tunnel 是单独的服务（`gst-<name>.service`、`local.gst.<name>`）。升级后第一个读取配置的 gst 命令会把有服务的 tunnel 交给 daemon 运行并删除这些服务，其余 tunnel 标记为已停止。
- Linux 上，要让 tunnel 在注销后继续运行、开机后自动启动，需要执行 `loginctl enable-linger "$USER"`；gst 发现没有开启时会提示。macOS 上，tunnel 在登录后启动、注销后停止。

## 连接多台机器（mesh）

多台机器上的 gst 会自动互相连接，不需要任何设置：有 tunnel 的机器自动加入 mesh，每条 tunnel 都会通过它的 SSH 连接，把本机的 gst 和服务器上的 gst 连起来；服务器上的 gst 在第一次被连接时也自动加入。双方都能在 `gst mesh peers` 中看到对方。相连的机器会互相转告各自知道的信息，所以在任意一台机器上运行 `gst mesh nodes` 都能看到整个网络：laptop 有到 `mini` 和 `devcn` 的 tunnel 时，在 mini 上运行 `gst mesh nodes` 也能看到 laptop 和 devcn，mini 也能经由 laptop 连接 devcn。

```bash
gst mesh peers                  # 本机的 link
gst mesh nodes                  # 所有机器以及它们之间的 tunnel
gst mesh enable --name laptop   # 可选：修改显示的节点名（默认取主机名）
```

- 建立 link 需要服务器上有 gst 0.2 或更高版本。gst 通过 SSH 在登录 shell 中执行 `gst peer-bridge`；如果该 shell 的 `PATH` 里找不到 gst，用 `--remote-gst` 指定路径，例如 `gst set myhost --remote-gst ~/.nix-profile/bin/gst`。没有 gst 或执行了 `gst mesh disable` 的服务器仍会作为 tunnel 能到达的机器出现在 mesh 中；它的 link 保持 `down`，重试间隔逐渐拉长，最长 5 分钟一次。
- `gst set <name> --no-mesh` 让这条 tunnel 不在服务器上运行 gst，例如服务器对每个会话都执行强制命令时；`--no-mesh=false` 恢复。
- 加入 mesh 后，即使没有启动任何 tunnel，daemon 也会保持运行，以便接受连接。
- link 的状态为 `connecting`、`up` 或 `down`。`gst mesh peers` 会显示 link 断开的原因。link 失败不影响 tunnel 的 forward。
- `gst mesh nodes` 中，gst 节点显示节点名，没有 gst 的 SSH 服务器显示连向它的 tunnel 名。服务器上的 gst 之后加入 mesh 时，它会显示为该 gst 节点。从本机经由运行中的 tunnel 能到达的机器标记为可达；要继续经过某台机器往下走，需要与它的 peer link 处于 up 状态。退出网络的机器会在 30 分钟内消失。
- peer 之间相互信任：link 以你的用户身份登录服务器，服务器的 daemon 只通过自己的 socket 接受它。因此你 tunnel 连到的任何一台运行 gst 的服务器，都能经由 mesh 反向访问你的机器所能到达的地方；对不完全可信的服务器使用 `--no-mesh`，或在本机执行 `gst mesh disable`。
- `gst mesh disable` 让本机退出 mesh：关闭所有 link，不再自动加入，没有启动的 tunnel 时 daemon 也会停止。`gst mesh enable` 重新加入，并生成新的节点 id。

### 经由 mesh 连接

目标为 `mesh:<machine>` 的 tunnel 经由与目标相连的机器到达 `gst mesh nodes` 中的某台机器，本机不需要登录它：由路径上最后一台 gst 发起连接；目标机器没有 gst 时，用那台 gst 自己到它的 tunnel。`localhost` 等目标地址都是相对目标机器而言。`gst connect` 对单个连接做同样的事，使用标准输入输出。

```bash
# 在 mini 上：本机 15432 端口经由 laptop 连到 devcn 的 5432 端口
gst add devcn-db mesh:devcn -L 15432:localhost:5432
gst check devcn-db          # 显示路径：mini → laptop → devcn

# 一个从 devcn 发出连接的 SOCKS5 代理
gst add devcn-proxy mesh:devcn -D 1080

# 经由 mesh ssh 到 devcn，使用你自己在 devcn 上的登录凭据
ssh -o ProxyCommand='gst connect %n 22' devcn
```

- `<machine>` 是 `gst mesh nodes` 中的名字；重名时用 id。`gst connect` 只给端口时表示该机器的 localhost，也可以给 `host:port` 或 Unix socket 的绝对路径。
- 这类 tunnel 只能有 `-L` 和 `-D` forward；SSH 设置中只有 `--connect-timeout` 适用。只要处于启动状态就会运行，不管 mesh 当前是否有可达路径；每个连接按当时最短的路径建立，没有路径时只有该连接失败。`gst check` 会显示路径。
- 路径只能经由 up 状态的 peer link 越过一台机器；最后一段可以是任何运行中、没有设置 `--no-mesh` 的 tunnel，服务器上没有 gst 也可以。
- mesh 中的每个 gst 都可以代表其他节点，连接它所在机器及其 tunnel 能到达的任何地方；只让你信任的机器加入。

### 网页

`gst ui` 在浏览器页面中显示本机的 tunnel、peer link 和 mesh 拓扑图，每隔几秒自动刷新。它在 `127.0.0.1` 上提供页面，直到按 Ctrl-C 为止；启动时打印地址并在浏览器中打开。页面上的按钮可以启动、停止、重启本机的 tunnel，效果与 `gst start`、`stop`、`restart` 相同；其他修改仍使用上面的命令。

```bash
gst ui
# 在没有浏览器的服务器上：指定端口并转发出来
gst ui --port 8370 --no-open
ssh -L 8370:localhost:8370 myhost   # 然后在本机打开打印出的地址
```

- 地址在 `#` 后带有一段随机密钥，页面需要它才能读取数据；`gst ui` 运行期间，拿到完整地址的人都能使用页面。每次运行生成新的密钥。
- 拓扑图按与本机的距离排列机器。蓝色箭头是 peer link 为 up 的 tunnel，灰色是运行中的 tunnel，红色虚线是未运行的 tunnel；箭头从运行 tunnel 的机器指向它的服务器。本机无法到达的机器显示为淡色。

## 排查问题

先运行 `gst check <name>`，它会逐项检查并对失败项给出建议。`add`、`set`、`start`、`restart` 也会先检查私钥和 known_hosts，不通过时不保存、不启动。

| 现象 | 处理 |
| --- | --- |
| `no usable key`、私钥 `passphrase-protected` | 按[使用前准备](#使用前准备)创建无口令私钥，并用 `--identity` 或 ssh 配置的 `IdentityFile` 指定 |
| `no host key for ...` | 用 `ssh <host>` 连接一次，记录主机密钥 |
| 主机密钥不匹配 | 确认服务器密钥确实更换后，执行 `ssh-keygen -R <host>`，再用 `ssh` 连接一次 |
| `-R` 端口被占用（`listener start failed`） | 通常是上一次连接在服务器上残留的会话，见下文 |
| `-R` socket 在重启后报 `listener start failed` | 上一次会话的 socket 文件还留在服务器上；在 sshd 中设置 `StreamLocalBindUnlink yes`，或开启 `--cleanup-listeners` |
| macOS 阻止访问本地网络服务器 | 在“本地网络”权限提示或“系统设置 → 隐私与安全性 → 本地网络”中允许 gst，然后运行 `gst restart <name>`。终端获得授权不一定代表后台服务也获得授权 |

**服务器端口被残留会话占用。** 网络中断后，旧的 SSH 会话可能还占着服务器上的 `-R` 端口。建议在服务器的 sshd 配置中设置 `ClientAliveInterval 15` 和 `ClientAliveCountMax 3`，让 sshd 及时回收失联的会话。

也可以开启 `--cleanup-listeners`：每次连接后、开始监听前，gst 会在服务器上终止属于登录用户、占用这些 `-R` 端口和 socket 的进程，并删除属于登录用户、没有进程监听的 socket 文件。被终止的**不限于 SSH 进程**，所以只能用于专属于这个 tunnel 的端口和 socket。Linux 上残留的 sshd 会话进程无法用这种方式清理。服务器需要在 `PATH` 中提供 `bash`、`lsof`、`ps`、`id`；Linux 上还需要可读的 `/proc/net/tcp` 和 `/proc/net/unix`，其他系统需要 `netstat`。

## 在脚本和 agent 中使用

- 结果输出到 stdout；错误输出到 stderr，以 `error:` 开头，通常还带一行 `hint:`，给出下一步该执行的命令。
- 退出码：`0` 成功，`1` 执行失败（包括 `check` 未通过），`2` 参数无效。
- `list`、`show`、`check`、`forward list`、`mesh peers`、`mesh nodes` 支持 `--json`。
- 所有命令都不需要交互输入。重复添加完全相同的 forward 不会改动任何东西；监听地址相同但 forward 不同则报错。`-L` 和 `-D` 都在本机监听，所以不能共用同一个地址。固定目标和动态（SOCKS）`-R` 都在服务器上监听，同样不能共用监听地址。

## 配置文件

tunnel 保存在 `~/.config/gst/config.json`（设置了 `XDG_CONFIG_HOME` 时为 `$XDG_CONFIG_HOME/gst/config.json`）。可以用 `--config` 或环境变量 `GST_CONFIG` 指定其他路径，daemon 会沿用同一个路径。每个用户只有一个 daemon，所以在另一个配置文件中启动或修改 tunnel，会让 daemon 切换到那个文件。这个文件由 gst 维护，请用 gst 命令修改，不要手动编辑；多个 gst 命令同时执行也是安全的。反向 SOCKS 不需要改变配置文件结构：它保存为目标为空的 `remote` forward。
