# gst

[English](README.md) | 简体中文

`gst`（go ssh tunnel）通过 SSH 把你的多台机器连成一个私有 mesh。体验接近 Tailscale，但只工作在第 4 层：只中转 TCP 连接和 Unix socket，不需要 root，直接使用你已有的 SSH 登录。支持 Linux 和 macOS。

- 每台机器都运行 gst（link 的两端都需要 0.3 或更高版本）和一个小的后台 daemon。
- **link** 是从一台机器到另一台同样运行 gst 的机器的 SSH 连接。相连的机器会互相转告各自知道的信息，所以每台机器都能经由它们之间的 link 到达其他所有机器：`laptop` 有到 `mini` 和 `devcn` 的 link 时，`mini` 经由 `laptop` 就能到达 `devcn`，不需要 `devcn` 的任何凭据。
- 到达一台机器的方式有：`gst connect`；SOCKS5/HTTP 代理，其中 `<machine>.gst` 就是那台机器；或者一个通向它的本地端口或 Unix socket（map）。

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

Nix 包附带 Bash、Zsh 和 Fish 的补全脚本；其他方式安装时用 `gst completion bash|zsh|fish` 生成。

运行 `gst version` 查看已安装的版本，例如输出 `gst version 0.3.0`。

mesh 中的每台机器都要安装 gst，并确保 gst 在 ssh 在该机器上启动的登录 shell 的 `PATH` 中（不在时见 `--remote-gst`）。后台 daemon 会记住 gst 的路径。升级或移动 gst 后，执行 `gst up`，用新的 gst 重启 daemon；任何修改配置的命令也会这样做。

## 使用前准备

gst 在后台运行，不能输入口令，也不能确认未知的主机密钥。对于你要连接的每台机器，需要：

1. **一个没有口令的私钥**，并已授权到那台机器上。建议单独创建一个：

   ```bash
   ssh-keygen -t ed25519 -N '' -f ~/.ssh/gst_ed25519
   ssh-copy-id -i ~/.ssh/gst_ed25519 user@devcn.example.com
   ```

2. **`~/.ssh/config` 里的 Host**，例如：

   ```
   Host devcn
     HostName ssh.example.com
     User user
     IdentityFile ~/.ssh/gst_ed25519
   ```

   也可以不写这一项，直接把目标地址交给 `gst link add`，例如 `user@ssh.example.com`。

3. **known_hosts 里有该机器的主机密钥**：先用 `ssh devcn` 连接一次，核对并接受主机密钥。

4. **那台机器上有 gst 0.3 或更高版本。** 它不需要自己的 link、配置或正在运行的 daemon：被连接就够了，gst 会在第一次 link 时在那里启动。

这些只对创建 link 的机器有要求；被连接的机器只需要 gst 和能执行命令的 sshd（gst 使用登录会话而不是端口转发，所以 sshd 的 `AllowTcpForwarding` 不影响它）。对每个会话都强制执行固定命令的服务器无法被连接。

## 快速开始

在 `laptop` 上连接 `devcn` 和 `mini`：

```bash
gst link add devcn        # ~/.ssh/config 中的 "devcn"
gst link add mini
gst link list             # NAME STATUS DESTINATION PEER；等到状态变为 "running"
gst nodes                 # mesh 中的所有机器，以及如何到达它们
gst status devcn          # devcn 自己的 link、代理、map 和 route
```

`gst nodes` 的输出类似：

```
NAME    ID        SOFTWARE   EXIT  ROUTE
laptop  4f1c9a27  gst 0.3.0  no    this machine
devcn   9db2b03b  gst 0.3.0  no    devcn
mini    c07e5d11  gst 0.3.0  no    mini

FROM    LINK   TO
laptop  devcn  devcn
laptop  mini   mini
```

到达一台机器的几种方式：

```bash
# 一个使用标准输入输出的连接：只给端口时表示 devcn 的 localhost
gst connect devcn 22

# 代理：127.0.0.1:1080 上的 SOCKS5 和 HTTP，devcn.gst 就是 devcn
gst proxy                 # 显示用法
export ALL_PROXY=socks5h://127.0.0.1:1080 http_proxy=http://127.0.0.1:1080 https_proxy=http://127.0.0.1:1080
curl http://devcn.gst:8080/   # devcn 的 8080 端口

# map：本机 15432 端口通向 devcn 的 5432 端口
gst map add 15432 devcn:5432
psql -h 127.0.0.1 -p 15432

# 两端也都可以是 Unix socket
gst map add 18082 devcn:/run/user/1000/app.sock
gst map add /tmp/devcn-docker.sock devcn:/var/run/docker.sock
```

