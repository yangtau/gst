# gst

[English](README.md) | 简体中文

`gst`（go ssh tunnel）让 SSH 端口转发在后台持续运行：登录后自动启动，断线后自动重连。它按你的 `~/.ssh/config` 连接服务器，转发的写法与 `ssh -R` / `ssh -L` 相同。支持 Linux 和 macOS。

- **tunnel**：一个有名字的 SSH 连接，例如 `myhost`。
- **forward**：tunnel 上的一条端口转发。
  - `-R [bind_address:]port:host:hostport`：在服务器上监听，由本机连接 `host:hostport`。
  - `-L [bind_address:]port:host:hostport`：在本机监听，由服务器连接 `host:hostport`。

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

后台运行的 tunnel 会记住 gst 的路径。升级或移动 gst 后，执行 `gst restart --all`。

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

每个命令都可以用 `--help` 查看说明和示例。`gst completion bash|zsh|fish` 生成 shell 补全脚本。

## 命令

| 命令 | 作用 |
| --- | --- |
| `gst add <name> [destination] [-R spec]... [-L spec]...` | 添加 tunnel 并在后台启动 |
| `gst set <name> [flags]` | 修改目标地址或连接设置 |
| `gst forward add <tunnel> (-R spec \| -L spec)...` | 添加 forward |
| `gst forward remove <tunnel> (-R [bind:]port \| -L [bind:]port)...` | 删除 forward |
| `gst forward list <tunnel>` | 列出 forward |
| `gst remove <name>...` | 停止并删除 tunnel |
| `gst start\|stop\|restart <name>... \| --all` | 启动、停止、重启 |
| `gst list` | 列出 tunnel 及状态 |
| `gst show <name>` | 查看 tunnel 的设置和 ssh 解析结果 |
| `gst logs <name> [-f] [-n N]` | 查看日志 |
| `gst check <name>` | 诊断：ssh 配置、私钥、known_hosts、网络、登录 |

修改运行中 tunnel 的 forward 或设置后，gst 会自动重启它；加 `--no-restart` 可以跳过。设置没有变化时什么也不做。

## 常见用法

```bash
# 目标地址不在 ssh 配置里时，直接写出来
gst add db admin@db.example.com -L 15432:127.0.0.1:5432
gst add web ssh://deploy@203.0.113.7:2200 -R 8080:localhost:3000

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
- `host` 由连接它的一方解析：`-L` 中的 `127.0.0.1` 指服务器自己，`-R` 中的 `localhost` 指本机。
- 服务器上的实际监听范围还受 sshd 的 `GatewayPorts` 控制。监听所有网卡时，记得用防火墙限制访问来源。
- sshd 的 `AllowTcpForwarding` 需要允许所用的方向：两个方向都用时设为 `yes`，只用 `-R` 时可以是 `remote`，只用 `-L` 时可以是 `local`。

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

- 每个启动的 tunnel 是一个用户级后台服务：Linux 上是 systemd 用户服务 `gst-<name>.service`，macOS 上是 launchd 代理 `local.gst.<name>`。服务由 gst 创建和删除，不需要手动管理。
- tunnel 退出后 5 秒自动重启，没有次数限制。
- `gst list` 中的状态：`running` 正在运行；`down` 失败后正在等待重启，用 `gst logs` 和 `gst check` 排查；`stopped` 已停止。
- 日志：Linux 上在 journald。macOS 上在 `~/Library/Logs/gst/<name>.log`，超过 1 MiB 时 gst 会把它改名为 `<name>.log.1`（覆盖更早的一份），所以每个 tunnel 的日志最多约 2 MiB；`gst remove` 会删除它们。为保护隐私，日志不记录服务器地址、用户、密钥路径和转发地址，forward 按编号记录（如 `forward 2`），编号与 `gst forward list` 一致。
- Linux 上，要让 tunnel 在注销后继续运行、开机后自动启动，需要执行 `loginctl enable-linger "$USER"`；gst 发现没有开启时会提示。macOS 上，tunnel 在登录后启动、注销后停止。

## 排查问题

先运行 `gst check <name>`，它会逐项检查并对失败项给出建议。`add`、`set`、`start`、`restart` 也会先检查私钥和 known_hosts，不通过时不保存、不启动。

| 现象 | 处理 |
| --- | --- |
| `no usable key`、私钥 `passphrase-protected` | 按[使用前准备](#使用前准备)创建无口令私钥，并用 `--identity` 或 ssh 配置的 `IdentityFile` 指定 |
| `no host key for ...` | 用 `ssh <host>` 连接一次，记录主机密钥 |
| 主机密钥不匹配 | 确认服务器密钥确实更换后，执行 `ssh-keygen -R <host>`，再用 `ssh` 连接一次 |
| `-R` 端口被占用（`listener start failed`） | 通常是上一次连接在服务器上残留的会话，见下文 |

**服务器端口被残留会话占用。** 网络中断后，旧的 SSH 会话可能还占着服务器上的 `-R` 端口。建议在服务器的 sshd 配置中设置 `ClientAliveInterval 15` 和 `ClientAliveCountMax 3`，让 sshd 及时回收失联的会话。

也可以开启 `--cleanup-listeners`：每次连接后、开始监听前，gst 会在服务器上终止属于登录用户、占用这些 `-R` 端口的进程。被终止的**不限于 SSH 进程**，所以只能用于专属于这个 tunnel 的端口。Linux 上残留的 sshd 会话进程无法用这种方式清理。服务器需要在 `PATH` 中提供 `bash`、`lsof`、`ps`、`id`；Linux 上还需要可读的 `/proc/net/tcp`，其他系统需要 `netstat`。

## 在脚本和 agent 中使用

- 结果输出到 stdout；错误输出到 stderr，以 `error:` 开头，通常还带一行 `hint:`，给出下一步该执行的命令。
- 退出码：`0` 成功，`1` 执行失败（包括 `check` 未通过），`2` 参数无效。
- `list`、`show`、`check`、`forward list` 支持 `--json`。
- 所有命令都不需要交互输入。重复添加完全相同的 forward 不会改动任何东西；监听地址相同但目标不同则报错。

## 配置文件

tunnel 保存在 `~/.config/gst/config.json`（设置了 `XDG_CONFIG_HOME` 时为 `$XDG_CONFIG_HOME/gst/config.json`）。可以用 `--config` 或环境变量 `GST_CONFIG` 指定其他路径，后台服务会沿用同一个路径。这个文件由 gst 维护，请用 gst 命令修改，不要手动编辑；多个 gst 命令同时执行也是安全的。
