# 集成测试

在仓库根目录运行 `make test-integration`，或在本目录运行：

```sh
go test -tags integration -count=1 -v .
```

自动探测已安装的运行时：macOS 优先 Apple `container`，其次 podman、docker；Linux 优先 podman，其次 docker。`CONTAINER_RUNTIME` 可以显式指定。

运行时服务和 sshd 测试镜像必须事先准备好。测试只读取已有镜像，不安装软件、不启动或停止宿主机服务、不构建镜像或操作共享构建器。镜像缺失时会退出并打印准备命令，供用户单独执行。Apple `container` 直接使用容器 IP；Docker/Podman 发布随机 loopback 端口。

## 隔离与清理

- 每次运行在本目录创建独立的 `.run-*` 工作目录，权限为 `0700`。gst 二进制、构建缓存、模块缓存、临时配置、密钥和容器记录都存放在其中。
- 每个用例创建带本次运行唯一前缀的容器；只对这些容器执行命令和删除操作。已有容器、镜像、用户 SSH 配置和已安装的 tunnel 服务不参与测试。
- gst 只以前台 `run` 模式启动，使用临时配置和 `ssh -F /dev/null`，不安装后台服务。
- 父进程监督测试子进程。成功、失败、panic、测试超时或收到 SIGINT/SIGTERM 后，都会终止本次子进程、删除本次容器和工作目录。清理失败时返回非零退出码，并保留资源记录和路径，便于恢复。
- SIGKILL、机器断电或容器服务不可用时无法保证即时清理；遗留资源记录保存在 `.run-*/containers/`，只按其中的名称清理，勿使用全局 prune。

Go 在启动测试程序前生成的测试编译缓存由 Go 自身管理。测试程序内部的构建和运行产物均在 `.run-*` 下，并随测试清理。

修改测试镜像内容后，需要根据测试提示单独准备新镜像。`IT_BASE_IMAGE` 可以选择已有的基础镜像版本；`IT_APT_MIRROR` 只用于生成镜像准备命令。
