SHELL := /bin/bash
# 禁止构建过程下载或切换 Go 工具链（ADR-007）
export GOTOOLCHAIN := local
MODULE      := github.com/shijianus/EpoCHA
BIN         := bin/epocha
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILDDATE   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) \
               -X $(MODULE)/internal/version.Commit=$(COMMIT) \
               -X $(MODULE)/internal/version.BuildDate=$(BUILDDATE)

# 资源预算（DESIGN §13），超限即失败，不放宽
MAX_BIN_MB  := 35
MAX_WIDGET_KB := 30

.PHONY: help
help:
	@echo "make build        构建单二进制到 bin/epocha（注入 version）"
	@echo "make test         go test ./..."
	@echo "make race         go test -race ./...   （每阶段收尾必跑）"
	@echo "make vet fmt      静态检查 / 格式化"
	@echo "make check        fmt-check + vet + race + size   （提交前）"
	@echo "make size         断言二进制 <= $(MAX_BIN_MB)MB（DESIGN §13）"
	@echo "make widget-size  断言 web/widget/v1.js gzip <= $(MAX_WIDGET_KB)KB（阶段 4）"
	@echo "make secret-scan  按值扫描日志密钥泄露（阶段 2 起生效）"
	@echo "make stage1-check curl 全流程验收（阶段 1 起生效）"
	@echo "make run          以临时 DB 启动（仅本地调试）"

.PHONY: build
build:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/server

.PHONY: run
run: build
	@mkdir -p /tmp/epocha-dev
	EPOCHA_MASTER_KEY=$$(head -c32 /dev/urandom | base64) \
	EPOCHA_PEPPER=$$(head -c32 /dev/urandom | base64) \
	$(BIN) --db /tmp/epocha-dev/epocha.db --listen :8080

.PHONY: test
test:
	go test -count=1 ./...

.PHONY: race
race:
	go test -race -count=1 ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@test -z "$$(gofmt -l . | grep -v '^vendor/')" || { echo "未格式化：$$(gofmt -l .)"; exit 1; }

.PHONY: check
check: fmt-check vet race size

.PHONY: size
size: build
	@size=$$(stat -c %s $(BIN)); mb=$$(( size / 1024 / 1024 )); \
	echo "bin/epocha = $$mb MB"; \
	if [ $$mb -gt $(MAX_BIN_MB) ]; then echo "超出二进制体积预算 $(MAX_BIN_MB)MB（DESIGN §13）"; exit 1; fi

.PHONY: widget-size
widget-size:
	@echo "阶段 4 交付物：web/widget/v1.js 尚未生成（DESIGN §17 阶段 4）"; exit 1

.PHONY: secret-scan
# 按值扫描：测试内部注入哨兵值并断言其不出现在日志/响应/启动输出里（DESIGN §14-7）。
# 真实服务进程的按值扫描在 docs/scripts/stage1.sh 里做（两者互补）。
secret-scan:
	go test -count=1 -v -run 'TestNothingLeaksSecretsOrKeys' ./internal/api/
	go test -count=1 -v -run 'TestRunRefusesWithoutSecrets|TestServeHealthzAndGracefulShutdown' ./cmd/server/
	@echo "按值扫描通过：哨兵 secret 未出现在日志、错误响应与启动输出中"

.PHONY: stage1-check
stage1-check:
	@bash docs/scripts/stage1.sh

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	rm -rf bin /tmp/epocha-dev
