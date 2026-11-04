# 集成测试

在仓库根目录运行 `make test-integration`，或在本目录运行：

```sh
go test -tags integration -count=1 -v .
```

自动探测已安装的运行时：macOS 优先 Apple `container`，其次 podman、docker；Linux 优先 podman，其次 docker。`CONTAINER_RUNTIME` 可以显式指定。

运行时服务和 sshd 测试镜像必须事先准备好。测试只读取已有镜像，不安装软件、不启动或停止宿主机服务、不构建镜像或操作共享构建器。镜像缺失时会退出并打印准备命令，供用户单独执行。所有测试都在容器之间进行，容器之间通过容器 IP 通信，不在本机发布端口。

容器启动命令的超时为 3 分钟，允许 Apple `container` 在首次启动时下载 init 镜像；其余运行时命令的超时为 30 秒。超时错误会明确显示等待时长。

## 用例

| 文件 | 用例 | 内容 |
| --- | --- | --- |
| `link_test.go` | `TestLinkLifecycle` | `gst link add`：状态变为 running，对端名称正确，peer-bridge 启动了 server 的 daemon，`gst nodes` 在 server 上显示 client；`link set` 重启；`stop` / `start`；杀掉 server 的 daemon 或断开 SSH 会话后先 down 再 running；`remove` |
| | `TestLinkRemoteGstMissing` | server 上没有 gst：状态 down 且错误提到 gst，`gst link check` 在 `remote gst` 一步失败并给出提示，`link set --remote-gst` 修复后 running |
| | `TestLinkStopIsSeenByServer` | link 关闭后 server 不再认为 client 可达（已知问题，被 `t.Skip` 标记为 BUG） |
| `mesh_test.go` | `TestMesh` | 三个节点 a → b ← c：拓扑与路径（`Topology`）；`gst connect` 的机器寻址（名称、`.gst`、link 名、id 前缀）和 TCP、Unix socket 目标（`Connect`）；`gst map`（`Maps`）；SOCKS5、HTTP CONNECT、普通 HTTP 请求和 `/proxy.pac`，以及改变代理地址（`Proxy`）；`gst route` 与 `gst exit`：c 不是 exit 时被拒绝，`gst exit on` 后允许，无路由时直连（`Exit`） |
| `migrate_test.go` | `TestMigrateVersion2Config` | 版本 2 的配置在第一条命令时转换：tunnel 变成 link，`-L` 变成 map，`-R` 被丢弃并在 stderr 给出说明，link 运行且 map 可用 |
| `daemon_test.go` | `TestDaemonRestartsAfterUpgrade` | 升级 gst 后下一条命令重启 daemon，没有升级则不重启 |
| | `TestDownAndUp` | `gst down` 停止 daemon，其他节点的 `gst peer-bridge` 被拒绝（"gst is down"），`gst up` 后 link 恢复 |
| `ui_test.go` | `TestUI` | `gst ui`：没有 token 返回 401，`/api/state` 返回 links 和 topology，`POST /api/links/<name>/{restart,stop,start}` 生效 |

`harness_test.go` 构建 gst 和 `gst-it` 并检查镜像，`supervisor_test.go` 是监督测试子进程并负责清理的父进程。

## 隔离与清理

- 每次运行在本目录创建独立的 `.run-*` 工作目录，权限为 `0700`。为 Linux 构建的 gst 和 `gst-it`、构建缓存、模块缓存和容器记录都存放在其中。
- 每个用例创建带本次运行唯一前缀的容器；只对这些容器执行命令和删除操作。已有容器、镜像、用户 SSH 配置和已安装的 tunnel 服务不参与测试。
- 本机上不运行 gst，也不监听或连接任何端口。每个用例启动若干节点容器（`node_test.go`）：每个节点都运行 sshd 和 gst，以 `tunnel` 用户身份运行，节点之间通过容器 IP 通信。`startPair` 给出一个 client 和一个 server，client 的 gst 用户用生成的身份和 known_hosts 登录 server（`gst link add` 带 `--identity` 与 `--known-hosts`）；`start3` 启动 a、b、c 三个节点，a 和 c 各自 link 到 b。测试把 gst 和网络辅助程序 `gst-it`（`ithelper/`，提供 echo 服务（TCP 或 Unix socket）、HTTP 服务、并发往返、SOCKS5 与 HTTP CONNECT 客户端和 HTTP 客户端）通过 stdin 复制进容器，不挂载本机目录。密钥在内存中生成，经 stdin 写入容器。
- server 不需要事先准备：client 的 link 通过 `gst peer-bridge` 登录后会在 server 上启动它的 daemon。镜像里没有 systemd，`sshd/systemctl` 这个 shim 代替 `systemctl --user`，只在容器内后台启动和停止 daemon，不模拟 `Restart=` 等单元设置。
- 等待状态一律轮询到期限为止（`eventually`、`waitLink`、`waitReachable`），不使用固定的 sleep。用例互相独立，都调用 `t.Parallel()`。
- 父进程监督测试子进程。成功、失败、panic、测试超时或收到 SIGINT/SIGTERM 后，都会终止本次子进程、删除本次容器和工作目录。清理失败时返回非零退出码，并保留资源记录和路径，便于恢复。
- SIGKILL、机器断电或容器服务不可用时无法保证即时清理；遗留资源记录保存在 `.run-*/containers/`，只按其中的名称清理，勿使用全局 prune。

Go 在启动测试程序前生成的测试编译缓存由 Go 自身管理。测试程序内部的构建和运行产物均在 `.run-*` 下，并随测试清理。

修改测试镜像内容后，需要根据测试提示单独准备新镜像。`IT_BASE_IMAGE` 可以选择已有的基础镜像版本；`IT_APT_MIRROR` 只用于生成镜像准备命令。
