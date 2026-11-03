# gst mesh 设计

状态：第 1–5 步已实现；Web UI 目前能启动、停止、重启本节点的 tunnel，其他配置仍只能用命令行。

## 目标

每台运行 gst 的机器都能看到由 gst 连接组成的全局拓扑，并能借助中间节点建立连接。例如：

```
m3 ──ssh──► mini
m3 ──ssh──► devcn
```

mini 上的 gst 能看到 m3 和 devcn，并能建立 `mini → devcn` 的连接，由 m3 中转。mini 不需要 devcn 的凭据：凭据只在 m3 上，m3 代为连接。

## 分步

1. **单进程 daemon**：每台机器只有一个 gst 后台服务，持有全部 tunnel，暴露一个 unix control socket；CLI 通过 [gst 协议](#gst-协议) 与它通信。（已实现）
2. **peer link**：tunnel 打开 `--mesh` 后，客户端一侧的 daemon 通过已有 SSH 连接在服务器上执行 `gst peer-bridge`，接通服务器 daemon 的 control socket，两侧以 `peer` 角色握手。`gst mesh peers` 列出直连节点。（已实现）
3. **拓扑扩散**：节点交换带版本号的 link-state 记录，`gst mesh nodes` 输出全局视图（文本与 `--json`）。（已实现）
4. **中转转发**：`gst add <name> <node> --via mesh -L ... -D ...` 与 `gst connect <node> <port>`（可作 `ProxyCommand`）。（已实现）
5. **Web UI**：`gst ui` 提供本地页面，显示本节点和全局拓扑，并能启动、停止、重启本节点的 tunnel。（已实现）

## 已确定的决策

- **信任**：control socket 权限为 `0600`，能连上的只有本机同一用户；peer link 由 SSH 登录建立，登录的就是该用户，同样不另做认证。每条 tunnel 通过 `--mesh` 显式加入 mesh：只有加入的 tunnel 才建立 peer link、被宣告到拓扑中、允许被中转。
- **远程配置**：第一版只能配置本节点；其他节点的信息只读。
- **隐私**：拓扑信息在节点之间传播、在 Web UI 和交互式命令中显示，属于交互式输出；后台日志仍不记录地址、用户和密钥路径。

## 节点身份

运行 gst 的节点以随机 id 标识，而不是名字：`gst mesh enable` 生成 128 位随机 id，保存在配置文件的 `mesh` 中；名字（`--name`，默认取主机名）只用于显示，可以重复。没有用 host key 指纹做 id，是因为 macOS 等机器未必有 sshd host key。

- 节点在 `node.info` 和自己的记录中附带本机 `/etc/ssh/ssh_host_*_key.pub` 的 SHA-256 指纹；tunnel 的边记录连接时验证过的服务器 host key 指纹。于是“作为 gst 节点的 mini”与“m3 看到的 mini sshd”合并为同一节点。
- 边指向的机器按以下顺序确定：peer link 报告的节点 id；host key 指纹属于某个 gst 节点时为该节点；否则为没有 gst 的 SSH 服务器，id 为 `ssh:<指纹>`；从未连上过（还没有指纹）时为 `tunnel:<节点 id>/<tunnel 名>`。
- 没有运行 gst 的 SSH 服务器（如 devcn）是叶子节点，只经由连向它的边出现；它不需要安装 gst 即可被中转。
- 叶子节点的显示名取自连向它的 tunnel 名（而不是 destination，后者可能是 `user@地址`）。gst 节点重名时，`gst mesh nodes` 在名字后附上 id 前缀。

## 单进程 daemon（第 1 步）

- 每个用户一个服务：Linux 上是 systemd 用户服务 `gst.service`，macOS 上是 launchd agent `local.gst`。命令行为 `gst [--config path] daemon`。
- 文件位置：

  | | Linux | macOS |
  | --- | --- | --- |
  | 状态目录 | `$XDG_STATE_HOME/gst`（默认 `~/.local/state/gst`） | `~/Library/Application Support/gst` |
  | control socket | `<状态目录>/gst.sock` | 同左 |
  | tunnel 日志 | `<状态目录>/logs/<name>.log` | `~/Library/Logs/gst/<name>.log` |
  | daemon 日志 | journald（`journalctl --user -u gst`） | `~/Library/Logs/gst-daemon.log` |

- daemon 通过 `<socket>.lock` 上的 flock 保证单实例。
- 配置文件仍是唯一事实来源。tunnel 新增 `stopped` 字段记录用户意图；daemon 运行所有未停止的 tunnel。
  - `daemon.reload`：按配置启动新的、停止已停止或已删除的 tunnel，**不重启**配置改变了的 tunnel（保留 `--no-restart` 的语义）。
  - `tunnel.restart`：用最新配置重启指定 tunnel。
- 每条 tunnel 在 daemon 内独立重连：失败后 5 秒重试，不设上限。状态为 `connecting`、`running`（已就绪）、`down`（失败，等待重试）；配置中已停止的为 `stopped`。
- 没有任何已启动的 tunnel 时，CLI 删除 daemon 服务，与以前“全部停止后不留服务”一致。
- CLI 每次修改运行状态时检查 daemon 的 build（可执行文件的 SHA-256）；与自身不同，就重启 daemon 服务，使升级后的 gst 生效。
- **迁移**：配置文件新增顶层 `version`（当前为 2）。CLI 第一次读到旧配置时：有旧版服务（`gst-<name>.service` / `local.gst.<name>`）的 tunnel 视为已启动，其余标记为 `stopped`；删除旧服务，再启动 daemon。

## gst 协议

CLI 与 daemon、daemon 与 daemon 使用同一套协议，实现位于 `internal/wire`。设计目标：帧格式固定，扩展只靠新增方法、新增字段和 feature，不改帧格式。

### 帧

```
+----------------+---------------------------+
| length: uint32 | payload: JSON (length 字节) |
|  big-endian    |                           |
+----------------+---------------------------+
```

- `length` 不超过 1 MiB；超出即为协议错误，连接关闭。
- 接收方忽略未知 JSON 字段，因此新增字段向后兼容。删除字段或改变字段含义必须提升协议版本。

### 握手

每条连接上，发起方先发 `Hello`，接收方回 `Hello`。

```json
{"protocol": 1, "software": "gst 0.2.0", "role": "control", "build": "sha256:…", "features": []}
```

| 字段 | 含义 |
| --- | --- |
| `protocol` | 发送方支持的最高协议版本；双方使用两者的较小值 |
| `software` | 人类可读的软件版本 |
| `role` | 连接用途：`control`（CLI → 本机 daemon）或 `peer`（daemon ↔ daemon） |
| `build` | 可执行文件标识，CLI 用它判断 daemon 是否需要重启 |
| `features` | 可选能力列表，用于在同一协议版本内协商扩展，如 `relay` |
| `error` | 仅出现在回复中：拒绝连接的原因（如不支持的角色或协议版本），随后连接关闭 |

### 请求与响应

握手之后，一个**流**承载一次调用：

```
请求：  {"method": "tunnel.restart", "params": {"names": ["myhost"]}}
响应：  {"result": {...}}
     或 {"error": {"code": "not_found", "message": "tunnel myhost is not started"}}
```

- `control` 连接本身就是一个流，可以顺序发多次请求。
- `peer` 连接的 Hello 带 feature `yamux`，握手后切换为 yamux 会话，双方都可以开子流，每个子流以一个请求开始。连接建立后双方各调用一次 `node.info` 认识对方；连向自己的 link 由发起方报错并关闭。
- **流式方法**在成功响应之后接管该流，内容由方法定义：`relay.open` 之后是原始字节，事件订阅类方法之后是连续的帧。
- 未知方法返回 `unsupported_method`，调用方据此降级。

错误码：

| code | 含义 |
| --- | --- |
| `unsupported_method` | 对方不认识该方法 |
| `unsupported_role` | 握手时：对方不接受该角色 |
| `unsupported_protocol` | 握手时：对方不支持该协议版本 |
| `invalid_params` | 参数无法解析或不合法 |
| `not_found` | 引用的 tunnel 或节点不存在 |
| `failed` | 方法执行失败 |

### 方法

`control`（协议 1，已实现）：

| 方法 | 参数 | 结果 |
| --- | --- | --- |
| `daemon.status` | 无 | `Status`：daemon 版本、build、pid、配置路径、各 tunnel 状态 |
| `daemon.reload` | 无 | 重新读取配置并同步后的 `Status` |
| `tunnel.restart` | `{"names": [...]}` | 重启后的 `Status` |

`control` 还有：

- `mesh.peers`（已实现）：返回本节点信息和全部 peer link（方向、所经 tunnel、状态、对端、失败原因）。
- `mesh.connect`（已实现）：参数 `{"node": 名字/id/id 前缀, "target": {"network", "address"}}`；成功响应后，该 control 连接变为到目标的原始字节流。`gst connect` 使用它。
- `mesh.nodes`（已实现）：返回 `Topology`，即本节点由全部有效记录算出的全局视图：机器（gst 节点与 SSH 服务器）、是否可达，以及它们之间的 tunnel。

`peer`：

| 方法 | 参数 | 结果 / 后续 |
| --- | --- | --- |
| `node.info` | 无 | 节点 id、名字、software、host key 指纹（已实现） |
| `topology.sync` | `{"known": {节点 id: seq}}`：调用方已有记录的 seq | `{"records": [...]}`：被调方比调用方新的记录；之后该流持续推送 `{"records": [...]}` 帧，每次变化一帧（已实现） |
| `relay.open` | `{"path": [节点...], "target": {"network": "tcp", "address": "127.0.0.1:5432"}}` | 成功后该流变为到目标的原始字节流；路径上每一跳把剩余路径转发给下一跳（已实现） |

### link-state 记录（已实现）

```json
{
  "node": {"id": "…32 位十六进制…", "name": "m3", "software": "gst 0.2.0", "host_keys": ["SHA256:…"]},
  "seq": 1793700000000000000,
  "expires": "2026-11-03T12:00:00Z",
  "edges": [
    {"tunnel": "devcn", "destination": "devcn", "state": "running", "host_key": "SHA256:…", "peer": "…", "linked": true}
  ]
}
```

- 每个节点只发布自己的记录，边只包括打开了 `--mesh` 的 tunnel。`seq` 取 `max(上一次 seq + 1, 当前 Unix 纳秒)`，因此 daemon 重启后仍然递增；若从 peer 收到自己旧进程发布的更大 `seq`，节点立即以更大的 `seq` 重新发布。
- tunnel 状态、host key 或 peer link 变化时重新发布；收到更大 `seq` 的记录就替换，并通过各条 `topology.sync` 流推送给其他 peer（洪泛）。
- 每条 peer link 建立后，双方各向对方调用一次 `topology.sync`，所以每个方向都有一条推送流。
- 记录有效期 30 分钟，节点每 10 分钟刷新一次，因此掉线节点会在 30 分钟内从视图中消失。过期记录不再转发。
- 可达性由查看方计算：从本节点出发，经运行中的 tunnel 到达的机器可达；要越过某台机器继续前进，需要该段 tunnel 的 peer link 处于 up（反方向也只能走 up 的 peer link），因为中转需要两端都有 gst。
- 每个节点最多保留 1024 条记录，每条记录最多 1024 条边。
- 路由由发起方计算（源路由），沿途节点只按 `relay.open` 的 `path` 转发，不运行路由协议。

## 中转转发（第 4 步）

- **路径**由发起方根据自己的 `Topology` 计算（`mesh.Route`），取最短路径：从本节点出发，只能经由 up 的 peer link（任一方向）越过一台机器；最后一段还可以是运行中但没有 peer link 的 `--mesh` tunnel，例如到没有 gst 的服务器。路径最多 16 个节点。
- `relay.open` 的 `path` 以被调方开头。每个节点检查 `path[0]` 是自己，然后：
  - 只剩自己：直接连接 `target`（地址相对本机）；
  - 与下一个节点有 up 的 peer link：在该会话上开新流，把 `path[1:]` 交给它；
  - 下一个节点是路径终点，且本节点有到它的运行中 tunnel：经该 tunnel 的 SSH 连接打开 `direct-tcpip` / `direct-streamlocal`，地址由那台服务器解析。
- 失败时返回带原因的错误，沿路径原样传回发起方；错误属于交互式输出，可以包含地址。后台 tunnel 日志只记 “target connection failed for forward N”。
- `--via mesh` 的 tunnel 保存为 `"via": "mesh"`，`destination` 是 mesh 中的机器（名字、id 或 id 前缀）。daemon 不建立 SSH 连接，只在本机监听 `-L`/`-D`，每个连接调用 `mesh.Dial` 按当时的拓扑选路；因此路径变化或暂时不可达不影响 tunnel 本身的状态。
- **信任**：与 peer link 相同，mesh 中的节点彼此完全信任：任何节点都可以让其他节点代为连接其所在机器和其 tunnel 能到达的地址。

## Web UI（第 5 步）

- HTTP 服务不在 daemon 里，而在前台的 `gst ui` 进程里：daemon 只有 `0600` 的 control socket 这一个入口，信任边界保持不变；`gst ui` 作为普通 CLI 客户端读取配置并调用 daemon，退出后不留下任何监听。
- `gst ui` 只监听 `127.0.0.1`（`--port`，默认任意空闲端口），启动时生成 128 位随机 token，打印 `http://127.0.0.1:<port>/#<token>` 并打开浏览器（`--no-open` 跳过）。
- 页面（`internal/cli/ui`，go:embed）是静态的 HTML、CSS、JS，不需要 token。页面从地址的 fragment 取出 token，以 `Authorization: Bearer` 请求 `GET /api/state`；fragment 不随请求发送，也不进入 Referer。
  - 其他网站的页面无法带上这个 header（需要 CORS 预检，`gst ui` 从不批准），因此不能读取状态；`Host` 不是 localhost 或回环地址时一律返回 403，防止 DNS rebinding。
  - 响应带 `Content-Security-Policy: default-src 'self'`、`nosniff`、`no-referrer`、`no-store`。页面用 DOM 文本节点渲染所有字段，因为名字和 destination 来自其他节点。
- `/api/state` 每次请求都重新读取配置并调用 daemon：`tunnels`（与 `gst list --json` 相同的视图）、`daemon`（daemon 是否应答）、`node`、`peers`（`mesh.peers`）、`topology`（`mesh.nodes`）。它只供页面使用，字段可以改变；脚本应使用各命令的 `--json`。
- 页面每 3 秒轮询一次，标签页隐藏时暂停。拓扑图按与本节点的 BFS 距离（沿边的任一方向）分列，无法连通的节点放在最后一列。
- `POST /api/tunnels/{name}/{start|stop|restart}` 同样需要 token，执行与 `gst start|stop|restart <name>` 相同的代码（包括启动前检查和必要时拉起 daemon），一次只执行一个；返回命令行会打印的 `message`，失败时为 `error` 和 `hint`（HTTP 409）。跨站页面发不出带 `Authorization` 的请求，`gst ui` 也不响应 CORS 预检，因此不需要另外的 CSRF token。
- 以后可以按同样的方式加入其他本节点配置（forward、mesh 开关等）；其他节点仍然只读。
