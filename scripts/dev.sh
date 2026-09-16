#!/usr/bin/env bash
# scripts/dev.sh — 本地调试环境,不走容器(design.md §16 本地开发)
#
#   scripts/dev.sh              # 后端(API-only)+ Vite 前端一起起
#   scripts/dev.sh --no-web     # 只起后端,自己另开终端跑 npm run dev
#   scripts/dev.sh --reset      # 先删掉 data/dev.db 再起(全新状态)
#   scripts/dev.sh --port 8081  # 换后端端口(Vite 代理指向 127.0.0.1:8080,换端口需同步改 web/vite.config.ts)
#
# 默认监听 0.0.0.0:局域网内其他机器可以直接连(装探针、开面板)。
# 只想绑本机:FOBE_LISTEN=127.0.0.1:8080 scripts/dev.sh
# ⚠ 开发环境密码固定(devpass123)、无 TLS,只适合可信内网。
#
# 除 server 外还会交叉编译一份 linux/amd64 的 agent 产物,并给两端注入同一个内容寻址的版本号
# dev-<哈希>(design §5.5):只有 agent 真编出来的二进制变了才换号,局域网里的真探针会跟着自更新。
#   FOBE_VERSION=<旧号> scripts/dev.sh            # 钉住版本号 → 降级演练(旧产物还在 DL 卷里)
#   FOBE_AGENT_UPDATE_STAGGER=1s scripts/dev.sh   # 一两台探针时把 0–5 分钟错峰压成"立即"
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
        -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
        *) echo "dev.sh: unknown arg: $1 (see --help)" >&2; exit 1 ;;
    esac
    shift
done

command -v go >/dev/null || { echo "dev.sh: 未找到 go,请先安装 Go" >&2; exit 1; }

# 只用于打印提示:取不到局域网 IP 不算错误(离线、没插网线等),留空即可
lan_ip() {
    if command -v ipconfig >/dev/null 2>&1; then
        ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true
    elif command -v hostname >/dev/null 2>&1; then
        hostname -I 2>/dev/null | awk '{print $1}'
    fi
}

# 开发主密钥:由固定字符串派生,每次启动一致(dev.db 里加密过的设置才解得开)。
# ⚠ 仅限本机调试,生产必须用 openssl rand -base64 32 生成。
export FOBE_MASTER_KEY="${FOBE_MASTER_KEY:-$(printf 'fobe-dev-master-key-not-for-prod' | base64)}"
export FOBE_DB="${FOBE_DB:-$ROOT/data/dev.db}"
export FOBE_DL_DIR="${FOBE_DL_DIR:-$ROOT/data/dl}"
# §5.5 自更新:dev 环境也要能真跟随,所以 dev.sh 自己产 agent 产物、并给自己一个"发布形态"的
# 版本号——裸 dev/compose 过不了发布门槛,而两端版本号恒等又会被判成"已收敛",两者叠加就是
# "测试环境永远不会自更新"。产物先落 seed 目录,由 server 启动时的 Seed 复制进 DL 卷:
# 和容器走同一条路径,顺带把"挂载卷遮蔽"那个真实坑也覆盖掉。
export FOBE_AGENT_SEED_DIR="${FOBE_AGENT_SEED_DIR:-$ROOT/data/agent-seed}"
# 占位版本号刻意不含数字:万一这份中间产物被误发,IsReleaseVersion 也会把它挡在发布门槛外。
AGENT_PENDING_VERSION="dev-pending"
# 国别库默认路径是容器里的 /data/geoip —— 本机跑时 /data 通常不可写
# (macOS 上直接是只读根),自动更新会在那里报 mkdir /data: read-only file
# system。所以 dev 显式指到仓库内的 data/ 下,和 dev.db / dl 同一层。
export FOBE_GEOIP_MMDB="${FOBE_GEOIP_MMDB:-$ROOT/data/geoip/GeoLite2-Country.mmdb}"
# 默认绑通配地址:探针装在局域网别的机器上,只绑回环它们连不上(见 AGENTS.md「已知陷阱」)。
export FOBE_LISTEN="${FOBE_LISTEN:-0.0.0.0:$PORT}"
export FOBE_INSTALL_TMPL="$ROOT/scripts/install.sh.tmpl"
export FOBE_BACKUP_DIR=""
export FOBE_ADMIN_PASSWORD="${FOBE_ADMIN_PASSWORD:-devpass123}"
export FOBE_AGENT_UPDATE_STAGGER="${FOBE_AGENT_UPDATE_STAGGER:-1s}"
# 留空 FOBE_WEB_DIR → API-only 模式:server 的 / 提示去访问 Vite(§16)
unset FOBE_WEB_DIR || true

