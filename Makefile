SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
# 显式安装 Go 1.27.3；禁止构建过程下载或切换工具链（ADR-020）
export GOTOOLCHAIN := local
export GOPROXY    := off
export GOSUMDB    := off
export GOENV      := off
export GOWORK     := off
GO          ?= go
MODULE      := github.com/shijianus/EpoCHA
BIN         := bin/epocha
OPS_BIN     := bin/epocha-ops
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT      := $(shell git rev-parse HEAD 2>/dev/null || echo none)
BUILDDATE   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) \
               -X $(MODULE)/internal/version.Commit=$(COMMIT) \
               -X $(MODULE)/internal/version.BuildDate=$(BUILDDATE)

# 资源预算（DESIGN §13），超限即失败；二进制按精确字节比较，35 MiB = 36700160 B。
MAX_BIN_BYTES := 36700160
MAX_WIDGET_KB := 30

.PHONY: help
help:
	@echo "make build        构建单二进制到 bin/epocha（注入 version）"
	@echo "make test         go test ./..."
	@echo "make race         go test -race ./...   （每阶段收尾必跑）"
	@echo "make vet fmt      静态检查 / 格式化"
	@echo "make check        fmt-check + vet + race + size   （提交前）"
	@echo "make size         断言二进制 <= $(MAX_BIN_BYTES) 字节（35 MiB；不截断）"
	@echo "make widget-size  断言 web/widget/v1.js gzip < $(MAX_WIDGET_KB)KiB（阶段 4）"
	@echo "make widget-checks 语法检查 + JS↔Go 共享 PoW 向量 + widget 生命周期竞态（Node 24，零第三方依赖）"
	@echo "make engineering-rules 发布入口/退出码/LF/部署配方的纯 Node 负向回归"
	@echo "make release-check    全发布工程入口；直接 node docs/scripts/release-check.mjs 才保留 0/1/3 真码"
	@echo "make engineering-check 仅工程子集，不作为完整发布验收；JSON 收据在 release-artifacts/"
	@echo "make browser-check 只跑浏览器交互验收（需先起服务与 exampleapp，见 docs/STAGE4A.md）"
	@echo "make admin-demo     起一套只有 loopback 的管理控制台演示（临时库+运行时凭据文件）"
	@echo "make admin-ui-check 管理控制台验收：挂载边界/安全头 + 浏览器交互（退出码三态）"
	@echo "make secret-scan  按值扫描日志密钥泄露（阶段 2 起生效）"
	@echo "make stage1-check 全流程验收（阶段 1 起生效）"
	@echo "make stage2-check 管理面 + 来源策略 + 轮换 + 重启语义端到端验收（阶段 2）"
	@echo "make stage3a-check 题池 + 生成器 + 单次作答端到端验收（阶段 3A）"
	@echo "make stage4a-check widget + 业务接入 + 真实浏览器交互（阶段 4A；逐项只跑一次）"
	@echo "make stage3b-check 视觉题 + 图片资产 + 预览 + 评测 端到端验收（阶段 3B，含 check 与前三阶段）"
	@echo "make eval-samples 生成离线评测样本（dev 调参集 + holdout 验收集，输出 eval-out/ 不入库）"
	@echo "make ocr-baseline 用本机离线 OCR 跑基线（无工具时二进制退出码 3=BLOCKED；注意 make 自身只会给 0 或 2）"
	@echo "make ocr-container 构建隔离 OCR 评测容器（仅开发期；记录镜像与模型指纹）"
	@echo "make ocr-baseline-real 阶段 4B 真实基线：dev 调参 + dev/holdout 正式测量（需先 ocr-container）"
	@echo "make blindtest     一条命令起人工可读性盲测（临时密钥/库/端口，页面导出无凭据 CSV/JSON）"
	@echo "make run          以临时 DB 启动（仅本地调试；它绑 :8080 即全部网卡，别在有公网的机器上试部署）"
	@echo "make build-ops    构建运维二进制到 bin/epocha-ops（备份/校验/恢复/探针/容器内管理）"
	@echo "make docker-image 规范构建：在容器内编译（需 registry 访问 + ≥2GB 可用磁盘给 Go 模块与编译中间产物）"
	@echo "make build-receipt-checks 离线构建收据与持续演练负向回归"
	@echo "make build-receipt-integration 真实 Go 静态交叉构建、独立目录验证与篡改拒绝（不运行 Linux 产物）"
	@echo "make docker-image-local 新唯一目录冻结构建输入/原始字节并核对实际镜像；输出 BUILD_RECEIPT_DIR"
	@echo "make stage5b-check 运行互斥（flock）、就地恢复拒绝、候选清单与 5 分钟持续演练（docs/STAGE5B.md）"
	@echo "                 退出码只由必需项（受支持 B 路径）决定；A 路径/amd64/65534/公网 TLS 属可选项，如实打印不决定结论"
	@echo "                 make 把任何非 0 折叠成 2，配方会打印脚本真实码；只验分类逻辑：make stage5b-check GATE_ARGS=--selftest-classify"
	@echo "make stage5a-check 部署包 + 备份恢复 + 容器语义 + 资源限额 的真实演练（会动 Docker，见 docs/STAGE5A.md）"
	@echo "make m4-baseline   M4 Gate B 开发期攻击基线（默认 1000 题/型，输出 tmp/m4-baseline-<UTC>；M4_OUT 可指定）"
	@echo "make loadtest ARGS=\"--base URL --site-key KEY --requests N\"  有界负载实测（需服务已在跑 + EPOCHA_SITE_SECRET）"

