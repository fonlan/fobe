# AGENTS.md

fobe 是一个自托管的**单用户探针管理面板**：`server`(Go) + `agent`(Go，装在探针上) + `web`(React/TS/Vite)。

**权威文档**，改动前先读对应章节：

- [`docs/design.md`](docs/design.md) — 规格与已接受的取舍。每条决策都标了它绑定的代价，实现要与它一致；偏离就先改它。
- [`README.md`](README.md) — 面向使用者的精简文档（项目介绍、使用、开发、环境变量）。

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
docker-compose.yml    仓库根：默认拉 GHCR 预构建镜像，--build 落回本地构建；零必填环境变量
deploy/               Dockerfile.server（三段构建）· docker-entrypoint.sh（主密钥兜底）
.github/workflows/    release.yml：v* tag → 测试 → 构建镜像 → 推送 GHCR
scripts/              dev.sh · build.sh · pgo.sh · install.sh.tmpl
```

前端产物**编译进镜像**（`/srv/web`）由 server 直出，不做宿主机挂载。`FOBE_WEB_DIR` 留空时 server 进 API-only 模式，`/` 只提示去访问 Vite。

## 常用命令

```bash
scripts/dev.sh                    # 后端 API-only + Vite（Ctrl+C 一起退；devpass123）
scripts/dev.sh --reset            # 先删 data/dev.db 再起
scripts/dev.sh --no-web           # 只起后端
FOBE_LISTEN=127.0.0.1:8080 scripts/dev.sh   # 只绑本机（默认 0.0.0.0，会打印局域网地址）
FOBE_VERSION=<旧号> scripts/dev.sh          # 钉住版本号 → 降级演练（§5.5）
FOBE_AGENT_UPDATE_STAGGER=1s scripts/dev.sh # 一两台探针时把 0–5 分钟错峰压成"立即"

go test ./...                     # 全部测试
go test ./internal/server/httpapi/ -run TestFullAgentPath   # 端到端主链路
gofmt -l internal cmd             # 必须无输出
go vet ./...