# agent 为体积编译(design §17):strip + trimpath + 空 buildid,有 upx 再压一道(实测
# 7.15MB → 2.26MB)。压缩必须发生在取哈希/写 .sha256 之前,否则"一个版本号"会对应两份
# 字节,sha256 校验就会在探针上失败。GOAMD64 一律钉 v1:探针的 CPU 是什么我们不知道,
# v2/v3 在老机器上是 SIGILL,也就是探针掉线。
UPX=""
if [ "${FOBE_AGENT_UPX:-1}" = "0" ]; then
    echo "dev.sh: FOBE_AGENT_UPX=0,agent 产物不压缩"
elif command -v upx >/dev/null 2>&1; then
    UPX="upx -q --best --lzma"
else
    echo "dev.sh: 未找到 upx,agent 产物不压缩(发布镜像默认压;brew install upx 即与发布一致)"
fi
pack_agent() { [ -z "$UPX" ] || $UPX "$1"; }
AGENT_LDFLAGS="-s -w -buildid="

# agent 的内容寻址版本号(§5.5)。先用固定占位版本交叉编译一份 agent,再对这份产物取哈希——
# 版本号因此反映"agent 真正编出来的东西",不需要维护一张"哪些源码算 agent"的清单(清单漏一个
# 目录,症状是改了代码却不换号:探针永远停在旧二进制,而面板一路显示"已收敛",比时间戳方案更难
# 发现)。Go 构建对相同输入是确定性的(同源码同 flags ⇒ 同哈希,实测两次不同输出路径哈希一致),
# 于是"没改 agent 就重启 dev.sh"不会产生新版本、不会让局域网探针白下一次 10MB。
agent_version() {
    mkdir -p "$FOBE_DL_DIR/.agent-fp"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go build -trimpath \
        -ldflags "$AGENT_LDFLAGS -X github.com/fonlan/fobe/internal/agent.Version=$AGENT_PENDING_VERSION" \
        -o "$FOBE_DL_DIR/.agent-fp/linux-amd64" ./cmd/agent
    # 与真产物走同一套打包步骤,所以 upx 的版本/开关一变,版本号也跟着变。
    pack_agent "$FOBE_DL_DIR/.agent-fp/linux-amd64"
    local fp
    fp="$(shasum -a 256 "$FOBE_DL_DIR/.agent-fp/linux-amd64" | cut -c1-12)"
    # IsReleaseVersion 要求版本号里含数字;十六进制哈希全字母的概率极低但存在,补一位更省心。
    case "$fp" in *[0-9]*) ;; *) fp="0$fp" ;; esac
    printf 'dev-%s' "$fp"
}

if [ "$RESET" = 1 ]; then
    rm -f "$FOBE_DB" "$FOBE_DB-wal" "$FOBE_DB-shm"
    echo "dev.sh: 已重置开发数据库 $FOBE_DB"
fi
mkdir -p "$(dirname "$FOBE_DB")" "$FOBE_DL_DIR" "$(dirname "$FOBE_GEOIP_MMDB")" "$FOBE_AGENT_SEED_DIR"

