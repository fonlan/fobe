# AGENTS.md

fobe 是一个自托管的**单用户探针管理面板**：`server`(Go) + `agent`(Go，装在探针上) + `web`(React/TS/Vite)。

**权威文档**，改动前先读对应章节：

- [`docs/design.md`](docs/design.md) — 规格与已接受的取舍。每条决策都标了它绑定的代价，实现要与它一致；偏离就先改它。
- [`README.md`](README.md) — 面向使用者的部署/接入文档（外部 nginx 配置、排障速查）。

文档语言是中文，保持一致；代码注释默认英文，解释"为什么这么选 / 踩过什么坑"时用中文。

## 仓库结构

```
cmd/server/           面板服务端 + scheduler + admin CLI
cmd/agent/            探针 agent（-register / -run）
internal/protocol/    agent↔server 的全部 wire 类型（两端共用，唯一的协议定义处）
internal/server/
  httpapi/            HTTP/WS 路由与 handler（server.go 集中注册全部路由）
  store/              SQLite（WAL）与全部查询，schema.sql 内嵌
  hub/                WSS 连接与指令下发；security/ 密码·会话·Cryptor·XFF 信任链
  singbox/ geoip/ notify/ scheduler/ quota/
internal/agent/       采集、sing-box 生命周期、终端、延迟、证书、系统服务
web/src/              pages/ · components/ · api.ts · i18n.tsx · styles.css
deploy/               Dockerfile.server（三段构建）· docker-compose.yml
scripts/              dev.sh · build.sh · install.sh.tmpl
```

前端产物**编译进镜像**（`/srv/web`）由 server 直出，不做宿主机挂载。`FOBE_WEB_DIR` 留空时 server 进 API-only 模式，`/` 只提示去访问 Vite。

## 常用命令

```bash
scripts/dev.sh                    # 后端 API-only + Vite（Ctrl+C 一起退；devpass123）
scripts/dev.sh --reset            # 先删 data/dev.db 再起
scripts/dev.sh --no-web           # 只起后端
FOBE_LISTEN=127.0.0.1:8080 scripts/dev.sh   # 只绑本机（默认 0.0.0.0，会打印局域网地址）

go test ./...                     # 全部测试
go test ./internal/server/httpapi/ -run TestFullAgentPath   # 端到端主链路
gofmt -l internal cmd             # 必须无输出
go vet ./...

cd web && npm run build           # tsc（类型检查）+ vite build
scripts/build.sh [outdir]         # 本地/CI 三件套：server、linux/amd64 agent+manifest、web
docker compose -f deploy/docker-compose.yml --env-file .env up -d --build

# 进容器用的逃生口
fobe-server admin unblock <ip|all> | reset-password | list-sessions --revoke | kill-switch on|off
```

前端没有单测与 lint；类型检查靠 `npm run build` 里的 `tsc`。改前端后请跑一次。

## 不要破坏的不变量

1. **探针永不监听管理端口**，只主动外连 `/ws/agent`。fobe 内不含任何反代/证书逻辑——TLS 是使用者自备 nginx 的事（design §3）。
2. **密钥**一律经 `security.Cryptor`（AES-GCM）存取；主密钥 `FOBE_MASTER_KEY` 缺失时**拒绝启动**，不降级成明文。凭据不得进日志；审计里的敏感值要脱敏（anytls 密码已用 `[redacted]`）。
3. **黑名单语义**：`expires_at=0` 只是失败计数、不算封禁，只有 `expires_at > now` 才拦截；回环/私有/可信代理网段永不加黑。`NeverBlacklist` 必须在 `handleLogin` 里接线（曾漏接导致本机自锁）。
4. **XFF 只信 `FOBE_TRUSTED_PROXIES` 内上游传来的最左地址**，范围外忽略 XFF 改用 socket 源地址。
5. `server.public_url` 是安装命令与订阅域名的准绳，**优先于请求 Host**；改了它要同步改 `install.sh` 的渲染预期。
6. **API 只返回结构化数据与 snake_case 错误码**（`writeErr(w, status, code)`），不出中文文案；文案在 `web/src/i18n.tsx`，zh/en 两份都要加。
7. **AI 执行默认放行，但元操作强制确认**（面板密码、主密钥、AI 自身配置、agent 自更新——design §12.3）；每条执行写 `audit_logs`；Kill Switch 必须能冻结全部执行。
8. **agent 不调用外部命令做采集**（OpenWrt 是 busybox），只读 `/proc`、`/sys`、`statfs`。
9. **流量计数器 `delta < 0` 判为回绕/重启**：不计负数、重设基准，并记 `counter_reset` 事件。
10. **sing-box 变更走三道闸门**：下载+sha256 → `check` → 启动 → 30s 观察（进程存活 + 端口可连 + TLS 握手）→ 失败回滚 `.prev`。版本必须显式指定，不追 latest。
11. 前端**全部请求走 `web/src/api.ts`**，静态资源自托管，不引 CDN。

## 代码约定

**Go**

