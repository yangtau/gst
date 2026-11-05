# gst 设计

gst 把多台机器上的 gst 连成一个 mesh：每台机器跑一个 daemon，daemon 之间经 SSH 互联，交换拓扑，并沿 mesh 中转 TCP 连接和 Unix socket 连接；网络允许时，两台机器之间再经 UDP 建立直连。使用体验接近 Tailscale，但只工作在第 4 层（连接），不需要 root。

## 概念

| 概念 | 是什么 | 命令 |
| --- | --- | --- |
| **node**（机器） | 一台运行 gst 的机器，以 128 位随机 id 标识，名字只用于显示和寻址 | `gst name`、`gst status` |
| **link** | 本机到另一台 gst 机器的 SSH 连接，承载两边 daemon 之间的 peer 会话 | `gst add`、`gst remove` |
| **direct link** | 两台 gst 机器之间经 UDP（QUIC）的连接，按需自动建立，是 SSH link 之外的捷径 | `gst status` |
| **proxy** | 本机 daemon 提供的 SOCKS5 / HTTP 代理入口（默认关闭，`gst proxy <address>` 开启），`<machine>.gst` 与路由规则命中的连接走 mesh | `gst proxy` |
| **map** | 本机的一个监听端口或 Unix socket，连接转到某台机器上的端口或 socket，给不支持代理的程序用 | `gst map …` |
| **route** | 出口规则：某些域名、网段或全部流量经某台机器出去 | `gst route …` |
| **exit** | 允许别的机器经本机连接本机以外的地址 | `gst exit` |

没有 `-L`、`-R`、`-D`：`-L` 由 map 取代；`-R` 由对方机器自己建 map 取代；`-D` 由 proxy 加 route 取代。两端都必须安装 gst。

```
laptop ──link──► homelab
laptop ──link──► devbox
```

homelab 上执行 `gst connect devbox 22`、访问 `devbox.gst:8080`，或建 `gst map add 15432 devbox:5432`，连接都经 laptop 中转到 devbox。homelab 不需要 devbox 的 SSH 凭据。

## 决策

- **mesh 不可关闭**：gst 本身就是 mesh。机器第一次写配置时生成 node 身份（`node.id`、`node.name`，名字默认取主机名）；作为服务器第一次被 `gst peer-bridge` 连到时同样生成身份并启动 daemon。
- **up / down**：`gst down` 在配置里写 `down: true`，停止 daemon 并删除服务；之后修改配置的命令不再启动 daemon，`gst peer-bridge` 也拒绝连接。`gst up` 清除它并启动 daemon。除 down 外，daemon 总在运行（即使没有 link），因为它要提供 proxy、map，并接受别人的 link。
- **信任**：control socket 权限 `0600`，只有本机同一用户能连；link 由 SSH 登录建立，登录的就是该用户。mesh 内任何节点都可以请求任何节点连接**它自己机器上的**地址（loopback 与 Unix socket）。连接本机以外的地址（局域网、互联网）需要该节点开启 exit：`gst exit on`。这条规则由执行连接的最后一跳检查。mesh 里的机器都属于同一个用户、彼此完全信任：能连本机 loopback 与 Unix socket，就能用上面的任何服务（sshd、本机 gst 的 proxy 与 control socket），所以 exit 是防止流量意外从某台机器出去的默认策略，不是机器之间的安全边界。
- **只有 TCP**：中转的是字节流。目标是 TCP `host:port` 或 Unix socket 路径。
- **没有 IP 层**：link 之下的公共抽象是可靠的多路复用流（`session`），不是 IP 包。mesh 只承载连接，目标还有 Unix socket 和由 exit 解析的域名，这些都没有 IP 表示；不用 TUN 时用户态 TCP 栈只是多走一遍，IP 包走 SSH 还是 TCP-over-TCP。
- **SSH 决定成员**：SSH 是控制面和兜底。direct link 只是捷径，凭据、信令都来自 mesh，不让本来不相连的机器相连。
- **隐私**：拓扑、路径、地址只出现在交互式输出中（命令输出、`gst ui`）。后台日志（daemon 日志，link 的事件也记在其中）不记录服务器地址、用户、密钥路径、目标地址，也不记录远端 gst 返回的错误原文。