# 提示用的地址:端口跟 FOBE_LISTEN 走(--port 只影响默认值),
# 只有确实绑了通配地址才宣传局域网地址,免得 FOBE_LISTEN=127.0.0.1:* 时提示撒谎
LISTEN_PORT="${FOBE_LISTEN##*:}"
LISTEN_HOST="${FOBE_LISTEN%:*}"
LAN_IP=""
case "$LISTEN_HOST" in
    0.0.0.0|""|::|"[::]") LAN_IP="$(lan_ip)" ;;
esac

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

# 版本号必须两端一致地注入:服务端版本号就是"该配哪一版 agent"(§5.5),注入错位的症状是
# 探针在自检里被判"版本不符"→ terminal 失败。FOBE_VERSION 可显式指定,回滚/降级演练靠它:
# 旧版本的产物还在 DL 卷里(Seed 从不覆盖),服务端一说旧号,探针就跟着降下去。
VERSION="${FOBE_VERSION:-$(agent_version)}"
AGENT_DIR="$FOBE_AGENT_SEED_DIR/agent/$VERSION"
if [ -s "$FOBE_DL_DIR/agent/$VERSION/linux-amd64" ] && [ -s "$FOBE_DL_DIR/agent/$VERSION/linux-amd64.sha256" ]; then
    echo "dev.sh: DL 卷里已有 agent 产物 $VERSION,跳过编译"
elif [ -s "$AGENT_DIR/linux-amd64" ] && [ -s "$AGENT_DIR/linux-amd64.sha256" ]; then
    echo "dev.sh: 复用 seed 里的 agent 产物 $VERSION"
else
    echo "dev.sh: 编译 agent $VERSION (linux/amd64) ..."
    mkdir -p "$AGENT_DIR"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go build -trimpath \
        -ldflags "$AGENT_LDFLAGS -X github.com/fonlan/fobe/internal/agent.Version=$VERSION" \
        -o "$AGENT_DIR/linux-amd64" ./cmd/agent
    pack_agent "$AGENT_DIR/linux-amd64"
    ( cd "$AGENT_DIR" && shasum -a 256 linux-amd64 > linux-amd64.sha256 )
fi

# 先编译再运行:kill 的是真正的 server 进程,go run 会留孤儿。
# server 为速度编译(design §17):保留 DWARF(pprof 要靠符号,-s -w 对速度没有任何
# 影响),并吃 cmd/server/default.pgo 这份 PGO 剖面(-pgo=auto 是 go 的默认行为,
# 显式写出来是为了让"这份 profile 真的被用上"在脚本里可见)。热点漂移后用
# scripts/pgo.sh 重采。
echo "dev.sh: 编译 server (PGO) ..."
go build -trimpath -pgo=auto -ldflags "-X main.version=$VERSION" -o "$FOBE_DL_DIR/.dev-server" ./cmd/server
"$FOBE_DL_DIR/.dev-server" &
BACKEND_PID=$!

echo "dev.sh: 后端就绪 → 本机 http://127.0.0.1:$LISTEN_PORT  (监听 $FOBE_LISTEN;API-only 模式;开发密码: $FOBE_ADMIN_PASSWORD)"
echo "dev.sh: 版本 $VERSION → 自更新目标 /dl/agent/$VERSION/linux-amd64(§5.5;FOBE_VERSION 可钉住旧号做降级演练)"
if [ -n "$LAN_IP" ]; then
    echo "        局域网 → http://$LAN_IP:$LISTEN_PORT  装探针时 --server 填这个"
    echo "        ⚠ 已暴露到局域网,密码是固定的开发密码且无 TLS,只限可信网络"
fi
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
    echo "dev.sh: 前端就绪 → 本机 http://127.0.0.1:5173  (浏览器访问这个)"
    if [ -n "$LAN_IP" ]; then
        echo "        局域网 → http://$LAN_IP:5173  (局域网内的浏览器用这个)"
    fi
fi

echo "dev.sh: Ctrl+C 退出"
wait