.PHONY: build
build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/server

.PHONY: build-ops
build-ops:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OPS_BIN) ./cmd/ops

# DESIGN §13/§17 点名的实测入口。它**不**替你启动服务，所以不会因"忘了起服务"报出一个假的低数字。
# 有界性在 cmd/loadtest 内部（并发 ≤16、请求 ≤5000），不靠这里的约定。
# 例：EPOCHA_SITE_SECRET=sk_... make loadtest ARGS="--base http://127.0.0.1:8080 --site-key pk_xxx --requests 150 --concurrency 4"
.PHONY: loadtest
loadtest:
	@test -n "$(ARGS)" || { echo "用法：make loadtest ARGS=\"--base URL --site-key KEY --requests N [--concurrency C]\"（还需 EPOCHA_SITE_SECRET）"; exit 2; }
	$(GO) build -trimpath -o bin/epocha-loadtest ./cmd/loadtest
	bin/epocha-loadtest $(ARGS)

# M4 Gate B：开发期攻击基线。只读公开 /api/challenge DTO（白名单 + base64 PNG），
# 不读 verifier/DB/生成器私有 seed，不进 cmd/server 的依赖图。输出 JSON/CSV 无答案。
# 默认写时间戳目录，避免覆盖 docs/reviews/evidence 里的正式报告证据；
# 要重建正式证据用 M4_OUT=docs/reviews/evidence。
M4_CHALLENGES ?= 1000
M4_OUT ?= tmp/m4-baseline-$(shell date -u +%Y%m%dT%H%M%SZ)
.PHONY: m4-baseline
m4-baseline:
	@mkdir -p bin
	$(GO) build -trimpath -o bin/epocha-m4baseline ./cmd/m4baseline
	bin/epocha-m4baseline --challenges $(M4_CHALLENGES) --out-dir $(M4_OUT)

# 生产镜像的两条构建路径，产物是**同一个运行时层**（deploy/docker/Dockerfile 的 runtime 阶段）：
#   docker-image        规范配方：toolchain 阶段在容器内 go build。需要 registry 可达，
#                       以及 /var/lib/docker 所在分区 ≥2GB 空闲（modernc.org 的生成代码解包就要 1GB 量级）。
#   docker-image-local   把 toolchain 阶段换成宿主已编译产物（--build-context 覆盖同名阶段）。
#                       磁盘或网络不足以在容器内编译时用这条；镜像内容与规范配方逐文件一致，
#                       由 stage5a.sh 的"docker save 文件清单"检查证明。
DOCKER_IMAGE ?= epocha:5a
DOCKERFILE  := deploy/docker/Dockerfile
.PHONY: docker-image
docker-image:
	docker build -f $(DOCKERFILE) --target runtime -t $(DOCKER_IMAGE) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_DATE=$(BUILDDATE) .

# A receipt directory is single-use. Existing .prebuilt/out and previous run-* trees are never overwritten.
# A caller such as stage5b supplies the same frozen tuple and directory to both build and verification.
PREBUILT_DIR ?=
BUILD_GOOS   ?= linux
BUILD_GOARCH ?= $(shell GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOENV=off GOWORK=off "$(GO)" env GOARCH)
.PHONY: docker-image-local build-receipt-checks build-receipt-integration
docker-image-local:
	@pb="$(PREBUILT_DIR)"; \
	if [[ -z "$$pb" ]]; then \
	  mkdir -p deploy/docker/.prebuilt; \
	  parent=$$(mktemp -d deploy/docker/.prebuilt/run-XXXXXX); pb="$$parent/build"; \
	fi; \
	node docs/scripts/build-receipt.mjs build --root "$(CURDIR)" --out "$$pb" --go "$(GO)" \
	  --version "$(VERSION)" --commit "$(COMMIT)" --build-date "$(BUILDDATE)" --goos "$(BUILD_GOOS)" --goarch "$(BUILD_GOARCH)"; \
	node docs/scripts/build-receipt.mjs image --root "$(CURDIR)" --out "$$pb" --go "$(GO)" --tag "$(DOCKER_IMAGE)"; \
	printf 'BUILD_RECEIPT_DIR=%s\n' "$$pb"