## 单进程 daemon

- 每个用户一个服务：Linux 上是 systemd 用户服务 `gst.service`，macOS 上是 launchd agent `local.gst`，命令行为 `gst [--config path] daemon`。
- 文件位置：

  | | Linux | macOS |
  | --- | --- | --- |
  | 配置 | `$XDG_CONFIG_HOME/gst/config.json`（默认 `~/.config/gst/config.json`） | 同左 |
  | 状态目录 | `$XDG_STATE_HOME/gst`（默认 `~/.local/state/gst`） | `~/Library/Application Support/gst` |
  | control socket | `<状态目录>/gst.sock` | 同左 |
  | daemon 日志 | journald（`journalctl --user -u gst`） | `~/Library/Logs/gst-daemon.log` |

- daemon 通过 `<socket>.lock` 上的 flock 保证单实例。
- 配置文件是唯一事实来源。CLI 修改配置后调用 `daemon.reload`：daemon 更新 node 身份与 exit，启动新的 link、停止已删除的 link、重启配置改变的 link，按配置重开 proxy 与 map 的监听，替换路由表。
- CLI 每次同步时比较 daemon 的 build（可执行文件的 SHA-256），不同就重启服务，让升级生效。

## 配置文件

```json
{
  "version": 3,
  "node": {"id": "3f2a9c10…（32 位十六进制）", "name": "laptop", "exit": false},
  "proxy": "127.0.0.1:1080",
  "links": {
    "devbox": {"destination": "devbox", "remote_gst": ""}
  },
  "maps": [
    {"listen": "127.0.0.1:15432", "machine": "devbox", "target": "localhost:5432"},
    {"listen": "127.0.0.1:18082", "machine": "devbox", "target": "/run/user/1000/app.sock"}
  ],
  "routes": [
    {"match": "corp.example.com", "via": "devbox"},
    {"match": "10.20.0.0/16", "via": "devbox"},
    {"match": "default", "via": "gateway"}
  ]
}
```

- `proxy` 是代理的监听地址，为空（默认）表示关闭。
- link 的字段：`destination`（ssh destination，每次连接用 `ssh -G` 解析；地址、用户、私钥、known_hosts、超时和保活都来自 ssh 配置，link 不另设）、`remote_gst`（服务器上的 gst 命令，默认 `gst`）。要修改 link，删除后重新添加。
- map 的 `listen` 是 `IP:port` 或绝对 socket 路径；`target` 是 `host:port` 或绝对 socket 路径，相对 `machine`。
- route 的 `match`：`default`；域名（匹配自身及所有子域名）；IP 或 CIDR（只匹配 IP 字面量，本机不为了匹配去解析域名）。多条命中时取最具体的：域名按长度，CIDR 按前缀长度，`default` 最后。
- **迁移**：读到版本 2 的配置时，`mesh` 变为 `node`；每个 SSH tunnel 变为同名 link；其 `-L` forward 变为 map（`machine` 为该 tunnel 名）；`mesh:<machine>` tunnel 的 `-L` 变为指向该 machine 的 map；`-R`、`-D` 被丢弃，CLI 在 stderr 列出被丢弃的 forward。版本 0、1 按版本 2 处理（gst 0.1 的服务不再清理）。

## 机器寻址

命令和代理里的 `<machine>` 按顺序解析：

1. 去掉末尾的 `.gst`；
2. 节点 id；
3. 节点名（唯一时）；
4. link 名：某条 link 的名字，指向该 link 对端的节点（任意节点发布的 link 都算，唯一时）；
5. 至少 4 位的 id 前缀（唯一时）。

