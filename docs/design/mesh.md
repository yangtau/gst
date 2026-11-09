# gst 设计

gst 把多台机器连成一个私有网络：每台机器跑一个 daemon，daemon 之间经 SSH 互联成 mesh，交换拓扑、WireGuard 公钥和打洞信令；机器之间的数据全部走 WireGuard，包经 UDP 直连，打洞不成时经 SSH 运送。使用体验接近 Tailscale，每台机器用 root 安装一次 helper。

分三层：

| 层 | 是什么 | 实现 |
| --- | --- | --- |
| 控制面 | 只走 SSH：拓扑、公钥和地址、UDP 打洞的信令、状态查询 | `gst peer-bridge` 加 yamux，`internal/mesh` |
| 数据面 | 只有 WireGuard：端到端加密的 IP 包 | wireguard-go，`internal/vnet` |
| 运送 | WireGuard 的包走哪条路：UDP 路径，或沿 SSH link 的流 | `internal/direct`、`internal/mesh/packets.go` |

## 概念

| 概念 | 是什么 | 命令 |
| --- | --- | --- |
| **node**（机器） | 一台运行 gst 的机器，以 128 位随机 id 标识，名字只用于显示和寻址 | `gst name`、`gst status` |
| **link** | 本机到另一台 gst 机器的 SSH 连接，承载两边 daemon 之间的 peer 会话；以 destination 区分，没有自己的名字 | `gst link`、`gst unlink` |
| **UDP 路径** | 两台机器之间打洞得到的一对 `IP:port`，只运送 WireGuard 的包，不是 link | `gst status` |
| **虚拟网络** | 一个私有网段，每台机器有 IP 地址和 `<machine>.gst` | `gst up`、`gst subnet` |

```
laptop ──link──► homelab
laptop ──link──► devbox
```

homelab 上 `ssh devbox.gst`：名字解析成 devbox 的虚拟地址，路由把包交给网卡，WireGuard 加密后经 UDP 路径发给 devbox，没有 UDP 路径时经 laptop 的两条 link 中转。homelab 不需要 devbox 的 SSH 凭据。

用户看到的术语比这里少：machine（node）、link、network（mesh 和虚拟网络合称）、gst IP、direct / relay（UDP 路径 / 沿 SSH link 运送）、network helper（helper）。node、peer、mesh、daemon、拓扑、握手、UDP 路径等只出现在 `-v`、日志和本文档中；术语表见 `AGENTS.md`。

## 决策

- **mesh 不可关闭**：gst 本身就是 mesh。机器第一次写配置时生成 node 身份（`node.id`、`node.name`，名字默认取主机名）。别的机器绝不替它做设置：没有身份的机器拒绝 `gst peer-bridge`，要有人在那台机器上执行 `gst up`。
- **up / down**：`gst down` 是 `gst up` 的逆操作：在配置里写 `down: true`，停止 daemon 并删除服务，再删除 helper；之后修改配置的命令不再启动 daemon，`gst peer-bridge` 也拒绝连接。`gst up` 清除它，启动 daemon，并处理 helper（见“虚拟网络”）。除 down 外，daemon 总在运行（即使没有 link），因为它要运行虚拟网络，并接受别人的 link。
- **虚拟网络不可关闭**：有 helper 就在网络上，没有就在 `gst status` 里说明。`gst up` 装好 helper 就是加入，`gst down` 删除 helper 就是离开，所以没有单独的开关。
- **信任**：control socket 权限 `0600`，只有本机同一用户能连；link 由 SSH 登录建立，登录的就是该用户。mesh 里的机器都属于同一个用户、彼此完全信任，WireGuard 公钥经 link 传递，所以它们信任 link 所信任的。一台机器在所有地址上打开的端口对其他机器开放，只监听 loopback 或 Unix socket 的服务需要显式用 `gst expose` 暴露。
- **只有一条数据链路**：应用的数据只走 WireGuard，没有按连接中转字节流的第二条路。mesh 的 `relay.open` 只为控制面服务：沿 link 调用 peer 方法，和运送 WireGuard 的包。
- **SSH 决定成员**：SSH 是控制面和兜底。UDP 路径只是捷径，公钥、信令都来自 mesh，不让本来不相连的机器相连。
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
- 配置文件是唯一事实来源。CLI 修改配置后调用 `daemon.reload`：daemon 更新 node 身份，启动新的 link、停止已删除的 link、重启配置改变的 link，把网段的选择交给虚拟网络。
- CLI 每次同步时比较 daemon 的 build（可执行文件的 SHA-256），不同就重启服务，让升级生效。

