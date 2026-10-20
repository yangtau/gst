.DEFAULT_GOAL := help

SERVICE_COMMANDS := setup start stop restart status logs enable disable

# Optional overrides for the integration tests; see README.
-include integration/local.mk
export CONTAINER_RUNTIME IT_BASE_IMAGE IT_APT_MIRROR IT_BUILD_FLAGS

.PHONY: help build test test-integration $(SERVICE_COMMANDS)

help:
	@printf '%s\n' \
	  '后台服务在 Linux 上是 systemd 用户服务，在 macOS 上是 launchd 用户代理。' \
	  '' \
	  'make build    构建 Go 程序' \
	  'make test     运行测试' \
	  'make test-integration  在容器中的真实 sshd 上运行集成测试' \
	  'make setup    构建、安装、启用并启动服务（不重启已运行服务）' \
	  'make start    启动服务' \
	  'make stop     停止服务（保留自启）' \
	  'make restart  构建、重新加载服务定义并重启服务' \
	  'make status   查看服务状态' \
	  'make logs     实时查看日志（Ctrl-C 退出）' \
	  'make enable   启用自启并启动服务' \
	  'make disable  禁用自启并停止服务' \
	  'make help     显示帮助（默认命令）'

build:
	mkdir -p bin
	go build -o bin/reverse-ssh-tunnel .

test:
	go test ./...

test-integration:
	cd integration && go test -tags integration -count=1 -v .

setup restart: build

$(SERVICE_COMMANDS):
	./deploy/service.sh $@
