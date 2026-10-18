# OCR 评测容器（**仅开发期**，绝不进生产依赖或生产镜像）。
#
# 为什么用容器而不是往宿主装：本机没有任何 OCR 可执行文件，而 AGENTS.md 禁止自动提权改宿主系统。
# 这里只用官方发行版基础镜像 + 官方 apt 仓库里的 tesseract，不使用来源不明的"一键 OCR 镜像"。
#
# 隔离边界（由 docs/scripts/ocr/run-ocr-container.sh 在 `docker run` 时落实，不靠镜像自身）：
#   - 识别阶段 `--network none`：样本不出机器，不接任何外部 OCR/LLM API。
#   - 样本目录**只读**挂载；输出目录单独可写挂载。
#   - 不用 --privileged，不挂 /var/run/docker.sock，不加任何 capability。
#   - ground truth 文件**从不**挂进来：容器里没有人能读到答案。
#
# 架构：本机 arm64。基础镜像与最终 digest 记录在 docs/STAGE4B.md（可复现性靠 digest，不靠 tag）。
FROM ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      tesseract-ocr tesseract-ocr-eng fontconfig \
 && rm -rf /var/lib/apt/lists/*

# 版本与模型信息在构建期落盘，运行期直接 cat 就能拿到本次基线的工具指纹，
# 不必事后靠 `tesseract --version` 的输出去猜装了什么。
# 包版本**不在 Dockerfile 里写死**：我不猜 Ubuntu 仓库里的具体版本号（猜错就是构建失败），
# 可复现性靠基础镜像 digest + 这里记录下来的实际解析版本。
RUN { echo "=== base ==="; . /etc/os-release; echo "=== apt resolved versions ==="; \
      dpkg-query -W -f='${Package} ${Version} ${Architecture}\n' tesseract-ocr tesseract-ocr-eng tesseract-ocr-osd fontconfig 2>&1; \
      echo "=== tesseract --version ==="; tesseract --version 2>&1; \
      echo "=== supported langs ==="; tesseract --list-langs 2>&1; \
      echo "=== eng traineddata ==="; ls -l /usr/share/tesseract-ocr/*/tessdata/eng.traineddata 2>&1; \
      md5sum /usr/share/tesseract-ocr/*/tessdata/eng.traineddata 2>&1; } > /TOOL_FINGERPRINT.txt 2>&1

ENTRYPOINT ["/usr/bin/tesseract"]