## 配置文件

```json
{
  "version": 5,
  "node": {"id": "3f2a9c10…（32 位十六进制）", "name": "laptop"},
  "links": [
    {"destination": "devbox", "remote_gst": ""}
  ],
  "net": {"subnet": "10.99.0.0/16", "since": 1793700000000000000}
}
```

- link 的字段：`destination`（ssh destination，每次连接用 `ssh -G` 解析；地址、用户、私钥、known_hosts、超时和保活都来自 ssh 配置，link 不另设）、`remote_gst`（服务器上的 gst 命令，默认 `gst`）。每个 destination 最多一条 link，它就是 link 的标识；要修改 link，unlink 后重新 link。
- **link id**：`config.LinkID(destination)`，destination 的 SHA-256 的前 6 字节（12 位十六进制）。它出现在记录、`link.remove` 和日志（`link <id>: …`）中，不泄露 destination。
- `net` 是用户用 `gst subnet` 选的网段和选择的时间，没选过时不存在。
- **旧版本**：版本 4 的 `links` 是以 link 名为键的对象，读取时按名字排序转成数组，丢掉 destination 重复的项，下次写入时成为版本 5。更早的版本不转换：报错，说明要删除文件并重新 link。

## 机器寻址

命令里的 `<machine>` 和 DNS 里的 `<machine>.gst` 按顺序解析：

1. 去掉末尾的 `.gst`；
2. 节点 id；
3. 节点名（唯一时）；
4. 至少 4 位的 id 前缀（唯一时）。

link 没有名字，不参与寻址。`gst unlink` 例外：它在本机的 link 中查找，除了上面这些，还接受 link 的 destination。

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
{"protocol": 5, "software": "gst 0.4.0", "role": "control", "build": "sha256:…", "features": []}
```

| 字段 | 含义 |
| --- | --- |
| `protocol` | 发送方的协议版本。当前为 5；接收方拒绝低于 5 的版本，并在错误中写出双方的 `software` |
| `software` | 人类可读的软件版本：发布版本加构建所用的 commit，如 `gst 0.5.0+a2ffc87`，仅供显示；判断是否同一个可执行文件用 `build` |
| `role` | `control`（CLI → 本机 daemon）或 `peer`（daemon ↔ daemon） |
| `build` | 可执行文件标识，CLI 用它判断 daemon 是否需要重启 |
| `features` | 可选能力；`peer` 连接必须带 `yamux` |
| `error` | 仅出现在回复中：拒绝原因，随后连接关闭 |

### 请求与响应

握手之后，一个**流**承载一次调用：

```
请求：  {"method": "mesh.status", "params": {"node": "devbox"}}
响应：  {"result": {...}}
     或 {"error": {"code": "not_found", "message": "no machine \"devbox\" in the network; see `gst status`"}}