- 每个包开头一段 package comment 说明职责并引用 `design.md §N`。
- 注释解释取舍与陷阱，不复述代码。
- 错误用 `fmt.Errorf("...: %w", err)` 包装，判断用 `errors.Is/As`。
- handler 签名统一 `func (s *Server) handleX(w http.ResponseWriter, r *http.Request)`，会话内接口用 `s.requireSession(...)` 包装。
- 需要新增 wire 类型时，只加在 `internal/protocol`；server 与 agent 都不另立定义。
- 数据库：表/列加在 `store/schema.sql`；给老库加列走 `migrateAdditive`（必须幂等）。连接是单写者（`SetMaxOpenConns(1)`），别引入并发写路径。
- 新设置项用 `域.键` 命名（如 `server.public_url`、`ai.kill_switch`），经 `SetSetting`/`GetSetting`（敏感的用加密版本）。
- 日志用 `log/slog`。
- 版本注入：agent 是 `-X github.com/fobe-panel/fobe/internal/agent.Version=$VERSION`，server 是 `-X main.version=$VERSION`——别弄混。

**前端**

- `pages/` 一页一文件，`components/` 放复用件，路由在 `App.tsx`。
- 文案全部经 `useI18n()` 的 `t('key')`；后端错误码的映射用 `err_<code>` 约定。
- 颜色/间距走 `styles.css` 的 CSS 变量 + `data-theme`，不要写死颜色。
- **xterm 终端**（`components/Terminal.tsx`）：严禁在 `onResize` 回调里调 `fit()`（fit 会再触发 resize，同步死循环冻死渲染进程）；fit 只由 rAF 防抖的 `ResizeObserver` 触发；**padding/border 只能挂在外层 `.terminal-shell`，`.terminal-host` 是纯 flex 尺寸的空盒子、`.xterm` 用 `position:absolute` 填满它**——FitAddon 量的是宿主元素的 border-box 高度（且只减 xterm 元素自身的 padding），宿主一旦被终端内容撑高，每次 fit 都会把自己的 24px padding + 2px border 重新折算成行数（> 一行 15px），现象是终端每帧长高一行、无限拉长；`fontFamily` 不能用 CSS 变量（canvas 不解析）；`main.tsx` 刻意不包 `StrictMode`（双挂载会交错 WS open/close 帧顶掉活 PTY 会话），别加回来。

## 变更流程

- 行为变了就同步文档：`design.md` 是规格，`README.md` 是使用者操作面。design.md 里已有的修订先例是在条目后标注日期（见 §19.11），照此办理。
- 加/改接口：`internal/server/httpapi/server.go` 注册路由 → `web/src/types.ts` 与 `web/src/api.ts` 同步类型 → 页面接线。
- 提交前至少过一遍：`gofmt -l internal cmd`（空）、`go vet ./...`、`go test ./...`；动了前端再跑 `cd web && npm run build`。

## 已知陷阱

- **`web/vite.config.js` 遮蔽 `.ts`**：Vite 配置查找里 `.js` 优先，`tsc` 曾生成它导致改了 `vite.config.ts` 却不生效。`tsconfig.node.json` 必须保持 `"emitDeclarationOnly": true`；改配置没生效就先 `ls web/vite.config.js`。
- **前端热更新、后端不热**：`scripts/dev.sh` 只在启动时 `go build` 一次后端（产物 `data/dl/.dev-server`），Vite 那侧的改动即时生效。所以改了后端接口后会出现"新前端调旧后端"——旧后端不认识这个 `POST`，落到静态兜底处理器上以 `200 text/html` 应答，前端解析 JSON 失败后报"网络错误,无法连接服务器"（`apiErrorMessage` 把任何非 `ApiError` 都算 `network_error`）。现已加固：未匹配的 `/api/*` 回 `404 unknown_endpoint`（design §16 实现修订），2xx 非 JSON 响应报 `bad_response`。**改了后端仍然必须重启 `scripts/dev.sh`。**
- **dev 库里 `server.public_url` 可能指向旧 IP**：它压过请求 Host，`/install.sh` 会渲染出错的 `DEFAULT_SERVER`，表现是"局域网服务器装不上探针"。换网络后先看这个设置。
- **dev 默认监听 `0.0.0.0`**（固定密码、无 TLS，仅限可信内网）；生产的暴露控制仍由 compose 只发布到宿主机回环承担，两者互不影响。
- **`FOBE_ADMIN_PASSWORD` 是密码准绳**：每次启动同步为该值，变更时吊销全部旧会话；不设置则不动现有密码。首次启动无用户且未设置时才打印一次性初始密码。
- **`.gitignore` 覆盖了 `.agents/`、`.zcode/`、`.mem/`**（本地 skills 与工具元数据）以及构建产物 `agent`、`agent.exe`、`server`、`web/dist/`、`data/`。仓库根目录里那些二进制是本地构建残留，不要提交。
- `data/sqlite/fobe.db` 是 WAL 单写者，别用多实例同时挂载。
