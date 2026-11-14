# gst 设计

gst 把多台机器连成一个私有网络：每台机器跑一个 daemon，daemon 之间经 SSH 链路连成 mesh，复制一张描述整个网络的表，并沿链路转发消息；机器之间的数据全部走 WireGuard，包经 UDP 直连，打洞不成时沿 SSH 链路转发。使用体验接近 Tailscale，每台机器用 root 安装一次 helper。

mesh 分三层，每层只用它下面的层：

```
┌────────────────────────────────────────────────────────────────┐
│ 应用：虚拟网络（WireGuard、DNS）、UDP 打洞、status、ping、remove   │
├────────────────────────────────────────────────────────────────┤
│ ③ 路由层  send(to, msg)：按路由表逐跳转发；请求 / 应答 / 包         │
│           路由表由状态层里的链路图算出                              │
├────────────────────────────────────────────────────────────────┤
│ ② 状态层  全网复制的一张表：node/<id>、removed/<id>、subnet        │
│           只和相邻机器说话：连上时互发整张表，之后洪泛变化           │
├────────────────────────────────────────────────────────────────┤
│ ① 链路层  SSH 链路：到一台相邻机器的、有序可靠的帧管道               │
└────────────────────────────────────────────────────────────────┘
```

| 层 | 接口 | 用到 | 不知道 | 实现 |
| --- | --- | --- | --- | --- |
| ① 链路 | 发一帧（`urgent` 与否）、收到的帧、对端是谁 | SSH | 帧里是什么、网络里还有谁 | `internal/mesh/link` |
| ② 状态 | 表的读写、变化通知，按条目类型合并 | 链路 | 消息怎么转发 | `internal/mesh/state` |
| ③ 路由 | `Call(ctx, to, method, params)`、`Handle(method, fn)`、`SendPacket(to, packet)` | 链路、状态算出的路由 | 状态怎么同步、谁在网络里 | `internal/mesh/route` |

**状态层在路由层下面**，和 OSPF 一样：先靠相邻机器之间的同步得到全网的链路图，才算得出路由表。所以同步不经过路由，只在一条链路的两端之间进行；路由层的消息可以跨多跳。`internal/mesh` 的 `Node` 把三层接起来，并实现加入与移除的规则、UDP 打洞的信令和拓扑。

UDP 路径不是链路：它不运帧，只是两台机器之间给 WireGuard 包走的捷径（`internal/direct`）。虚拟网络（`internal/vnet`）是唯一的数据通路。

## 概念

| 概念 | 是什么 | 命令 |
| --- | --- | --- |
| **node**（用户叫 machine） | 机器上的 gst，以 128 位随机 id 标识，名字只用于显示和寻址。本文的“机器”只指计算机本身：网卡、路由、SSH 服务、系统服务 | `gst name`、`gst status` |
| **网络** | 加入了、没有被移除的所有 node。node 在网络里，离线也在 | `gst add`、`gst remove` |
| **link**（链路） | 本机到另一台 gst 机器的 SSH 连接，以 destination 区分，没有自己的名字 | `gst add`、`gst unlink` |
| **UDP 路径** | 两个 node 之间打洞得到的一对 `IP:port`，只运送 WireGuard 的包，不是 link | `gst status` |
| **虚拟网络** | 一个私有网段，每台机器有 IP 地址和 `<machine>.gst` | `gst up`、`gst subnet` |

```
laptop ──link──► homelab
laptop ──link──► devbox
```

homelab 上 `ssh devbox.gst`：名字解析成 devbox 的虚拟地址，路由把包交给网卡，WireGuard 加密后经 UDP 路径发给 devbox，没有 UDP 路径时经 laptop 转发。homelab 不需要 devbox 的 SSH 凭据。

每个概念只用一个词。用户看到的词和代码里的词一一对应：machine ↔ node、network ↔ network、direct / relay ↔ way（`WayDirect` / `WayRelay`）、relay via X ↔ route（沿 link 经过的 node）、gst IP ↔ address、network helper ↔ helper。mesh、peer、拓扑、表、条目、代数、握手、UDP 路径只出现在代码、日志、`-v` 和本文档中；“路径”只指 UDP 路径，“网络”从不指网段（网段叫 subnet，机器上已占用的网段叫 used prefixes）。完整的术语表见 `AGENTS.md`。

## 决策

- **mesh 不可关闭**：gst 本身就是 mesh。机器第一次写配置时生成 node 身份（`node.id`、`node.name`，名字默认取主机名）。别的机器绝不替它做设置：没有身份的机器拒绝 `gst peer-bridge`，要有人在那台机器上执行 `gst up`。
- **up / down**：`gst down` 是 `gst up` 的逆操作：在配置里写 `down: true`，停止 daemon 并删除服务，再删除 helper；之后修改配置的命令不再启动 daemon，`gst peer-bridge` 也拒绝连接。`gst up` 清除它，启动 daemon，并处理 helper（见“虚拟网络”）。除 down 外，daemon 总在运行（即使没有 link），因为它要运行虚拟网络，并接受别人的 link。
- **在网络里和在线分开**：node 加入后一直在网络里，离线只是状态：名字照样解析，gst IP 照样占用，WireGuard 照样认识它，它一回来就能用。只有 `gst remove` 让它离开。
- **link 只是连接方式**：`gst add` 管加入和 link，`gst unlink` 只删本机的一条 link，不通知任何 node，也不改变谁在网络里。
- **最终一致**：不用共识；每类共享状态有一条写明的合并规则，任意两台机器连上就收敛。
- **虚拟网络不可关闭**：有 helper 就在网络上，没有就在 `gst status` 里说明。`gst up` 装好 helper 就是加入，`gst down` 删除 helper 就是离开，所以没有单独的开关。
- **信任**：control socket 权限 `0600`，只有本机同一用户能连；link 由 SSH 登录建立，登录的就是该用户。网络里的机器都属于同一个用户、彼此完全信任，WireGuard 公钥经 link 传递，所以它们信任 link 所信任的。网络里任何 node 都能移除任何 node。一台机器在所有地址上打开的端口对其他机器开放，只监听 loopback 或 Unix socket 的服务需要显式用 `gst expose` 暴露。
- **只有一条数据通路**：应用的数据只走 WireGuard，没有按连接中转字节流的第二条路。路由层只运送控制消息和 WireGuard 的包。
- **SSH 决定谁在网络里**：SSH 是控制面和兜底。控制消息（表的同步与泛洪、调用、UDP 路径的信令）永远只走 SSH link，不走 UDP 路径或 WireGuard。UDP 路径只是捷径，公钥、信令都来自 mesh，不让本来不相连的机器相连。所以 SSH link 图被切成两半时，两半之间各自收不到对方的变更，直到有 link 把它们连起来；`gst status` 把这种只能直连到达的 node 报为问题。
- **隐私**：拓扑、路径、地址只出现在交互式输出中（命令输出、`gst ui`）。后台日志（daemon 日志，link 的事件也记在其中）不记录服务器地址、用户、密钥路径、目标地址，也不记录远端 gst 返回的错误原文。

## 单进程 daemon