```

- `control` 连接本身就是一个流，可以顺序发多次请求。
- `peer` 连接握手后切换为 yamux 会话，双方都可以开子流，每个子流以一个请求开始。
- **流式方法**在成功响应之后接管该流：`relay.open` 之后是到路径末端的流，`topology.sync` 之后是连续的帧，`net.packets` 之后是包。
- 未知方法返回 `unsupported_method`。

错误码：`unsupported_method`、`unsupported_role`、`unsupported_protocol`、`invalid_params`、`not_found`（节点、link 或路径不存在）、`failed`。

### control 方法

| 方法 | 参数 | 结果 |
| --- | --- | --- |
| `daemon.status` | 无 | `Status`：software、build、pid、config、`node`、`links`（配置中的每条 link，按配置中的顺序：`destination`、state、since、error、reconnects、对端 `peer`）、`net`（`NetState`：subnet、address、interface、error、hint、`peers`；daemon 不能运行网络时没有） |
| `daemon.reload` | 无 | 重新读取配置并同步后的 `Status` |
| `mesh.nodes` | 无 | `Topology` |
| `mesh.status` | `{"node": <machine>}` | 该机器 daemon 的 `Status`：经 mesh 以 `node.status` 向它查询（`<machine>` 可以是本机，也可以是任意可达的机器）；找不到、不可达或对方不支持时返回错误 |
| `mesh.ping` | `{"node": <machine>}`（`NodeParams`） | `{"node": NodeInfo, "via": [NodeInfo...], "time_ms": 数字}`：经 link 以 `node.info` 向该机器调用一次；`node` 是它的应答，`via` 是路径上按顺序的中间机器（有 link 直达时为空），`time_ms` 是从发起调用到收到应答的毫秒数（含沿途每台机器上建立中转的时间）；找不到或不可达时返回错误。`gst ping -v` 用它检查 link。CLI 自己按 `-v` 和 relay 的说法显示这些结果，协议里的 `ssh` 对用户显示为 `relay` |
| `net.ping` | `{"node": <machine>, "size": 字节数}`（`NetPingParams`；`size` 可省略） | `{"node": NodeInfo, "address": 地址, "path": "direct" 或 "ssh", "endpoint": UDP 路径对端的 IP:port, "via": [NodeInfo...], "time_ms": 数字}`：经虚拟网络向该机器的 daemon 发一个包并收回；`path` 是回来时包走的路，本机为空，`via` 是走 SSH 时途中的机器。本机或对方不在虚拟网络上、没有回复时返回错误。`gst ping` 每秒调用一次 |
| `mesh.direct` | `{"node": <machine>}` | `DirectReport`：`node`、`off`（本机关闭了 UDP 路径）、`trying`（有尝试在进行）、`path`（`local`、`remote`，本机看到的两端 `IP:port`）、`next`（失败后下一次可以尝试的时间）、`stats`（UDP socket 的端口、绑定的网卡和实际缓冲区，以及丢包计数）、`local` 与 `peer`（双方最近一次尝试的 `DirectTrace`；对方的经 `direct.trace` 取得）、`peer_error`（取不到对方记录的原因）。`gst ping -v` 用它 |

link 状态：`connecting`（建立 SSH 与 peer 会话中）、`running`（peer 会话已建立）、`down`（失败，等待重试）。失败后的等待从 5 秒起每次翻倍，最长 5 分钟；link 进入 running 后重置。

### peer 方法

| 方法 | 参数 | 结果 / 后续 |
| --- | --- | --- |
| `node.info` | 无 | `NodeInfo`：`id`、`name`、`software` |
| `topology.sync` | `{"known": {节点 id: seq}}` | `{"records": [...]}`：比调用方新的记录；之后该流持续推送 `{"records": [...]}`，每次变化一帧 |
| `relay.open` | `{"path": [节点 id...]}` | 成功后该流通到路径末端的节点，由它继续处理 peer 方法，见下文 |
| `node.status` | 无 | 被调方 daemon 的 `Status`（与 `daemon.status` 相同）；不提供状态的节点返回 `unsupported_method` |
| `direct.offer` | `DirectOffer` | 被调方的 `DirectOffer`；之后双方寻找 UDP 路径，见下文 |
| `link.remove` | `{"node": 调用方 id, "link": link id}` | 无；调用方即将删除这条 link，见 UDP 路径一节 |
| `direct.update` | `DirectOffer`（运行中路径的 token 和调用方现在的地址） | 无；被调方加入这些地址，重新测量这条路径 |
| `direct.trace` | `{"node": 调用方 id}` | `{"events": [{"time": …, "text": …}...]}`：被调方与调用方最近一次寻找 UDP 路径的过程 |
| `net.packets` | `{"node": 调用方 id}` | 无；之后该流双向运送虚拟网络的包，见[虚拟网络](#虚拟网络)。被调方不在虚拟网络上时返回 `failed` |
| `net.networks` | 无 | `{"interfaces": ["192.168.1.0/24", …], "routes": ["10.0.0.0/8", …]}`：被调方网卡的网段和路由的目的网段（IPv4），供选择网段时避开 |

### link 的建立

1. daemon 用 `ssh -G` 解析 destination，建立 SSH 连接（只信任 known_hosts 中的主机密钥），按 keepalive 设置探测。
2. 在该连接上开 session，执行 `<remote_gst> peer-bridge`。peer-bridge 把标准输入输出接到服务器 daemon 的 control socket；服务器还没有 node 身份或 `down` 时报错退出（`gst is not up on this machine`），`gst link` 看到这个错误后提示在那台机器上执行 `gst up`，并给出 `ssh -t` 的命令，但不替用户执行；link 保留，之后自动连上。daemon 只是意外停止或运行别的 gst 时，peer-bridge 重启它。
3. 双方以 `peer` 角色握手（30 秒内完成），切换到 yamux，各调用一次 `node.info`；连到自己时报错断开。
4. 双方各向对方调用一次 `topology.sync`。
5. SSH 连接、session、yamux 会话任一结束，link 即结束，daemon 按退避重连。远端 gst 失败时，其 stderr 的第一行出现在 `Status` 的 error 中，不进日志。

### link-state 记录

```json
{
  "node": {"id": "…", "name": "laptop", "software": "gst 0.4.0"},
  "seq": 1793700000000000000,
  "expires": "2026-11-03T12:00:00Z",
  "links": [{"link": "2e340426d1c2", "peer": "…devbox 的 id…"}]
}
```

- 每个节点只发布自己的记录，`links` 是它处于 running 的 link：自己发起的带 `link`（link id），接受的 `link` 为空。`seq` 取 `max(上一次 seq + 1, 当前 Unix 纳秒)`；若收到自己旧进程发布的更大 `seq`，立即以更大的 `seq` 重新发布。
- 节点信息或 link 变化时立即重新发布；收到更大 `seq` 的记录就替换，并经各条 `topology.sync` 流推送给其他 peer（洪泛）。
- 在虚拟网络上的节点在记录里带 `net`（网卡起来之后才带，别的机器不会向答不了话的机器发包）：`{"subnet": …, "since": …, "address": …, "public_key": …}`，见[虚拟网络](#虚拟网络)。收到的记录中 `net` 不合法时整条丢弃。
- 记录有效期 30 分钟，节点每 10 分钟刷新。每个节点最多保留 1024 条记录，每条记录最多 1024 条 link。
- `Topology` 由查看方计算：`nodes`（id、name、software、reachable、`path`：从本节点出发的最短路径上的节点 id、`net`、`direct`：查看方有到该节点的 UDP 路径）与 `edges`（from、to、from 上的 link id；按 from、to、link 排序）。只有两端的记录都列出对方时，一条 link 才算存在（对端没有有效记录时以发起方为准）：失去 link 的一端可能无法再把新记录传给其他节点，另一端可以。`edges` 只含发起方列出的 link。link 是双向的，路径可以沿任一方向经过。UDP 路径不在记录里，也不算路径：它只运送包。
- 只有 SSH link 能续期记录，UDP 路径不能。记录过期时，到那个节点的 UDP 路径随之关闭。
- 人看到的（`mesh.nodes`、`gst status`、`gst ui`、`gst status -v` 的两张表）只有现在连得上的机器，也就是有 SSH 路径或运行中的 UDP 路径的机器，以及它们之间的 link。其余只是还没过期的记录，内部继续用来保留地址和解析名字，不显示。`gst status` 的 `offline` 只出现在 `mesh.nodes` 里仍有、但此刻既不可达也没有 UDP 路径的机器上；本机的 link 断了，由 `Problems:` 报告。

### 中转

- 发起方根据自己的 `Topology` 计算最短路径（源路由），最多 16 个节点。`relay.open` 的 `path` 以被调方开头，每个节点检查 `path[0]` 是自己，然后：
  - 只剩自己：在该流上继续处理 peer 方法，一个接一个，直到发起方关闭流。
  - 否则在与 `path[1]` 的 peer 会话上开新流，把 `path[1:]` 交给它，之后两边对拷字节。
- 发起方用它调用任意节点的 peer 方法：`mesh.status` 经此调用 `node.status`，`mesh.ping` 调用 `node.info`，`direct.offer` 和 `net.packets` 也一样。调用自己时在进程内完成。
- 失败时错误沿路径原样传回发起方。

### UDP 路径

两个节点之间可以有一条 UDP 路径（`internal/direct`），即使两者没有 SSH link。不需要配置、标记或 STUN 服务器。[`udp-hole-punching.html`](udp-hole-punching.html) 从 UDP 包头讲起，解释防火墙、NAT 和打洞，并带逐步的模拟器。

路径上没有 QUIC、TLS 或流，只有 WireGuard 的包和下面这几种自己的小包，靠首字节区分：WireGuard 的首字节是 1 到 4，gst 的是 `0x11` 起，后面跟 16 字节 token。

| 首字节 | 名字 | 内容 | 含义 |
| --- | --- | --- | --- |
| `0x11` | probe | 发送方 id（8 字节）、时间（8 字节）、目标地址（18 字节） | 探测 |
| `0x12` | answer | probe 的时间和目标地址、看到的来源地址（18 字节） | 回应探测，证明这条路通，并告诉对方它的来源 |
| `0x13` | bye | 无 | 关闭 |

地址是 16 字节 IP 加 2 字节端口。

- **socket**：daemon 启动时开 IPv4 和 IPv6 各一个通配 socket（`direct.Transport`），所有路径共用，端口默认 15355，被占用时由系统分配，每次 offer 都带上，所以不用记住。不绑定具体地址：发包听系统路由，走别的 VPN 也可以，只要能通。macOS 例外：代理的 TUN 接管路由时，它用自己的 socket 重新发送每个流，规则是直连也一样（实测 Shadowrocket：本机的端口 15411 发出的包，由它从端口 58259 重新发出），端口变了，对方防火墙为原端口开的洞就对不上。分不清代理和路由型 VPN，所以 socket 用 `IP_BOUND_IF` 和 `IPV6_BOUND_IF` 绑到有网关、不是点对点的默认路由所在的网卡，优先不带 scope 的默认路由，每 5 秒重新看一次，直接改 socket 选项，端口不变；没有这样的网卡时交给路由。代价是 macOS 上经 VPN 才能到的地址走不了 UDP。socket 收发缓冲 4 MiB。
- **区分对端**：probe、answer、bye 靠 token 找到路径。WireGuard 的包靠来源 `IP:port` 找到路径，只有用 token 证明过的来源才放行，其余丢弃。这个归属不一定准：同一个 NAT 后的两台机器访问本机不同的 IP 时，来源可能相同。所以 WireGuard 关掉了 roaming，endpoint 始终是记录里的 node id，包的来源只用于投递；解密靠 WireGuard 自己的索引，回包不会被这种归属带偏。
- **候选地址**：系统发包用的源地址排在最前（向公网地址 connect 一个 UDP socket，再读本地地址，不发包），然后是本机所有接口上的地址（跳过回环和 link-local，每个地址族最多 8 个），加上最近 10 分钟别的节点看到的本机地址（最多 4 个）。源地址必须在列：socket 是通配的，不管探测发到本机哪个地址，回包都从这个地址出去，而对方的防火墙只放行它发过包的地址。只排除 gst 网段里的地址，否则包会钻进它自己组成的路径：提供、收到和从来包学到的地址都过滤。
- **换网络**：每 5 秒检查一次本机地址和绑定的网卡。有变化时清掉退避和别的节点看到的本机地址，所有运行中的路径从头测量，并经 `direct.update` 把新地址告诉对方。
- **触发**：`Node.SendPacket` 发现包只能走 SSH 时，最多每 5 秒请求一次（`gst ping` 的包和 `-v` 也会）。尝试不阻塞包。失败或路径因对方沉默而结束后等 5 秒再试，连续失败时加倍，最长 5 分钟，成功后重置。一方主动关闭的路径（空闲、记录过期、link 删除、新 offer 顶替）不算失败，不等待。
- **信令**：发起方经 mesh 调用对方的 `direct.offer`（`Node.Call`）。`DirectOffer` 是 `{"node": 发起方 id, "token": …, "addresses": ["IP:port", …]}`，响应是对方的 `addresses`。候选地址只出现在这次调用中，不进记录，不进日志。
- **同时发起**：两个节点同时发起时，id 较小一方的 offer 继续，id 较大的一方放弃自己的。收到 offer 时本节点还有到对方的路径，说明它已失效，先关闭。
- **探测**：双方每 200 ms 向对方所有地址发 probe，收到后回 answer。收到 probe 时，把来源地址也加入探测目标：NAT 后的节点不知道自己的公网地址，但只要一方能到达另一方，另一方就从来包学到地址。双方都有状态防火墙时，同时发包也让两边放行对方。发送方 id 让 daemon 认出自己的 probe：两台机器可能有相同的地址（比如容器网桥）和相同的端口，probe 会回到自己。
- **公网地址**：answer 带着对方看到的来源。NAT 对一个 socket 保持同一个公网端口时，这个地址对所有节点都有效，所以写进之后给别的节点的 offer，两台都在 NAT 后的机器也有机会打通。不需要 STUN 服务器。
- **选路**：两端各自选，不用协商，来回可以走不同的路。第一个回 answer 的地址证明路径可用，开始承载包。成绩记在 probe 的目标地址上，不看 answer 从哪个地址来，多地址的机器不一定从收到探测的地址回复。之后持续比较来回时间（平滑后）：新地址至少测过 3 次，比当前的低出一个余量才切换，余量是当前地址来回时间的一成，至少 2 ms。私网地址（`netip.Addr.IsPrivate`：RFC 1918 和 `fc00::/7`）比较时按来回时间减去自己余量的两倍算：包留在局域网或两台机器共同的 VPN 里，更稳，也不占公网带宽。所以私网地址只要慢得不超过约一个余量就顶替公网地址，公网地址要快出约三个余量才能换回来。路径刚建好后的 1 秒，以及之后每 30 秒，向所有地址探测 1 秒。切换不换 socket，WireGuard 不用重新握手。整次尝试限时 15 秒。
- **认证**：token 只用来找路径，不用于认证。身份和加密都是 WireGuard 的：对方拿不出记录里那把公钥对应的私钥，握手就不成立。
- **维持**：每秒向在用的地址发 probe，同时让 NAT 和防火墙的表项不过期。在用的地址 2 秒没有 answer，就重新探测所有地址，有回应的地址立刻顶替，所以一端换了网络时路径不用重建。3 秒什么都没收到（answer 和 WireGuard 的包都算），路径结束，包回到 SSH，WireGuard 的会话不受影响，所以程序的连接不断。只有一个方向断了时，听不到对方的一端先结束，另一端随后也收不到包，约 6 秒后两端都回到 SSH。UDP 发送出错的包当场改走 SSH。关闭时发 bye，对方立即知道。
- **空闲**：本端 45 秒没在路径上发过包（`Path.Idle`，probe 不算），就关闭路径，之后不再有 probe 和全量探测。只看发送就能代表双向的流量：WireGuard 收到数据后 10 秒内一定回包，没有数据可回时发 keepalive；发送的包都是本机 WireGuard 产生的，不会被伪造的来包续命。下一个包经 SSH 发出，同时触发新的尝试，不等待；这时 NAT 表项可能已过期，所以总是重新打洞，而不是先试旧地址。SSH link 到不了对方时不关闭：这时路径是唯一的路，靠探测维持到记录过期。
- **SSH 决定成员**：SSH 是控制面和兜底。UDP 路径只是捷径，公钥、信令都来自 mesh，不让本来不相连的机器相连。
- **隐私**：拓扑、路径、地址只出现在交互式输出中（命令输出、`gst ui`）。后台日志（daemon 日志，link 的事件也记在其中）不记录服务器地址、用户、密钥路径、目标地址，也不记录远端 gst 返回的错误原文。

## 虚拟网络

虚拟网络（`internal/vnet`）让机器在一个私有网段里互相用 IP 通信，是 gst 唯一的数据链路。机器身份、成员、路径和信令都来自 mesh，所以没有协调服务器。

```
程序 ── 网卡（utun / gst0）── wireguard-go ── bind ── Node.SendPacket ─┬─ UDP 路径
                                                                       └─ 沿 SSH link 的流
