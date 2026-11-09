# TODO

## Linux 上 UDP 路径不走 VPN

UDP 路径应该只走本机真实 IP，不走任何 VPN 或代理：TUN 代理用自己的 socket 重新发送每个流，规则是直连也一样，端口变了，打洞就失败。macOS 已经这样做（`internal/direct/bind_darwin.go`）。Linux 上包还是按系统路由走，开了 TUN 模式的代理时，机器会留在 SSH 上。

已知情况：

- 普通用户设置 `SO_MARK` 会失败；`SO_BINDTODEVICE` 每个 socket 只能设一次，改绑或清除都会失败（在 5.15 内核上实测）。daemon 以用户身份运行。
- Tailscale 用 `SO_MARK` 标记 socket，再加 `ip rule`，让带标记的包抢在其他规则之前查 `main` 表，不行时退回 `SO_BINDTODEVICE`。它以 root 运行。

可选方案：

- helper 打标记并加规则。所有网卡都能收包，跟着 `main` 表走，不用轮询；但要升级 helper，代理改了 `main` 表时绕不开。
- daemon 用 `SO_BINDTODEVICE` 绑到默认路由所在的非隧道网卡。不需要 root，代理改了 `main` 表也可能绕开，只要还留着物理网卡的默认路由；但 socket 只收这块网卡的包，换网卡要在原端口上重开 socket。

无论选哪个，都要做：

- 单元测试走回环地址，而 Linux 上绑了网卡的 socket 发不了回环，所以 `direct.Listen` 要能指定是否绑定。
- offer 里去掉点对点接口的地址，macOS 也一样：现在还会提供这些地址，只是白白多探测。
- 加一个集成测试：用 `ip rule` 把 UDP 引到一个 TUN 设备上，确认路径还能打通。

## 空闲时停止探测

现在路径建立后一直每秒探测一次，每 30 秒全量探测一次，不管有没有流量。机器多、长期空闲时是白白的包和唤醒。

要做的：

- 路径分活跃和空闲两种状态，空闲时停止全量探测，停止或降低保活频率。Tailscale 距上次发送超过 45 秒就停止心跳。
- 只把心跳调慢不行：`silenceLimit` 是 3 秒，要一起改，否则空闲路径会被判断开。
- 新流量到来时要马上恢复，这时 NAT 映射可能已经过期，首包可以先走 SSH。gst 没有常在线的中继，SSH 也可能已经断开，要测恢复时间。
- 活动信号用本机发送和 WireGuard 认证过的接收，不能用未认证的包。

## 地址选择给私网地址加分

现在只看 RTT。同一个局域网里的路径不经过外部网络，更稳定，也不占公网带宽，所以 RTT 接近时应该优先。

要做的：

- 在 `Path.record` 的切换判断里给私网地址加分，比如 RTT 差距在一定范围内时优先私网地址。Tailscale 的做法是私网 +20、链路本地 +30、回环 +50、IPv6 +10。
- 明显更慢的私网地址不能压过公网地址。

## 批量收发

`vnet.bind` 已经按批次拿到包，但 `Send` 把批次拆开，经 mesh 逐包写 UDP。Linux 上可以把批次传到 socket，用 `sendmmsg`、`recvmmsg`，再评估 UDP GSO/GRO。Tailscale 的实现在 `net/batching`，没有 macOS 后端。

先看 `gst ping -v` 的丢包计数和 CPU，确认是瓶颈再做。
