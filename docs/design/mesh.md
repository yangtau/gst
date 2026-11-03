# gst mesh 设计

状态：第 1 步（单进程 daemon + 控制协议）已实现；第 2 步起为设计。

## 目标

每台运行 gst 的机器都能看到由 gst 连接组成的全局拓扑，并能借助中间节点建立连接。例如：

```
m3 ──ssh──► mini
m3 ──ssh──► devcn
```

mini 上的 gst 能看到 m3 和 devcn，并能建立 `mini → devcn` 的连接，由 m3 中转。mini 不需要 devcn 的凭据：凭据只在 m3 上，m3 代为连接。

## 分步

1. **单进程 daemon**：每台机器只有一个 gst 后台服务，持有全部 tunnel，暴露一个 unix control socket；CLI 通过 [gst 协议](#gst-协议) 与它通信。（已实现）
2. **peer link**：tunnel 打开 `--mesh` 后，客户端一侧的 daemon 通过已有 SSH 连接在服务器上执行 `gst peer-bridge`，接通服务器 daemon 的 control socket，两侧以 `peer` 角色握手。`gst peers` 列出直连节点。
3. **拓扑扩散**：节点交换带版本号的 link-state 记录，`gst nodes` 输出全局视图（文本与 `--json`）。
4. **中转转发**：`gst forward add <node> --via mesh -L ...` 与 `gst connect <node> <port>`（可作 `ProxyCommand`）。
5. **Web UI**：daemon 提供 HTTP API，`gst ui` 打开本地页面；先只读，再支持配置本节点。

## 已确定的决策

- **信任**：control socket 权限为 `0600`，能连上的只有本机同一用户；peer link 由 SSH 登录建立，登录的就是该用户，同样不另做认证。每条 tunnel 通过 `--mesh` 显式加入 mesh：只有加入的 tunnel 才建立 peer link、被宣告到拓扑中、允许被中转。
- **远程配置**：第一版只能配置本节点；其他节点的信息只读。
- **隐私**：拓扑信息在节点之间传播、在 Web UI 和交互式命令中显示，属于交互式输出；后台日志仍不记录地址、用户和密钥路径。

## 节点身份

节点以 SSH host key 指纹标识，而不是名字：同一台机器在不同节点的 ssh config 里可能有不同别名，host key 却是全局唯一的。

- 运行 gst 的节点读取本机 `/etc/ssh/ssh_host_*_key.pub`，在 `node.info` 中报告全部指纹。于是“作为 gst 节点的 mini”与“m3 看到的 mini sshd”合并为同一节点。
- 没有运行 gst 的 SSH 服务器（如 devcn）是叶子节点，只经由连向它的边出现；它不需要安装 gst 即可被中转。
- 显示名取自边上的 ssh 别名；冲突时由用户在本地设置别名。

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
- `peer` 连接在握手后切换为多路复用（计划使用 yamux），每个子流以一个请求开始。
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

`peer`（计划）：

| 方法 | 参数 | 结果 / 后续 |
| --- | --- | --- |
| `node.info` | 无 | 节点 id、名字、host key 指纹、software、features |
| `topology.sync` | 发送方已知的 link-state 记录 | 对方较新的记录；之后以事件帧持续推送更新 |
| `relay.open` | `{"path": [节点...], "target": {"network": "tcp", "address": "127.0.0.1:5432"}}` | 成功后该流变为到目标的原始字节流；路径上每一跳把剩余路径转发给下一跳 |

### link-state 记录（计划）

```json
{
  "node": "SHA256:…",
  "seq": 42,
  "expires": "2026-11-03T12:00:00Z",
  "name": "m3",
  "edges": [
    {"to": "SHA256:…", "alias": "devcn", "state": "running", "relay": true}
  ]
}
```

- 每个节点只发布自己的记录，`seq` 单调递增；收到更大 `seq` 的记录就替换并转发给其他 peer（洪泛）。
- 记录在 `expires` 后失效，节点定期刷新，因此掉线节点会自然从视图中消失。
- 路由由发起方计算（源路由），沿途节点只按 `relay.open` 的 `path` 转发，不运行路由协议。