`mini` 现在看到的是同一个 mesh：在 `mini` 上运行 `gst nodes` 会列出 `laptop` 和 `devcn`，`gst connect devcn 22` 经由 `laptop` 连接。

每个命令都可以用 `--help` 查看说明。在终端中，帮助的标题加粗，命令名和选项名显示为青色。管道或重定向输出保持纯文本；设置 `NO_COLOR=1` 或 `TERM=dumb` 也可以关闭终端中的配色。

## 概念

| 概念 | 是什么 | 命令 |
| --- | --- | --- |
| **node** | 一台运行 gst 的机器。有随机的 128 位 id 和名字（默认取主机名） | `gst name`、`gst nodes` |
| **link** | 从本机到另一台 gst 机器的 SSH 连接，在后台保持；两边的 daemon 经由它加入对方的 mesh | `gst link …` |
| **proxy** | 本机 daemon 提供的 SOCKS5 和 HTTP 代理：`<machine>.gst` 和路由命中的主机走 mesh | `gst proxy` |
| **map** | 本机的一个端口或 Unix socket，其连接转到某台机器的端口或 socket；给不能使用代理的程序用 | `gst map …` |
| **route** | 一条规则：把代理中到某些主机的连接经由另一台机器发出 | `gst route …` |
| **exit** | 允许其他机器经由本机连接本机以外的地址 | `gst exit` |

### node 和 link

- node 的名字用于显示和寻址，不要求唯一；id 是唯一的。用 `gst name laptop` 修改名字。名字和 link 名一样，由 1-32 个小写字母、数字或 `-` 组成。
- link 是双向的：流量可以沿任一方向通过，所以 `devcn` 不需要自己的 link 就能到达 `laptop`。link 有名字（`gst link add devcn`），在对端报告自己的名字之前，它也是对端机器的名字。
- 已启动的 link 在后台运行，登录后自动启动，断线后自动重连。相连的机器会互相转告各自知道的信息，所以在任意一台机器上运行 `gst nodes` 都能看到整个 mesh；退出网络的机器会在 30 分钟内消失。路径只能经由运行中的 link 越过一台机器。

### 机器寻址

所有命令和代理都用 `<machine>` 指定机器。它按以下顺序解析，某一步只有在恰好匹配一台机器时才生效：

1. 去掉末尾的 `.gst`。
2. node id。
3. node 名字。
4. link 名，表示该 link 另一端的机器。mesh 中任何机器的 link 都算，所以 `devcn` 在哪里都能用，不论它是 `laptop` 上的 link 名还是 `devcn` 自己的名字。
5. 至少 4 位的 id 前缀。

名字重复时，`gst nodes` 用 id 区分这些机器；此时请使用 id 或 id 前缀。

### 代理

daemon 在同一个端口上提供 SOCKS5 和 HTTP，默认是 `127.0.0.1:1080`。每个连接的去向：

1. 到 `<machine>.gst` 的端口 `p`：连接该机器的 `localhost:p`。
2. 主机命中某条 route：经由该 route 的机器连接，域名由那台机器自己解析，它必须是 exit。
3. 否则：由本机直接连接。

```bash
gst proxy                 # 显示地址和用法
gst proxy 127.0.0.1:1081  # 修改（TCP 地址：端口或 bind:port）
gst proxy off             # 关闭
```

- SOCKS5 支持无认证的 `CONNECT`；域名原样交给路由，等同 `socks5h://`。不支持 UDP、`BIND` 和 SOCKS4，所以 UDP DNS 查询和 QUIC 无法通过。
- HTTP 支持 `CONNECT host:port` 和普通的 `GET http://host/...` 请求。
- 能访问代理地址的人都能使用代理。除非用防火墙限制访问来源，否则请保持默认的 loopback 地址。
- 不支持代理的程序，可以使用 map，或用 tun2socks 等工具把它们的 TCP 流量导入代理；gst 不创建 TUN 设备，也不配置路由。

### map

```bash
gst map add <listen> <machine>:<target>
```