```

- **WireGuard**：daemon 里跑 wireguard-go，加密和重放保护都用它的。mesh 对它来说是一张网（`conn.Bind`）：endpoint 是 node id，不是地址，所以路径怎么变都不需要 WireGuard 知道，切换时不用重新握手，也不用 PersistentKeepalive（打洞由 beat 维持）。私钥只在 `net.json`（control socket 旁边，`0600`），公钥在记录里，信任来自 SSH。
- **路径**：`Node.SendPacket` 有 UDP 路径就直接发，否则放进队列，由一条 `net.packets` 流沿 mesh 运送（帧：2 字节大端长度加包，两个方向都走，1 分钟空闲关闭）。任何一方都可以开这条流。包在 mesh 上不需要保护：途中只有密文，丢了就是丢了。队列满时丢包，和拥塞的网络一样。
- **MTU**：网卡 MTU 是 1168，加上 WireGuard 的 32 字节是 1200 字节的 UDP 载荷，几乎所有路径都能承载，所以不需要路径 MTU 发现。
- **网段**：每个节点把 `{subnet, since, address, public_key}` 放进自己的记录（`net` 字段）。`since` 最大的网段胜出，相同时取字符串较小的，所以所有节点独立算出同一个结果。`gst subnet` 把 `since` 写成当前时间；gst 自己选的网段写成当前时间的相反数，所以它让位给任何人选的网段，自己选的之间最早的留下：一台新机器在还不知道别人时选的网段，不会把已有的网络搬走。第一个节点用 `net.networks` 问所有能到达的节点各自的网卡和路由，选一个 `/16`（优先 `10.64.0.0` 到 `10.255.0.0`，起点由 node id 决定）：先找和这些网段都不重叠的，因为宽路由后面也可能有机器；找不到时只避开冲突的。冲突指网卡的网段和它重叠，或者路由落在它里面（前缀不短于它）。更宽的路由输给网段自己更窄的那条，不占它的地址：VPN 常把所有地址分成几半来路由，有的还把所有私网路由给本地路由器，这时没有一个网段完全不重叠。网段和本机其他网络冲突时不阻止运行，只报告。
- **gst ping**：每个 daemon 在自己的网络地址上监听 UDP 端口 15354，把收到的包原样发回。发送方是普通的 UDP socket，所以包经过路由、网卡、WireGuard 和实际路径，和程序的包完全一样：有回复说明这些都通，时间就是程序感受到的延迟。标签来自回复时的路径。`-s` 指定包的大小，超过网卡 MTU 时分片。
- **本机地址**：Linux 自己投递发往本机地址的包。macOS 把 utun 的地址路由进 utun，这些包到了 WireGuard，而 WireGuard 没有这个地址的 peer，所以 `vnet` 在 WireGuard 之前把它们写回网卡（`loopback`）。
- **地址**：由 node id 的哈希决定，发生冲突时 id 较小的节点保留，较大的重选。网段、地址和私钥存在 `net.json`，重启后不变。私钥和地址属于一个身份，`net.json` 记下这个 node id；id 变了就换新私钥、重选地址，只留下属于网络的网段，否则别的机器手里旧身份的记录会和新身份共用一把公钥。
- **名字**：daemon 在 `127.0.0.1:15353` 回答 `.gst` 的 A 记录（`mesh.FindNode`，所以 id 前缀也能用），未知名字 NXDOMAIN，AAAA 返回空。macOS 用 `/etc/resolver/gst`，有 systemd-resolved 的 Linux 用 drop-in（`Domains=~gst`），其他 Linux 写 `/etc/hosts` 中 `# gst begin` 到 `# gst end` 之间的部分。
- **helper**（`internal/tun`）：daemon 以用户身份运行，创建网卡、地址、路由和 DNS 配置需要 root，由系统服务（`gst net-helper --user NAME`）完成。`gst up` 用 `gst helper-version` 询问 root 那份 gst 的 `tun.HelperVersion`，这一步不需要 root；helper 缺失、版本不同或没有在监听时，它显示要执行的 `sudo <gst 的绝对路径> install --user NAME` 和这条命令的改动，询问后在当前终端执行，密码由 sudo 询问。版本号只在 helper 的代码或 daemon 对它的要求变化时提高，所以普通升级不需要 root。`gst down` 经同一个 socket 发 `Spec{Remove: true}`，不需要 root：这条消息没有参数，只能让 helper 删除固定路径上的服务和那份 gst。helper 先删文件，回复，最后让服务管理器停止自己，因为这一步会结束进程。helper 没有应答时，`gst down` 才说明并询问后用 `sudo <gst> uninstall`。它运行 root 拥有的一份 gst（Linux `/usr/local/libexec/gst-net`，macOS `/Library/PrivilegedHelperTools/local.gst.net`），绝不运行用户可写的文件。daemon 经 `/var/run/gst-net.sock`（属于该用户，`0600`）发 `Spec`（子网、地址、hosts），helper 校验后执行，把打开的设备用 `SCM_RIGHTS` 交给 daemon；包都在 daemon 里处理，helper 不碰。Spec 只能配置私有网段里的一个地址和 `.gst` 下的名字。网卡的生命周期是这条连接：daemon 退出，helper 撤销一切。新 daemon 连上时取代旧的。daemon 每轮重发 Spec，内容相同时什么也不做，兼作存活检查。
- **管理循环**：`vnet.Manager` 在拓扑变化、配置变化和每 5 秒时比较一遍：算网段和地址、发布记录、调 helper、同步 WireGuard 的 peer。`Configure` 等这一轮结束再返回，所以 `gst subnet` 看到的 `Status` 已经反映新配置。没有 helper 或网段重叠是 `NetState` 里的 `error` 和 `hint`，不是失败退出，装好 helper 后下一轮就启动。
- **没有 IPv6**：网段只有 IPv4，AAAA 返回空。

