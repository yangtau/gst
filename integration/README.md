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
| `link_test.go` | `TestLinkLifecycle` | `gst add`：状态变为 running，对端名称正确，`gst status` 在两端列出对方；daemon 日志以 `link NAME: ` 为前缀记录 link；重复 `add` 失败并提示先 `remove`；杀掉 server 的 daemon 或断开 SSH 会话后先 down 再 running；`remove` |
| | `TestLinkDownIsAProblem` | 连不上的 link 状态为 down，`gst status` 在 `# Problems:` 中列出 |
| | `TestLinkRemoteGstMissing` | server 上没有 gst：状态 down 且错误提到 gst，`gst status` 列出该 link 的错误，删除后带 `--remote-gst` 重新添加修复后 running |
| | `TestLinkRemoveIsSeenByServer` | link 删除后 server 不再认为 client 可达 |
| | `TestLinkToAMachineThatIsNotUp` | server 没执行过 `gst up`：`gst add` 退出码 1 并给出 `ssh -t` 命令，server 上没有 daemon 和配置；在 server 上执行 `gst up` 后 link 自动 running |
| `mesh_test.go` | `TestMesh` | 三个节点 a → b ← c：`gst status` 列出的机器与路径（`Topology`）；`gst ping -v` 同时显示经 b 的回复和 SSH link 的检查，不存在的机器失败并给 hint（`Ping`）；在 a 上查看两跳外 c 的 link（含连不上的），c 自己的 `gst status` 列出问题（`Status`） |
| `direct_test.go` | `TestUDPPath` | a → b ← c，a 和 c 之间没有 link：流量触发 UDP 路径，`gst status` 显示 `online; direct`，mesh 的路径仍是 a,b,c，`gst ping -v` 显示过程；b `gst down` 后路径继续；c 删除 link 后路径断开。其他用例的节点以 `GST_DIRECT=off` 运行 |
| `expose_test.go` | `TestExpose` | 三节点经 SSH 的 TCP、Unix stream、UDP 转发，同端口双协议、本机 `.gst` 访问、物理 IP 不可访问、daemon 重启恢复、网段切换及删除规则 |
| `net_test.go` | `TestNet` | a → b ← c，`GST_DIRECT=off`：helper 装好后选出不与容器网络重叠的网段，地址唯一，路由指向 `gst0`；经 b 的 ICMP、TCP、UDP（`Protocols`）；`getent`、`ping c.gst`、daemon 的 DNS 回答 link 名，未知名字失败（`Names`）；`gst status`、`gst status c`、`gst status --all`（`Status`）；`gst ping` 经虚拟网络回复，显示地址和 `over SSH via b`，`--json`，ping 本机不显示路径（`Ping`）；非法网段退出码 2，`gst subnet` 传播到所有机器且旧路由被删除，与容器网络重叠时退出码 1 带 hint 并出现在 `gst status`（`Subnet`）；`gst down` 删除网卡和名字，别的机器到不了它，`gst up` 后地址不变（`Down`） |
| | `TestNetDirect` | 包走 UDP 路径（`gst status --all` 显示 `direct`），TCP 速率和按帧成批的 UDP 到达率；用 `iptables`/`ip6tables` 丢弃 UDP 后几秒内改走 SSH，恢复后回到 UDP |
| | `TestNetHelper` | 没有 helper 时 `gst subnet` 和 `gst ping` 退出码 1 并提示 `gst up`，普通用户不能直接执行 `gst install`；安装后机器自动加入，之前选的网段生效；helper 的文件和 socket 权限；daemon 被杀后网卡消失，`gst up` 后地址不变；`gst down` 后网卡消失，helper 的进程、socket、服务文件和那份 gst 都不在了 |
| | `TestUpInstallsTheHelper` | 镜像里没有 sudo，用只记录参数的替身：没有终端时 `gst up` 不运行 sudo，`--yes` 时显示并运行 `sudo <gst> install --user`，以 root 执行记录下的命令后机器加入网络；helper 版本相同时不再运行 sudo |
| `daemon_test.go` | `TestDaemonRestartsAfterUpgrade` | 升级 gst 后下一条命令重启 daemon，虚拟网络恢复，没有升级则不重启 |
| | `TestDownAndUp` | `gst down` 停止 daemon，其他节点的 `gst peer-bridge` 被拒绝（"gst is not up"），`gst up` 后 link 恢复 |
| `ui_test.go` | `TestUI` | `gst ui`：没有 token 返回 401，`/api/state` 返回 links 和 topology，页面只读（没有 link 的 POST 接口） |

`harness_test.go` 构建 gst 和 `gst-it` 并检查镜像，`supervisor_test.go` 是监督测试子进程并负责清理的父进程。

## 隔离与清理

- 每次运行在本目录创建独立的 `.run-*` 工作目录，权限为 `0700`。为 Linux 构建的 gst 和 `gst-it`、构建缓存、模块缓存和容器记录都存放在其中。
- 每个用例创建带本次运行唯一前缀的容器；只对这些容器执行命令和删除操作。已有容器、镜像、用户 SSH 配置和已安装的 tunnel 服务不参与测试。
- 本机上不运行 gst，也不监听或连接任何端口。每个用例启动若干节点容器（`node_test.go`）：每个节点都运行 sshd 和 gst，以 `tunnel` 用户身份运行，节点之间通过容器 IP 通信。`startPair` 给出一个 client 和一个 server，client 的 gst 用户用生成的身份和 known_hosts 登录 server（通过 ssh 配置的 `IdentityFile` 与 known_hosts 文件）；`start3` 启动 a、b、c 三个节点，a 和 c 各自 link 到 b。测试把 gst 和网络辅助程序 `gst-it`（`ithelper/`，提供 TCP echo 服务、并发往返、HTTP 客户端、UDP 与 DNS 客户端和测速工具）通过 stdin 复制进容器，不挂载本机目录。密钥在内存中生成，经 stdin 写入容器。
- server 要先执行 `gst up`，`trust` 替用例做这一步。镜像里没有 systemd，`sshd/systemctl` 这个 shim 代替 `systemctl`，带 `--user` 时管用户的单元，不带时管系统的单元（虚拟网络的 helper），只在容器内后台启动和停止程序，不模拟 `Restart=` 等单元设置。节点容器带 `NET_ADMIN` 和 `NET_RAW`，让 helper 能在容器内创建 `gst0`。虚拟网络的测试只覆盖 Linux 上写 `/etc/hosts` 的解析方式，macOS 的 `utun`、路由、`/etc/resolver` 和 systemd-resolved 由单元测试覆盖到命令一级，实际效果要在真机上验证。
- 等待状态一律轮询到期限为止（`eventually`、`waitLink`、`waitReachable`），不使用固定的 sleep。用例互相独立，都调用 `t.Parallel()`。
- 父进程监督测试子进程。成功、失败、panic、测试超时或收到 SIGINT/SIGTERM 后，都会终止本次子进程、删除本次容器和工作目录。清理失败时返回非零退出码，并保留资源记录和路径，便于恢复。
- SIGKILL、机器断电或容器服务不可用时无法保证即时清理；遗留资源记录保存在 `.run-*/containers/`，只按其中的名称清理，勿使用全局 prune。

Go 在启动测试程序前生成的测试编译缓存由 Go 自身管理。测试程序内部的构建和运行产物均在 `.run-*` 下，并随测试清理。

修改测试镜像内容后，需要根据测试提示单独准备新镜像。`IT_BASE_IMAGE` 可以选择已有的基础镜像版本；`IT_APT_MIRROR` 只用于生成镜像准备命令。