cd web && npm run build           # tsc（类型检查）+ vite build
scripts/build.sh [outdir]         # 本地/CI 三件套：server、linux/amd64 agent+manifest、web
scripts/pgo.sh [--url http://127.0.0.1:6060 | --file cpu.pprof]   # 采 CPU 剖面 → cmd/server/default.pgo
docker compose up -d              # 拉预构建镜像（ghcr.io/fonlan/fobe），零必填环境变量
docker compose up -d --build      # 本机构建（VERSION 占位 compose 会在镜像内换成内容寻址裸哈希，§5.5 跟随真实生效）

# 进容器用的逃生口
docker compose exec server fobe-server admin unblock <ip|all> | reset-password | list-sessions --revoke | kill-switch on|off
```

前端没有单测与 lint；类型检查靠 `npm run build` 里的 `tsc`。改前端后请跑一次。

## 不要破坏的不变量

1. **探针永不监听管理端口**，只主动外连 `/ws/agent`。fobe 内不含任何反代/证书逻辑——TLS 是使用者自备 nginx 的事（design §3）。
2. **密钥**一律经 `security.Cryptor`（AES-GCM）存取；主密钥 `FOBE_MASTER_KEY` 缺失时**拒绝启动**，不降级成明文（镜像入口脚本只是"替用户填上"——生成随机密钥落盘 `/data/.master_key` 并复用，见 design §4.4 修订；改入口脚本时不得引入明文回退）。凭据不得进日志；审计里的敏感值要脱敏（anytls 密码已用 `[redacted]`）。
3. **黑名单语义**：`expires_at=0` 只是失败计数、不算封禁，只有 `expires_at > now` 才拦截；回环/私有/可信代理网段永不加黑。`NeverBlacklist` 必须在 `handleLogin` 里接线（曾漏接导致本机自锁）。
4. **XFF 只信 `FOBE_TRUSTED_PROXIES` 内上游传来的最左地址**，范围外忽略 XFF 改用 socket 源地址。
5. `server.public_url` 是安装命令与订阅域名的准绳，**优先于请求 Host**；改了它要同步改 `install.sh` 的渲染预期。
6. **API 只返回结构化数据与 snake_case 错误码**（`writeErr(w, status, code)`），不出中文文案；文案在 `web/src/i18n.tsx`，zh/en 两份都要加。
7. **AI 执行默认放行，但元操作强制确认**（面板密码、主密钥、AI 自身配置、**由 AI 发起的** agent 自更新——design §12.3）；每条执行写 `audit_logs`；Kill Switch 必须能冻结全部执行，**包括 agent 自动更新**（on 时服务端不下发 target）。§5.5 的**系统自动跟随**不属于元操作（服务端版本变更触发，AI 侧只有只读查询）。
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
- 数据库：表/列加在 `store/schema.sql`；给老库加列走 `migrateAdditive`（必须幂等）。改 schema 必须同时 bump `store.SchemaVersion`——它驱动迁移前自动快照与降级告警；降级安全的前提是 schema 永远只做增量、所有查询显式列名（design §6 兼容策略）。连接是单写者（`SetMaxOpenConns(1)`），别引入并发写路径。
- 新设置项用 `域.键` 命名（如 `server.public_url`、`ai.kill_switch`），经 `SetSetting`/`GetSetting`（敏感的用加密版本）。
- 日志用 `log/slog`。
- 版本注入：agent 是 `-X github.com/fonlan/fobe/internal/agent.Version=$VERSION`，server 是 `-X main.version=$VERSION`——别弄混。
- **两端编译口径不同且是有意的**（design §17）：agent 求体积（`-s -w -trimpath` + 空 `-buildid=`，构建链里有 upx 就再压一道），server 求速度（**保留 DWARF** + PGO + `FOBE_SERVER_GOAMD64`，默认 v1）。三处构建入口（`deploy/Dockerfile.server`、`scripts/dev.sh`、`scripts/build.sh`）的 flag 要一起改，其中两个硬约束：**打包必须发生在写 sha256 之前**（否则一个版本号对应两份字节，探针校验必失败）、**内容寻址的占位构建也要走同一步打包**（否则 upx 版本变了版本号不变）；**agent 的 `GOAMD64` 永远 v1**（探针 CPU 未知，SIGILL 就是掉线）。
- **改模块名必须同步 5 处 ldflags**（`deploy/Dockerfile.server` ×2、`scripts/dev.sh` ×2、`scripts/build.sh` ×1）——它们把 module path 当字面量抄了一遍。`-X` 指向不存在的符号时 **`go build` 不报错、静默忽略**，`Version` 留在 `dev`，而 §5.5 的发布门槛 fail-closed ⇒ **探针自更新被静默停用**，编译/测试/CI 全绿。验证别用 `strings <binary> | grep <版本号>`：`-ldflags` 原文会被记进 buildinfo，必然命中，是假阳性；要真跑出来——临时 `main` 打印 `agent.Version`、带 `-X` 构建后运行，看到哨兵号才算绑上。

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
- **dev 默认监听 `0.0.0.0`**（固定密码、无 TLS，仅限可信内网）；生产的暴露面 = compose 的 `8080:8080` 全接口明文映射，要收敛改回 `127.0.0.1:8080:8080` 或用防火墙限制来源，两者互不影响。只想绑本机用 `FOBE_LISTEN=127.0.0.1:8080 scripts/dev.sh`。
- **局域网探针集体掉线 → 先查监听地址，再查 `server.public_url`**：dev.sh 曾一度硬绑 `127.0.0.1`，探针装的是局域网地址（`server.public_url`），于是全部连不上、面板数据看起来「不会自动更新」（页面上其实是离线状态）。判断顺序：`lsof -nP -iTCP:8080 -sTCP:LISTEN` 看是不是 `*:8080`，再看 `server.public_url` 是否等于本机当前局域网 IP（`ipconfig getifaddr en0`）。改完 dev.sh 必须**重启**它才生效。
- **`FOBE_ADMIN_PASSWORD` 是密码准绳**：每次启动同步为该值，变更时吊销全部旧会话；不设置则不动现有密码。首次启动无用户且未设置时才打印一次性初始密码。
- **`.gitignore` 覆盖了 `.agents/`、`.zcode/`、`.mem/`**（本地 skills 与工具元数据）以及构建产物 `agent`、`agent.exe`、`server`、`web/dist/`、`data/`。仓库根目录里那些二进制是本地构建残留，不要提交。
- **挂载卷遮蔽镜像内置的 agent 产物**：compose 把 `./data` 挂为 `/data`，`FOBE_DL_DIR=/data/dl` 在卷内——镜像里 COPY 进卷的东西运行时读不到，所以 agent 产物放在卷外的 `/srv/agent-seed`，服务端启动时**复制进 DL 卷**（不做这一步，"探针跟随服务端" design §5.5 会静默失效）；DL 目录落到容器可写层时挂载自检会 WARN 并置 `singbox.dl_mount_ok=false`（判据是覆盖挂载不能只是 `/`，见 `singboxcache/mount.go`）。排查"节点一直没跟上"先看 `data/dl/agent/` 里有没有该版本目录和 `.sha256`。
- **`data/dl/agent/latest` 是真实目录 → "重装"会把你装回旧二进制**：`pointLatest` 只重指自己建的符号链接布局（`ownedLatest` 一看到真实文件就放弃），而 `/install.sh` 与面板「重装命令」都固定取 `dl/agent/latest/linux-amd64`。本地手工 staged 的 `latest/` 因此会让重装永远装那份旧产物（可能就是没有自更新代码的那版），现象是"重装了还是不支持跟随"。删掉该目录，下次启动让 server 重建符号链接布局。
- **`scripts/dev.sh` 自己产 agent 产物并注入内容寻址版本号 `dev-<哈希>`**（§5.5）：版本号 = 占位构建产物的 sha256 前 12 位，所以只随 **agent 编出来的二进制**变——改后端代码重启不换号，**只改注释/格式也不换号**（二进制相同；已验证：改日志字符串换号 `…afdfc60` → `…4286310`，还原后回到原号）；`FOBE_VERSION=<旧号>` 钉住版本号做降级演练；`FOBE_AGENT_UPDATE_STAGGER=1s` 把 0–5 分钟错峰压成"立即"。所以改了 `internal/agent` / `internal/protocol` 的**语义**后重启 dev.sh，局域网探针会**真的自更新一次**（自己 exit、systemd 拉起）——看到探针短暂掉线是预期，不是故障。
- **重装命令 ≠ 生效**：`install.sh` 曾用 `systemctl enable --now` / `/etc/init.d/… start` 收尾，**对已经在跑的 agent 是 no-op** —— 重装只换磁盘上的文件，进程仍在跑被替换掉的旧 inode，于是"装好了却永远报旧版本"（`strings $(command -v fobe-agent)` 有新代码，`systemctl status` 的 `Active since` 却远早于这次安装）。已改成 `enable` + `restart`（procd 同理），fallback 分支先 `pkill -f "^$BIN_DIR/fobe-agent"` 再起。判断这类问题永远先对时间线：磁盘二进制 vs `/proc/<MainPID>/exe`。
- **`fileExists` 语义是"存在且不是目录"**：它被拿去检测 `/run/systemd/system`（systemd 自己建的**目录**）⇒ 每台 systemd 机器都判成 fallback。症状看起来毫不相干，别按表面去查：面板报"no service manager to restart the agent / 不支持自更新"（caps `systemd=false,fallback=true` ⇒ 自更新被禁）、sing-box 走 agent 自己 spawn 的兜底分支而不是 systemd 单元（今 `one-sing.service`）、面板 sing-box 启停回 `no service manager detected`。存在性检测用 `pathExists()`，`fileExists()` 只用于 unit/二进制/配置；`Detect()` 已拆出 `detectAt(root)` 可用假根目录单测。判定探针环境优先看探针**自己报的原因**，别只看 caps。
- **sing-box 产物上限与"安装中"卡住**：`internal/agent/singbox.go` 的 `maxDownloadBytes` 曾是 64 MiB，而 1.14 起的官方 linux-amd64-musl 二进制已近 90 MiB——注意 `/dl/singbox/<version>/linux-amd64` 直出的是**解包后的单文件二进制**，不是 33 MB 的 tar.gz。现象是"老老实实下载一整遍 → `artifact exceeds 67108864 bytes` → 回滚"，面板上只剩"安装中"（真原因只在 `singbox_down` / `singbox_rollback` 告警里；`data/dl/singbox/<version>/manifest.json` 的 `size` 能直接对上号）。同源的空间闸门是「64 MiB + 产物」（替换已有二进制时再 +1 份给 `.prev`），别退回平坦阈值。面板侧的对症是节点编辑页只读一次快照就永不刷新 ⇒ `installing` 一直挂着、失败原因又被周期性 `reportOnly` 报成空串（两端已修，见 design §9.2 实现修订 2026-09-16）。
- **改这个上限只过了第一关**：同一台探针紧接着倒在闸门①——`internal/server/singbox/config.go` 生成的 dns 段用了 **1.14.0 已移除**的地址式格式（`{"address":"local"}`），现行版本一律 `config check: … legacy DNS server formats … removed in sing-box 1.14.0`。现在写 `{"type":"local","tag":"local-dns"}`（type 形式 1.12 起才有，模板兼容下限 = 1.12）。**排查 sing-box 安装的顺手工具**：产物是静态 linux/amd64，`docker run --rm --entrypoint /sb/linux-amd64 -v <dl>/singbox/<ver>:/sb:ro -v <cfgdir>:/cfg:ro ghcr.io/fonlan/fobe:latest check -c /cfg/x.json` 就能在本地复现闸门①（记得 `certificate_path` 指向真实存在的证书，否则 `check` 会报读不到证书而掩盖 DNS 的问题）。
- **改模板 ≠ 存量节点会跟上**：每台节点的 `config.json` 只生成一次、缓存在 `settings` 的 `singbox_config:<node_id>` 里，agent 只比 hash ⇒ **改了生成器，旧配置会一直下发**（唯一出路曾是操作员再点一次"安装"）。现在服务端启动会 `SyncSingboxConfigs()` 重建不匹配的（design §9.1）。同一类陷阱：`agent` 的"版本/配置都对、只是没在跑"那条路径**曾经跳过闸门①**，于是一份 sing-box 拒绝加载的配置能永远留在盘上、每轮只报闸门③的"process exited"——真原因一次都不出现（design §9.2）。两者都已修，改动 singbox 生命周期时别把它们改回去。
- **sing-box 的单元名是 one-sing.sh 的 `one-sing.service`（2026-09-16 改）**：改名是为了让面板与那个脚本作用于同一个单元，而不是各自 disable 对方。**改回去就退化成两套 supervisor 抢一个进程**。带上迁移是硬要求：改名前的 `fobe-singbox.service` 仍 enable 且在跑，agent 启动时 `RetireLegacySingboxUnit()` 负责停+disable+删（procd 侧只删含 one-sing 路径的自己那份，发行版的 `/etc/init.d/sing-box` 不碰）；`AdoptForeignSingboxService` 已删——它会 disable 掉 fobe 自己刚建的单元。日志来源同步：`journalctl -u one-sing`。
- **one-sing 装的 sing-box 必须"能看见"，看见 ≠ 已安装（2026-09-17，17i 修订）**：`State.Singbox` 只在面板声明过期望版本后才上报，所以纯 one-sing.sh 的探针在管理态上是 absent。现在 `State.SingboxLocal` 每轮上报（不受期望状态限制），服务端解析并存加密快照；17d 起无「接管」动作、17g 起监听全部平等（**不再写 `node_singbox.port`——发现型节点该列恒 0**）、17i 起面板判定与订阅渲染同源读上报文件：编辑页状态 Tile 显示「已识别（本机实例）」，§10.2 中转候选/自动落账读 anytls 入站集合而不是管理态端口。**改判定时先问一句它读的是哪本账**：`node_singbox.port`/`cert_pem` 是管理态，发现型节点两列皆空——中转入口曾因此在选择器里凭空消失（渲染端读文件、判定读管理列，两边打架）。**进配置的字段必须过白名单**（`internal/server/singbox/config.go` 的 `allowedInboundKeys`/`allowedTLSKeys`/`allowedRealityKeys`）：闸门①是 live 探针上唯一的防线，真机上踩过两次服务级事故——用户级 anytls 密码被写到 vless 入站的**顶层** `password`、以及把**客户端**的 `reality.public_key` 写进服务端配置，两次都是"停服→写盘→check 失败→回滚"。新增任何进 `config.json` 的字段，先确认它是 sing-box 认的键并跑 `TestGeneratedInboundCarriesNoUnknownKeys`。
- **sing-box 配置的真相在探针盘上，不在服务端（2026-09-17b）**：`/etc/one-sing/config.json` 是唯一真相，面板只读它、编辑走"读-合并-写 + `reported_hash` 校验"，服务端**不再按模板重写**（`SyncSingboxConfigs` 跳过 `settings.singbox_edited:<id>` 标记过的节点）。三处真机坑：① agent 侧无版本的期望帧**不是** unmanaged（`Version=="" && ConfigJSON==""` 才算），否则面板对脚本节点的编辑被静默丢弃；② 无版本时**不得**触发安装（否则去下 `/dl/singbox//linux-amd64` 得 404 → 整次 apply 回滚，日志只有 `download : get sha256: 404 Not Found`）；③ `buildDesiredState`/`pushDesired` 在只有 config 时也要发帧（`ConfigHash != ""` 算有期望态）。合并时**缺省凭据必须继承文件里的值**（面板回显脱敏字段，清空密码 = 打断客户端）。证书是 `certificate_path` 指向的**内容**由 agent 上报（按端口索引），否则手工加的 anytls 入站永远进不了订阅（本项目不做 `insecure=true`）。
- **anytls 全局密码是"服务端生成、面板不回显"的凭据（2026-09-16）**：`settings.anytls_password` 缺省时由 `ensureAnytlsPassword`（`internal/server/httpapi/anytls_password.go`）在**使用时机**现生成 16 位 `[A-Za-z0-9]`——安装/改端口、渲染订阅、`SyncSingboxConfigs` 发现已有被管理节点；AES-GCM 落库 + 一条 `anytls_password_generated` 审计（值 `[redacted]`）。设置页的输入卡片已删，轮换入口是 AI 工具 `set_anytls_password` 与 `PUT /api/settings`，**空值 = 重新生成**且会立刻重推所有已启用 sing-box 的节点。三处别改回去：①**密文存在但解不开**（换过主密钥、恢复了旧库）**绝不能当"未配置"**去生成新密码——那是静默轮换，会把还在用旧密码的客户端全部打断；此时安装/同步直接报错、原值不动。②生成的读-改-写必须持 `Server.anytlsMu`，去掉锁后两个并发安装会烤出两份不同密码，节点配置与订阅立刻对不上。③字符集留在 `[A-Za-z0-9]`：同一个串要原样进 JSON 字段、第三方客户端的 URI auth 位与 Clash YAML 标量，只有字母数字在三种上下文里都不用转义。
- **验证 PGO 别 grep 编译器输出**：`go build -gcflags=all=-m | grep -i pgo` 会命中 `prepGoExitFrame` 这类名字，全是假阳性（实测 11 条命中、0 条是真的）。要看 `go version -m <binary> | grep -- -pgo`——它会打印实际吃进去的剖面路径（`-pgo=default.pgo` / 绝对路径）。同理，**UPX 压过的 agent 读不出 buildinfo**：`go version -m` 直接报 `not a Go executable`，`strings | grep` 也不再命中——验证版本只能真跑（无参启动会打印 `fobe-agent <version>`）。
- **`FOBE_PPROF` 只接受回环地址**（design §17）：非回环直接记 error 并**不监听**——heap profile 就是内存快照，而这个进程内存里有主密钥、AI key、bot token。容器里采集不需要发布端口：`docker compose exec server wget -qO- 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30' > cpu.pprof`（`wget` 是镜像自带的 busybox 版）。空闲面板的 2 秒 CPU 剖面只有几百字节（gzip），别把它当成"接口坏了"。
- **延迟测量目标曾只随 `hello_ack` 下发（2026-09-17k 修）**：面板改完 `latency_target_ids` 不推任何帧，稳定长连接的探针永远在测握手时的那份列表——现象是"给已有节点加了延迟测量，详情页永远不出新折线"。现在编辑/删除后即发带 `targets` 的 `latency_config`（`hub.PushLatencyTargets`；`targets:null`＝不变、`[]`＝清空——该字段**不能加 `omitempty`**，否则清空到不了 agent）。**修完还见"没折线"先查目标是不是 100% 丢包**（`SELECT SUM(icmp_ms>=0), COUNT(*) FROM latency_samples …`）：防火墙拦 ICMP 的目标永远产不出 RTT 点，前端对没有有效 RTT 点的目标不画线——现在详情页会显式标注「全部丢包 · 目标无回应」，别再当成下发链路坏了。新增"服务端可改、agent 要跟着变"的状态先问一句：`hello_ack` 之外有没有第二条即时下发的路？**改已有目标走 `PATCH /api/latency-targets/{id}`（2026-09-18）**：端点（kind/host/port）一变，同一事务里清掉该 `target_id` 的 `latency_samples`——样本表只按 target_id 键、不存端点，留着就把两台主机的测量画成一条折线（"这条线测的是谁"事后无法分辨）；只改名字保留历史。改完必须立刻向仍选中它的探针推新定义（host/kind/port 才是探针真正去拨的东西），只改库不动连接 = 长连接探针继续测旧端点。icmp 的端口在写库前一律归零（创建/编辑共用 `normalizeLatencyTarget`），且 **icmp↔icmp 的端口差异不算端点变化**——存量 icmp 行存的是旧表单默认值 443，判成端点变更就会"改个名字清掉全部历史"。
- `data/fobe.db` 是 WAL 单写者，别用多实例同时挂载。