所以 `devbox.gst`、`devbox`（laptop 上 link 的名字，或 devbox 自己的节点名）在整个 mesh 中都能用。

## 代理

daemon 在 `proxy` 地址上同时提供 SOCKS5 和 HTTP 代理，按首字节区分（`0x05` 为 SOCKS5）：

- SOCKS5：仅 CONNECT、无认证；域名原样交给路由（等同 `socks5h`）。
- HTTP：`CONNECT host:port`；绝对形式的普通请求（`GET http://host/…`），转发时加 `Connection: close`；其他请求返回 400。

每个连接的去向：

1. 主机名为 `<machine>.gst`：连接该机器的 `localhost:port`；
2. 命中某条 route：经 `via` 机器连接原地址，域名由 `via` 解析；`via` 必须开启 exit；
3. 否则由本机直接连接。

## gst 协议

CLI 与 daemon、daemon 与 daemon 使用同一套协议，实现位于 `internal/wire`。帧格式固定，扩展只靠新增方法、新增字段、新增错误码和 feature。

### 帧

```
+----------------+---------------------------+
| length: uint32 | payload: JSON (length 字节) |
|  big-endian    |                           |
+----------------+---------------------------+
```

`length` 不超过 1 MiB；超出即为协议错误。接收方忽略未知 JSON 字段。删除字段或改变字段含义必须提升协议版本。

### 握手

每条连接上，发起方先发 `Hello`，接收方回 `Hello`：

```json
{"protocol": 2, "software": "gst 0.3.0", "role": "control", "build": "sha256:…", "features": []}
```

| 字段 | 含义 |
| --- | --- |
| `protocol` | 发送方的协议版本。当前为 2；接收方拒绝低于 2 的版本 |
| `software` | 人类可读的软件版本 |
| `role` | `control`（CLI → 本机 daemon）或 `peer`（daemon ↔ daemon） |
| `build` | 可执行文件标识，CLI 用它判断 daemon 是否需要重启 |
| `features` | 可选能力；`peer` 连接必须带 `yamux` |
| `error` | 仅出现在回复中：拒绝原因，随后连接关闭 |

### 请求与响应

握手之后，一个**流**承载一次调用：

```
请求：  {"method": "link.restart", "params": {"names": ["devbox"]}}
响应：  {"result": {...}}
     或 {"error": {"code": "not_found", "message": "link devbox is not started"}}
```

- `control` 连接本身就是一个流，可以顺序发多次请求。
- `peer` 连接握手后切换为 yamux 会话，双方都可以开子流，每个子流以一个请求开始。
- **流式方法**在成功响应之后接管该流：`relay.open` 与 `mesh.connect` 之后是原始字节，`topology.sync` 之后是连续的帧。
- 未知方法返回 `unsupported_method`。

错误码：`unsupported_method`、`unsupported_role`、`unsupported_protocol`、`invalid_params`、`not_found`（节点、link 或路径不存在）、`forbidden`（目标不在该节点上，而该节点没有开启 exit）、`failed`。

### control 方法