build-receipt-checks:
	@node docs/scripts/node-tests.mjs --test-reporter=tap docs/scripts/build-receipt.test.mjs docs/scripts/soak-evidence.test.mjs

build-receipt-integration:
	@RECEIPT_GO="$(GO)" node docs/scripts/node-tests.mjs --test-reporter=tap docs/scripts/build-receipt.integration.test.mjs

.PHONY: run
run: build
	@mkdir -p /tmp/epocha-dev
	EPOCHA_MASTER_KEY=$$(head -c32 /dev/urandom | base64) \
	EPOCHA_PEPPER=$$(head -c32 /dev/urandom | base64) \
	$(BIN) --db /tmp/epocha-dev/epocha.db --listen :8080

.PHONY: test
test:
	$(GO) test -count=1 ./...

.PHONY: race
race:
	$(GO) test -race -count=1 ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@node docs/scripts/release-check.mjs --fmt-check

.PHONY: check
check: fmt-check vet race size

.PHONY: size
size: build
	@node docs/scripts/release-check.mjs --binary-size $(BIN)

.PHONY: widget-size
widget-size:
	@node docs/scripts/release-check.mjs --widget-size

.PHONY: engineering-rules engineering-check release-check
engineering-rules: build-receipt-checks
	@node docs/scripts/node-tests.mjs --test-reporter=tap docs/scripts/release-check.test.mjs

engineering-check:
	@node docs/scripts/release-check.mjs --profile engineering

# make 折叠非零为2；JSON收据及脚本末行保留真实0/1/3。没有人工PASS覆盖开关。
release-check:
	@node docs/scripts/release-check.mjs --profile full


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
	go test -count=1 -v -run 'TestSecretNeverReachesBrowser|TestClientCannotChooseVerifyTarget|TestPostWritesOnlyWhenVerifySucceeds' ./cmd/exampleapp/
	go test -count=1 -v -run 'TestBackupVerifyRestoreThroughCLI|TestAdminSubcommandIsLoopbackOnly|TestMissingKeyMaterialRefusesWithExit1|TestKeyFileEntryIsHonoredByOps' ./cmd/ops/
	go test -count=1 -v -run 'TestFlowAgainstStubRecordsAllThreeStages' ./cmd/loadtest/
	@echo "按值扫描通过：哨兵 secret、3A/3B 的私有 verifier 与图片资产、5A 的运维输出与负载工具都不出现在日志、错误响应、启动输出与管理面响应中"

.PHONY: stage1-check
stage1-check:
	@bash docs/scripts/stage1.sh

.PHONY: stage2-check
stage2-check:
	@bash docs/scripts/stage2.sh

.PHONY: stage4a-check
# **不嵌套**其它总门禁：脚本内部把 fmt-check / vet / race / size / secret-scan / widget-size /
# stage1..3b 各跑一次并逐条记录退出码（同一套 -race 跑几遍不多证明任何东西，只烧墙钟）。
stage4a-check:
	@bash docs/scripts/stage4a.sh

.PHONY: stage4b-check
# 4B 自己的工程项（评分口径、容器隔离实证、盲测守卫）+ 一次 stage4a.sh：
# 回归与浏览器套件各只跑一遍，不把同一套 -race 再跑第二遍。
stage4b-check:
	@bash docs/scripts/stage4b.sh

.PHONY: stage5b-check
# 5B 的门禁：运行锁的真实进程测试、容器/宿主共享同一把锁、就地恢复拒绝、
# 候选构建清单，以及约 5 分钟的节流持续演练。回归只跑一次 -race ./...，不重跑 5A 的整套演练。
# 退出码三态，且**只由必需项（受支持的 B 路径）决定**：0=必需项全 PASS、1=有 FAIL、
# 3=无失败但必需项里有 BLOCKED/NOT_RUN。可选项（A 路径、amd64、UID 65534、公网 TLS）的
# NOT_RUN 如实打印但不决定结论——这是"缩小第一版正式支持范围"，不是把它写成通过。
# 实测：make 把任何非 0 一律折叠成 **2**。所以配方先原样打印脚本的真实码，再按真实码退出；
# 要机器可读就读最后的 RESULT / SCRIPT_RC 行，或直接 bash docs/scripts/stage5b.sh。
# GATE_ARGS 只用于验证门禁自身的分类与包装层行为（不必为此重跑五分钟演练）。
stage5b-check:
	@rc=0; bash docs/scripts/stage5b.sh $(GATE_ARGS) || rc=$$?; \
	if [ $$rc -ne 0 ]; then \
		echo "[make] stage5b.sh 真实退出码 = $$rc（make 自身只能报 2；结论看 RESULT/SCRIPT_RC 行）"; \
	fi; \
	exit $$rc