## 端口暴露

- `gst expose <target> [--port port] [--udp]` 写本机配置中的 `exposes` 数组，每项是 `{protocol, port, target}`，以 `(protocol, port)` 唯一标识。target 为 `tcp://host:port`、`udp://host:port` 或 `unix:///absolute/path`，Unix stream 在网络侧使用 TCP。CLI 复用配置锁、原子写入、`daemon.reload` 和状态查询，不增加 peer 方法。
- `vnet.Manager` 在接口配置成功后把当前地址和规则交给 `internal/expose.Manager`，网络未就绪时传空地址；down 先关闭 expose。地址变更、规则替换或移除都会关闭旧监听和现存连接；相同规则重载不影响连接。监听失败只影响对应规则，每轮重试，不阻止虚拟网络运行。
- expose 只使用明确的 gst IPv4 地址创建 `tcp4` / `udp4` socket，绝不回退到通配地址。系统 TCP/IP 栈和现有 WireGuard 路径承载入口流量，不增加 mesh 数据路径。出口由用户 daemon 的 `net.Dialer` 连接，使用 5 秒连接超时；TCP/Unix stream 复用 `relay.Pipe`，保留半关闭。
- UDP 每个来源 IP:port 有一个独立 connected UDP socket，回包通过原监听 socket 返回。每规则最多 256 个映射，每映射有 16 个报文的有界队列，溢出丢弃；独立 goroutine 拨号，不阻塞其他客户端。双向流量刷新 2 分钟空闲超时。规则停止时取消拨号、关闭 socket，并等待 goroutine 退出。
- `Status.exposes` 新增 `{protocol, port, target, address?, state, error?}`；state 为 `waiting`、`running` 或 `failed`。CLI 在 daemon 不报告规则时显示 `stopped`，停机也能读配置列出规则。`running` 仅证明绑定成功，不探测目标。状态可交互查看，但目标、客户端地址和原始网络错误不写 daemon 日志。新增可选字段不改变帧或现有字段语义。

## Web UI

- `gst ui` 是前台进程，只监听 `127.0.0.1`（`--port`，默认任意空闲端口），启动时生成随机 token，打印 `http://127.0.0.1:<port>/#<token>` 并打开浏览器（`--no-open` 跳过）。daemon 只有 control socket 这一个控制入口，退出后不留监听。
- 页面（`internal/cli/ui`，go:embed）以 `Authorization: Bearer <token>` 请求 `GET /api/state`；`Host` 不是 localhost 或回环地址时返回 403；响应带严格的 CSP 等安全头。页面用 DOM 文本节点渲染所有字段。
- `/api/state` 返回本机 node、`Status`、daemon 是否应答、`Topology`；只供页面使用，字段可以改变。页面只读：不能启动、停止或重启 link；daemon 不运行时，页面说明这一点。机器表显示 `direct`、`relay` 或 `relay via …`。
- `GET /api/nodes/{node}`（同样需要 token）返回 mesh 中任一机器的 `Status`，即 `mesh.status` 的结果，`{node}` 是机器的称呼（名称、id 或 id 前缀）；出错时返回 502 和错误信息。页面选中某台机器时请求它。