- `<listen>` 是端口（监听 `127.0.0.1`）、`address:port`（`0.0.0.0:8080` 或 `*:8080` 监听所有网卡；IPv6 地址加方括号），或 Unix socket 路径——可以是相对路径或以 `~` 开头，gst 会保存为绝对路径。
- `<target>` 是端口（该机器的 `localhost`）、`host:port`，或该机器上的 socket 绝对路径。路径中不能有 `:`。
- gst 创建的 socket 只有你自己能访问（权限 `0600`）。开始监听前，gst 会替换没有进程在接受连接的 socket 文件（例如崩溃后留下的）；不会替换其他文件或正在使用的 socket。
- 不论机器是否可达，map 都会保持监听；没有路径时只有对应的连接失败。`gst map list` 显示每个监听是否正常。

### route

```bash
gst route add corp.example.com devcn   # 该域名及其所有子域名
gst route add 10.20.0.0/16 devcn       # 一个 IP 网段（或单个 IP）
gst route add default devsg           # 其余所有主机
```

- route 作用于代理中的连接。最具体的规则优先：域名和网段越长越优先，`default` 最后。添加匹配项相同的 route 会替换原有的。
- 域名规则按名字匹配。IP 和网段规则只匹配 IP 地址：gst 不会为了选择路由而解析域名，所以 `curl http://intranet.example.com/` 不会命中 `10.20.0.0/16`，即使该域名解析到这个网段内。
- `via` 机器必须是 exit。`gst route add` 发现 mesh 中它还不是 exit 时会给出提示。
- 没有 route 命中的主机由本机连接。

### exit

默认情况下，一台机器替其他机器只连接它自己上的地址：`localhost`、loopback 地址和 Unix socket。只有 exit 还会连接其他地址，例如它所在的局域网或互联网。在要作为 exit 的机器上执行 `gst exit on` 开启（`gst exit off` 关闭，`gst exit` 查看）。无论经过哪条路径，这条规则都由发起最后一跳连接的机器检查。

### 信任

mesh 中的机器彼此信任。link 以你的用户身份登录，daemon 只通过自己的 socket 接受它。mesh 中任何一台机器都可以请求任何一台机器连接**它自己的** `localhost` 端口和 Unix socket（本地数据库、Docker socket、ssh-agent）。exit 的范围更大：任何机器都可以连接 exit 所能到达的任何地方。只连接你信任的机器，只在需要的机器上开启 exit。`gst down` 让本机退出 mesh。

## 取代 ssh 转发

没有 `-L`、`-R`、`-D`；mesh 覆盖了同样的场景，而且监听的一端可以是任意一台机器。

| ssh | gst |
| --- | --- |
| `ssh -L 15432:127.0.0.1:5432 devcn` | `gst map add 15432 devcn:5432` |
| `ssh -L 15432:db.example.com:5432 devcn` | `gst map add 15432 devcn:db.example.com:5432`，并在 devcn 上执行 `gst exit on` |
| `ssh -L ~/db.sock:/run/postgresql/.s.PGSQL.5432 devcn` | `gst map add ~/db.sock devcn:/run/postgresql/.s.PGSQL.5432` |
| `ssh -R 2222:localhost:22 devcn` | 在 devcn 上：`gst map add 2222 laptop:22` |
| `ssh -R /home/user/agent.sock:$SSH_AUTH_SOCK devcn` | 在 devcn 上：`gst map add /home/user/agent.sock laptop:"$SSH_AUTH_SOCK"`（绝对路径） |
| `ssh -D 1080 devcn` | `gst route add default devcn`（也可以只路由部分域名或网段），并在 devcn 上执行 `gst exit on`；代理本来就在 `127.0.0.1:1080` |
| 反向代理：devcn 上的程序使用 laptop 的网络 | 在 devcn 上：`gst route add default laptop`，并在 laptop 上执行 `gst exit on` |

devcn 上这种相当于 `-R` 的 map 之所以可行，是因为 mesh 中每台机器都能经由 mesh 到达其他所有机器，所以 `laptop` 不需要自己的监听或开放端口。崩溃后服务器上不会留下失效的 `-R` 端口或 socket 文件，`GatewayPorts` 也不适用。

## 命令

