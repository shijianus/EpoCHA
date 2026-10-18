#!/usr/bin/env bash
# 单张图片的 OCR 调用：把宿主机上的图片送进**隔离容器**识别，文本打到 stdout。
#
# 这是 cmd/evalocr 的 --ocr-cmd 目标，所以它的契约很简单：argv[1]=图片路径，stdout=识别文本。
# 退出码非 0 或输出为空都会被 evalocr 记成"工具失败"（而不是答错），两个分母都会算它。
#
# 隔离边界（本脚本是唯一落实点，别处声明都不算）：
#   --network none      识别阶段不联网：样本不出这台机器，不接任何外部 OCR/LLM API
#   --read-only         容器根文件系统只读，堵住写回宿主的可能
#   --cap-drop ALL      不需要任何 capability
#   --memory/--cpus     限制资源，别让一次评测把 1GB 的目标机形状打穿
#   样本目录 :ro        OCR 只能读图；ground truth **从不**挂载进来
#   不挂 docker.sock、不 --privileged
#
# 为什么挂载点是 $EPOCHA_OCR_STAGE 而不是图片自己的目录：
# evalgen 把 ground-truth.jsonl 和 PNG 放在同一个目录里。直接挂那个目录，
# 就等于把答案交给 OCR 进程——哪怕它"没有理由去读"，隔离也不再成立。
# 所以评测编排脚本先把 PNG 硬链接进一个**只含图片**的暂存目录，这里只挂那一个。
set -euo pipefail

IMG="${1:-}"
[[ -n "$IMG" ]] || { echo "用法: $0 <图片绝对路径>" >&2; exit 2; }
[[ -f "$IMG" ]] || { echo "图片不存在: $IMG" >&2; exit 2; }
STAGE="${EPOCHA_OCR_STAGE:-}"
[[ -n "$STAGE" && -d "$STAGE" ]] || { echo "EPOCHA_OCR_STAGE 必须指向只含图片的暂存目录" >&2; exit 2; }

case "$IMG" in
  "$STAGE"/*) ;;   # 允许：图片就在只读暂存目录里
  *) echo "拒绝：图片不在暂存目录内（$IMG ∉ $STAGE）" >&2; exit 2 ;;
esac

IMAGE="${EPOCHA_OCR_IMAGE:-epocha-ocr-eval:stage4b}"
# PSM 与白名单由外层评测编排固定传入，不在这里留"默认值"：调参结果必须能被复述。
# 用 `-v` 而不是 `:?` 判定：白名单**允许是空字符串**（= 不限制字符集，也是一种候选参数），
# 但"没设置"必须报错，否则一次忘记传参就会被读成"OCR 在没有字符集约束下通过了"。
[[ -v EPOCHA_OCR_PSM ]] || { echo "EPOCHA_OCR_PSM 必须设置（7=单行，6=块，10=单字）" >&2; exit 2; }
[[ -v EPOCHA_OCR_WHITELIST ]] || { echo "EPOCHA_OCR_WHITELIST 必须设置（无白名单请显式传空字符串 ''）" >&2; exit 2; }
PSM="$EPOCHA_OCR_PSM"
WHITELIST="$EPOCHA_OCR_WHITELIST"
OEM="${EPOCHA_OCR_OEM:-3}"
NAME="${IMG##*/}"

# 注意 tesseract 的 argv 顺序：`-c key=value` 里等号两侧不能有空格，否则被解析成位置参数。
exec docker run --rm \
  --network none \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --memory 512m --cpus 1 \
  -v "$STAGE:/in:ro" \
  "$IMAGE" \
  /in/"$NAME" stdout \
  --oem "$OEM" --psm "$PSM" -l eng \
  -c "tessedit_char_whitelist=$WHITELIST"