- 每个用户一个服务：Linux 上是 systemd 用户服务 `gst.service`，macOS 上是 launchd agent `local.gst`，命令行为 `gst [--config path] daemon`。
- 文件位置：

  | | Linux | macOS |
  | --- | --- | --- |
  | 配置 | `$XDG_CONFIG_HOME/gst/config.json`（默认 `~/.config/gst/config.json`） | 同左 |
  | 状态目录 | `$XDG_STATE_HOME/gst`（默认 `~/.local/state/gst`） | `~/Library/Application Support/gst` |
  | control socket | `<状态目录>/gst.sock` | 同左 |
  | 网络的表 | `<状态目录>/state.json` | 同左 |
  | 私钥和地址 | `<状态目录>/net.json` | 同左 |
  | 给 link 运行的 gst | `~/.local/state/gst/bin/gst`（符号链接） | 同左 |
  | daemon 日志 | journald（`journalctl --user -u gst`） | `~/Library/Logs/gst-daemon.log` |

- daemon 通过 `<socket>.lock` 上的 flock 保证单实例。
- 配置文件是本机设置的唯一事实来源，`state.json` 是网络的表在本机的副本。CLI 修改配置后调用 `daemon.reload`：daemon 更新 node 身份，启动新的 link、停止已删除的 link、重启 ssh 参数改变的 link。
- CLI 每次同步时比较 daemon 的 build（可执行文件的 SHA-256），不同就重启服务，让升级生效。
- `gst up` 把正在运行的 gst 链接到 `~/.local/state/gst/bin/gst`（先建临时链接再改名）。别的机器的 link 运行这个固定路径，所以 gst 不必在 SSH 登录 shell 的 `PATH` 里。

## 配置文件

```json
{
  "version": 6,
  "node": {"id": "3f2a9c10…（32 位十六进制）", "name": "laptop", "generation": 0},
  "links": [
    {"destination": "devbox", "ssh_args": ["-p", "2200"], "target": "<devbox 的 id>"},
    {"destination": "admin@ssh.example.com", "admit": true}
  ]
}
```

