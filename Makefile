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
	@echo "make stage1-check 全流程验收（阶段 1 起生效）"
	@echo "make stage2-check 管理面 + 来源策略 + 轮换 + 重启语义端到端验收（阶段 2）"
	@echo "make stage3a-check 题池 + 生成器 + 单次作答端到端验收（阶段 3A）"
	@echo "make stage3b-check 视觉题 + 图片资产 + 预览 + 评测 端到端验收（阶段 3B，含 check 与前三阶段）"
	@echo "make eval-samples 生成离线评测样本（dev 调参集 + holdout 验收集，输出 eval-out/ 不入库）"
	@echo "make ocr-baseline 用本机离线 OCR 跑基线（无工具时以退出码 3 报\"未测\"）"
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
	go test -count=1 -v -run 'TestPrivateLeakGuardWorks|TestPublicHasNoPrivateFields' ./internal/generator/
	go test -count=1 -v -run 'TestPoolStatusIsReadOnlyAndLeakFree|TestAuditCoversWritesAndLeaksNothing' ./internal/admin/
	go test -count=1 -v -run 'TestPowQuestionHappyPath' ./internal/api/
	go test -count=1 -v -run 'TestTextImage' ./internal/api/
	go test -count=1 -v -run 'TestTextImagePrivateFieldsNeverLeave|TestTextImageAssetHasNoTextChunks' ./internal/generator/
	go test -count=1 -v -run 'TestSecretNeverReachesBrowser|TestVerifyTargetIsNotBrowserControlled' ./cmd/preview/
	@echo "按值扫描通过：哨兵 secret、3A/3B 的私有 verifier 与图片资产都不出现在日志、错误响应、启动输出与管理面响应中"

.PHONY: stage1-check
stage1-check:
	@bash docs/scripts/stage1.sh

.PHONY: stage2-check
stage2-check:
	@bash docs/scripts/stage2.sh

.PHONY: stage3a-check
stage3a-check:
	@bash docs/scripts/stage3a.sh

# 阶段 3B 的验收**包含**前面全部检查：图片资产不能改变既有一次性消费与生命周期语义，
# 这件事只能靠回归证明，不能靠"我看代码没动"。
.PHONY: stage3b-check
stage3b-check: check secret-scan stage1-check stage2-check stage3a-check
	@bash docs/scripts/stage3b.sh

.PHONY: eval-samples
eval-samples:
	@mkdir -p eval-out
	go run ./cmd/evalgen --set dev --count 60 --difficulty 3 --out eval-out
	go run ./cmd/evalgen --set dev --count 60 --difficulty 5 --out eval-out
	go run ./cmd/evalgen --set holdout --count 100 --difficulty 3 --out eval-out
	@echo "输出在 eval-out/（含 ground truth，已在 .gitignore 里）；holdout 集不要用来调参"

.PHONY: ocr-baseline
ocr-baseline:
	@gt=$$(ls eval-out/holdout-*/ground-truth.jsonl 2>/dev/null | head -n1); \
	  if [ -z "$$gt" ]; then echo "先跑 make eval-samples"; exit 1; fi; \
	  go run ./cmd/evalocr --groundtruth "$$gt"; \
	  rc=$$?; \
	  if [ $$rc -eq 3 ]; then echo "OCR 实测：未执行（本机无离线 OCR 工具，退出码 3 = 未测而非 0%%）"; fi; \
	  exit $$rc

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	rm -rf bin /tmp/epocha-dev
