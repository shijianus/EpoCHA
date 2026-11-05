#!/bin/sh
set -eu

# Cloady 的 values 是环境变量；EPOCHA_SITES_FILE 需要一个文件。
# 这里只在启动时把环境里的 JSON 落到 0600 文件，不在镜像层里保存任何站点凭据。
if [ -n "${EPOCHA_SITES_JSON:-}" ]; then
    umask 077
    printf '%s' "${EPOCHA_SITES_JSON}" > /tmp/epocha-sites.json
    export EPOCHA_SITES_FILE=/tmp/epocha-sites.json
fi

exec /usr/local/bin/epocha "$@"
