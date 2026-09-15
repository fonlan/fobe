#!/usr/bin/env bash
# scripts/dev.sh — 本地调试环境,不走容器(design.md §16 本地开发)
#
#   scripts/dev.sh              # 后端(API-only)+ Vite 前端一起起
#   scripts/dev.sh --no-web     # 只起后端,自己另开终端跑 npm run dev
#   scripts/dev.sh --reset      # 先删掉 data/dev.db 再起(全新状态)
#   scripts/dev.sh --port 8081  # 换后端端口(Vite 代理指向 127.0.0.1:8080,换端口需同步改 web/vite.config.ts)
#
# Ctrl+C 一次性退出两个进程。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

NO_WEB=0
RESET=0
PORT="${FOBE_PORT:-8080}"
while [ $# -gt 0 ]; do
    case "$1" in
        --no-web) NO_WEB=1 ;;
        --reset)  RESET=1 ;;
        --port)   PORT="$2"; shift ;;
        -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
        *) echo "dev.sh: unknown arg: $1 (see --help)" >&2; exit 1 ;;
    esac
    shift
done

command -v go >/dev/null || { echo "dev.sh: 未找到 go,请先安装 Go" >&2; exit 1; }

# 开发主密钥:由固定字符串派生,每次启动一致(dev.db 里加密过的设置才解得开)。
# ⚠ 仅限本机调试,生产必须用 openssl rand -base64 32 生成。
export FOBE_MASTER_KEY="${FOBE_MASTER_KEY:-$(printf 'fobe-dev-master-key-not-for-prod' | base64)}"
export FOBE_DB="${FOBE_DB:-$ROOT/data/dev.db}"
export FOBE_DL_DIR="${FOBE_DL_DIR:-$ROOT/data/dl}"
# 国别库默认路径是容器里的 /data/geoip —— 本机跑时 /data 通常不可写
# (macOS 上直接是只读根),自动更新会在那里报 mkdir /data: read-only file
# system。所以 dev 显式指到仓库内的 data/ 下,和 dev.db / dl 同一层。
export FOBE_GEOIP_MMDB="${FOBE_GEOIP_MMDB:-$ROOT/data/geoip/GeoLite2-Country.mmdb}"
export FOBE_LISTEN="127.0.0.1:$PORT"
export FOBE_INSTALL_TMPL="$ROOT/scripts/install.sh.tmpl"
export FOBE_BACKUP_DIR=""
export FOBE_ADMIN_PASSWORD="${FOBE_ADMIN_PASSWORD:-devpass123}"
# 留空 FOBE_WEB_DIR → API-only 模式:server 的 / 提示去访问 Vite(§16)
unset FOBE_WEB_DIR || true

if [ "$RESET" = 1 ]; then
    rm -f "$FOBE_DB" "$FOBE_DB-wal" "$FOBE_DB-shm"
    echo "dev.sh: 已重置开发数据库 $FOBE_DB"
fi
mkdir -p "$(dirname "$FOBE_DB")" "$FOBE_DL_DIR" "$(dirname "$FOBE_GEOIP_MMDB")"

BACKEND_PID=""
FRONTEND_PID=""
cleanup() {
    # 每步都吞掉非零返回:进程可能已退出,pkill 无匹配也返回 1,
    # 在 set -e 下中断会跳过剩余的清理步骤
    [ -n "$FRONTEND_PID" ] && pkill -P "$FRONTEND_PID" 2>/dev/null || true
    [ -n "$FRONTEND_PID" ] && kill "$FRONTEND_PID" 2>/dev/null || true
    [ -n "$BACKEND_PID" ] && kill "$BACKEND_PID" 2>/dev/null || true
    pkill -P $$ 2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# 先编译再运行:kill 的是真正的 server 进程,go run 会留孤儿
echo "dev.sh: 编译 server ..."
go build -o "$FOBE_DL_DIR/.dev-server" ./cmd/server
"$FOBE_DL_DIR/.dev-server" &
BACKEND_PID=$!

echo "dev.sh: 后端就绪 → http://127.0.0.1:$PORT  (API-only 模式;开发密码: $FOBE_ADMIN_PASSWORD)"
echo "        /install.sh 已可用(模板: scripts/install.sh.tmpl)"

if [ "$NO_WEB" != 1 ]; then
    command -v npm >/dev/null || {
        echo "dev.sh: 未找到 npm;先装 Node 22,或用 --no-web 只跑后端" >&2
        exit 1
    }
    if [ ! -x "$ROOT/web/node_modules/.bin/vite" ]; then
        echo "dev.sh: 安装前端依赖(web/node_modules)..."
        (cd web && npm install)
    fi
    # exec vite:$! 就是 vite 本身,避免 npm 中间层留孤儿
    (cd web && exec ./node_modules/.bin/vite) &
    FRONTEND_PID=$!
    echo "dev.sh: 前端就绪 → http://127.0.0.1:5173  (浏览器访问这个)"
fi

echo "dev.sh: Ctrl+C 退出"
wait