| 方法 | 参数 | 结果 |
| --- | --- | --- |
| `daemon.status` | 无 | `Status`：software、build、pid、config、`node`、`proxy`（`listen` 与错误；代理关闭时 `listen` 为空）、`links`（配置中的每条 link：name、`destination`、state、since、error、reconnects、对端 `peer`；按名字排序）、`maps`（按配置顺序：listen 与错误、`machine`、`target`）、`routes`（按配置顺序：`match`、`via`） |
| `daemon.reload` | 无 | 重新读取配置并同步后的 `Status` |
| `mesh.nodes` | 无 | `Topology` |
| `mesh.status` | `{"node": <machine>}` | 该机器 daemon 的 `Status`：经 mesh 以 `node.status` 向它查询（`<machine>` 可以是本机，也可以是任意可达的机器）；找不到、不可达或对方不支持时返回错误 |
| `mesh.ping` | `{"node": <machine>}`（`NodeParams`） | `{"node": NodeInfo, "via": [NodeInfo...], "time_ms": 数字, "direct": 布尔}`：经 mesh 以 `node.info` 向该机器调用一次；`node` 是它的应答，`via` 是路径上按顺序的中间机器（直连时为空），`time_ms` 是从发起调用到收到应答的毫秒数（含沿途每台机器上建立中转的时间）；找不到或不可达时返回错误。`gst ping` 每秒调用一次 |
| `mesh.direct` | `{"node": <machine>}` | `DirectReport`：`node`、`off`（本机关闭了 direct link）、`trying`（有尝试在进行）、`link`（`local`、`remote`，本机看到的两端 `IP:port`）、`next`（失败后下一次可以尝试的时间）、`local` 与 `peer`（双方最近一次尝试的 `DirectTrace`；对方的经 `direct.trace` 取得）、`peer_error`（取不到对方记录的原因）。`gst ping -v` 用它 |
| `mesh.connect` | `{"node": <machine>, "target": {"network": "tcp"\|"unix", "address": …}}` | 成功后该连接变为到目标的原始字节流 |

link 状态：`connecting`（建立 SSH 与 peer 会话中）、`running`（peer 会话已建立）、`down`（失败，等待重试）。失败后的等待从 5 秒起每次翻倍，最长 5 分钟；link 进入 running 后重置。

### peer 方法

| 方法 | 参数 | 结果 / 后续 |
| --- | --- | --- |
| `node.info` | 无 | `NodeInfo`：`id`、`name`、`software`、`exit` |
| `topology.sync` | `{"known": {节点 id: seq}}` | `{"records": [...]}`：比调用方新的记录；之后该流持续推送 `{"records": [...]}`，每次变化一帧 |
| `relay.open` | `{"path": [节点 id...], "target": {...}}` | 成功后该流变为到目标的原始字节流；`target.network` 为 `gst` 时见下文 |
| `node.status` | 无 | 被调方 daemon 的 `Status`（与 `daemon.status` 相同）；不提供状态的节点返回 `unsupported_method` |
| `direct.offer` | `DirectOffer` | 被调方的 `DirectOffer`；之后双方建立 direct link，见下文 |
| `link.remove` | `{"node": 调用方 id, "link": link 名}` | 无；调用方即将删除这条 link，见 direct link 一节 |
| `direct.trace` | `{"node": 调用方 id}` | `{"events": [{"time": …, "text": …}...]}`：被调方与调用方最近一次尝试建立 direct link 的过程 |

### link 的建立

1. daemon 用 `ssh -G` 解析 destination，建立 SSH 连接（只信任 known_hosts 中的主机密钥），按 keepalive 设置探测。
2. 在该连接上开 session，执行 `<remote_gst> peer-bridge`。peer-bridge 把标准输入输出接到服务器 daemon 的 control socket；服务器还没有 node 身份或 daemon 没在运行时，先生成身份并启动 daemon；服务器 `down` 时报错退出。
3. 双方以 `peer` 角色握手（30 秒内完成），切换到 yamux，各调用一次 `node.info`；连到自己时报错断开。
4. 双方各向对方调用一次 `topology.sync`。
5. SSH 连接、session、yamux 会话任一结束，link 即结束，daemon 按退避重连。远端 gst 失败时，其 stderr 的第一行出现在 `Status` 的 error 中，不进日志。

### link-state 记录

```json
{
  "node": {"id": "…", "name": "laptop", "software": "gst 0.3.0", "exit": false},
  "seq": 1793700000000000000,
  "expires": "2026-11-03T12:00:00Z",
  "links": [{"name": "devbox", "peer": "…devbox 的 id…"}]
}
```