- `node.generation`：这台机器第几次加入网络，见[加入与移除](#加入与移除)。它和 id 一样是身份，所以在配置里，不在 `state.json` 里：表丢了可以从邻居补回来，代数丢了，这台机器会带着旧代数被当成已移除而拒绝。
- link 的字段：
  - `destination`：ssh destination，每次连接用 `ssh -G [ssh_args] -- <destination>` 解析（`GST_SSH_CONFIG` 设置时加 `-F`）；地址、用户、私钥、known_hosts、超时都来自 ssh 配置，link 不另设。每个 destination 最多一条 link，它就是 link 的标识。
  - `ssh_args`：`gst add` 里 `--` 之后的参数，原样保存。
  - `target`：这条 link 第一次握手时对方的 node id；为空表示还没连上过。同一个 `target` 最多一条 link：带 `admit` 的新 link 第一次连上时替换到同一台机器的旧 link；不带 `admit` 的 link（比如从旧配置来的）连上时发现已有别的 link 到同一台机器，就从配置里删掉自己，记一条日志。
  - `admit`：只在 `gst add` 之后、第一次握手成功之前存在，见[加入](#加入gst-add)。
- **link id**：`config.LinkID(destination)`，destination 的 SHA-256 的前 6 字节（12 位十六进制），只用于日志（`link <id>: …`），不泄露 destination。
- 网段不在配置里，在表的 `subnet` 条目里。
- **旧版本**：版本 5 读取时丢掉 `remote_gst` 和 `net`（网段由网络重新选，或用 `gst subnet` 再设一次），下次写入时成为版本 6。更早的版本不转换：报错，说明要删除文件并重新 add。

## 机器寻址

命令里的 `<machine>` 和 DNS 里的 `<machine>.gst` 按顺序解析，只在网络里的 node 中查找：

1. 去掉末尾的 `.gst`；
2. 节点 id；
3. 节点名（唯一时）；
4. 至少 4 位的 id 前缀（唯一时）。

link 没有名字，不参与寻址。`gst unlink` 只接受本机 link 的 destination。

## gst 协议

CLI 与 daemon、daemon 与 daemon 使用同一套协议，实现位于 `internal/wire`。帧格式固定，扩展只靠新增方法、新增字段、新增错误码。

### 握手

每条连接先交换 `Hello`：一个大端 `uint32` 长度加这么多字节的 JSON，不超过 1 MiB。发起方先发，接收方回：

```json
{"protocol": 6, "software": "gst 0.7.0+a2ffc87", "role": "peer", "build": "sha256:…",
 "node": {"id": "…", "name": "laptop"}, "membership": {"generation": 0, "admit": true}}
```

| 字段 | 含义 |
| --- | --- |
| `protocol` | 发送方的协议版本，当前为 6。版本不同时拒绝，错误里写出双方的 `software` |
| `software` | 人类可读的软件版本：发布版本加构建所用的 commit，仅供显示；判断是否同一个可执行文件用 `build` |
| `role` | `control`（CLI → 本机 daemon）或 `peer`（daemon ↔ daemon，一条 link） |
| `build` | 可执行文件标识，CLI 用它判断 daemon 是否需要重启 |
| `node` | `peer`：发送方是谁 |
| `membership` | `peer`：发送方的 `generation`，`admit`（这条 link 来自 `gst add`、还没连上过），拒绝时可带对方的移除记录 `removed` |
| `error` | 仅出现在回复中：拒绝原因，随后连接关闭 |

接收方忽略未知 JSON 字段。删除字段或改变字段含义必须提升协议版本。

### control 连接

握手之后，control 连接上顺序进行调用，每次一个请求帧、一个响应帧（同样是长度加 JSON）：

```
请求：  {"method": "mesh.status", "params": {"node": "devbox"}}
响应：  {"result": {...}}
     或 {"error": {"code": "not_found", "message": "no machine \"devbox\" in the network; see `gst status`"}}
```

错误码：`unsupported_method`、`unsupported_role`、`unsupported_protocol`、`invalid_params`、`not_found`（机器或 link 不存在）、`forbidden`、`unreachable`（没有路到那台机器）、`removed`（本机已被移除）、`failed`。

| 方法 | 参数 | 结果 |
| --- | --- | --- |
| `daemon.status` | 无 | `Status`：software、build、pid、config、`node`、`links`（配置中的每条 link，按配置中的顺序：`destination`、`ssh_args`、state、since、error、reconnects、对端 `peer`）、`exposes`、`net`（`NetState`；daemon 不能运行网络时没有）、`removed`（本机被移除时：谁、什么时候） |
| `daemon.reload` | 无 | 重新读取配置并同步后的 `Status` |
| `mesh.topology` | 无 | `Topology`，见[拓扑](#拓扑) |
| `mesh.status` | `{"node": <machine>}` | 该机器 daemon 的 `Status`，经路由层调用它的 `status` |
| `mesh.ping` | `{"node": <machine>}` | `{"node": NodeInfo, "via": [NodeInfo...], "time_ms": 数字}`：经路由层调用一次 `ping`；`via` 是路由上的中间机器。`gst ping -v` 用它检查 link |
| `mesh.remove` | `{"node": <machine>}` | `Removal`：`node`、`self`（移除的是本机）、`stranded`（只经它连到本机、移除后会离线的机器），见[移除](#移除gst-remove-machine) |
| `mesh.subnet` | `{"subnet": cidr}` | 无：写入人选的 `subnet` 条目 |
| `net.ping` | `{"node": <machine>, "size": 字节数}` | `{"node", "address", "way": "direct" 或 "relay", "endpoint", "via", "time_ms"}`：经虚拟网络向该机器的 daemon 发一个包并收回。`gst ping` 每秒调用一次 |
| `mesh.direct` | `{"node": <machine>}` | `DirectReport`：本机到它的 UDP 路径、尝试的状态、双方最近两次尝试的过程（对方的经 `direct.trace` 取得）和丢包计数。`gst ping -v` 用它 |

link 状态：`connecting`（建立 SSH 与握手中）、`running`（握手完成）、`down`（失败，等待重试）。失败后的等待从 5 秒起每次翻倍，最长 5 分钟；link 进入 running 后重置。本机换到另一个网络时（[UDP 路径](#udp-路径)的地址检查发现变化，`Node.Moved`），等待中的 link 立即重试，等待回到 5 秒：挡住它们的原因可能已经不在了，而手机这样常换网络的机器否则每次都要等满最长的间隔。

## ① 链路层

### 建立

1. daemon 用 `ssh -G` 解析 destination，建立 SSH 连接（只信任 known_hosts 中的主机密钥）。gst 不发 SSH 层的 keepalive，对端是否还在由链路的 `alive` 帧判断。
2. 在该连接上开 session，执行固定的命令：

   ```sh
   sh -c 'g="$HOME/.local/state/gst/bin/gst"; [ -x "$g" ] || g=gst; exec "$g" peer-bridge'
   ```

   也就是 `gst up` 放好的那份 gst，没有时用 `PATH` 里的。`sh -c` 让它不受登录 shell 的影响。peer-bridge 把标准输入输出接到服务器 daemon 的 control socket；服务器还没有 node 身份或 `down` 时报错退出（`gst is not up on this machine`），`gst add` 看到这个错误后提示在那台机器上执行 `gst up`，并给出 `ssh -t` 的命令，但不替用户执行；link 保留，之后自动连上。daemon 只是意外停止或运行别的 gst 时，peer-bridge 重启它。远端 gst 失败时，其 stderr 的第一行出现在 `Status` 的 error 中，不进日志。
3. 双方以 `peer` 角色握手，30 秒内完成。接收方检查发起方，拒绝时在回复的 `Hello` 里带 `error`；发起方检查接收方，拒绝时发一个 `bye` 帧再关闭。两边检查的内容一样：协议版本、不是自己、加入与移除的规则（见[加入与移除](#加入与移除)）。
4. 握手之后双方把整张表发给对方（[同步](#同步)），link 进入 running。

SSH 连接或 session 结束，link 即结束，daemon 按退避重连。被拒时带回的移除记录可能让本机改变代数后立即可以通过（见[加入](#加入gst-add)），这时 daemon 立即重试一次，不等退避。

### 帧

```
+----------------+------------+--------------------+
| length: uint32 | type: byte | body (length 字节)  |
|  big-endian    |            |                    |
+----------------+------------+--------------------+
```

| type | 名字 | body | 归谁 |
| --- | --- | --- | --- |
| 1 | `sync` | `{"entries": [...]}`，一批表的条目 | 状态层 |
| 2 | `msg` | 一条路由消息，见下文的路由层 | 路由层 |
| 3 | `alive` | 空 | 链路层 |
| 4 | `bye` | `{"error": …, "membership": …}`：主动结束的原因，可带移除记录 | 链路层 |

- body 不超过 1 MiB。链路层只按 `type` 分发，不解析 `sync` 和 `msg` 的内容。
- **保活**：15 秒没发过任何帧就发一个 `alive`；45 秒一个字节都没收到，link 结束（收到大帧的一部分也算，慢 link 上的大批同步不会被当成断开）。
- **两个发送队列**：`sync`、`bye` 和控制消息进紧急队列，不设上限；虚拟网络的包进普通队列，最多 1024 帧，满了就丢，和拥塞的网络一样。写的一方先发紧急队列。所以包再多，控制消息也照常发出。
- **bye**：一方主动结束 link 时先发 `bye`，写失败时只关闭连接，让读的一方还能读到已经到达的 `bye`。
- **每个方向一条**：两台机器之间每个方向最多一条 link，两个方向可以各有一条。同一方向连上新的一条时，两端都关闭旧的那条（`bye` 说明被新 link 替换）：旧的多半已经断了，只是还没超过 45 秒。状态层和路由层把两台机器之间的 link 看成一个邻居，发送时用最新的那条。

## ② 状态层

### 表

全网复制一张表：键 → 条目。每个条目有键、版本和值，**每种键一条合并规则**：比较版本，大的胜。同一个键只保留一条。表写在 `state.json`，不过期。

```json
{"key": "node/3f9c…", "version": [0, 1793700000000000000], "value": {…}}
```

#### `node/<id>`：一台机器

只有 `<id>` 那台机器写：

```json
{
  "key": "node/3f9c…",
  "version": [0, 1793700000000000000],
  "value": {
    "name": "laptop",
    "software": "gst 0.7.0+a2ffc87",
    "links": [
      {"peer": "<devbox 的 id>", "opened": true},
      {"peer": "<tablet 的 id>", "opened": false}
    ],
    "net": {"address": "10.99.3.7", "public_key": "<64 位十六进制>"}
  }
}
```

- `version` 是 `[generation, seq]`，先比 `generation`，相同再比 `seq`。
- **`seq`**：内容变化（改名、link 建立或断开、换地址）时加大，取 max(上次 + 1, 当前 Unix 纳秒, 表里自己条目的 seq + 1)。所以 `seq` 也就是这台机器最后一次发布的时间：别的机器用它显示离线的 node `offline, seen 3h ago`。在线的机器每 10 分钟重新发布一次，精度是 10 分钟。这是唯一的定时发布，只用于显示：没有东西会因为它旧而过期。
- **`generation`**：这台机器第几次加入网络。一直是 0，只有被移除之后又被 `gst add` 加回来时才加 1，存在配置里。只有 `seq` 的话，移除记录只能写成"`seq` 不超过 N 的都作废"，被移除的机器在收到消息之前继续发布更大的 `seq`，又回到网络里；`generation` 只在有人执行 `gst add` 时才变，所以移除压得住这一代的所有条目。
- `links` 是此刻 running 的 link，`opened` 表示是不是本机发起的。`net` 在本机还没进入虚拟网络时为空。
- 收到自己的条目、版本比自己发布的大（`state.json` 丢了、或旧进程发布过）时，用更大的 `seq` 重新发布；代数更大时先采用那个代数。

#### `removed/<id>`：一条移除记录

- 网络里任何 node 都能写（`gst remove`）。
- 版本 `[generation]`：被移除时那台机器的代数。值 `{"by": <id>, "by_name": <名字>, "at": <时间>}`，`by_name` 让被移除的机器在忘掉整个网络之后还能说出是谁移除了它。版本相同而值不同时（两台机器同时移除它），`at` 较早的胜，再相同则 `by` 较小的胜。
- 生效：`node/<id>` 的 `generation` 不大于移除记录的版本，这个 node 就不在网络里了，它的 `node` 条目从表里删除、也不再接受和转发。
- 永久保存。每条几十字节；如果回收，长期离线的机器回来时会把被移除的机器带回网络。

#### `subnet`：网络的 subnet

- 网络里任何 node 都能写：`gst subnet`，或网络里第一台机器自动选。
- 值 `{"subnet": "10.99.0.0/16"}`，版本 `[kind, order]`：
  - 人选的 `kind` 为 1，网络自动选的为 0，独自一台的机器自动选的为 −1。人选的总是胜过自动选的，网络的胜过独自一台的。
  - 人选的 `order` = max(当前 Unix 纳秒, 见过的最大人选 order + 1)：在看到别人的选择之后再选，一定胜出，不受时钟偏差影响。
  - 自动选的 `order` = −(当前 Unix 纳秒)，更早的胜：两个网络合并时，先有的网络保留它的 subnet。
  - 自动选的总是先写成 −1；表里有了另一个没被移除的 node，节点就把它原样升为 `[0, order]`（同一条升级在各节点上结果相同，不会来回覆盖）。所以一台很早就独自 `gst up` 的机器被加进网络时，它的选择输给网络的，不会把整个网络搬走。这依赖同步的顺序：整张表发送时 `subnet` 条目排在最前，新加入的 node 先收到网络的选择，再看到其他 node。
  - 版本相同时，subnet 字符串较小的胜。

### 同步

只在一条 link 的两端之间进行，用 `sync` 帧，不经过路由层：路由表是从表里的链路图算出来的，表还没同步时没有路由；同步的对象本来就是 link 另一端那台机器。规则只有两条，就是洪泛：

1. **link 建立时，双方把整张表发给对方**（太大时拆成多帧）。
2. **收到一条条目，按键的规则和手里的比**：比手里的新就替换、写盘，然后发给**除了来源以外**的每个邻居；不比手里的新就丢弃。本机自己写的条目没有来源，发给所有邻居。

- 邻居和来源指的是机器，不是 link：两台机器之间两个方向的 link 算一个邻居，条目只经最新的那条发一次。
- 传播会停：一条条目在每台机器上只有第一次到达时算新，只转发那一次。网络里有环时，环上会多出一两帧，对方一比就丢。
- link 是有序可靠的，运行中发出的帧不会丢；断了重连又按第 1 条整张表重发。所以断开多久都能补齐，不靠定时器，每条 link 也不用记对方有什么。
- 同一个连通分量内最终一致。网络分开时两边各自写，连上后按合并规则收敛，和收到的先后无关。
- 上限：`node` 条目最多 1024 条，`removed` 条目最多 1024 条，单条的值不超过 64 KiB，一条 `node` 最多列 1024 条 link。超出的条目不合并，记一条日志。值不合法（名字、地址）的条目整条丢弃。

## ③ 路由层

### 消息

`msg` 帧的 body 是一条消息：

```
+---------+-----------+----------+-----------+-----------+------------+
| to (16) | from (16) | ttl (1)  | kind (1)  | id (4)    | payload    |
+---------+-----------+----------+-----------+-----------+------------+
```

| kind | payload |
| --- | --- |
| 1 请求 | `{"method": …, "params": …}` |
| 2 应答 | `{"result": …}` 或 `{"error": …}`，`id` 和请求的相同 |
| 3 包 | 一个虚拟网络的包（WireGuard 的密文） |

- `to`、`from` 是 node id 的 16 字节。`ttl` 从 16 起，每转发一次减 1，到 0 丢弃。
- 请求和应答进紧急队列，包进普通队列。

### 转发和路由表

- 每台机器收到 `to` 不是自己的消息，查路由表找到下一跳，从到它的最新 link 发出；没有路由时，对请求回一个 `unreachable` 的应答，对包直接丢弃。
- 路由表由本机从表里算：图的边是本机此刻 running 的 link，加上两端的 `node` 条目都列出对方的两个其他 node 之间的 link（只有一端列出时不算：失去 link 的一端可能已经无法把新条目传出来）。按跳数做逐层的 BFS，跳数相同时取 id 较小的第一跳，所以结果是确定的。表或 link 变化时重算。
- 中间的机器只转发，从不改走 UDP 路径：是否用 UDP 路径只由两端决定（见[UDP 路径](#udp-路径)）。

### 调用

`Call(ctx, to, method, params)` 发一个请求，等 `id` 相同的应答，默认 10 秒超时。被调方按 `Handle` 注册的方法处理，处理函数知道请求来自哪台机器。调用自己时在进程内完成。未知方法返回 `unsupported_method`。

| 方法 | 参数 | 结果 |
| --- | --- | --- |
| `ping` | 无 | `NodeInfo`：`id`、`name`、`software` |
| `status` | 无 | 被调方 daemon 的 `Status`（与 `daemon.status` 相同） |
| `direct.offer` | `DirectOffer` | 被调方的 `DirectOffer`；之后双方寻找 UDP 路径 |
| `direct.update` | `DirectOffer`（运行中路径的 token 和调用方现在的地址） | 无；被调方加入这些地址，重新测量这条路径 |
| `direct.trace` | 无 | `{"events": [{"time": …, "text": …}...], "previous": [...]}`：被调方与调用方最近一次寻找 UDP 路径的过程，和它之前的一次（显示那次怎么结束）。机器睡眠时，每个过程都记下睡了多久 |
| `net.prefixes` | 无 | `UsedPrefixes`：`{"interfaces": [...], "routes": [...]}`，被调方网卡的网段和路由的目的网段（IPv4），供选择网段时避开 |

### 拓扑

`Topology`（`mesh.topology`、`gst status`、`gst ui`）由查看方从表算出：

- `nodes`：网络里所有的 node（id、name、software、`reachable`：有路由、`route`：路由上的 node id、`direct`：有 UDP 路径、`seen`：`seq` 换算的时间，和本机发现它离线的时间取较晚的、`net`），本机排第一。本机 link 的对端还不在表里时，也先列出来。
- `links`：本机 running 的 link，加上两端都列出对方的其他 link，方向从发起方到接受方。
- `subnet`：`subnet` 条目的网段。

一个 node `reachable` 或 `direct` 就是在线，否则是离线（`offline, seen …`）。本机的 link 断了，由 `Problems:` 报告；`direct` 而不 `reachable` 的 node 也由它报告，因为表只沿 link 泛洪，这样的 node 与本机之间收不到对方的移除和改名。

## 加入与移除

### 规则

- **在网络里**：表里有 `node/<id>`，且没有压过它的 `removed/<id>`。
- **在线**：本机现在有到它的路由，或有到它的 UDP 路径。
- **离线**：在网络里但不在线。名字照样解析，gst IP 照样占用，WireGuard 照样认识它。

握手时双方都按这些规则检查对方：

- 对方被移除（`removed/<id>` 压得住它在 `Hello` 里报的代数）：拒绝，并把移除记录带给它。
- 本机被移除：对方的 link 没有 `admit` 就拒绝；有 `admit` 就把自己的代数加到移除代数 + 1，在自己的 `Hello` 里回报，通过。
- 被移除的机器也不发起没有 `admit` 的 link。

### 加入：`gst add`

1. 本机保存一条带 `admit` 的 link，连上目标，握手时 `Hello` 带 `"admit": true`。
2. 握手通过，双方互发整张表。两个网络就此合并：表取并集，各条目按规则收敛。
3. 第一次握手成功后，daemon 在配置里写下 `target`，清掉 `admit`，之后的重连不再带它。

`admit` 的作用是让被移除的机器回来。**代数总是由被移除的那台机器自己加**，条件是它知道自己被移除，并且有一次人执行的 `gst add`：

| 情况 | 过程 |
| --- | --- |
| 在网络里的机器上 `gst add X`，X 知道自己被移除了 | X 收到带 `admit` 的 `Hello`，把代数设为移除代数 + 1，在自己的 `Hello` 里回报，握手通过 |
| 同上，但 X 还不知道 | X 回报旧代数；发起方用 `bye` 带上移除记录；X 由此知道，发起方立即重连，按上一行处理 |
| 在被移除的 X 上 `gst add <网络里的机器>` | X 已知自己被移除时，发起前先把代数加 1；还不知道时，对方拒绝并带上移除记录，X 的这条 link 有 `admit`，所以加代数后立即重连，通过 |
| X 用旧的 link 连回来（没有 `admit`） | 被拒，X 按[移除](#移除gst-remove-machine)第 3 步处理，不会自己回来 |

**移除不是吊销。** 被移除的机器只要还能 SSH 到网络里的某台机器，在它上面执行一次 `gst add` 就回来了，和加入的规则一致：谁能 SSH 过去，谁就能加入。要彻底挡住一台机器，得同时在各机器的 `authorized_keys` 里去掉它的公钥。

### 移除：`gst remove <machine>`

在任意一台机器上执行：

1. 写入 `removed/<id>`，版本为对方当前的代数，然后同步出去。执行前算出只经由它连到本机的机器，在输出里说明它们会离线。
2. 每台机器收到这条记录后：断开与它的 link（发 `bye` 带上移除记录），关闭到它的 UDP 路径，把它从路由、WireGuard、DNS 中去掉，释放它的地址；daemon 删掉配置里指向它的、没有 `admit` 的 link。
3. 被移除的机器收到这条记录（从任意一条 link，或在握手被拒时）：
   - 删掉表里所有其他条目，只留下关于自己的移除记录，成为一个人的网络；
   - 关闭所有 UDP 路径，断开所有 link；daemon 删掉配置里没有 `admit` 的 link；
   - `gst status` 显示 `This machine was removed from the network by <name> 2h ago.`，并提示用 `gst add` 回来。
4. **移除本机**就是离开网络：写入关于自己的移除记录，发给所有邻居，然后按第 3 步处理自己。别的机器看到的和它被别人移除一样。

### 一致性场景

| 场景 | 结果 |
| --- | --- |
| 移除时 B 离线 | B 连上任意知情的机器，对方发来的表里有移除记录，执行第 2 步 |
| 被移除的 X 离线，回来后用自己的 link 去连 A | A 在握手时拒绝，并带上移除记录；X 执行第 3 步 |
| B 和 X 都不知道，回来后 B 先连上 X | 暂时组成一个小岛；任意一方连上知情的机器后收敛 |
| B 离线期间，X 被移除又被 `add` 回来 | B 手里 X 的代数较旧；互发表之后按代数收敛到新的 |
| 两台机器同时 `remove X` | 两条记录版本相同，按 `at`、`by` 留下同一条 |
| 一台 `remove X`，另一台同时对已在网络里的 X 执行 `add` | 不提升代数，移除胜 |
| 网络分开时一边移除了 X | 合并后移除记录胜，另一边也移除 X |

## UDP 路径

两个节点之间可以有一条 UDP 路径（`internal/direct`），即使两者没有 SSH link。不需要配置、标记或 STUN 服务器。[`udp-hole-punching.html`](udp-hole-punching.html) 从 UDP 包头讲起，解释防火墙、NAT 和打洞，并带逐步的模拟器。

路径上没有 QUIC、TLS 或流，只有 WireGuard 的包和下面这几种自己的小包，靠首字节区分：WireGuard 的首字节是 1 到 4，gst 的是 `0x11` 起，后面跟 16 字节 token。

| 首字节 | 名字 | 内容 | 含义 |
| --- | --- | --- | --- |
| `0x11` | probe | 发送方 id（8 字节）、时间（8 字节）、目标地址（18 字节） | 探测 |
| `0x12` | answer | probe 的时间和目标地址、看到的来源地址（18 字节） | 回应探测，证明这条路通，并告诉对方它的来源 |
| `0x13` | bye | 无 | 关闭 |

地址是 16 字节 IP 加 2 字节端口。

- **socket**：daemon 启动时开 IPv4 和 IPv6 各一个通配 socket（`direct.Transport`），所有路径共用，端口默认 15355，被占用时由系统分配，每次 offer 都带上，所以不用记住。不绑定具体地址：发往哪个网卡听系统路由，走别的 VPN 也可以，只要能通。macOS 例外：代理的 TUN 接管路由时，它用自己的 socket 重新发送每个流，规则是直连也一样（实测 Shadowrocket：本机的端口 15411 发出的包，由它从端口 58259 重新发出），端口变了，对方防火墙为原端口开的洞就对不上。分不清代理和路由型 VPN，所以 socket 用 `IP_BOUND_IF` 和 `IPV6_BOUND_IF` 绑到有网关、不是点对点的默认路由所在的网卡，优先不带 scope 的默认路由，每 5 秒重新看一次，直接改 socket 选项，端口不变；没有这样的网卡时交给路由。代价是 macOS 上经 VPN 才能到的地址走不了 UDP。socket 收发缓冲 4 MiB。
- **源地址**：从哪个本机地址发包由 gst 定，不由系统定。对方的防火墙和 NAT 只认它见过的那个来源，而有多个地址的机器（比如带 IPv6 临时地址的 macOS）可能每个包换一个源地址，换了的包就被丢掉。所以 socket 打开 `IP_PKTINFO` / `IPV6_RECVPKTINFO`（x/net 的 `FlagDst`），收 probe 和 answer 时读出包到达的本机地址，发包时用同样的控制消息指定源地址，只给地址、不给网卡，macOS 上网卡仍由 socket 的绑定决定：answer 从 probe 到达的地址发出；一个对方地址的 answer 到达哪个本机地址，之后到它的 probe 就从哪里发；路径在用的是一对地址（本机地址，对方地址），WireGuard 的包和 bye 都从这对里的本机地址发。还没有 answer 的对方地址（或 2 秒没回应的），probe 让系统选源地址，哪个 answer 先回来就定下哪个，所以只有被 answer 证明过的一对地址才承载包。一个本机地址在对方地址仍有回应时不被别的地址顶替：系统选源发出的 probe 迟到的 answer 不改它。指定的源地址发不出去（地址没了，macOS 上 IPv4 还要求它在绑定的网卡上）时，probe 和 answer 当场改由系统选源再发一次，那个对方地址的源地址重新由 answer 决定；WireGuard 的包不这样重发，出错就走 SSH，直到 answer 定下新的本机地址。系统不告诉包到达的地址时，一切照旧由系统选。
- **区分对端**：probe、answer、bye 靠 token 找到路径。WireGuard 的包靠来源 `IP:port` 找到路径，只有用 token 证明过的来源才放行，其余丢弃。这个归属不一定准：同一个 NAT 后的两台机器访问本机不同的 IP 时，来源可能相同。所以 WireGuard 关掉了 roaming，endpoint 始终是 node id，包的来源只用于投递；解密靠 WireGuard 自己的索引，回包不会被这种归属带偏。
- **候选地址**：系统发包用的源地址排在最前（向公网地址 connect 一个 UDP socket，再读本地地址，不发包），然后是本机所有接口上的地址（跳过回环和 link-local，每个地址族最多 8 个），加上最近 10 分钟别的节点看到的本机地址（最多 4 个）。系统选的源地址必须在列：最初的 probe 从它发出，而对方的防火墙只放行它发过包的地址。只排除 gst 网段里的地址，否则包会钻进它自己组成的路径：提供、收到和从来包学到的地址都过滤。
- **换网络**：每 5 秒检查一次本机地址和绑定的网卡。有变化时清掉退避和别的节点看到的本机地址，所有运行中的路径从头测量，probe 改由系统选源地址，并经 `direct.update` 把新地址告诉对方。在用的一对地址保持到 answer 定下新的本机地址为止。
- **谁用**：只在网络里的 node 之间，只运送两端自己的包。是否走 UDP 路径由 `Node.SendPacket` 决定：有路径就直接发，否则交给路由层；中间的机器只按路由表转发，从不改走自己的 UDP 路径。
- **触发**：`Node.SendPacket` 发现包只能走 SSH 时，最多每 5 秒请求一次（`gst ping` 的包和 `-v` 也会）。尝试不阻塞包。失败或路径因对方沉默而结束后等 5 秒再试，连续失败时加倍，最长 5 分钟，成功后重置。一方主动关闭的路径（空闲、对方被移除、新 offer 顶替）不算失败，不等待。
- **信令**：发起方经路由层调用对方的 `direct.offer`。`DirectOffer` 是 `{"token": …, "addresses": ["IP:port", …]}`，调用方是谁由消息的 `from` 得知，响应是对方的 `addresses`。被移除的机器发来的 offer、或本机已被移除时收到的 offer 一律拒绝。候选地址只出现在这次调用中，不进表，不进日志。
- **同时发起**：两个节点同时发起时，id 较小一方的 offer 继续，id 较大的一方放弃自己的。收到 offer 时本节点还有到对方的路径，说明它已失效，先关闭。
- **探测**：双方每 200 ms 向对方所有地址发 probe，收到后回 answer。收到 probe 时，把来源地址也加入探测目标：NAT 后的节点不知道自己的公网地址，但只要一方能到达另一方，另一方就从来包学到地址。双方都有状态防火墙时，同时发包也让两边放行对方。发送方 id 让 daemon 认出自己的 probe：两台机器可能有相同的地址（比如容器网桥）和相同的端口，probe 会回到自己。
- **公网地址**：answer 带着对方看到的来源。NAT 对一个 socket 保持同一个公网端口时，这个地址对所有节点都有效，所以写进之后给别的节点的 offer，两台都在 NAT 后的机器也有机会打通。不需要 STUN 服务器。
- **选路**：两端各自选，不用协商，来回可以走不同的路。第一个回 answer 的地址证明路径可用，和 answer 到达的本机地址一起开始承载包。成绩记在 probe 的目标地址上，不看 answer 从哪个地址来，对方的 NAT 可能改了它。之后持续比较来回时间（平滑后）：新地址至少测过 3 次，比当前的低出一个余量才切换，余量是当前地址来回时间的一成，至少 2 ms。私网地址（`netip.Addr.IsPrivate`：RFC 1918 和 `fc00::/7`）比较时按来回时间减去自己余量的两倍算：包留在局域网或两台机器共同的 VPN 里，更稳，也不占公网带宽。所以私网地址只要慢得不超过约一个余量就顶替公网地址，公网地址要快出约三个余量才能换回来。路径刚建好后的 1 秒，以及之后每 30 秒，向所有地址探测 1 秒。切换不换 socket，WireGuard 不用重新握手。整次尝试限时 15 秒。
- **认证**：token 只用来找路径，不用于认证。身份和加密都是 WireGuard 的：对方拿不出记录里那把公钥对应的私钥，握手就不成立。
- **维持**：每秒向在用的地址发 probe，同时让 NAT 和防火墙的表项不过期。在用的地址 2 秒没有 answer，就重新探测所有地址，有回应的地址立刻顶替，所以一端换了网络时路径不用重建。3 秒什么都没收到（answer 和 WireGuard 的包都算），路径结束，包回到 SSH，WireGuard 的会话不受影响，所以程序的连接不断。只有一个方向断了时，听不到对方的一端先结束，另一端随后也收不到包，约 6 秒后两端都回到 SSH。UDP 发送出错的包当场改走 SSH。关闭时发 bye，对方立即知道。
- **空闲**：本端 45 秒没在路径上发过包（`Path.Idle`，probe 不算），就关闭路径，之后不再有 probe 和全量探测。只看发送就能代表双向的流量：WireGuard 收到数据后 10 秒内一定回包，没有数据可回时发 keepalive；发送的包都是本机 WireGuard 产生的，不会被伪造的来包续命。下一个包经 SSH 发出，同时触发新的尝试，不等待；这时 NAT 表项可能已过期，所以总是重新打洞，而不是先试旧地址。没有路由到对方时不因空闲关闭：这时路径是唯一的路，靠探测一直维持，直到对方沉默。
- **移除**：对方被移除、或本机被移除时，路径和进行中的尝试立即关闭。路径不因 SSH link 断开而关闭，离线只影响信令。

## 虚拟网络

虚拟网络（`internal/vnet`）让机器在一个私有网段里互相用 IP 通信，是 gst 唯一的数据通路。node 的身份、谁在网络里、UDP 路径和信令都来自 mesh，所以没有协调服务器。

```
程序 ── 网卡（utun / gst0）── wireguard-go ── bind ── Node.SendPacket ─┬─ UDP 路径
                                                                       └─ 路由层的包消息
```

- **WireGuard**：daemon 里跑 wireguard-go，加密和重放保护都用它的。mesh 对它来说是一张网（`conn.Bind`）：endpoint 是 node id，不是地址，所以路径怎么变都不需要 WireGuard 知道，切换时不用重新握手，也不用 PersistentKeepalive（打洞由 probe 维持）。私钥只在 `net.json`（control socket 旁边，`0600`），公钥在 `node` 条目的 `net` 里，信任来自 SSH。WireGuard 的 peer 是所有带 `net`、地址在当前网段里的 node，包括离线的：它们一回来就能通。
- **路径**：`Node.SendPacket` 有 UDP 路径就直接发，否则作为路由层 kind 3 的消息逐跳转发，进每条 link 的普通队列。包在 mesh 上不需要保护：途中只有密文，丢了就是丢了。队列满时丢包，和拥塞的网络一样。
- **MTU**：网卡 MTU 是 1168，加上 WireGuard 的 32 字节是 1200 字节的 UDP 载荷，几乎所有路径都能承载，所以不需要路径 MTU 发现。
- **网段**：表的 `subnet` 条目，合并规则见[状态层](#subnet网络的-subnet)：人选的胜过自动选的，自动选的之间最早的留下，所以一台新机器在还不知道别人时选的网段，不会把已有的网络搬走。`gst subnet` 经 `mesh.subnet` 写入人选的条目。表里还没有 `subnet` 条目时，第一个进入虚拟网络的节点用 `net.prefixes` 问所有能到达的节点各自的网卡和路由，选一个 `/16`（优先 `10.64.0.0` 到 `10.255.0.0`，起点由 node id 决定）：先找和这些网段都不重叠的，因为宽路由后面也可能有机器；找不到时只避开冲突的。冲突指网卡的网段和它重叠，或者路由落在它里面（前缀不短于它）。更宽的路由输给网段自己更窄的那条，不占它的地址：VPN 常把所有地址分成几半来路由，有的还把所有私网路由给本地路由器，这时没有一个网段完全不重叠。网段和本机其他网络冲突时不阻止运行，只报告。
- **gst ping**：每个 daemon 在自己的网络地址上监听 UDP 端口 15354，把收到的包原样发回。发送方是普通的 UDP socket，所以包经过路由、网卡、WireGuard 和实际路径，和程序的包完全一样：有回复说明这些都通，时间就是程序感受到的延迟。标签来自回复时的路径。`-s` 指定包的大小，超过网卡 MTU 时分片。
- **本机地址**：Linux 自己投递发往本机地址的包。macOS 把 utun 的地址路由进 utun，这些包到了 WireGuard，而 WireGuard 没有这个地址的 peer，所以 `vnet` 在 WireGuard 之前把它们写回网卡（`loopback`）。
- **地址**：每台机器在自己的 `node` 条目里写 `net.address`，由 node id 的哈希决定，必须在当前网段里；和别的 node 冲突时 id 较小的保留，较大的重选。离线的 node 仍然占着它的地址；地址不在当前网段里的 node（网段换了，它还没回来）暂时不解析、不进 WireGuard，回来后自己换地址。地址和私钥存在 `net.json`，重启后不变。私钥和地址属于一个身份，`net.json` 记下这个 node id；id 变了就换新私钥、重选地址，否则别的机器手里旧身份的条目会和新身份共用一把公钥。
- **名字**：daemon 在 `127.0.0.1:15353` 回答 `.gst` 的 A 记录（`mesh.FindNode`，所以 id 前缀也能用）：网络里的 node（包括离线的）返回 A 记录，TTL 10 秒；不在网络里时 `NXDOMAIN`，名字存在但没有该类型的记录（如 AAAA）时空应答，两者都附 SOA，否定缓存 5 秒；虚拟网络还没起来时 `SERVFAIL`。DNS 的 socket 在 daemon 运行期间一直打开，不随虚拟网络开关。macOS 用 `/etc/resolver/gst`，有 systemd-resolved 的 Linux 用 drop-in（`Domains=~gst`），其他 Linux 写 `/etc/hosts` 中 `# gst begin` 到 `# gst end` 之间的部分。
- **helper**（`internal/tun`）：daemon 以用户身份运行，创建网卡、地址、路由和 DNS 配置需要 root，由系统服务（`gst net-helper --user NAME`）完成。`gst up` 用 `gst helper-version` 询问 root 那份 gst 的 `tun.HelperVersion`，这一步不需要 root；helper 缺失、版本不同或没有在监听时，它显示要执行的 `sudo <gst 的绝对路径> install --user NAME` 和这条命令的改动，询问后在当前终端执行，密码由 sudo 询问。版本号只在 helper 的代码或 daemon 对它的要求变化时提高，所以普通升级不需要 root。`gst down` 经同一个 socket 发 `Spec{Remove: true}`，不需要 root：这条消息没有参数，只能让 helper 删除固定路径上的服务和那份 gst。helper 先删文件，回复，最后让服务管理器停止自己，因为这一步会结束进程。helper 没有应答时，`gst down` 才说明并询问后用 `sudo <gst> uninstall`。它运行 root 拥有的一份 gst（Linux `/usr/local/libexec/gst-net`，macOS `/Library/PrivilegedHelperTools/local.gst.net`），绝不运行用户可写的文件。daemon 经 `/var/run/gst-net.sock`（属于该用户，`0600`）发 `Spec`（子网、地址、hosts），helper 校验后执行，把打开的设备用 `SCM_RIGHTS` 交给 daemon；包都在 daemon 里处理，helper 不碰。Spec 只能配置私有网段里的一个地址和 `.gst` 下的名字。网卡的生命周期是这条连接：daemon 退出，helper 撤销一切。新 daemon 连上时取代旧的。daemon 每轮重发 Spec，内容相同时什么也不做，兼作存活检查。
- **管理循环**：`vnet.Manager` 在拓扑变化、配置变化和每 5 秒时比较一遍：算网段和地址、发布自己的 `net`、调 helper、同步 WireGuard 的 peer。`gst subnet` 写入条目后等 `Status` 反映新网段再返回。没有 helper 或网段重叠是 `NetState` 里的 `error` 和 `hint`，不是失败退出，装好 helper 后下一轮就启动。
- **没有 IPv6**：网段只有 IPv4，AAAA 返回空。


## iOS

iPhone 和 iPad 没有命令行、ssh 和后台服务，系统也不让 VPN 进程自己的 socket 进入它的网卡。gst 在那里是一个 app 加一个 packet tunnel 扩展（`ios/`），Go 代码是 `internal/mobile`，编译成 C 库（`gstkit`，`gstkit.h` 的五个函数，Android 也用它，见下一节）静态链接进这两个进程。

- **两个进程**：app 相当于其他机器上的 gst 命令，扩展相当于 daemon，两者共用 app group 里的一个目录。app 进程里的 gst 应答 `app.…` 方法：改配置文件（同一把锁、同样的原子写入）、生成密钥、检查服务器。扩展里 `daemon.Run` 运行同一个 daemon，不监听 socket，`Daemon.Call` 在进程内应答 control 方法。app 把请求（`{"method", "params"}`，与 control 连接上的请求相同）作为 provider message 发给扩展，取回 `{"result"}` 或 `{"error"}`，所以它和 CLI 用的是同一套方法：`daemon.status`、`mesh.topology`、`net.ping`、`mesh.remove`。app 改完配置后调 `daemon.reload`。
- **目录**：`config.json`、`state.json`、`net.json`、`id_ed25519`（app 生成的无口令私钥）、`known_hosts`、`gst.log`（1 MiB 轮转，Go 运行时崩溃时的输出也在其中）。目录不进备份：恢复到另一台手机上，会有两台 id 相同的 node。
- **app 的方法**：

  | 方法 | 参数 | 结果 |
  | --- | --- | --- |
  | `app.info` | 无 | `Info`：`node`、`protocol`、`public_key`（给服务器 `authorized_keys` 的一行）、`links`（`destination`、`target`） |
  | `app.name` | `{"name"}` | 改名后的 `Info` |
  | `app.check` | `{"destination"}` | `Check`：登录一次的结果，`state` 为 `ready`、`unknown_host`、`changed_host`（两者带 `host_key`：类型、`SHA256:` 指纹、密钥本身）、`unauthorized` 或 `failed`（带 `error`） |
  | `app.trust` | `{"destination", "key"}` | 无：把人核对过的主机密钥记入 `known_hosts`，替换该地址原有的记录 |
  | `app.add` | `{"destination"}` | `Info`：同 `gst add`，写入带 `admit` 的 link；主机密钥还没记录时拒绝 |
  | `app.unlink` | `{"destination"}` | `Info`：同 `gst unlink` |
  | `app.new_key` | 无 | `Info`：换一把新私钥（`id_ed25519`），旧的不再使用；已认可旧公钥的服务器要加上新的，link 下次连接时用新私钥登录 |
  | `app.log` | 无 | `{"text"}`：日志的最后 64 KiB |

- **link**：destination 只能是 `user@host` 或 `ssh://user@host:port`（`sshconfig.Literal`），必须写出用户，没有 ssh 参数；私钥和 known_hosts 就是目录里的那两个文件。主机密钥由人在 app 里核对指纹后信任：`sshconn.Try` 登录一次，区分未知的主机密钥、变了的主机密钥和未授权的私钥；对未知的服务器优先要 ED25519 密钥，和 OpenSSH 给人看的一致。没有 node 能 link 到手机：它没有 sshd，也没有 control socket。
- **网卡**：系统创建并配置 utun，gst 只说怎么配。`vnet` 打开和更新网卡变成对扩展的一次回调（`mobile.System.Apply`），参数是 `Settings`：本机地址、路由（subnet 和 DNS 地址）、DNS 地址和它负责的域 `gst`、MTU。扩展据此调用 `setTunnelNetworkSettings`，完成后返回 -1（不交出描述符）；之后 `tun.Inherited` 在进程已打开的描述符中找到 utun，复制一份交给 wireguard-go。只有 subnet 和 `.gst` 经过 VPN，其他流量和名字解析不变。
- **在网卡的包上应答**（`vnet` 的 `inside`）：系统只向一个地址的 53 端口查询，daemon 又不能在自己的地址上收发，所以这些都直接在包上做。DNS 地址是 `198.18.0.53`：`198.18.0.0/15` 留给网络设备测试（RFC 2544），不会出现在手机遇到的任何网络里，也不在 gst 只用私有地址的 subnet 里。系统发给它的 UDP 查询在读网卡时被截下，应答直接写回网卡。`gst ping` 也一样：WireGuard 解密后发往本机 15354 端口的包被截下并原样送回 WireGuard，本机发出的 ping 是直接交给 WireGuard 的 UDP 包。不处理分片：从手机发出的 ping 最大为 MTU − 28 字节，别的机器发给手机的超过 MTU 的 ping 没有应答。
- **内存**：iOS 给 VPN 进程 50 MiB。`bind` 的入站队列每个包只占 2048 字节（所有平台都是），Go 的内存软上限设为 32 MiB。
- **开关**：在 app 里开启，就是保存带 on-demand 规则的 VPN 配置并启动它，系统在重启和进程结束后会再次启动；关闭则先关掉 on-demand 再停止。配置里不写 `down`。

## Android

Android 同样没有命令行、ssh 和后台服务。gst 在那里是一个 app（`android/`，Kotlin 和 Jetpack Compose），daemon 运行在它的 `VpnService` 里。Go 代码与 iOS 相同：`internal/mobile` 经 `gstkit` 编译成共享库 `libgstkit.so`，`gstkit/jni.c`（只在 `android` 或 `jni` 构建标签下编译）把 `gstkit.h` 的函数变成 Java 类 `gst.app.Core` 的 native 方法，文本以 UTF-8 字节数组传递。

- **一个进程**：app 和 `VpnService` 在同一个进程里，共用一个 `mobile.App`。`app.…` 方法和 daemon 的 control 方法都直接经 `gst_call` 调用，不需要 iOS 的 provider message。目录是 app 的 `noBackupFilesDir/gst`，不进备份；内容和方法与 iOS 相同。
- **网卡**：`VpnService.Builder` 按 `Settings` 配置地址、路由、DNS 和 MTU，`establish()` 交出网卡的描述符，`Apply` 把它返回给 gst，gst 用 `tun.Adopt` 接管。每次设置变化（地址或网段）都会建一张新网卡并结束旧的，所以 `mobile` 给 wireguard-go 的是一个会切换的设备（`switching`）：读写总在最新的那张网卡上，旧的被关掉时正在进行的读换到新的上继续。gst 自己的 app 不经过 VPN（`addDisallowedApplication`），link 和 UDP 直连的 socket 直接走下面的网络，这和 iOS 一样需要 `inside` 在包上应答 DNS 和 ping。
- **DNS**：Android 没有按域名分流的 DNS，VPN 的 DNS 服务器收到所有查询。`.gst` 之外的查询由 `vnet.Config.Forward`（`mobile.System.Forward`，C 里是 `gst_forward_fn`）交给系统自己的解析器：app 用 `DnsResolver.rawQuery` 在 VPN 下面的网络上查询，把原样的应答写回网卡。同时等待的转发最多 64 个，多出的丢弃，由查询方重试。所以 Android 要求 10 及以上。
- **地址**：Android 不让 app 列出网卡（`net.Interfaces` 失败），UDP 直连只提供发包使用的那个地址。
- **开关**：开启时系统先问一次是否允许 gst 建立 VPN（`VpnService.prepare`）。app 记住开着的状态，手机重启或 app 更新后（`BootReceiver`）重新开启；也支持系统设置里的“始终开启的 VPN”。另一个 VPN 接管或用户在系统里关掉时（`onRevoke`），gst 停止并记为关闭。

## 端口暴露

- `gst expose <target> [--port port] [--udp]` 写本机配置中的 `exposes` 数组，每项是 `{protocol, port, target}`，以 `(protocol, port)` 唯一标识。target 为 `tcp://host:port`、`udp://host:port` 或 `unix:///absolute/path`，Unix stream 在网络侧使用 TCP。CLI 复用配置锁、原子写入、`daemon.reload` 和状态查询，不增加 peer 方法。
- `vnet.Manager` 在接口配置成功后把当前地址和规则交给 `internal/expose.Manager`，网络未就绪时传空地址；down 先关闭 expose。地址变更、规则替换或移除都会关闭旧监听和现存连接；相同规则重载不影响连接。监听失败只影响对应规则，每轮重试，不阻止虚拟网络运行。
- expose 只使用明确的 gst IPv4 地址创建 `tcp4` / `udp4` socket，绝不回退到通配地址。系统 TCP/IP 栈和现有 WireGuard 路径承载入口流量，不增加 mesh 数据路径。出口由用户 daemon 的 `net.Dialer` 连接，使用 5 秒连接超时；TCP/Unix stream 复用 `relay.Pipe`，保留半关闭。
- UDP 每个来源 IP:port 有一个独立 connected UDP socket，回包通过原监听 socket 返回。每规则最多 256 个映射，每映射有 16 个报文的有界队列，溢出丢弃；独立 goroutine 拨号，不阻塞其他客户端。双向流量刷新 2 分钟空闲超时。规则停止时取消拨号、关闭 socket，并等待 goroutine 退出。
- `Status.exposes` 新增 `{protocol, port, target, address?, state, error?}`；state 为 `waiting`、`running` 或 `failed`。CLI 在 daemon 不报告规则时显示 `stopped`，停机也能读配置列出规则。`running` 仅证明绑定成功，不探测目标。状态可交互查看，但目标、客户端地址和原始网络错误不写 daemon 日志。新增可选字段不改变帧或现有字段语义。

## Web UI

- `gst ui` 是前台进程，只监听本机 gst IP（`--port`，默认任意空闲端口），网络里的每台机器（包括本机，经 `loopback`）都用 `http://<name>.gst:<port>/` 打开；虚拟网络没起来时报错。启动时生成随机 token，打印 `http://<name>.gst:<port>/#<token>`，不打开浏览器。daemon 只有 control socket 这一个控制入口，退出后不留监听。
- 页面（`internal/cli/ui`，go:embed）以 `Authorization: Bearer <token>` 请求 `GET /api/state`；`Host` 不是 `<name>.gst` 或本机 gst IP 时返回 403（`.gst` 只有 gst 解析，挡住 DNS rebinding）；响应带严格的 CSP 等安全头。页面用 DOM 文本节点渲染所有字段。
- `/api/state` 返回本机 node、`Status`、daemon 是否应答、`Topology`；只供页面使用，字段可以改变。页面只读：不能启动、停止或重启 link；daemon 不运行时，页面说明这一点。机器表显示 `direct`、`relay`、`relay via …` 或 `offline, seen …`。
- `GET /api/nodes/{node}`（同样需要 token）返回 mesh 中任一机器的 `Status`，即 `mesh.status` 的结果，`{node}` 是机器的称呼（名称、id 或 id 前缀）；出错时返回 502 和错误信息。页面轮询所有可达机器，先问展开的那几台。