.PHONY: stage5a-check
# 5A 的门禁**不**嵌套其它总门禁：脚本内部跑一次 stage3b-check 承担回归，其余逐项是
# 镜像/密钥/备份恢复/边界/资源/反代的真实演练。
# 退出码：0=全部 PASS；1=有 FAIL；3=无失败但存在 BLOCKED/NOT_RUN（部署验收未通过）。
# `make` 会把 1 与 3 都折叠成 2 —— 要机器可读就直接跑脚本并读最后的 RESULT 行。
stage5a-check:
	@bash docs/scripts/stage5a.sh

.PHONY: widget-checks
widget-checks:
	@node --check web/widget/v1.js
	@node --check web/widget/v1.worker.js
	@node docs/scripts/node-tests.mjs docs/scripts/browser/pow_vectors.test.mjs docs/scripts/browser/widget_lifecycle.test.mjs

.PHONY: browser-check
# 只跑浏览器交互（需要外部已按 docs/scripts/stage4a.sh 的方式起好服务与 exampleapp）。
# 缺环境变量时 node 侧会直接报错退出，不会静默跳过。
browser-check:
	@node docs/scripts/node-tests.mjs docs/scripts/browser/widget_browser.test.mjs

.PHONY: admin-ui-check
# 管理控制台 M1 的门禁：Go 层挂载边界/安全头 + curl 端到端判据 + 十项真实浏览器交互。
# 退出码三态（0 全 PASS / 1 有 FAIL / 3 有 BLOCKED 或 NOT_RUN）。make 只能报 2，
# 所以要机器可读的结论就直接 bash docs/scripts/admin-ui-check.sh 并读最后的 RESULT 行。
admin-ui-check:
	@rc=0; bash docs/scripts/admin-ui-check.sh || rc=$$?; \
	if [ $$rc -ne 0 ]; then \
		echo "[make] admin-ui-check.sh 真实退出码 = $$rc（make 自身只能报 2；结论看 RESULT 行）"; \
	fi; \
	exit $$rc

.PHONY: admin-demo
# 本地演示入口：临时库 + 运行时随机凭据（0600 文件）+ 两个 loopback 监听器。
# 前台运行，Ctrl-C 只清理本轮临时目录，不碰仓库里的 bin/、.env、keys/。
admin-demo:
	@bash docs/scripts/admin-demo.sh

.PHONY: adminui-syntax
adminui-syntax:
	@node --check web/adminui/app.js

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

# 退出码分层写在 docs/scripts/ocr-baseline.sh 头部：二进制 3 = BLOCKED，
# 而 `go run` 折叠成 1、`make` 折叠成 2 —— "make ocr-baseline 返回 2"只表示配方里有非 0，
# 真实结论（PASS / BLOCKED / FAIL）读脚本打印的那一行。
.PHONY: ocr-baseline
ocr-baseline:
	@bash docs/scripts/ocr-baseline.sh

# OCR 评测容器：官方 ubuntu 基础镜像 + 官方仓库的 tesseract，只用于开发期测量。
# 生产服务与生产镜像**不**依赖它（AGENTS：纯 Go、无外部二进制）。
.PHONY: ocr-container
ocr-container:
	@docker build --platform linux/arm64 -t epocha-ocr-eval:stage4b docs/scripts/ocr/
	@docker run --rm --entrypoint cat --network none epocha-ocr-eval:stage4b /TOOL_FINGERPRINT.txt

# 真实 OCR 基线（阶段 4B）：dev 调参 + dev/holdout 正式测量。需要 make ocr-container 先建好镜像。
# 识别阶段 --network none、样本只读挂载、ground truth 从不进容器。
.PHONY: ocr-baseline-real
ocr-baseline-real:
	@bash docs/scripts/ocr/run-baseline.sh all

.PHONY: blindtest
blindtest:
	@bash docs/scripts/blindtest.sh

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	rm -rf bin /tmp/epocha-dev