- 每个节点只发布自己的记录，`links` 是它处于 running 的 link：自己发起的带 `name`，接受的 `name` 为空。`seq` 取 `max(上一次 seq + 1, 当前 Unix 纳秒)`；若收到自己旧进程发布的更大 `seq`，立即以更大的 `seq` 重新发布。
- 节点信息或 link 变化时立即重新发布；收到更大 `seq` 的记录就替换，并经各条 `topology.sync` 流推送给其他 peer（洪泛）。
- direct link 由发起方列出，`links` 中 `name` 为空、`direct` 为 true，`local` 和 `remote` 是发起方看到的两端 `IP:port`；接受方的 `name` 同样为空，没有 `direct`。
- 记录有效期 30 分钟，节点每 10 分钟刷新。每个节点最多保留 1024 条记录，每条记录最多 1024 条 link。
- `Topology` 由查看方计算：`nodes`（id、name、software、exit、reachable、`path`：从本节点出发的最短路径上的节点 id）与 `edges`（from、to、link 名）。只有两端的记录都列出对方时，一条 link 才算存在（对端没有有效记录时以发起方为准）：失去 link 的一端可能无法再把新记录传给其他节点，另一端可以。`edges` 只含发起方列出的 link，direct link 带 `direct: true` 和 `local`、`remote`。`nodes` 的 `direct` 表示查看方有到该节点的 direct link。link 是双向的，路径可以沿任一方向经过。

### 中转

- 发起方根据自己的 `Topology` 计算最短路径（源路由），最多 16 个节点。`relay.open` 的 `path` 以被调方开头，每个节点检查 `path[0]` 是自己，然后：
  - 只剩自己：连接 `target`。目标不是本机地址（Unix socket、`localhost`、loopback IP）且本节点没有开启 exit 时返回 `forbidden`；
  - 否则在与 `path[1]` 的 peer 会话上开新流，把 `path[1:]` 交给它。
  - 目标的 `network` 为 `gst`（`address` 必须为空，否则 `invalid_params`）：路径末端的节点不连接任何地址，而是在该流上继续处理 peer 方法，一个接一个，直到发起方关闭流。它不是连接地址，不受 exit 规则限制；发起方用它调用任意节点的 peer 方法（`mesh.status` 即经此向目标节点调用 `node.status`，`mesh.ping` 经此调用 `node.info`；调用自己时在进程内完成）。
- 失败时错误沿路径原样传回发起方。

### direct link

SSH link 之外，两个节点之间可以再有一条经 UDP 的 direct link（`internal/direct`），即使两者没有 SSH link。不需要配置、标记或 STUN 服务器。[`udp-hole-punching.html`](udp-hole-punching.html) 从 UDP 包头讲起，解释防火墙、NAT、打洞和 direct link 的加密，并带逐步的模拟器。

