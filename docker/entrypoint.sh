#!/bin/sh
# marvis2api 容器入口：一键 docker run 即可，配置读取规则——
#
#   1. 挂载了 config.json（-v ./config.json:/app/config.json）→ 直接使用用户的配置
#      （upstream.* / 冷却参数以它为准）；
#   2. 未挂载或文件不存在 → 回退到数据卷内 /app/data/config.json
#      （不存在则首次启动复制镜像内的示例配置）。面板密码和 API Key 不在这个文件里。
#
# 说明：宿主机文件不存在时，Docker 会把挂载点创建为空目录，因此用 [ -f ] 判定
# 而不是 [ -e ]——目录不算"有配置"。
set -eu

CFG="/app/data/config.json"
if [ -f /app/config.json ]; then
    CFG="/app/config.json"
    echo "[entrypoint] 使用挂载的配置: /app/config.json"
else
    if [ ! -f "$CFG" ]; then
        cp /app/config.example.json "$CFG"
    fi
    echo "[entrypoint] 使用数据卷配置: $CFG"
fi

exec /app/marvis2api -config "$CFG"