命令的分组与 `gst --help` 一致。机器名和 link 名见[概念](#概念)。

### 构建 mesh

| 命令 | 作用 |
| --- | --- |
| `gst link add <name> [destination] [settings]` | 添加 link 并在后台启动 |
| `gst link set <name> [--destination d] [settings] [--no-restart]` | 修改 link 的目标地址或设置；运行中的 link 会重启以生效 |
| `gst link remove <name>...`（`rm`） | 停止并删除 link，连同日志 |
| `gst link start\|stop\|restart <name>... \| --all` | 启动、停止（保留配置）、重启 link |
| `gst link list`（`ls`） | 列出 link 及状态：NAME、STATUS、DESTINATION、PEER |
| `gst link show <name>` | 查看 link 的设置和 ssh 对目标地址的解析结果 |
| `gst link logs <name> [-f] [-n N]` | 查看日志 |
| `gst link check <name>` | 逐步诊断：ssh 配置、私钥、known_hosts、网络、登录、对方机器上的 gst |
| `gst nodes` | 显示本机所知道的 mesh 中所有机器：NAME、ID、SOFTWARE、EXIT、ROUTE（连接途经的机器），然后是机器之间的 link |
| `gst status [machine]` | 显示本机或 mesh 中另一台机器的 link（含状态、目标和对端）、代理、map 和 route；另一台机器经由 mesh 查询 |

`destination` 可以是 `host`、`user@host` 或 `ssh://user@host:port`，省略时等于 link 名。link 的设置会覆盖你的 ssh 配置（见 [ssh 配置](#ssh-配置)）：

| 参数 | 含义 |
| --- | --- |
| `--identity <file>` | 没有口令的私钥文件 |
| `--known-hosts <file>` | known_hosts 文件 |
| `--connect-timeout <duration>` | 连接和登录的超时，须为整数秒，例如 `20s` |
| `--keepalive-interval <duration>` | 保活探测的间隔，须为整数秒 |
| `--keepalive-count <n>` | 探测连续这么多个间隔无响应后放弃 |
| `--remote-gst <command>` | gst 不在对方机器登录 shell 的 `PATH` 中时，对方机器上的 gst 命令，例如 `~/.nix-profile/bin/gst` |

`gst link set` 中，`''` 或 `0` 表示恢复默认（`gst link set devcn --identity ''`）。设置没有变化时什么也不做；`--no-restart` 只保存修改，不重启运行中的 link。

link 的状态：`running`（两边的 daemon 已相连）、`connecting`、`down`（失败后正在重试；会显示最近一次错误，包括对方 gst 报告的错误的第一行）、`stopped`。失败的 link 5 秒后重试，之后每次翻倍，最长 5 分钟一次，进入 running 后重新计时。

### 到达机器

| 命令 | 作用 |
| --- | --- |
| `gst connect <machine> <port \| host:port \| socket>` | 把标准输入输出连到某台机器的端口或 socket，例如用作 ssh 的 `ProxyCommand` |
| `gst proxy [address \| off]` | 查看或修改代理 |
| `gst map add <listen> <machine>:<target>` | 把本机的端口或 socket 映射到某台机器 |
| `gst map remove <listen>...`（`rm`） | 删除 map |
| `gst map list`（`ls`） | 列出 map 及是否在监听 |
| `gst route add <match> <machine>` | 把到匹配主机的连接经由某台机器发出 |
| `gst route remove <match>...`（`rm`） | 删除 route |
| `gst route list`（`ls`） | 列出 route |

`<match>` 是 `default`、域名、IP 地址或 CIDR 网段。不属于该机器自己的 `host:port` 需要该机器是 exit。

### 本机

| 命令 | 作用 |
| --- | --- |
| `gst name [name]` | 查看或修改本机的名字，并显示它的 id |
| `gst exit [on \| off]` | 查看或修改本机是否为 exit |
| `gst up` | 现在运行 daemon，并在登录时自动运行 |
| `gst down` | 停止 daemon 并不再启动；其他机器无法连接本机 |
| `gst ui [--port N] [--no-open]` | 在浏览器中查看 mesh，以及任一机器的 link、代理、map 和 route |
| `gst version` | 查看 gst 版本 |

所有命令都接受 `--config <file>`。`list`、`show`、`check`、`nodes`、`status`、`proxy`、`map list`、`route list` 支持 `--json`。

## ssh 配置

每次 link 连接时，gst 都会用 `ssh -G` 重新解析它的目标地址，所以修改 `~/.ssh/config` 后执行 `gst link restart <name>` 即可生效。

| ssh 配置 | 用途 | gst 的覆盖参数 | 都未设置时 |
| --- | --- | --- | --- |
| `HostName`、`Port`、`User` | 地址和登录用户 | `gst link set --destination` | ssh 的默认值 |
| `IdentityFile` | 私钥，依次尝试，跳过不存在或有口令的 | `--identity` | ssh 的默认私钥列表 |
| `UserKnownHostsFile`、`GlobalKnownHostsFile` | 主机密钥校验 | `--known-hosts` | ssh 的默认文件 |
| `ConnectTimeout` | 连接和登录超时 | `--connect-timeout` | 15 秒 |
| `ServerAliveInterval` | 保活间隔 | `--keepalive-interval` | 15 秒 |
| `ServerAliveCountMax` | 连续多少个间隔无响应后断开重连 | `--keepalive-count` | 3 |

优先级：gst 参数，然后是 ssh 配置，最后是默认值。

限制：

- 只使用没有口令的私钥，不使用 ssh-agent。
- 严格校验 known_hosts，不接受未知主机。
- link 不支持 `ProxyJump`、`ProxyCommand`、`HostKeyAlias`。要到达另一台机器背后的机器，请在两者上都运行 gst，经由前面的那台建立 link。

## 后台运行

- 每个用户有一个后台进程，即 gst daemon，运行所有已启动的 link、代理和 map，并接受其他机器的 link。它是一个用户级服务：Linux 上是 systemd 用户服务 `gst.service`，macOS 上是 launchd 代理 `local.gst`。第一次修改配置时，或者另一台机器第一次连接本机时，gst 会安装它，不需要手动管理。`gst down` 停止它并删除服务，`gst up` 恢复。
- gst 命令通过只有你能访问的 socket 与 daemon 通信：Linux 上是 `~/.local/state/gst/gst.sock`（设置了 `$XDG_STATE_HOME` 时在其下），macOS 上是 `~/Library/Application Support/gst/gst.sock`。
- daemon 本身退出后，服务会在 5 秒后重启它。
- 每个 link 有自己的日志：Linux 上在 `~/.local/state/gst/logs/<name>.log`，macOS 上在 `~/Library/Logs/gst/<name>.log`，用 `gst link logs` 查看。超过 1 MiB 时 gst 会把它改名为 `<name>.log.1`（覆盖更早的一份），所以每个 link 的日志最多约 2 MiB；`gst link remove` 会删除它们。daemon 自身的日志：Linux 上在 journald（`journalctl --user -u gst`），macOS 上在 `~/Library/Logs/gst-daemon.log`。
- 为保护隐私，日志不记录服务器地址、用户、密钥路径和目标地址，也不记录对方 gst 返回的错误。这些只出现在交互式输出中：`gst link list`、`gst link check`、`gst nodes` 和 `gst ui`。
- Linux 上，要让 link 在注销后继续运行、开机后自动启动，需要执行 `loginctl enable-linger "$USER"`；gst 发现没有开启时会提示。macOS 上，gst 在登录后启动、注销后停止。
- 服务运行的是 `gst daemon`，link 经 SSH 在对方机器上运行 `gst peer-bridge`；这两个都不是需要你自己执行的命令。

### 网页

`gst ui` 在浏览器页面中显示 mesh 的拓扑图，以及本机的 link、代理、map 和 route；选中 mesh 中的另一台机器，也能看到它的这些内容（与 `gst status <machine>` 相同）。页面每隔几秒自动刷新。它在 `127.0.0.1` 上提供页面，直到按 Ctrl-C 为止；启动时打印地址并在浏览器中打开。页面上的按钮可以启动、停止、重启本机的 link，效果与 `gst link start`、`stop`、`restart` 相同；其他修改仍使用上面的命令。

```bash
gst ui
# 在没有浏览器的机器上：指定端口并转发出来
gst ui --port 8370 --no-open
ssh -L 8370:localhost:8370 devcn   # 然后在你自己的机器上打开打印出的地址
```

地址在 `#` 后带有一段随机密钥，页面需要它才能读取数据；`gst ui` 运行期间，拿到完整地址的人都能使用页面。每次运行生成新的密钥。

## 从 0.2 升级

gst 0.3 用上面的 mesh 模型取代了 tunnel 和 forward。配置会自动转换：第一个读取 0.2 配置的 gst 命令会改写它，不需要你做任何事。

- 每个 tunnel 变成同名的 link；目标为 `mesh:<machine>` 的 tunnel 没有对应的 link。
- 每个 `-L` forward 变成指向该 tunnel 所连机器的 map（`mesh:` tunnel 则指向其中的 `<machine>`）。如果它的目标不在那台机器上，请在那里执行 `gst exit on`。
- `-R` 和 `-D` forward 会被丢弃，并逐条在 stderr 列出，例如 `note: dropped tunnel myhost: -R 2222:localhost:22; recreate it with gst map on the other machine, or with gst route`。按[取代 ssh 转发](#取代-ssh-转发)重建：`-R` 在对方机器上建 map，`-D` 用代理加 route。
- 其他机器也需要 gst 0.3。gst 协议现在是版本 2，gst 拒绝与更旧的版本通信，并提示 `speaks an older gst protocol; upgrade gst there to 0.3 or later`。请升级你连接的每一台机器。
- `gst mesh …`、`gst add`、`gst list` 等 0.2 的命令都已移除，见[命令](#命令)。升级后重启 daemon 用 `gst up`，不再是 `gst restart --all`。

## 排查问题

先运行 `gst link check <name>`，它会逐项检查并对失败项给出建议。`link add`、`link set`、`link start`、`link restart` 也会先检查私钥和 known_hosts，不通过时不保存、不启动。link 为 `down` 时，`gst link list` 会显示最近一次错误。

| 现象 | 处理 |
| --- | --- |
| `no usable key`、私钥 `passphrase-protected` | 按[使用前准备](#使用前准备)创建无口令私钥，并用 `--identity` 或 ssh 配置的 `IdentityFile` 指定 |
| `no host key for ...` | 用 `ssh <host>` 连接一次，记录主机密钥 |
| 主机密钥不匹配 | 确认该机器的密钥确实更换后，执行 `ssh-keygen -R <host>`，再用 `ssh` 连接一次 |
| link 为 `down`，错误来自对方 gst，例如 command not found | gst 不在 ssh 在那里启动的登录 shell 的 `PATH` 中。安装它，或指定路径：`gst link set devcn --remote-gst ~/.nix-profile/bin/gst` |
| link 为 `down`，错误是 `gst is down on this machine; run gst up there` | 在对方机器上执行 `gst up` |
| `speaks an older gst protocol; upgrade gst there to 0.3 or later` | 升级它指出的那台机器上的 gst（每条 link 的两端都要 0.3） |
| `... is not an exit, so it connects only to addresses on its own machine` | 目标不在你途经的那台机器上。在那里执行 `gst exit on`，或改为连接它自己的 `localhost` |
| `no route to ...: no running links lead there` | 没有运行中的 link 把本机和那台机器连起来。检查 `gst nodes`（机器为 `unreachable` 或缺失）和 `gst link list` |
| 代理或 map 报 `cannot listen on ...` | 地址被其他程序占用：用 `gst proxy <address>` 或 `gst map` 换一个地址，或释放它，然后执行 `gst up` |
| `gst status <machine>` 失败 | 那台机器必须可达，并运行 0.3 或更新版本的 gst 且 daemon 在运行；用 `gst nodes` 查看到它的路径 |
| `the gst daemon does not answer` | 执行 `gst up`；查看 daemon 日志（`journalctl --user -u gst` 或 `~/Library/Logs/gst-daemon.log`） |
| 机器名匹配到多台机器 | 使用 `gst nodes` 中的 id 或 id 前缀 |
| macOS 阻止访问本地网络服务器 | 在“本地网络”权限提示或“系统设置 → 隐私与安全性 → 本地网络”中允许 gst，然后运行 `gst link restart <name>`。终端获得授权不一定代表后台服务也获得授权 |

## 在脚本和 agent 中使用

- 结果输出到 stdout；错误输出到 stderr，以 `error:` 开头，通常还带一行 `hint:`，给出下一步该执行的命令。不是错误的提示以 `note:` 开头。
- 退出码：`0` 成功，`1` 执行失败（包括 `link check` 未通过），`2` 参数无效。
- `link list`、`link show`、`link check`、`nodes`、`status`、`proxy`、`map list`、`route list` 的 `--json` 字段是稳定的。
- 所有命令都不需要交互输入。添加匹配项相同的 route 会替换原有的；为已映射的地址再添加 map 会报错。代理和 map 不能共用同一个监听地址。

## 配置文件

配置保存在 `~/.config/gst/config.json`（设置了 `XDG_CONFIG_HOME` 时为 `$XDG_CONFIG_HOME/gst/config.json`），包括本机的 node（id、名字、exit）、代理地址、link、map 和 route。可以用 `--config` 或环境变量 `GST_CONFIG` 指定其他路径，daemon 会沿用同一个路径。每个用户只有一个 daemon，所以修改另一个配置文件会让 daemon 切换到那个文件。这个文件由 gst 维护，请用 gst 命令修改，不要手动编辑；多个 gst 命令同时执行也是安全的。