- **触发**：`Node.Dial` 向某节点建立连接时，如果没有到它的 direct link，就在后台尝试，这个连接不等待、继续走原路，之后的连接走 direct link。失败或断开后等待 1 分钟再试，连续失败时加倍，最长 30 分钟，成功后重置。
- **信令**：发起方经 mesh 调用对方的 `direct.offer`（`Node.Call`）。`DirectOffer` 是 `{"node": 发起方 id, "token": …, "addresses": ["IP:port", …], "fingerprint": …}`，响应是对方的 `addresses` 和 `fingerprint`。候选地址只出现在这次调用中。建好的 direct link 所用的两端地址写进发起方的 link-state 记录，让每个节点都能显示它们。地址都不进日志。
- **同时发起**：两个节点同时发起时，id 较小一方的 offer 继续，id 较大的一方放弃自己的。收到 offer 时本节点还有到对方的 direct link，说明它已失效，先关闭。
- **socket**：每次尝试为每个本机地址绑定一个 UDP socket（每个地址族最多 8 个），跳过回环、link-local 和点对点接口（VPN、代理的 TUN）。必须绑定具体地址：通配 socket 回包的源地址不一定是对方发往的地址，默认路由被代理的 TUN 接管时回包还会被截走。
- **探测**：双方每 200 ms 向对方所有地址发探测包，`0x01` 加 16 字节 token，收到后回复 `0x02` 加 token。首字节让 quic-go 把它们当作非 QUIC 包交出来。收到探测包时，把来源地址也加入探测目标：NAT 后的节点不知道自己的公网地址，但只要一方能到达另一方，另一方就从来包学到地址，所以不需要 STUN。双方都有状态防火墙时，同时发包也让两边放行对方。
- **连接**：发起方收到回复即证明路径可用，在该 socket 上向回复的来源地址发起 QUIC 连接，失败就换下一条路径。整次尝试限时 15 秒。没用上的 socket 随即关闭。
- **认证**：每次尝试生成一张自签名证书，指纹经 mesh 传递，双方都校验对方证书的指纹（TLS 1.3，ALPN `gst-direct`）。信任来自 SSH，没有长期密钥。探测包只用来找路径，不用于认证。
- **会话**：QUIC 连接之后是一个 `session`：每个 QUIC 流相当于一个 yamux 子流，没有 `Hello`（ALPN 已经确定协议），各调用一次 `node.info`，然后 `topology.sync`，与 SSH link 一样。keepalive 5 秒，20 秒没有收到数据视为断开。
- **选路**：`Topology` 的路径计入 direct link。每一跳由 `sessionTo` 选择 session，有 direct link 就用它，所以 `relay.open` 不变。direct link 在调用途中断开时，这一跳改走到同一节点的其他 link，连接不失败，只是最多等到 20 秒的超时。
- **断开**：direct link 断开时，走它的连接随之中断，新连接回到其他 link。已有连接不在 link 之间迁移。
- **SSH 决定成员**：丢失 SSH link 不改变成员，direct link 继续运行，例如两个节点之间的机器休眠时。只有 `gst remove` 删除 link 才改变成员：打开方在停止 link 前调用 `link.remove`（最多等 3 秒），之后 link 两端各检查一次，不计这条 link 时只经 SSH link 能否到达 direct link 的对端，到不了就关闭。对方没有收到 `link.remove`（link 当时已断）时，它的 direct link 继续运行，直到自行断开。
- **过程记录**：每个节点在内存里保留与每个对端最近一次尝试的事件（提供的地址、听到的探测、被证明的路径、QUIC 握手、结果），新的尝试开始时清空，最多 200 条。记录含地址，只经 `mesh.direct` 和 `direct.trace` 交给发出请求的用户，不进日志。
- `GST_DIRECT=off` 让 daemon 不发起也不接受 direct link，集成测试用它固定路径。

## Web UI

- `gst ui` 是前台进程，只监听 `127.0.0.1`（`--port`，默认任意空闲端口），启动时生成随机 token，打印 `http://127.0.0.1:<port>/#<token>` 并打开浏览器（`--no-open` 跳过）。daemon 只有 control socket 这一个控制入口，退出后不留监听。
- 页面（`internal/cli/ui`，go:embed）以 `Authorization: Bearer <token>` 请求 `GET /api/state`；`Host` 不是 localhost 或回环地址时返回 403；响应带严格的 CSP 等安全头。页面用 DOM 文本节点渲染所有字段。
- `/api/state` 返回本机 node、proxy、links、maps、routes、daemon 是否应答、`Topology`；只供页面使用，字段可以改变。页面只读：不能启动、停止或重启 link；daemon 不运行时，页面说明这一点。机器表的 Status 列显示 online、offline 或 online via …。
- `GET /api/nodes/{node}`（同样需要 token）返回 mesh 中任一机器的 `Status`，即 `mesh.status` 的结果，`{node}` 是机器的称呼（名称、id、id 前缀或 link 名）；出错时返回 502 和错误信息。页面选中某台机器时请求它。
