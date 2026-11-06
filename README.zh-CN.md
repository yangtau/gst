# gst

[English](README.md) | 简体中文

`gst`（go ssh tunnel）通过 SSH 把你的多台机器连成一个私有网络。体验接近 Tailscale，直接使用你已有的 SSH 登录：每台机器都在一个 WireGuard 网络上拥有 IP 地址和名字 `<machine>.gst`，所有程序和协议都能用。网络允许时，包经 UDP 直连，否则经 SSH link 传送。支持 Linux 和 macOS，每台机器需要用一次 root。

- 每台机器都运行 gst（所有机器都需要 0.4 或更高版本）和一个小的后台 daemon。
- **link** 是从一台机器到另一台同样运行 gst 的机器的 SSH 连接。相连的机器会互相转告各自知道的信息，所以每台机器都能经由它们之间的 link 到达其他所有机器：`laptop` 有到 `homelab` 和 `devbox` 的 link 时，`homelab` 经由 `laptop` 就能到达 `devbox`，不需要 `devbox` 的任何凭据。
- link 只负责发现机器、传递机器之间的信息。你的程序发出的所有内容都是端到端的 WireGuard，走两条路之一：两台机器之间经 link 找到的 UDP 路径，或者没有这种路径时的 SSH link。

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

运行 `gst version` 查看已安装的版本，例如输出 `gst version 0.4.0`。

mesh 中的每台机器都要安装 gst，并确保 gst 在 ssh 在该机器上启动的登录 shell 的 `PATH` 中（不在时见 `--remote-gst`）。后台 daemon 会记住 gst 的路径。升级或移动 gst 后，执行 `gst up`，用新的 gst 重启 daemon；任何修改配置的命令也会这样做。

## 使用前准备

gst 在后台运行，不能输入口令，也不能确认未知的主机密钥。对于你要连接的每台机器，需要：

1. **一个没有口令的私钥**，并已授权到那台机器上。建议单独创建一个：

   ```bash
   ssh-keygen -t ed25519 -N '' -f ~/.ssh/gst_ed25519
   ssh-copy-id -i ~/.ssh/gst_ed25519 user@devbox.example.com
   ```

2. **`~/.ssh/config` 里的 Host**，例如：

   ```
   Host devbox
     HostName ssh.example.com
     User user
     IdentityFile ~/.ssh/gst_ed25519
   ```

   也可以不写这一项，直接把目标地址交给 `gst add`，例如 `user@ssh.example.com`。所有 SSH 设置（地址、用户、私钥、known_hosts、超时）都来自 `~/.ssh/config`，见 [ssh 配置](#ssh-配置)。

3. **known_hosts 里有该机器的主机密钥**：先用 `ssh devbox` 连接一次，核对并接受主机密钥。

4. **那台机器上有 gst 0.4 或更高版本，并在那里执行过一次 `gst up`。** 它不需要自己的 link。gst 绝不替别的机器做设置：那里没执行过 `gst up` 时，`gst add` 会说明并给出命令，执行之后 link 自动连上。

这些只对创建 link 的机器有要求；被连接的机器只需要 gst、执行过 `gst up` 和能执行命令的 sshd（gst 使用登录会话而不是端口转发，所以 sshd 的 `AllowTcpForwarding` 不影响它）。对每个会话都强制执行固定命令的服务器无法被连接。

## 快速开始

在每台机器上执行一次 `gst up`：`laptop`、`devbox` 和 `homelab`。然后在 `laptop` 上连接另外两台：

```bash
gst up                     # 每台机器执行一次
gst add devbox             # ~/.ssh/config 中的 "devbox"
gst add homelab
gst status                 # mesh 中的每台机器，每行一台
```

`gst status` 的输出类似这样（第一台是本机，显示为 `-`）：

```
4f1c9a27  laptop   gst 0.4.0  10.87.26.1   -
3f2a9c10  devbox   gst 0.4.0  10.87.61.7   online; direct
c07e5d11  homelab  gst 0.4.0  10.87.140.3  online
```

之后任何程序都能用任何协议到达任何机器：

```bash
ping devbox.gst
ssh devbox.gst
psql -h devbox.gst
curl http://homelab.gst:8080/
```

link 在后台连接，连上之前机器显示为 `offline`。列表下方的 `# Problems:` 部分列出本机未运行的 link，以及本机不能加入网络的原因。`gst ping devbox` 检查 mesh，打印 `pong from devbox (3f2a9c10) over SSH in 12.3 ms; packets go over UDP`。

现在 `homelab` 能看到同一个 mesh：在 `homelab` 上，`gst status` 列出 `laptop` 和 `devbox`，`ping devbox.gst` 不需要到 `devbox` 的 link 也能通。

每个命令都可以用 `--help` 查看说明。在终端中，帮助使用粗体标题，命令和选项名为青色。经管道或重定向输出时是纯文本；设置 `NO_COLOR=1` 或 `TERM=dumb` 也可以在终端中关闭样式。

## 概念

| 概念 | 是什么 | 命令 |
| --- | --- | --- |
| **node** | 一台运行 gst 的机器，有一个 128 位随机 id 和一个名字（默认是主机名） | `gst name`、`gst status` |
| **link** | 从本机到另一台 gst 机器的 SSH 连接，在后台保持；两边的 daemon 通过它加入彼此的 mesh | `gst add`、`gst remove` |
| **虚拟网络** | 一个私有网段，每台机器都有一个 IP 地址和名字 `<machine>.gst`，所有程序和协议都能用 | `gst up`、`gst subnet` |

### node 和 link

- node 的名字用于显示和寻址，不要求唯一；id 是唯一的。用 `gst name laptop` 修改名字。名字和 link 名一样，由 1-32 个小写字母、数字或 `-` 组成。
- link 是双向的：流量可以沿任一方向通过，所以 `devbox` 不需要自己的 link 就能到达 `laptop`。link 有名字（`gst add devbox`），在对端报告自己的名字之前，它也是对端机器的名字。
- 已启动的 link 在后台运行，登录后自动启动，断线后自动重连。相连的机器会互相转告各自知道的信息，所以在任意一台机器上运行 `gst status` 都能看到整个 mesh；退出网络的机器会在 30 分钟内消失。路径只能经由运行中的 link 越过一台机器。

### 机器寻址

所有命令都用 `<machine>` 指定机器，`<machine>.gst` 也按同样的方式解析。它按以下顺序解析，某一步只有在恰好匹配一台机器时才生效：

1. 去掉末尾的 `.gst`。
2. node id。
3. node 名字。
4. link 名，表示该 link 另一端的机器。mesh 中任何机器的 link 都算，所以 `devbox` 在哪里都能用，不论它是 `laptop` 上的 link 名还是 `devbox` 自己的名字。
5. 至少 4 位的 id 前缀。

名字重复时，`gst status` 用 id 区分这些机器；此时请使用 id 或 id 前缀。

### 虚拟网络

虚拟网络让每台机器在一个私有网段里拥有 IP 地址和名字 `<machine>.gst`，任何程序都能用任何协议到达它：TCP、UDP 和 ping，例如游戏串流或屏幕共享。程序不需要任何设置。

- **root。** 创建网卡和路由需要 root，而 daemon 以你的用户身份运行，所以由一个小的系统服务 helper 为你的 daemon 做这件事，它运行一份 root 拥有的 gst。helper 缺失或过期时，`gst up` 会显示它要执行的 `sudo` 命令，以及这条命令会改动本机的哪些内容，并先询问你，因为 `sudo` 不一定会要密码。之后 `sudo` 自己询问密码，gst 看不到密码。没有终端时 `gst up` 不询问，也什么都不执行；`gst up --yes` 不询问直接执行，但仍然显示命令。在已经 up 的机器上执行 `gst up` 不会改变任何东西，升级 gst 后也只有 helper 变了才需要再次使用 root。机器会自动加入；在此之前，`gst status` 说明缺什么。
- **服务。** 其他机器能到达程序在所有地址或本机 `.gst` 地址上打开的端口。只监听 `127.0.0.1` 或 Unix socket 的程序从网络上到不了；请修改它的监听地址，或者用 `ssh <machine>.gst` 登录后在那里使用。
- **网段。** 所有机器共用一个网段。第一台机器根据能应答的机器上的网卡和路由，选一个不与它们重叠的 `/16`。`gst subnet 10.99.0.0/16` 为整个网络设置另一个网段：最新的选择胜出，所有机器都迁移过去，地址随之改变。它必须是私有网段（在 `10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16` 或 `100.64.0.0/10` 之内）。网段与某台机器上的其他网络重叠时，那台机器在 `gst status` 中报告。
- **地址。** 机器重启后地址不变。只承载 IPv4。
- **名字。** `<machine>.gst` 对所有程序生效，可以用机器的名字、到它的 link 的名字或 id 前缀。在 macOS 和使用 systemd-resolved 的 Linux 上，只有 `.gst` 交给 daemon 解析。其他 Linux 上，这些名字在网络运行期间写在 `/etc/hosts` 中。
- **路径。** WireGuard 对每个包做端到端加密，途中的机器只能看到密文。包一开始经 SSH link 传送，这些流量让两台机器经 link 寻找 UDP 路径：不需要任何配置，也不依赖外部服务器。前提是 UDP 能通，并且一台机器的某个地址（例如公网 IPv6 地址或共同网络上的地址）能被另一台访问；两台都在 NAT 后又没有这种地址的机器继续走 SSH。`gst status` 在机器后面显示 `direct`，`gst status --all` 显示每台机器走哪条路。UDP 失效时，包在几秒内改走 SSH，程序的连接不断。UDP 路径建立后不再依赖 link，所以两台机器之间的机器休眠时它继续运行；`gst remove` 之后如果已没有 link 通向某台机器，到它的 UDP 路径也会断开。经 SSH 时，TCP 跑在 TCP 里，能用，但在丢包的网络上会变慢。
- **MTU。** 网卡的 MTU 是 1168 字节，适合每条 UDP 路径。自己选择包大小的程序应该低于这个值，否则包会被分片。
- `gst down` 撤销 `gst up`：删除 daemon 的服务、网卡、路由、名字和 helper。helper 自己删除自己，所以不需要密码，下次 `gst up` 会重新询问。只留下配置和日志，机器的地址和 link 都保留。daemon 只是意外停止时，网卡、路由和名字也会消失，helper 保留。

### 信任

mesh 中的机器彼此信任。link 以你的用户身份登录，daemon 只通过自己的 socket 接受它。一台机器在所有地址上打开的每个端口，对网络中的其他机器都是开放的；机器之间经 link 得知彼此的 WireGuard 密钥，所以它们信任 link 所信任的。只连接你信任的机器。`gst down` 让本机退出 mesh。

## 命令

命令按 `gst --help` 的分组排列。机器和 link 的名字见[概念](#概念)。

### 构建 mesh

| 命令 | 用途 |
| --- | --- |
| `gst add <name> [destination] [--remote-gst cmd]` | 添加 link 并在后台启动 |
| `gst remove <name>...`（`rm`） | 停止并删除 link |
| `gst status` | 列出本机所知的 mesh 中的每台机器：短 id、名字、gst 版本、地址和状态；然后是本机的问题 |
| `gst status --all` | 再列出所有机器之间运行中的 link，以及到每台机器的包走哪条路、最近一次握手和收发字节数 |
| `gst status <machine>` | 经 mesh 查询，显示一台机器的地址和 link（含状态和目标） |
| `gst ping <machine> [-c N] [-v]` | 每秒请求一台机器经 SSH link 应答一次（默认 3 次），打印每次应答的路径和时间；`-v` 还显示到它的 UDP 路径或没有的原因，以及双方最近一次尝试的过程 |

`destination` 是 `host`、`user@host` 或 `ssh://user@host:port`，默认是 link 名。gst 不在对方登录 shell 的 `PATH` 中时，用 `--remote-gst <command>` 指定那台机器上的 gst 命令，例如 `~/.nix-profile/bin/gst`。SSH 连接的其他设置都来自你的 ssh 配置（见 [ssh 配置](#ssh-配置)）。要修改 link，请删除后重新添加。`gst add` 先检查是否有可用的无口令私钥、known_hosts 是否记录了服务器，不通过时不保存。

`gst status` 每台机器一行，本机在最前面：

| 状态 | 含义 |
| --- | --- |
| `-` | 本机 |
| `online` | 有运行中的 link 直接通向它 |
| `online; via devbox` | 到它的 link 经过列出的机器 |
| `online; direct` | 到它的包经 UDP 直连 |
| `offline` | 没有运行中的 link 通向它 |

link 显示为两端的机器，例如 `laptop->gateway`；link 名与它通向的机器不同时，后面跟着 `gst remove` 要用的名字，例如 `laptop->prod (db)`。`# Problems:` 部分列出本机未运行的 link，以及本机不能加入网络的原因。link 的状态是 `running`（两边的 daemon 已相连）、`connecting` 或 `down`（失败并重试中；显示最近一次错误，包括对方 gst 错误的第一行）。失败的 link 在 5 秒后重试，然后 10 秒，逐次翻倍，最长每 5 分钟一次，运行后重新计时。

`gst ping` 的输出例如 `pong from gateway (1a2b3c4d) via devbox in 41.3 ms`，包走 UDP 时再加上 `; packets go over UDP`。它测量的是 link，时间包括经过每台机器时建立通路的时间。要测量网络本身，用 `ping <machine>.gst`。没有收到应答时退出码为 1。

### 虚拟网络

| 命令 | 用途 |
| --- | --- |
| `gst subnet [cidr]` | 显示虚拟网络的网段，或为所有机器修改它 |

### 本机

| 命令 | 用途 |
| --- | --- |
| `gst name [name]` | 显示或修改本机名字，并显示 id |
| `gst up [--yes]` | 现在运行 daemon，并在登录时运行；helper 缺失或过期时，说明 `sudo` 要执行什么并询问后安装 |
| `gst down` | 撤销 `gst up`：停止 daemon，删除它的服务和 helper；其他机器不能连接本机 |
| `gst ui [--port N] [--no-open]` | 在浏览器中显示本机状态、mesh 和任意机器的 link |
| `gst version` | 显示 gst 版本 |

每个命令都接受 `--config <file>`。`status` 和 `ping` 接受 `--json`；`gst status --json` 打印 `{"self": <本机状态>, "mesh": <机器和 link>}`。

## ssh 配置

link 没有自己的 SSH 设置。每次连接时，gst 都会用 `ssh -G` 重新解析它的目标地址，所以 link 跟随 `~/.ssh/config`。修改该文件后，要立刻重连，请执行 `gst remove <name>` 再重新 `gst add <name>`；否则修改在 link 下次连接时生效。

| ssh 配置 | 用途 | 未设置时 |
| --- | --- | --- |
| `HostName`、`Port`、`User` | 地址和登录用户 | ssh 的默认值 |
| `IdentityFile` | 私钥，依次尝试，跳过不存在或有口令的 | ssh 的默认私钥列表 |
| `UserKnownHostsFile`、`GlobalKnownHostsFile` | 主机密钥校验 | ssh 的默认文件 |
| `ConnectTimeout` | 连接和登录超时 | 15 秒 |
| `ServerAliveInterval` | 保活间隔 | 15 秒 |
| `ServerAliveCountMax` | 连续多少个间隔无响应后断开重连 | 3 |

限制：

- 只使用没有口令的私钥，不使用 ssh-agent。
- 严格校验 known_hosts，不接受未知主机。
- link 不支持 `ProxyJump`、`ProxyCommand`、`HostKeyAlias`。要到达另一台机器背后的机器，请在两者上都运行 gst，经由前面的那台建立 link。

## 后台运行

- 每个用户有一个后台进程，即 gst daemon，运行所有已启动的 link 和虚拟网络，并接受其他机器的 link。它是一个用户级服务：Linux 上是 systemd 用户服务 `gst.service`，macOS 上是 launchd 代理 `local.gst`。`gst up` 或第一次修改配置时，gst 会安装它，不需要手动管理。别的机器连接本机时绝不会安装它。`gst down` 停止它并删除服务，`gst up` 恢复。
- gst 命令通过只有你能访问的 socket 与 daemon 通信：Linux 上是 `~/.local/state/gst/gst.sock`（设置了 `$XDG_STATE_HOME` 时在其下），macOS 上是 `~/Library/Application Support/gst/gst.sock`。
- daemon 本身退出后，服务会在 5 秒后重启它。
- daemon 的日志（也包含 link 的事件：已连接、失败、重连）：Linux 上在 journald（`journalctl --user -u gst`），macOS 上在 `~/Library/Logs/gst-daemon.log`。
- 为保护隐私，日志不记录服务器地址、用户、密钥路径和目标地址，也不记录对方 gst 返回的错误。这些只出现在交互式输出中：`gst status`、`gst ping` 和 `gst ui`。UDP 路径的地址同样不进日志。要看 link 失败的完整原因，执行 `ssh -v <destination>`。
- Linux 上，要让 link 在注销后继续运行、开机后自动启动，需要执行 `loginctl enable-linger "$USER"`；gst 发现没有开启时会提示。macOS 上，gst 在登录后启动、注销后停止。
- 服务运行的是 `gst daemon`，link 经 SSH 在对方机器上运行 `gst peer-bridge`；这两个都不是需要你自己执行的命令。

### 网页

`gst ui` 在浏览器页面中显示 mesh，每隔几秒自动刷新：所有机器及其状态和问题、机器之间的 link 关系图，以及所选机器的地址、link 和其他机器连过来的 link（与 `gst status <machine>` 相同）。按 J 和 K 在机器之间移动。本机 daemon 没有运行时，页面会说明。它在 `127.0.0.1` 上提供页面，直到按 Ctrl-C 为止；启动时打印地址并在浏览器中打开。页面只用来查看；修改仍使用上面的命令。

```bash
gst ui
# 在没有浏览器的机器上：指定端口并转发出来
gst ui --port 8370 --no-open
ssh -L 8370:localhost:8370 devbox   # 然后在你自己的机器上打开打印出的地址
```

地址在 `#` 后带有一段随机密钥，页面需要它才能读取数据；`gst ui` 运行期间，拿到完整地址的人都能使用页面。每次运行生成新的密钥。

## 从 0.3 升级

gst 0.4 只经虚拟网络到达机器：`gst connect`、代理、map、route 和 exit 都已移除，direct link 也不再承载连接。它不读取 0.3 的配置，每台机器都要重新开始：

```sh
rm ~/.config/gst/config.json
gst up
gst add devbox               # 在原来有 link 的机器上
```

- `gst up` 会替换 0.3 的 daemon。每台机器得到新的身份，用 `gst name` 重新设置名字。
- 原来用 map 或代理的地方，改用 `devbox.gst` 到达那台机器；只监听 loopback 或 Unix socket 的程序见[虚拟网络](#虚拟网络)中的“服务”。
- 把你连接的所有机器一起升级。gst 协议现在是版本 3，gst 拒绝与更旧的版本通信，并提示 `speaks an older gst protocol; upgrade gst there to 0.4 or later`。
- `gst net on`、`gst net off` 和 `gst net status` 都已移除：网络总是开启。`gst subnet` 取代 `--subnet`，`gst status --all` 显示 peer，`gst up` 取代 `sudo gst net install`。

## 排查问题

由外向内排查：`gst status` 显示机器是否 `online`，并列出本机未运行的 link 及其最近一次错误，以及本机不能加入网络的原因；`gst ping <machine>` 检查到它的路径是否通，`gst ping <machine> -v` 显示 UDP 路径为什么建立或没建立；daemon 日志记录 link 的事件（Linux 上 `journalctl --user -u gst`，macOS 上 `~/Library/Logs/gst-daemon.log`）；`ssh -v <destination>` 显示 ssh 自己对登录过程的说明。`gst add` 也会先检查私钥和 known_hosts，不通过时不保存。

| 现象 | 处理 |
| --- | --- |
| `no usable key`、私钥 `passphrase-protected` | 按[使用前准备](#使用前准备)创建无口令私钥，并在 `~/.ssh/config` 中用 `IdentityFile` 指定 |
| `no host key for ...` | 用 `ssh <host>` 连接一次，记录主机密钥 |
| 主机密钥不匹配 | 确认该机器的密钥确实更换后，执行 `ssh-keygen -R <host>`，再用 `ssh` 连接一次 |
| link 为 `down`，错误来自对方 gst，例如 command not found | gst 不在 ssh 在那里启动的登录 shell 的 `PATH` 中。安装它，或指定路径：先 `gst remove devbox`，再 `gst add devbox --remote-gst ~/.nix-profile/bin/gst` |
| link 为 `down`，错误是 `gst is not up on this machine; run gst up there`，或者 `gst add` 这样提示 | 在对方机器上执行 `gst up`，例如 `ssh -t <destination> gst up`；之后 link 自动连上 |
| `speaks an older gst protocol; upgrade gst there to 0.4 or later` | 升级它指出的那台机器上的 gst（每条 link 的两端都要 0.4） |
| `config version 3 is too old` | 删除它指出的文件，重新添加 link（见[从 0.3 升级](#从-03-升级)） |
| `no route to ...: no running links lead there` | 没有运行中的 link 把本机和那台机器连起来。检查 `gst status`（机器为 `offline` 或缺失，以及未运行的 link） |
| `gst status <machine>` 或 `gst ping <machine>` 失败 | 那台机器必须可达，并运行 0.4 或更新版本的 gst 且 daemon 在运行；用 `gst status` 查看到它的路径 |
| `the gst daemon does not answer` | 执行 `gst up`；查看 daemon 日志（`journalctl --user -u gst` 或 `~/Library/Logs/gst-daemon.log`） |
| 机器名匹配到多台机器 | 使用 `gst status` 中的 id 或 id 前缀 |
| `the helper of the virtual network does not run` | 执行 `gst up`，之后机器会自动加入 |
| `the subnet ... overlaps ... on this machine` | 在网络中任意一台机器上用 `gst subnet <cidr>` 换一个网段 |
| `gst status --all` 的 `HANDSHAKE` 一直是 `never` | 需要有流量才会开始：试试 `ping <machine>.gst`。如果还是这样，检查另一台机器的 `gst status` 有没有报告问题 |
| 某台机器上的程序在它的 `.gst` 名字上没有应答 | 它可能只监听 `127.0.0.1` 或 Unix socket；见[虚拟网络](#虚拟网络)中的“服务” |
| macOS 阻止访问本地网络服务器 | 在“本地网络”权限提示或“系统设置 → 隐私与安全性 → 本地网络”中允许 gst，然后删除并重新添加该 link。终端获得授权不一定代表后台服务也获得授权 |

## 在脚本和 agent 中使用

- 结果输出到 stdout；错误输出到 stderr，以 `error:` 开头，通常还带一行 `hint:`，给出下一步该执行的命令。不是错误的提示以 `note:` 开头。
- 退出码：`0` 成功，`1` 执行失败（包括 `gst ping` 没有收到应答），`2` 参数无效。
- `status` 和 `ping` 的 `--json` 字段是稳定的。
- 所有命令都不需要交互输入。

## 配置文件

配置保存在 `~/.config/gst/config.json`（设置了 `XDG_CONFIG_HOME` 时为 `$XDG_CONFIG_HOME/gst/config.json`），包括本机的 node（id 和名字）、link，以及你选择的网段。可以用 `--config` 或环境变量 `GST_CONFIG` 指定其他路径，daemon 会沿用同一个路径。每个用户只有一个 daemon，所以修改另一个配置文件会让 daemon 切换到那个文件。这个文件由 gst 维护，请用 gst 命令修改，不要手动编辑；多个 gst 命令同时执行也是安全的。
