# fobe — 探针面板设计方案 v1

> 单用户、单端口、Docker Compose 自托管的多探针管理面板：集成 sing-box 服务端生命周期、流量与硬件监控、延迟测量、Web 终端、AI 助手、订阅输出。
>
> **接入层不在本仓库范围内**：反向代理、TLS 终止与证书签发/续期由你自备的 nginx 完成，fobe 只监听一个普通 HTTP 端口。nginx 该怎么配见 `README.md`。
>
> 本文档由 `/grill-me` 四轮盘问收敛而成，**每条决定后面都标了它绑定的代价**。标 ⚠ 的是已接受但仍需你知道的风险。

---

## 0. 关键决策速览

| # | 决策 | 选择 | 绑定的代价 |
|---|---|---|---|
| 1 | 产品闭环 | 面板输出订阅 `/sub/<token>`，sing-box + Clash 双格式 | 需要订阅渲染 + token 生命周期 |
| 2 | 网络通道 | 外部 nginx 单端口接入，全路径转单一上游；agent 走 WSS | 你必须自备 nginx + 真证书；探针必须能解析面板域名 |
| 3 | 流量口径 | 整机网卡 `/proc/net/dev`，面板选定网卡（默认默认路由出口） | 配额含系统更新与其他服务流量；需计数器回绕/重启检测 |
| 4 | 使用率语义 | 分母 = 周期配额；4 模式决定取哪个方向 | 每节点必须落库：配额值、锚点、时区 |
| 5 | Web 终端 | 浏览器 → server → agent 本地 PTY（不依赖 sshd、不存凭据）（2026-09-15 修订：原 SSH + 凭据托管方案废弃） | agent 需要 PTY 权限；协议保留 `mode` 字段仅为滚动升级兼容 |
| 6 | 平台 | x86 Linux + x86 OpenWrt（v1 硬需求） | procd/init.d、musl 静态、flash 写最小化 |
| 7 | 到期/超量 | 只提醒，不自动停服 | 需要告警通道，且没有自动止损 |
| 8 | AI 模型 | OpenAI 兼容 API，自填 base_url + key，服务端加密存储 | key 在服务端；服务端需能出网 |
| 9 | AI 执行权 ⚠ | **默认放行**：AI 自评有风险才弹确认 | 见 §12，间接提示注入可静默拿到探针 root |
| 10 | 构建形态 | server / agent 分开编译；前端**不嵌入 Go 二进制**但**编进生产镜像**；agent 产物进服务端镜像 | 共享 protocol 模块 + 产物分发链路；镜像构建多一个前端 stage；**挂载卷会遮蔽镜像内的 agent 产物，服务端启动时得把它补进卷**（§5.5） |
| 11 | 数据库 | SQLite（WAL），文件挂载在容器外 | 单写者；指标靠保留期控盘 |
| 12 | 订阅模型 | 单用户 + 多订阅，每订阅独立节点集与模板文件 | 模板管理 + 双格式引擎 |
| 13 | 探针凭据 | 一个入站 + 一个**全局共享** anytls 密码 | 无法按订阅吊销代理访问，只能全局轮换 |
| 14 | 重置锚点 | 循环锚点，填一次自动滚动；缺省继承缴费锚点 | 需处理月末边界与时区 |
| 15 | 缴费周期 | 周期类型 + 下次到期日，手动改；**无续费按钮、无历史** | 查不到"上期什么时候交的" |
| 16 | 指标保留 | 明细只存 7 天；另存永久「按天流量」表 | 7 天以外的曲线不可得（月曲线靠日表） |
| 17 | 延迟测量 | 探针**主动**测面板配置的目标；5s 本地测、60s 批量上报；ICMP + TCP 握手两种 | 拿不到"用户→探针"的真实延迟 |
| 18 | 登录加固 | 失败 3 次拉黑 IP（持久化）+ CLI 解封；**不做 2FA** | 黑名单依赖 XFF 信任链；无第二因子 |
| 19 | 告警 | Telegram Bot + 通用 Webhook | 没装 Telegram 就收不到 |
| 20 | 接入层 | **不在 fobe 内实现**：外部 nginx 提供 TLS 与反代，证书自备自管 | fobe 不碰证书；你必须让 nginx 传对 XFF 与 Upgrade 头（见 §3 与 README） |
| 21 | agent 自更新 | **一直跟随服务端**：版本不一致就切（含降级），agent 自发、免确认；Kill Switch on 时冻结（§5.5） | 服务端版本号成为对外契约；不保留 `.prev` ⇒ **没有本地回滚**；存量探针必须人工重装一次才进入自动跟随 |

---

## 1. 范围

**v1 做**：单用户面板；探针注册与心跳；硬件/网络指标；流量配额与四种口径；缴费与重置周期；sing-box 安装/更新/回滚/启停；anytls 自签证书服务端；订阅渲染（sing-box / Clash 模板）；Web 终端；延迟测量；IP 与国旗；Telegram/Webhook 告警；中英双语；明暗主题；AI 助手；**外部接入文档（README 给出可直接复制的 nginx 配置）**。

**v1 不做**：**反向代理与证书签发/续期（由你自备的外部 nginx 负责，fobe 内不含任何反代组件）**；多用户与权限体系；在线支付；到期自动停服；TOTP；非 x86 架构；除 anytls 以外的协议；探针集群编排；Prometheus 导出。

---

## 2. 系统架构

```
        外部 nginx（不在本仓库，你自备；配置见 README.md）
                 ┌──────────────── 公网 :443（唯一暴露端口）────────────────┐
  浏览器 ────────►│ TLS 终止 + 证书签发续期（你自己管）                       │
                 │ location / → 单一上游 http://127.0.0.1:8080（原样透传）   │
                 └───────────────────────────────┬───────────────────────────┘
                                                 │
                                        ┌────────▼─────────┐
                                        │  server (Go)     │  SQLite(WAL) 挂载在 /data
                                        │  ├ /api  /ws/*   │  AI key 等敏感设置 AES-GCM 加密
                                        │  ├ /sub /install │
                                        │  ├ /dl → /srv/dl │  agent 与 sing-box 产物直出
                                        │  ├ /   → /srv/web│  前端 dist（镜像内直出）
                                        │  + scheduler     │
                                        │  + CLI admin     │
                                        └────────┬─────────┘
                                                 │ WSS（探针主动外连，无需开放入站）
        ┌────────────────────────────────────────┼───────────────────────────────────┐
        │                                        │                                   │
   ┌────▼─────┐                            ┌─────▼────┐                        ┌─────▼────┐
   │ probe A  │                            │ probe B  │                        │ probe C  │
   │ fobe-agent (root)                      │ ...      │                        │ x86 OpenWrt │
   │ ├ 指标采集 /proc                        └──────────┘                        └──────────┘
   │ ├ sing-box 生命周期 + 健康闸门
   │ ├ 自签证书生成（私钥不出探针）
   │ ├ 延迟测量（ICMP / TCP）
   │ └ SSH 客户端 + 本地 PTY
   └──────────┘
```

要点：

- **探针永不监听控制端口**，只主动外连面板 → 不需要在探针上开管理端口，NAT 后面也能用。
- **单端口**：所有控制面流量都是普通 HTTPS/WSS 的路径，外部 nginx 只需一个 `location /` 透传，不需要 stream 模块做协议嗅探。
- **server 只绑定回环端口**（`127.0.0.1:8080`），对外唯一入口是你自备的 nginx；fobe 仓库内不含任何反代组件，也不生成/管理证书。
- 前端编译产物**在镜像构建阶段打进镜像**（`/srv/web`）由 **server 直出**（不嵌入 Go 二进制，避免体积膨胀；也不做宿主机挂载）。

---

## 3. 接入层（外部 nginx，不在本仓库范围内）

fobe **不实现**反向代理，也**不做**证书签发与续期。它只做一件事：监听一个普通 HTTP 端口（默认 `8080`），把上面列出的全部路径都当作自己的职责——静态前端、订阅、产物下载、WebSocket 全由 server 自己处理。

**外部 nginx 必须满足的四件事**（完整可复制配置与逐条解释见 `README.md`）：

1. **单端口承接**：一个 `server` 块（同一域名、同一 `443`）收下全部流量，`location /` 原样透传到 `http://127.0.0.1:8080`。路径不需要任何分发规则。
2. **WebSocket 升级**：`proxy_http_version 1.1` + `Upgrade`/`Connection` 头 + `proxy_read_timeout 3600s`。少任何一条，探针长连接或浏览器终端就连不上（表现是登录正常、节点永远离线）。
3. **真实 IP 传递**：`Host` / `X-Real-IP` / `X-Forwarded-For` / `X-Forwarded-Proto`。传错的话，黑名单会封错对象，安装命令与订阅里生成的域名也会错。
4. **体量与缓存**：`client_max_body_size` 放宽（模板上传），`/sub/` 关闭缓存（否则客户端换了订阅还是旧节点）。

**信任链**：server 只信任 `FOBE_TRUSTED_PROXIES`（默认 `127.0.0.1/32,::1/128,172.16.0.0/12`）范围的上游所传的 `X-Forwarded-For`，并且只取最左侧地址；上游不在白名单内时**忽略 XFF，改用 socket 源地址**。若你日后在前面再叠一层 CDN/反代（Cloudflare 等），必须把它的回源网段加进来。

**替代做法（可选）**：若你想让 nginx 直接吐静态文件，需自己产出前端文件（`cd web && npm run build`，或 `docker cp <容器>:/srv/web ./dist`），再把 `location /` 指向该目录，但**必须保留** `/api`、`/ws/`、`/sub/`、`/install.sh`、`/dl/` 转发到 server。默认推荐前者：配置最短，且不存在"静态文件与 API 走不同域名导致 Cookie/WS 跨域"的坑。

---

## 4. 认证与安全模型

### 4.1 面板

- 单用户，密码 `argon2id` 存库。
- 会话：HttpOnly + Secure + SameSite=Lax Cookie，服务端存 session 表，可一键吊销全部。
- **不做 2FA**（你的选择）。补偿：登录失败黑名单（见 4.3）+ 强制 HTTPS + 绑域名。
- CLI 逃生口（进容器执行，不依赖 Web）：
  - `fobe-server admin unblock <ip|all>` 解封
  - `fobe-server admin reset-password`
  - `fobe-server admin list-sessions --revoke`
  - `fobe-server admin kill-switch on|off`（冻结所有 AI 执行）

### 4.2 探针凭证

- **服务端访问网址**：设置项 `server.public_url`，由用户填写探针可达的完整 `http(s)` 地址；添加节点生成的安装命令和 `/install.sh` 默认值优先使用它，未配置时才回退当前请求的 `Host`。
- **注册 token**：面板添加节点时生成，**节点名称必填**、单次有效、TTL 30 分钟；token 绑定节点名称与可选备注，探针注册后名称写入 `nodes.name`。
- 安装脚本用注册 token 调 `POST /api/agent/register`，服务端返回 `node_id` + `node_secret`（只此一次明文下发，服务端只存 hash）。
- 之后 agent 用 `node_id + node_secret` 建立 WSS，服务端按 `machine_id` 去重：
  - 已有同一 `machine_id` 且凭证校验通过 → **复用节点**，更新 IP/版本，不产生垃圾节点；
  - 已存在但凭证不符 → 拒绝注册，面板显示"疑似重复安装"，由你手动选择接管。
- agent 侧落盘：`/etc/fobe-agent/{config.json,machine-id}`（0600，root）。OpenWrt 上 `/etc` 是 overlay，重启保留。

### 4.3 登录失败黑名单

- 连续 3 次失败 → 拉黑该 IP；持久化到 SQLite（重启不丢）。
- 真实 IP 来源：`FOBE_TRUSTED_PROXIES` 白名单内上游传来的 `X-Forwarded-For` 最左侧地址；上游不在白名单内 → **忽略 XFF，改用 socket 源地址**（防伪造头绕过或嫁祸）。
- **防自锁**（三条硬规则）：
  1. 回环与私有网段（含 Docker 网段、`X-Forwarded-For` 里的内网地址）**永不加黑**；
  2. 上游代理地址与 `FOBE_TRUSTED_PROXIES` 网段永不加入黑名单；
  3. 黑名单表带 `expires_at`，CLI 可无条件清空。
- 面板提供"当前封禁列表 + 一键解封"页面，不逼你非进容器不可（CLI 是兜底）。

### 4.4 密钥托管

- 主密钥 `FOBE_MASTER_KEY`（32 字节，环境变量 / Docker secret）。用它 AES-GCM 加密：AI API Key、Telegram Bot Token、订阅模板中的敏感段。（2026-09-15：不再加密任何 SSH 凭据——Web 终端已改走 agent 本地 PTY，见 §11。）
- 未设置主密钥时，服务端**拒绝启动**并打印生成命令（不静默降级为明文）。
- 所有审计写 `audit_logs`：谁、何时、对哪个节点、什么动作、命令原文、来源 IP。

---

## 5. 探针 agent

### 5.1 形态

- 单个静态二进制（`CGO_ENABLED=0`，`linux/amd64`，musl 兼容）——**OpenWrt 与常规发行版同一份产物**。
- **不调用外部命令做采集**：CPU/内存/磁盘/网络全部读 `/proc`、`/sys`、`statfs`。原因：OpenWrt 是 busybox，字段与工具集都可能缺。
- 需要 root（安装 systemd unit/procd 服务、写 `/etc`、ICMP raw socket）。缺少 `CAP_NET_RAW` 时 ICMP 降级为 TCP-only 并上报能力位。

### 5.2 安装流程

面板"添加节点" → 生成注册 token → 给出一条命令：

```sh
curl -fsSL https://panel.example.com/install.sh | bash -s -- --token <REGTOKEN> --server https://panel.example.com
```

> 命令不带 `sudo`：脚本以当前用户运行，root 直接安装；非 root 且装有 sudo 时自动提权；两者都不满足时明确报错（多数纯净 VPS 的 root 用户没有 sudo）。

`install.sh` 由 server 动态渲染（注入 server URL 与 token），脚本做：

1. 探测架构（v1 只接受 `x86_64`，否则明确报错退出，不做半吊子兼容）；
2. 从 `https://panel.example.com/dl/agent/<version>/linux-amd64` 下载二进制，**校验 sha256**（校验值由脚本内嵌，来自服务端版本清单）；
3. 安装到 `/usr/local/bin/fobe-agent`（OpenWrt 用 `/usr/bin/fobe-agent`）；
4. 写 `/etc/fobe-agent/config.json`；
5. 注册 → 拿到 `node_id` + `node_secret` 落盘；
6. 安装并启动系统服务；
7. 立即上报一次完整信息（IP、系统、CPU 核数、版本）。

### 5.3 服务管理抽象

| 平台 | 检测 | 服务形态 |
|---|---|---|
| 常规 Linux | 存在 `/run/systemd/system` | systemd unit（`fobe-agent.service` / `fobe-singbox.service`） |
| x86 OpenWrt | 存在 `/sbin/procd` 或 `/etc/rc.common` | `/etc/init.d/fobe-agent`、`/etc/init.d/sing-box`（procd，`USE_PROCD=1`） |
| 兜底（容器/WSL） | 两者皆无 | 前台进程 + pidfile + 看门狗 |

`sing-box` 的服务文件由 agent 自己写（面板只下发期望状态），这样才能保证不同平台一致。

### 5.4 OpenWrt 专项

- 二进制与配置统一放 `/etc/one-sing/`（`sing-box` + `config.json` + `cert/`），与 one-sing.sh 同一套路径（实现修订 2026-09-15，见 §9.3）；`/etc` 在 OpenWrt 上同样是 overlay 持久。
- **写放大控制**：指标与日志不落盘（内存环形缓冲），仅上报；错误日志按行数上限写 `/tmp`（tmpfs）。
- 内存：agent 目标常驻 < 30MB；采集周期在低内存设备上可从 60s 放宽（面板可配）。
- 首次运行检测 overlay 剩余空间，低于阈值时拒绝安装 sing-box 并给出提示（避免把路由器写满）。

### 5.5 agent 自更新（跟随服务端）（实现修订 2026-09-15）

> 目标：**探针 agent 一直跟着服务端走**——服务端换了版本，探针就换成与之匹配的那一版，**不看高低**（降级与升级共用同一套逻辑）。本节取代 §9.2 里"agent 自更新走同一条链路"那句：自更新**不复用 sing-box 的三道闸门**（sing-box 有 `check` 与 30s 观察期，agent 没有可比的"先验后启"手段），换来的是**旁路自检 + 原子提交**，且**不保留 `.prev`**。

- **判据只有一个：服务端版本号**。服务端在 `hello_ack` 里下发 `agent_target_version`，agent 拿它和自己的 `internal/agent.Version` 比，不等就切换（含降级）。两端版本由构建时同一个 `$VERSION` 注入（`cmd/server` 的 `main.version` 与 `internal/agent.Version`），所以"服务端版本"就是"该配哪一版 agent"。
- **发布门槛（fail-closed）**：仅当 ① 服务端版本是发布形态（非空、非 `dev`）且 ② `<FOBE_DL_DIR>/agent/<version>/{linux-amd64,linux-amd64.sha256}` 都在时，才下发 target。任一不满足 → 不下发，面板显示停用原因。理由：dev 构建没有"发布"概念，而"下发一个取不到的版本"只会让每台探针反复重试 404。
- **触发 = agent 自发，不需要任何人确认**：agent 每次握手看到不一致就自己动手。这是**系统行为**，不属于 §12.3 的"元操作"清单；**Kill Switch on 时服务端不下发 target**，因此"永远跟随"在这段时间让位给止损闸。
- **错峰由服务端编排**：服务端重启会让全部探针同时重连、同时发现不一致，因此 `hello_ack` 里带 `agent_update_after`（unix 秒），服务端按 `hash(nodeID + target)` 在 0–5 分钟内给出确定性偏移（同一 node+target 稳定，target 变则重排），并把计划时刻写进 `nodes.agent_update_planned_at` 供面板显示"计划中/进行中"。**这不是概率问题**：不错峰就是 N 台同时拉 10MB，而那一刻服务端刚起来。
- **执行链**（agent 侧，收到 target 且已过 `update_after`）：
  1. 下载到**目标二进制所在目录**里的临时文件（同目录才能 `rename`，跨文件系统会 `EXDEV`）；目标路径由 `os.Executable()` 解析（跟随符号链接）——不猜 `BIN_DIR`，systemd 装的是 `/usr/local/bin`、OpenWrt 是 `/usr/bin`；
  2. 与 `linux-amd64.sha256` 比对 sha256，不符即 `terminal` 失败；
  3. 以 `-selfcheck` 跑一次**新二进制自己**：用同一份 config 连服务端，`hello` 带 `selfcheck:true`，hub 只回 `hello_ack` 后立即关闭——**不注册、不落库、不顶掉线上连接**（`hub.go` 的重复注册会 `old.close()`：天真地握手会把正在跑的 agent 踢下线，还会让面板显示一个并没生效的版本号）。自检还必须校验 `hello_ack.agent_target_version`：非空且不等于自己的 `Version` 即失败，挡住"目录名与二进制内版本错位"这类产物错放（对方为空说明服务端此刻已不再下发 target，那是环境变化而不是这个产物的问题，不作为失败）；超时 60s；
  4. 自检通过 → `rename` 覆盖目标二进制 → **主动 `exit`**，由 supervisor（systemd `Restart=always` / procd `respawn`）拉起新版本。**不保留 `.prev`**：不留就没有本地回滚，代价是"自检过但 `-run` 起不来"这种残余情形只能 SSH 重装（§20）；换来的是 OpenWrt overlay 上少 10MB 常驻占用；
  5. 自检或下载失败 → 线上二进制**一动不动**。
- **fallback 不参与**：`install.sh` 的 fallback 分支用 `nohup` 起 agent，没有 supervisor，"重启自己"无处落地；这类节点上报 `self_update=false`，面板只显示需人工处理。
- **失败分类（两本账）**：
  - `terminal`（不再重试，等 target 变更或人工重试）：sha256 不符 / 无法 exec / 自检报版本不符；
  - `transient`（退避 1m → 1h 封顶，不计入熔断）：连不上服务端、`/dl` 404/5xx、同目录剩余空间 < 2×产物、目标不可写。
- **熔断双记账**：agent 本地 `/etc/fobe-agent/update-state.json` 记 `target / attempts / last_error / class`（跨重启有效），**同一 target 连续 3 次 `terminal`** 就停手并上报；服务端 `nodes` 表同样记一份（面板可见 + 人工解锁），target 变化时两边计数清零。本地那份是唯一能在"替换无效、反复重启"时救命的账，服务端那份负责可见性——**只留一边都会在某个场景下失效**。
- **状态与面板**：`nodes` 增 `agent_target_version / agent_update_state / agent_update_attempts / agent_update_error / agent_update_planned_at / agent_update_done_at`（`migrateAdditive`，幂等）；节点页显示 当前版本 / 期望版本 / 计划时刻 / 上次结果与原因；`POST /api/nodes/{id}/agent/retry` 清计数（人工解锁）。每次尝试写 `audit_logs`（`actor=system`）。
- **告警三档**：`terminal` 立即；`transient` 连续 3 次；分发后 15 分钟仍未收敛的节点**汇总一条**（与 §9.5 同一口径与去重窗口）。
- **存量探针只能人工重装一次**：今天已装的 agent 二进制里没有这段代码，服务端下发 target 它也不认识（Go 忽略未知 JSON 字段，它会照常跑）。判定靠**能力位而非版本号猜测**：新 agent 在 `hello.Caps` 里报 `self_update=true`，不报的一律在面板标"需人工重装（不支持自更新）"并给出重装命令。**不许假装它会自动跟上。**
- **产物供给**：镜像里**不能**把 agent 产物放在 `/srv/dl`——compose 把 `../data/dl` 挂到 `/srv/dl`，挂载点在容器启动时就已生效，镜像里那份**根本不可见**（不是"被覆盖"，是读不到）。因此镜像把产物放在卷外的 `/srv/agent-seed/agent/<version>/`，服务端启动时**复制进 DL 卷**（缺什么补什么，已有则不覆盖；`FOBE_AGENT_SEED_DIR` 可改，置空即关闭；与 §9.5 的 sing-box 缓存互不影响）。同一个启动步骤还会把卷里的 `agent/latest` 指向**服务端自己这一版**——否则升级容器后 `/install.sh` 会继续装上一版的 agent（`latest` 是本地脚手架留下的真实目录时不动它）。不这么做，"跟随服务端"在标准 compose 部署里默认就是 404。
- **混版承诺**：同一大版本内双向兼容（新增字段一律可选、未知字段/帧忽略并记日志）。升级/降级过渡期必然是混版，"版本不匹配就拒绝"会把探针直接锁死。`protocol.Version` 保持 `1`。
- **开关**：`settings.agent.auto_update`（默认开，面板可关）。关掉或 Kill Switch on = 不下发 target。AI 侧**不新增任何工具**（§12.2）：状态随既有节点查询返回，触发/冻结/解锁/重试都只能由人在面板操作。
- **面板与接口**（全部走会话鉴权，`GET /api/agent/update` 集群状态、`POST /api/nodes/{id}/agent/retry` 人工解锁、`POST /api/nodes/{id}/agent/reinstall-command` 生成一次重装命令）。节点视图新增 `agent_target_version / agent_update_state / agent_update_attempts / agent_update_error / agent_update_planned_at / agent_update_done_at` 与能力位 `agent_self_update`（外加 `agent_caps_seen`：没握过手的节点不算"不支持"，否则新建节点会被误标重装）；`agent_update_state` 取 `planned|downloading|verifying|committed|failed|transient|suppressed|unsupported`。
- **"重试"要能推动在线的探针**：`hello_ack` 之外的载体是普通 `desired` 帧（`DesiredState` 里带上同样两个字段），因此解锁不必等下一次握手；服务端由同一个函数同时填两个载体，不可能填出不一致的两份。
- **两个必须失败关闭的点**（都在实现里显式处理）：① 临时文件必须落在**目标二进制同目录**再 `rename`（跨文件系统 `EXDEV`；`open+truncate` 覆盖正在运行的文件会 `ETXTBSY`）；② 同目录可用空间 < 2×产物即判 `transient` 拒绝，宁可不动也不能写半个二进制。

---

## 6. 数据模型（SQLite，WAL）

| 表 | 关键字段 | 说明 |
|---|---|---|
| `users` | id, password_hash | 单行 |
| `sessions` | id, created_at, last_seen, ua, ip, revoked | 可批量吊销 |
| `ip_blacklist` | ip, reason, fail_count, created_at, expires_at | 持久化 |
| `settings` | key, value, encrypted | 全局 anytls 密码、AI 配置、Telegram、保留期等 |
| `reg_tokens` | token_hash, note, expires_at, used_at | 单次 |
| `nodes` | id, name, machine_id, node_secret_hash, status, last_seen, agent_version, os, arch, kernel, cpu_cores, primary_ip, country_code, tz；**自更新（§5.5）**：agent_target_version, agent_update_state, agent_update_attempts, agent_update_error, agent_update_planned_at, agent_update_done_at | 探针主表 |
| `node_ips` | node_id, ip, family, scope, is_primary | 多 IP 全量上报 |
| `node_network` | node_id, iface, mode(in/out/both/max), quota_bytes, cycle_days, anchor_at, tz | 流量口径与配额 |
| `node_billing` | node_id, cycle_type, cycle_days, next_due_at, note | 缴费周期 |
| `traffic_counters` | node_id, iface, direction, last_raw, last_ts, period_start, period_used | 回绕/重启检测 |
| `traffic_daily` | node_id, date, rx_bytes, tx_bytes | **永久**，月曲线与配额靠它 |
| `metrics_samples` | node_id, ts, cpu, mem_used, mem_total, disk_used, disk_total, net_rx_rate, net_tx_rate, uptime | 保留 7 天 |
| `latency_targets` | id, name, kind(icmp/tcp), host, port | 面板配置的目标 |
| `node_latency_targets` | node_id, target_id | 每探针选用的目标集 |
| `latency_samples` | node_id, target_id, ts, icmp_ms, tcp_ms, loss | 保留 7 天 |
| `subscriptions` | id, name, token_hash, format, template_id, enabled, ua_filter | 多订阅 |
| `subscription_nodes` | subscription_id, node_id | |
| `templates` | id, name, format(singbox/clash), content | 完整配置模板 |
| `node_singbox` | node_id, version, desired_version, config_hash, status, last_error, rollback_version, cert_pem, cert_sha256, port | sing-box 期望/实际状态 |
| `commands` | id, node_id, kind, payload, status, created_at, sent_at, finished_at, result | 指令队列 |
| `audit_logs` | ts, actor, node_id, action, command, risk, source_ip, ai_session_id | |
| `alerts` | id, kind, node_id, payload, created_at, delivered_at | |

索引要点：`metrics_samples(node_id, ts)`、`latency_samples(node_id, target_id, ts)`、`traffic_daily(node_id, date)`。

**保留策略**：定时任务每 10 分钟删除 `metrics_samples`/`latency_samples` 中超过 7 天的行，并 `PRAGMA incremental_vacuum`。

---

## 7. Agent ↔ Server 协议

传输：单条 WSS（`/ws/agent`），JSON 信封，版本化：

```json
{ "v": 1, "type": "metrics", "id": "uuid", "ts": 1730000000, "payload": { } }
```

**核心设计：声明式期望状态（desired state）**。服务端只下发"我要什么"（sing-box 版本、配置、anytls 参数、延迟目标集、终端开关），agent 负责收敛并回报实际状态。好处：agent 离线重连后自动补齐，服务端不需要重放命令序列。

| 方向 | type | 说明 |
|---|---|---|
| agent → server | `hello` | machine_id、版本、os/arch、能力位（含 `self_update`，§5.5） |
| | `agent_update` | 自更新逐次上报：phase / class(terminal\|transient) / error / attempts（§5.5） |
| | `metrics` | 60s 一次；面板打开详情页时服务端可请求 5s 实时流 |
| | `traffic` | 60s 一次：选定网卡的累计计数 + 日增量 |
| | `latency` | 60s 一次，批量（每目标 12 个 5s 采样点） |
| | `state` | 节点信息、IP 列表、sing-box 实际状态 |
| | `cmd_result` | 指令执行结果（stdout/stderr/exit code，截断） |
| | `terminal` | 终端输出/关闭 |
| server → agent | `hello_ack` | 期望状态全量下发（含 `agent_target_version` / `agent_update_after`，§5.5） |
| | `desired` | 增量下发期望状态（sing-box 版本/配置/端口/密码/证书要求） |
| | `cmd` | 一次性命令（AI 执行、面板操作） |
| | `terminal_open/input/resize/close` | 终端会话 |
| | `probe_metrics` | 请求临时高频采集 5s |

- 心跳：agent 每 15s 一次 `ping`（或空帧），服务端 90s 无心跳判定离线并告警。
- 重连：指数退避 + 抖动（1s → 5min 上限）。
- 离线指令：入队 `commands`，重连后下发；TTL 10 分钟，超时标记 `timeout` 并在面板显示（避免你三小时前点的"重启"突然生效）。
- 所有指令带 `id`，agent 幂等执行（同 id 重复下达只执行一次）。
- **自更新（§5.5）靠三个字段落地，不新增"下发一条更新命令"的路径**：`hello.Caps.self_update`（能力位，旧 agent 不报）、`hello_ack.agent_target_version` + `agent_update_after`（服务端判定与错峰，`DesiredState` 里镜像一份，使人工"重试"能借普通 `desired` 帧推动在线探针——agent 读的是 `DesiredState` 那份，两个载体由服务端同一个函数填写，不会不一致）、`hello.selfcheck`（自检握手——hub 收到带该标记的 hello 后只回 `hello_ack` 就关闭，**不注册、不落库、不关闭同节点的现有连接**；实现里这条路由由 `X-Fobe-Selfcheck` 请求头选择，payload 里的 `selfcheck:true` 必须同时存在）。

---

## 8. 指标采集与流量口径

### 8.1 采集项

| 指标 | 来源 |
|---|---|
| CPU 使用率 | `/proc/stat` 两次采样取差；核数 `/proc/cpuinfo` |
| 负载 | `/proc/loadavg` |
| 内存使用率 | `/proc/meminfo`：优先 `MemAvailable`，缺失时 `MemFree+Buffers+Cached`（OpenWrt 兼容） |
| 交换 | `SwapTotal/SwapFree` |
| 磁盘 | `statfs` 遍历挂载点，面板选主分区展示 |
| 网络累计 | `/proc/net/dev`（选定网卡） |
| 网络速率 | 累计值差分 / 时间差 |
| 运行时长 | `/proc/uptime` |
| 重启检测 | `/proc/sys/kernel/random/boot_id` + 累计计数回绕双重判定 |

### 8.2 流量累计与重启处理

1. 每次上报 `(last_raw_rx, last_raw_tx)`。
2. `delta = now - last`；若 `delta < 0`（回绕/重启/网卡重置）→ 判定异常，取 `now` 为新基准，**不把负数计入**，并记一条 `counter_reset` 事件供你排查。
3. `delta` 累加进 `traffic_daily`（按探针时区切日）与 `traffic_counters.period_used`。
4. 周期用量 = 周期起点至今日表的和（日表是权威，`period_used` 只是缓存）。

### 8.3 四种模式

设周期内 `IN` = 入向累计（rx），`OUT` = 出向累计（tx），配额 = `quota_bytes`：

| 模式 | 已用量 | 使用率 |
|---|---|---|
| `in` | IN | IN / quota |
| `out` | OUT | OUT / quota |
| `both` | IN + OUT | (IN+OUT) / quota |
| `max` | max(IN, OUT) | max(IN,OUT) / quota |

- 模式是**每台探针**独立配置；切换模式只影响计算与展示，原始数据始终两个方向都存。
- 未设配额（`quota_bytes = NULL`）时，只显示累计用量与实时速率，不显示百分比。
- 达到 80% / 100% 触发告警（阈值可配），**不自动停服**（你的选择）。

---

## 9. sing-box 生命周期与 anytls

### 9.1 期望状态

服务端为每个节点生成完整 `config.json`（inbound + log + 必要的出站），存 `config_hash`；面板改任何参数 → hash 变化 → 下发 → agent 应用。

### 9.2 安装/更新三道闸门

```
下载 → sha256 校验 → 备份当前二进制为 .prev
   ↓
① config:  sing-box check -c config.json        （失败即中止，不动线上）
   ↓
② start:   写入新二进制 + 配置 → 启动服务
   ↓
③ verify:  30s 观察期：进程存活 && 入站端口 TCP 可连 && TLS 握手成功
   ↓ 失败
rollback:  恢复 .prev 二进制 + 旧配置 + 重启 → 告警"回滚已执行"
```

- 版本**必须显式指定**（面板展示可选版本，来自服务端 release 清单），不追 latest。一键批量更新里的 `latest` 只在**点击那一刻**解析成具体版本号并固化后下发，不变量不被破坏（见 §9.5）。
- 产物来源：优先面板 `/dl/singbox/<version>/linux-amd64`（服务端缓存，规避国内拉 GitHub 的问题），失败回退官方地址。**缓存由服务端进程自己填充**（启动时/手动重试，见 §9.5）——agent 侧只拉面板、不补官方回退。
- agent 自更新**不走**这条链路（实现修订 2026-09-15）：agent 没有 `check` 与 30s 观察期可用，改用"旁路自检 + 原子提交 + 主动 exit 交给 supervisor"，且**不保留 `.prev`**——详见 **§5.5**。
- 每次变更写 `audit_logs`，并在面板节点页显示"当前版本 / 期望版本 / 上次操作结果"。

> **实现修订 2026-09-15（服务端产物缓存 + 一键批量更新）**：本节原本假定"版本清单里总是有货"，但从没有一条链路负责**把货取回来**——服务端只直出 `/dl` 目录里已有的文件，agent 也只会从面板拉取。本次补齐这条链路，并且**不新增任何安装路径**：
>
> - **服务端产物缓存**：server 在启动时（仅当 `<FOBE_DL_DIR>/singbox` 里没有任何有效版本）后台下载"当时最新稳定版"到 `<FOBE_DL_DIR>/singbox/<version>/`，已有缓存则**完全不联网**；可用 `FOBE_SINGBOX_AUTO_DOWNLOAD=0` 关闭。下载校验 **fail-closed**，失败只记 WARN + 设置页状态 + 手动重试，不阻塞启动。
> - **一键批量更新**：设置页点「更新 sing-box」→ 先展示受影响节点名单 → 二次确认 → 把目标版本写进每个已启用 sing-box 节点的 `desired_version`（保持端口与配置不变）→ 复用 agent 现有的三道闸门与 `hello_ack` 收敛。异步 job + SSE 进度 + 每节点结果表；分发后 15 分钟未收敛的节点汇总成一条告警。
> - 详细规则、接口与错误码见 **§9.5**；目录与卷见 **§17**。

### 9.3 anytls 与自签证书

> **实现修订 2026-09-15（探针侧布局与服务管理对齐 one-sing.sh）**：入站配置与服务管理参考 `one-sing.sh`（同类需求的成熟实现），落地三件事，**协议与订阅语义不变**：
> - **目录/证书布局**：探针上统一为 `/etc/one-sing/{sing-box,config.json,cert/{cert.crt,private.key}}`（原 `/usr/local/bin/sing-box`、`/etc/sing-box/{config.json,cert/{cert.pem,key.pem}}`）。同名同路径意味着**已经用 one-sing.sh 管起来的机器可以直接被接管**，不必重下二进制或重签证书。证书文件名由服务端写进 `config.json`，两侧必须一致，`internal/server/singbox` 有一条跨包测试盯着（`TestCertPathsMatchAgentLayout`）。
> - **anytls `padding_scheme`**：采用 one-sing.sh 的方案（`stop=6` / `0=30-30` / `1=80-120` / `2=350-550,c` / `3=900-1400` / `4=250-600` / `5=250-600`），不再用 sing-box 默认值。它进 `config.json`，因此**改这一项等于改 `config_hash`，会在下一次收敛时把全部节点重新下发一遍**——有意的、一次性的代价。
> - **systemd 单元对齐**：`fobe-singbox.service` 补上 `CapabilityBoundingSet` / `AmbientCapabilities`、`ExecReload=/bin/kill -HUP $MAINPID`、`LimitNOFILE=infinity`、`RestartSec=10s`（保留 `User=root`、`NoNewPrivileges=true`）。**单元名仍是 fobe 自己的**：写 `one-sing.service` 会与 one-sing.sh 抢同一个进程，卸载 fobe 时还会删掉别人的单元；作为补偿，agent 首次收敛前会把已存在的 `one-sing.service` **停掉并 disable**（`AdoptForeignSingboxService`，只认 systemd）——两个 supervisor 抢一个进程只会反复重启，而面板的启停会作用在一个它并不拥有的进程上。
> - **迁移是幂等的**：agent 启动时 `MigrateSingboxLayout` 只在「目标不存在且源存在」时搬文件（二进制连同 `.prev`/`.download`、配置连同 `.prev`、证书改名 `cert.pem→cert.crt`、`key.pem→private.key`），搬完删空的旧目录；搬不动只记 WARN，收敛循环照样能把缺的东西重新下载回来。
> - **`config.json` 是 fobe 独占的**：同一台机器上又跑 one-sing.sh 又由 fobe 托管，两边会互相覆盖同一份 `config.json`（one-sing.sh 加的协议会消失）。要共存就改路径，别只改服务名。

- **私钥永不离开探针**：首次启用时由 agent 用 Go 标准库 `crypto/x509` 现场生成自签证书（不依赖 openssl——OpenWrt 常常没有），存 `/etc/one-sing/cert/{cert.crt,private.key}`（0600）。密钥算法保持 ECDSA P-256（而非 one-sing.sh 的 RSA-4096）：探针是小机器、密钥在设备上现场生成，而客户端是按 SHA256 指纹 pinning 的，算法对客户端不可见。
- agent 上报**证书 PEM + SHA256 指纹 + 有效期**给面板（不含私钥）。
- 订阅渲染时把证书 PEM 写进客户端的 `tls.certificate` 字段做 **pinning**，而不是让客户端 `insecure: true`。这样自签也不会被中间人。
- 入站口令 = `settings.anytls_password`（全局共享，见 §11.3 的取舍）。
- 端口：默认随机高位端口（10000-60000），面板可改；改端口时 agent 尝试自动放行防火墙（ufw / firewalld / nft / OpenWrt fw4），失败则返回需要你手动执行的命令原文。

### 9.4 sing-box 入站模板（生成物示意）

```json
{
  "type": "anytls",
  "tag": "anytls-in",
  "listen": "::",
  "listen_port": 23456,
  "users": [{ "name": "default", "password": "<全局密码>" }],
  "padding_scheme": ["stop=6", "0=30-30", "1=80-120", "2=350-550,c", "3=900-1400", "4=250-600", "5=250-600"],
  "tls": {
    "enabled": true,
    "server_name": "www.bing.com",
    "certificate_path": "/etc/one-sing/cert/cert.crt",
    "key_path": "/etc/one-sing/cert/private.key"
  }
}
```

### 9.5 服务端产物缓存与批量更新（实现修订 2026-09-15）

> 背景：§9.2 假定"面板能列出可选版本"，但**从来没有任何一条链路负责把产物取回来**——`/dl` 只是直出 `<FOBE_DL_DIR>` 里已有的文件，`GET /api/singbox/versions` 只读 `<FOBE_DL_DIR>/singbox/<version>/manifest.json`。缓存空的部署里，面板永远只有一个空版本列表。本节补齐"取货"与"分发"两段，且**不新增安装路径**：agent 侧的下载 → 校验 → 备份 → 三道闸门 → 回滚完全不变，批量更新只是把 `desired_version` 批量改掉。

> **实现修订 2026-09-15（版本列表 = 一版本一行 + 显式下载）**：设置页的 sing-box 区块就是缓存列表本身，**一行一个本地版本**，行尾两个图标按钮——**发布**（把这一行的版本下发到所有已启用 sing-box 的节点，仍走 §9.5.2 的影响面 + 二次确认）与**删除**（从服务端磁盘删除，仍被 `desired_version` 引用时要 force）。下载中的版本也是同一张表里的一行：`download` 快照带 `version`，面板把「不在磁盘上且正在下载/刚失败」的版本合成一行，就地显示阶段、字节与速度；失败的那一行保留原因并给一个重试图标——服务端在 release 查找失败时会补一个 `failed` 相位（`failSingboxDownload`），否则这一行会永远停在「获取校验信息」。
>
> 两条新接口，都是操作员显式触发，**页面渲染永不联网**：
>
> - `GET /api/singbox/releases`：上游 release 列表（新 → 旧，只取**一页 30 条**，不翻页——一页解压后约 10 MB），10 分钟内存缓存，`?refresh=1` 绕过；每条带 `cached`（磁盘上已有）与 `latest_stable`（该页最大的非 prerelease）。上游不可达时返回上一次的好列表并置 `stale:true`（一次都没成功过才 502）。
> - `POST /api/singbox/versions/{version}/download`：按**显式版本**下载，不接受 `latest`（§9.2「版本必须显式指定」不破）。已缓存 → 200 `cached:true`；另一个版本正在下载 → 409 `download_in_progress`（面板只有一根进度条，不做队列）；受理 → 202，进度沿用同一个 `singbox_download` 快照。写 `audit_logs`（`singbox_version_download`）。受理后立刻合成 `resolving` 相位，否则「查找 release」这段时间（最多走满几页）面板上没有任何东西可显示。

#### 9.5.1 缓存布局与下载源

```
<FOBE_DL_DIR>/singbox/<version>/          # version 去掉前导 "v"，如 1.10.0
├─ linux-amd64                            # 0755，官方单文件静态二进制（agent 既有的下载 URL）
├─ linux-amd64.sha256                     # "<hex>  linux-amd64"（两个空格，sha256sum 格式）
└─ manifest.json                          # 版本/大小/sha256/来源 asset/下载时间，供面板与 /dl 阅读
```

- **下载源**：GitHub Release（默认 `SagerNet/sing-box`）。`FOBE_SINGBOX_API_BASE`（默认 `https://api.github.com`）覆盖 release 列表来源，`FOBE_SINGBOX_DOWNLOAD_BASE`（默认 `https://github.com`）用于拼路径；两者都指向 GitHub 兼容镜像即可在拉不到 GitHub 的环境里工作。
- **选版本**：只认**稳定版**——跳过 draft，跳过 prerelease；语义化版本比较（`1.10.0 > 1.9.9`，不是字典序）。**列表按页取，且尽早停**（实现修订 2026-09-15，见下）。
- **列表页很大，别按"小接口"对待**（实现修订 2026-09-15）：releases 列表把每个 release 的 ~170 个 asset 全量返回，`per_page=100` 一页解压后约 33 MB（gzip 约 2 MB；上游单个 release 约 300 KB）。因此：① 解码上限为 64 MiB，且触顶时报 `response exceeds N bytes`——此前 8 MiB 的 `io.LimitReader` 会把 body 截断，json 报出误导性的 `unexpected EOF`（真实原因是尺寸，不是网络）；② 「当前最新稳定版」只请求 `per_page=30`，并在**第一个含稳定版的页就停**（页按创建时间倒序，能超过该页最大稳定版的版本只可能创建得更晚、即更靠前），单版本查找命中即停。原实现固定走满 5 页 × 33 MB（实测 ~18 s），已贴着面板 `latest` 解析的 20 s 上限。
- **产物变体固定为 `linux-amd64-musl`**：官方标准构建是 tar.gz 且带 `libcronet.so`，而 agent 的安装路径是**单文件二进制**（下载 → sha256 → 直接写 `/etc/one-sing/sing-box`），两者不兼容。musl 变体是同一个 tar.gz 里唯一的静态单文件产物，装到常规发行版也能跑（无 glibc 依赖），代价是失去 cronet 相关能力（fobe 只用 anytls 入站，不受影响）。

#### 9.5.2 校验：fail-closed

校验值来源按顺序回退，**任何一步取不到就拒绝安装**（fail-closed，绝不"跳过校验先装上"）：

1. GitHub API asset 的 `digest`（`sha256:<hex>`，新 release 才有）；
2. 同目录的 `<asset>.sha256`（镜像/离线源常见做法）。

不匹配（`singboxdl: sha256 mismatch`）或解包不出 sing-box 二进制（`singboxdl: sing-box binary not found in archive`）时：**中止，且不污染缓存**——下载与解包全程在 `<FOBE_DL_DIR>/singbox/.tmp-*` 里进行，只有校验通过才 `rename` 进 `<version>/`；半成品目录对 `ScanCache` 不可见，并由清理例程回收。

#### 9.5.3 启动自动下载、开关与状态

- **仅在缓存为空时联网**：`<FOBE_DL_DIR>/singbox` 里已有任何**有效**版本（存在 `linux-amd64` 文件的目录）就完全不请求上游，升级/重启不会重复下载。
- **开关**：`FOBE_SINGBOX_AUTO_DOWNLOAD`（默认开，置 `0`/`false`/`off`/`no` 关闭）。关闭或未配置 `FOBE_DL_DIR` 时状态为 `disabled` 并记录原因。
- **不阻塞启动**：下载在后台 goroutine 里跑，服务端进程该起就起；失败只写 WARN 日志 + 状态，面板设置页显示失败原因与「重试」按钮（`POST /api/singbox/cache/retry`，后台执行，结果经 `/ws/events` 的 `singbox_cache` 事件回归）。开发模式的"面板拉不到 GitHub"因此不会拖垮整个 UI。
- **状态落库**（服务端自有设置，客户端不可写）：
  - `singbox.cache_status` = `{state: ok|disabled|failed|pending, version, updated_at, error, auto_download}`
  - `singbox.dl_mount_ok` = `"true"`/`"false"`，**仅容器内**写入：检测到 `/.dockerenv` 或 cgroup 标记时，检查 `FOBE_DL_DIR` 是否为 `/proc/self/mountinfo` 中的独立挂载点；不是则以 WARN 提示"升级容器会丢失已下载版本"并在设置页标红。非容器环境跳过（避免 dev 噪声）。
- **旧版本不自动删**：缓存里的历史版本一直保留；设置页显示每个版本的占用、下载时间与被多少节点引用，手动删除（`DELETE /api/singbox/versions/{version}`），仍被 `desired_version` 引用时需要显式 force。
- **下载进度可见 + 同一产物只下一次**（实现修订 2026-09-15）：安装过程经 `singboxdl.Config.Progress` 上报阶段（`waiting` / `resolving` / `downloading` / `verifying` / `extracting` / `publishing` / `done` / `failed`；下载阶段带已下载字节与总大小，末次必报精确值），服务端折成一个 `download` 快照：既随 `GET /api/singbox/cache` 返回（中途打开页面也有进度条），也走 `/ws/events` 的 `singbox_download` 事件（已打开的页面实时刷新；字节级事件按 250ms 节流，相位变化、首字节与终态立即推送）。快照**只存内存**——字节计数器若落库就是每秒多次写 SQLite，撞 §5 的单写者。同时**全进程只有一个 `singboxdl.Client`**（`httpapi.Server.SingboxDL()`，`cmd/server` 把它交给启动缓存管理器）：`Install` 按该 client 的互斥量串行，启动自动下载 / 手动重试 / 批量更新撞同一个版本时后来者只报 `waiting` 并排队，拿到锁后发现版本已发布即返回 `ErrVersionExists`（调用方按成功处理），**同一个 tarball 不会被下载第二次**。此前每个请求各自 `singboxdl.New` 一个 client，"启动自动下载"与"更新 sing-box"各持一把锁，同一版本会被并行抓两遍。面板在下载进行中将「更新 sing-box」置灰并提示原因。

#### 9.5.4 一键批量更新

```
设置页版本行尾的「发布」图标（实现修订后没有全局选择器：发布的永远是这一行的版本）
  → GET  /api/singbox/update/impact        列出受影响节点（已启用 sing-box、desired_version 非空）
  → 二次确认（强制，不可跳过；弹窗列出节点名单与目标版本）
  → POST /api/singbox/update {version|latest, confirm:true}   → 返回 job id
  → GET  /api/singbox/update/{job}         进度 + 每节点结果（页面刷新后仍可见）
  → /ws/events 的 singbox_update 事件      实时进度（不必轮询）
  → 15 分钟后复查收敛，未收敛节点汇总为一条告警
```

- **目标版本**：请求可以是 `latest`，也可以是一个具体版本号（缓存里没有就先把它下载下来）。`latest` **在点击那一刻**经上游解析成具体版本号并固化——落库、审计、下发的都是具体版本号，因此 §9.2 的"版本必须显式指定、不追 latest"不变量**不被破坏**（追 latest 的只是"点击"这个动作，不是 agent 的常态行为）。目标版本不在缓存里时，job 先下载它（同样 fail-closed），下载失败则整个 job 失败且不写任何 `desired_version`。
- **改写期望状态而非发命令**：对每个目标节点写 `desired_version`（**保持既有端口与配置不变**），然后对在线节点 `pushDesired`；离线节点什么都不发，等它重连时由 `hello_ack` 全量下发自然收敛（§7 声明式期望状态）。
- **结果分档**（每档逐节点带原因）：`already_current`（本来就已是该版本且期望一致）/ `pushed`（已写入并推送成功）/ `offline_pending`（已写入，agent 离线，待重连收敛）/ `failed`（写入或推送失败，附原因）。
- **job 持久化**：状态写在 `settings.singbox.last_update`，页面刷新、甚至服务端重启后都能看到同一次更新的进度与结果；服务端重启时按持久化的 deadline **重新武装**收敛检查（未完成的 job 标记为失败）。
- **审计**：每一次更新、重试、删除版本都写 `audit_logs`。
- **15 分钟收敛告警**：分发后 15 分钟（`DefaultConvergeAfter`）复查各目标节点上报的 `node_singbox.version`，仍未等于目标版本的节点**汇总成一条**告警（kind `singbox_update_stale`），走既有的 `deliverAlerts` → Telegram/Webhook 通道，并在 1 小时窗口内去重。告警只提示、不自动回滚（同 §0 决策 7 的取向）。

#### 9.5.5 边界与取舍

- **不暴露给 AI 助手**：本节新增的接口不在 §12.2 工具集内。批量更新影响面覆盖所有节点，不适合落进"默认放行"的模型执行路径；单节点更新仍可用既有的 `update_singbox` 工具。
- **保持 panel-only**：agent 侧**不补官方回退**——§9.2 里"失败回退官方地址"这句对 agent 依然不成立，产物只从面板拉。理由：让"探针从哪里拿二进制"只有一个答案，出问题时可复现；服务端缓存本身已经承担了规避 GitHub 不可达的职责（镜像源/手动投放）。
- **本机手动投放**：把产物按上面 9.5.1 的布局放进 `<FOBE_DL_DIR>/singbox/<version>/` 即可，无需联网——这也是完全离线环境的兜底用法。

---

## 10. 订阅与模板

- 路径：`/sub/<token>`；`token` 高熵随机，库中只存 hash。
- 格式决策：`?format=singbox|clash` 显式指定；缺省按 UA 嗅探（`clash` / `sing-box` / `mihomo` / `surge`），嗅探失败默认 `singbox`。
- 输出 = **模板渲染**：模板是每格式一份完整配置文件（Clash YAML / sing-box JSON），使用 `{{nodes}}`、`{{rules}}` 占位符；同一模板可被多个订阅复用。
- **模板中禁止出现可手填的凭据**：节点数据统一由 `{{nodes}}` 注入，密码来自全局设置。这样"轮换密码"才不会漏改。
- 面板功能：模板编辑器（含语法校验 + 预览渲染结果）、订阅的节点多选、UA 过滤、一键轮换 token、访问日志（时间/IP/UA）。
- 节点渲染为 anytls outbound：`server`（探针主 IP 或你指定的域名）、`server_port`、`password`、`tls.certificate`（内嵌 PEM pinning）、`tls.server_name`。

### 10.1 共享密码的取舍 ⚠

所有订阅共用一个 anytls 密码 → **订阅 token 只能吊销 URL，吊销不了代理访问**。真泄密时的补救链路是：

```
面板「轮换代理密码」→ 更新 settings → 批量下发所有节点 desired state
                    → agent 重载 sing-box（不断开现有连接，新连接用新密码）
                    → 重新渲染所有订阅 → 客户端重新拉订阅
```

面板必须在轮换对话框里写明："此操作会要求所有客户端重新拉取订阅，旧密码立即失效"。建议也顺手实现"节点级密码覆盖"字段，方便你以后想隔离时不必重构数据模型（v1 UI 可以先不暴露）。

---

## 11. Web 终端（2026-09-15 修订：单一 Agent PTY，SSH 模式与凭据托管移除）

- 前端 xterm.js ↔ `/ws/terminal?node=<id>` ↔ server 转发 ↔ agent；agent 收到 `terminal_open` 一律用 `creack/pty` 起本地 shell，**不再内置 SSH 客户端、不依赖探针 sshd、不在面板保存任何 SSH 凭据**。适用于禁密码登录的 VPS、dropbear 行为怪异的 OpenWrt。
- 代价绑定：探针必须能起 PTY（容器内挂载 devpts）；无法再"借道"探针上已有的 sshd 配置。
- 兼容：`TerminalOpen.Mode` 字段保留在 v1 wire 上（server 恒写 `pty`）；旧浏览器发来的 `ssh` 由 server 规范化为 `pty`，旧 agent 收到 `ssh` 也只起本地 PTY。旧 `node_ssh` 凭据表在服务端启动迁移时删除。
- 会话初始化时记审计：操作者、节点、来源 IP、会话 ID、开始/结束时间。浏览器帧限 1 MiB，会话 ID 由 server 生成并覆写，浏览器不能伪造。
- 终端与 AI 执行共用 agent 指令通道 → 审计口径统一。
- v1 不做 PTY 全量录制（体积与隐私成本高），但保留 `session_id`，便于后续开启录制。

---

## 12. AI 助手

### 12.1 形态

- 服务端代理到 OpenAI 兼容 API：`base_url`、`api_key`（AES-GCM 加密存 `settings`）、`model` 均由面板配置；支持流式输出。
- 上下文注入（默认）：节点列表、当前指标、流量与配额、延迟、sing-box 版本与状态、最近告警。
- **默认不注入原始日志**：需要时由你在对话里显式打开"附带日志（最近 N 行）"开关。这既减少 token，也显著缩小注入面——**但不改变你选的默认放行语义**。

### 12.2 工具集

| 工具 | 类型 | 说明 |
|---|---|---|
| `list_nodes` / `get_metrics` / `get_traffic` / `get_latency` | 只读 | 结构化查询 |
| `get_singbox_status` / `tail_logs` | 只读 | 需显式开关日志 |
| `restart_singbox` / `stop_singbox` / `start_singbox` | 变更 | 走同一闸门 |
| `install_singbox` / `update_singbox` | 变更 | 指定版本 |
| `set_singbox_port` / `set_anytls_password` | 变更 | 影响面大 |
| `run_shell` | 变更 | 裸 shell |

**不在工具集里**：§9.5 的服务端产物缓存与**一键批量更新**接口（`/api/singbox/cache*`、`/api/singbox/update*`、`DELETE /api/singbox/versions/{version}`）。它们影响面覆盖全部节点、且会写服务端缓存，不适合落进"默认放行"的执行路径；单节点更新仍走上面的 `update_singbox`（需显式版本）。

**agent 自更新（§5.5）不新增任何工具**（连只读的也不加）：期望版本、计划时刻、尝试次数与失败原因随既有节点查询一并返回，AI 看得到但做不了——不能触发、冻结、解锁或改 target。系统自动跟随是服务端版本变更触发的行为，不在模型的可达范围内。

### 12.3 执行策略（⚠ 你签下的风险）

**你的选择：默认放行。** 即：

- 模型返回的 tool_call **直接执行**；
- 仅当**模型自己**把该动作标记为 risky 时，前端弹出确认框展示完整命令；
- 确认框不提供"记住选择/不再询问"（不给攻击者一次性扩权到永久的机会）。

我按你的决定实现，同时**无条件**加上这几条补偿控制（它们不改变默认体验，成本极低）：

1. **全量审计**：每条命令的原文、模型给出的理由、风险标记、结果全部落 `audit_logs`；
2. **全局 Kill Switch**：面板一键冻结所有 AI 执行（CLI 也能开），冻结后 AI 退化为只读；
3. **速率与影响面熔断**：单节点每分钟命令数上限、连续失败自动暂停该节点的 AI 执行；
4. **元操作强制确认**：涉及面板密码、主密钥、AI 自身配置、**由 AI 发起的** agent 自更新的动作，无论模型怎么判定都强制确认（防"AI 给自己扩权"）。注意区分：§5.5 的**系统自动跟随**不在这一列——那是服务端版本变更触发的系统行为，没有人在点它；实际实现里 AI 侧连写工具都没有，只有只读查询（§12.2）。
5. `run_shell` 的 stdout/stderr 截断入审计，防止把凭据回灌进上下文。

### 12.4 已知未缓解风险（写在这里以便你日后反悔）

> **间接提示注入 → 静默执行 → 探针 root。**
> 攻击者只要能连上任意一个代理节点，就能影响探针上的日志/进程名/连接来源等文本；这些文本一旦进入模型上下文，模型可能被诱导调用 `run_shell`。由于默认放行，该命令会被静默执行，且以 root 身份。攻击者在拿下第一台探针后，可以横向影响你在面板里配置的其他节点。
>
> 缓解到"白名单外默认确认"只需改一个配置项（`ai.default_policy: allow|confirm`），数据模型与工具集都不用动。建议你至少在暴露面扩大（加节点、给别人用订阅）之前重新评估一次。

---

## 13. 延迟测量

- 目标（面板配置，可复用）：`name` + `kind` + `host` + `port`。
  - `icmp`：ICMP echo RTT（需要 root 或 `CAP_NET_RAW`，缺失则跳过并标记）。
  - `tcp`：TCP 三次握手 RTT（连接目标 host:port 后立即关闭）。
- 每台探针选择自己用哪些目标（多选）。
- 采集：**探针本地每 5s 测一次**，本地保留 60s 窗口，**每 60s 批量上报 12 个采样点**（不是每 5s 上报——否则 10 探针会出现 2 次/秒的常驻写入，且网络抖动会污染测量本身）。
- 存储：`latency_samples` 保留 7 天。
- 数据量估算：5s × 7d = 120,960 点 / (探针×目标) 对；10 探针 × 5 目标 ≈ 600 万行，SQLite 可承受（每行 ~40B，约 250MB 量级）。**图表必须降采样**：1h 视图取原始点，24h/7d 视图按 5min/30min 桶取平均 + P95。
- 前端：折线图 + 丢包率；每探针一张"多目标对比"图。

---

## 14. IP 检测与国旗

- agent 首次启动、网络变化（每 5 分钟检查 IP 集合是否有变化）、以及面板手动触发时，枚举本机地址（`net.Interfaces`，排除 loopback/link-local）→ **全量上报**。
- 服务端判定：
  1. 本地 GeoLite2-Country MMDB（离线，面板可上传更新，**也会自动下载**——见 §14.1）；
  2. 未命中且可配置在线 API（ip-api / ipinfo，可选配 key）作为回退。
- 国旗 = ISO 3166-1 alpha-2 → emoji/图标资源（前端内置，不依赖 CDN）。
- 主 IP 选择：默认第一个公网 IPv4，否则第一个公网 IPv6；面板可手动指定（`node_ips.is_primary`），订阅渲染使用主 IP（或你填的域名）。

### 14.1 数据库的自动下载与更新（实现修订 2026-09-15）

本节原本只有"面板可上传"一条路径：**安装即空白，除非操作者自己找一个 MMDB 传上去**。补齐后，数据库自己有生命周期，上传降级为离线逃生口。

- **来源：只用免密钥镜像，不碰 MaxMind License Key。** 默认按序尝试三个镜像（`raw.githubusercontent.com/P3TERX/GeoLite.mmdb` → `cdn.jsdelivr.net` 同一文件 → `Loyalsoldier/geoip` 最新 release），都是官方 GeoLite2-Country 数据的自动构建；挂一个还有下一个。**不做 License Key 支持**——MaxMind 官方下载要注册账号，让"开箱可用"依赖一个凭据是反向取舍。内网/离线部署用 `geoip.url`（或 `FOBE_GEOIP_URL`，后者优先）钉一个 URL，钉了就不回退到内置镜像：操作者既然配了私有镜像，静默回落到 GitHub 会绕过他的网络策略。
- **节奏：每天检查一次，数据库文件超过 `geoip.max_age_days`（默认 7 天，MaxMind 每周更新）就换。** 检查本身是一次本地 `stat` 与一次设置读取，只有过期才会联网；`0` 表示不做定期更新，只在文件缺失时下载。面板上的 `geoip.auto_update` 开关（缺省开）管的是这条路；`FOBE_GEOIP_AUTO_UPDATE=0` 是运维的硬闸——连启动时补缺都不做（手动更新与上传不受影响），此时面板开关置灰并说明原因。
- **安装必须原子且必须能解析**。MMDB **没有文件头 magic**：真实文件以搜索树开头、以 `\xab\xcd\xefMaxMind.com` 元数据标记结尾。此前上传接口检查的是前缀 `MMDB`——没有任何真实数据库长这样，所以**每一次合法上传都被拒**（测试用造出来的假头，掩盖了这一点）。现在统一走 `geoip.WriteMMDB`：临时文件 → 尺寸上限（64MB）→ `geoip2.Open` 真解析（空搜索树也算无效）→ `chmod 0644` → `rename`。下载与上传共用这一条，任何一步失败都**不会**动到线上文件（`Ensure` 的测试专门盯这一点：所有镜像都失败时，磁盘上的旧库必须还在）。
- **进度与结果都可见**：更新阶段（`connecting` / `downloading` / `verifying` / `done` / `failed`）经 `/ws/events` 的 `geoip_update` 事件推送（字节级 250ms 节流，相位变化立即推），并作为 `download` 快照挂在 `GET /api/geoip/status` 上——中途打开页面也有进度条。落库的只有 `geoip.status`（状态、来源 URL、上次成功/检查时间、失败原因），字节计数**只存内存**，理由同 §9.5.3：每秒多次写 SQLite 会撞 §5 的单写者。
- **手动更新是异步的**：`POST /api/geoip/update` 只启动后台任务并回 `202`（`accepted:false` 表示已有一个在跑，不是错误）。9MB 的下载不该占着一个 HTTP 响应，面板靠事件与状态端点收敛。
- **一个进程只有一个 MMDB 句柄**（`geoip.NewWith`）：hub 的国别判定与面板的状态视图共用它，更新成功后 `Reload()` 立即生效。此前 `cmd/server` 建了两个句柄，第二个要等自己的 stat 检查才发现文件变了。
- 设置页新增：状态两块（数据库状态、数据日期）、进度条、「立即更新」、「上传 MMDB」，以及三个设置项（自动更新、最长天数、自定义下载源）。改这三项会**当场**跑一次策略检查（`Manager.Check`），不必等下一个日切。来源 URL 与上次检查时间仍由 `GET /api/geoip/status` 返回（数据来源不占面板位置，需要时再加回）。

---

## 15. 告警

| 类型 | 触发 | 默认阈值 |
|---|---|---|
| 到期提醒 | `node_billing.next_due_at` | 提前 7 / 3 / 1 天 |
| 流量阈值 | 周期使用率 | 80% / 100% |
| 探针离线 | 90s 无心跳 | — |
| sing-box 异常 | 进程退出 / 端口不可连 / 回滚发生 | 立即 |
| sing-box 批量更新未收敛 | 分发后 15 分钟仍未报告目标版本（§9.5.4） | 15 分钟 |
| 计数器重置 | 流量计数回绕 | 立即（信息级） |

通道：**Telegram Bot** + **通用 Webhook**（JSON POST，HMAC 签名头）。

- 同类告警去重合并（同一节点同一类型 1 小时内只发一次，恢复时补一条 recovery）。
- 面板内"通知中心"保留全部历史，`alerts` 表为权威。

---

## 16. 前端

- 技术：**React + TypeScript + Vite**；构建产物输出到 `web/dist`。**生产镜像在构建阶段把产物烤进镜像**（`/srv/web`），由 server 直出（`FOBE_WEB_DIR`）——不嵌入 Go 二进制（避免体积膨胀），也不再依赖宿主机挂载前端目录。若想让外部 nginx 直接吐静态文件，见 §3 末尾的替代做法。
- **本地开发不走容器**：前端 `npm run dev`（Vite dev server，把 `/api`、`/ws`、`/sub`、`/install.sh`、`/dl` 代理到 `http://127.0.0.1:8080`，**WebSocket 代理必须开 `ws: true`**），后端 `go run ./cmd/server`。此时把 `FOBE_WEB_DIR` 留空 → server 进 **API-only 模式**：`/` 返回一句"请访问 Vite dev server"的提示（不 404、不白屏），其余接口行为与生产一致。
- 页面：登录 / 概览（卡片墙）/ 节点详情（**只读监控**：指标 + 图表 + 流量 + 延迟历史 + 命令）/ 订阅与模板 / 延迟目标 / 告警 / 终端（全屏）/ AI 助手（侧栏）/ 设置（AI、通知、GeoIP、保留期、主密钥状态、**服务器**）。（2026-09-15 修订）
- **配置入口收敛（2026-09-15）**：设置里新增「服务器」页，表格列出已接入的服务器，行尾为编辑/删除图标按钮。「编辑服务器」页集中承载该服务器的全部配置：节点设置（名称/备注/网卡/配额/账单）、延迟测量端点选择、IP 列表（含手动主 IP）、sing-box 服务端配置（版本/启停/端口）。节点详情页不再承载配置表单与删除按钮——监控与配置分离，删除服务器统一走设置页（带确认）。
- 实时（2026-09-15 修订）：`/ws/events` 只覆盖状态类变化（节点增删改、sing-box、订阅、设置、GeoIP）——**常规指标上报不产生任何事件**，所以数据新鲜度必须靠「轮询 + 高频上报」两条腿：
  - **概览**：挂载且标签页可见期间，对每个在线节点打开 §16 的 5s 探测流，并 5s 拉一次列表。打开探测流是必要的：不打开就只能等 60s 基线节奏，卡片墙看起来像「不自动更新」。
  - **详情**：5s 拉节点快照（与探测流对齐），30s 拉图表 / 流量 / 延迟曲线（重查询）。
  - **标签页隐藏时全部交还 60s 基线并停轮询**（`visibilitychange`），避免一个忘记关闭的标签页把每个 agent 永久钉在 5s。离线节点 409 直接忽略；节点重新上线后会重新打开探测流。
- 节点卡片展示：名称、国旗、IP、在线状态、CPU%、内存%、磁盘%、CPU 核数、流量使用率（按所选模式）、今日上下行、到期倒计时、延迟摘要。
- **i18n**：`zh-CN` + `en-US`，前端 i18n 框架；后端只返回错误码与结构化数据，**不出中文文案**（避免后端拼字符串导致双语文案漏翻）。
- **主题**：CSS 变量 + `prefers-color-scheme` 默认跟随系统 + 手动切换持久化（localStorage 与服务端设置双写，跨设备一致）。
- 全部静态资源自托管，不引 CDN（离线/内网可用）。

---

## 17. 部署与目录

```
fobe/
├─ cmd/
│  ├─ server/main.go          # 面板服务端 + scheduler + admin CLI
│  └─ agent/main.go           # 探针 agent
├─ internal/
│  ├─ protocol/               # 共享消息定义、版本、校验（server 与 agent 共用）
│  ├─ server/{httpapi,hub,store,quota,geoip,notify,scheduler,security,singbox}
│  │  ├─ singboxdl/           # §9.5 产物下载/校验/落盘（GitHub release → <DLDir>/singbox/<ver>/）
│  │  ├─ singboxcache/        # §9.5 启动自动下载策略 + 容器挂载校验 + 状态落库
│  │  └─ singboxupdate/       # §9.5 一键批量更新 job + 15 分钟收敛告警
│  └─ agent/{collect,singbox,latency,terminal,cert,service,netinfo}
├─ web/                       # React + TS + Vite（src/、vite.config.ts、产物 dist/）
├─ deploy/
│  ├─ docker-compose.yml
│  └─ Dockerfile.server       # 多阶段：node 构建前端 → go 构建 server/agent → 运行镜像（不含 nginx）
├─ scripts/
│  ├─ build.sh                # 本地/CI 三件套构建（镜像内自行多阶段构建，不依赖它）
│  └─ install.sh.tmpl         # 安装脚本文本模板（服务端注入 token 后下发）
├─ data/                      # 宿主挂载（gitignore）
│  ├─ sqlite/fobe.db
│  ├─ dl/                     # → 容器内 /srv/dl（**必须挂载**，见下）
│  │  ├─ agent/<version>/     # agent 产物 + manifest.json（首次启动由 seed 目录补进卷，§5.5）
│  │  └─ singbox/<version>/   # §9.5 服务端自动下载的 sing-box 产物
│  │     ├─ linux-amd64
│  │     ├─ linux-amd64.sha256
│  │     └─ manifest.json
│  └─ backup/
└─ docs/design.md
```

`docker-compose.yml` 要点：

```yaml
services:
  server:
    build: { context: ., dockerfile: deploy/Dockerfile.server }
    ports:
      - "127.0.0.1:8080:8080"      # 只让本机 nginx 访问；nginx 不在本机时改绑定并限制来源
    volumes:
      - ./data/sqlite:/data
      - ./data/dl:/srv/dl
      - ./data/backup:/backup
    environment:
      FOBE_MASTER_KEY: ${FOBE_MASTER_KEY:?needed}
      FOBE_DB: /data/fobe.db
      FOBE_WEB_DIR: /srv/web
      FOBE_DL_DIR: /srv/dl
      FOBE_TRUSTED_PROXIES: "127.0.0.1/32,::1/128,172.16.0.0/12"
      # §9.5 产物缓存：缓存为空才联网下载当时的最新稳定版；置 0 可关闭。
      # 镜像源/离线环境改 API 与下载根地址（GitHub 兼容即可）。
      FOBE_SINGBOX_AUTO_DOWNLOAD: "1"
      FOBE_SINGBOX_API_BASE: ""          # 缺省 https://api.github.com
      FOBE_SINGBOX_DOWNLOAD_BASE: ""     # 缺省 https://github.com
```

- **没有 nginx 服务，也没有证书卷**：接入层完全外部化（见 §3 与 `README.md`）。
- **`/data`、`/backup`、`/srv/dl` 三个卷一个都不能少**（`Dockerfile.server` 已 `VOLUME` 声明）。`/srv/dl` 尤其容易漏：它存 agent 产物与 §9.5 自动下载的 sing-box 版本，**没有独立挂载点时升级/重建容器会把这些版本全部丢掉**。服务端在容器内会自检 `FOBE_DL_DIR` 是否为 `/proc/self/mountinfo` 里的独立挂载点，不是就写 WARN 并置 `singbox.dl_mount_ok=false`（设置页标红）。症状与处置见 `README.md` 排障表。
- **agent 产物必须放在卷外的 seed 目录**（实现修订 2026-09-15）：compose 把 `../data/dl` 挂到 `/srv/dl`，而挂载在容器启动时就生效——镜像里 `COPY` 进 `/srv/dl` 的东西**运行时根本读不到**，所以标准部署下 `/dl/agent/<version>/linux-amd64` 默认 404，`/install.sh` 也拉不到 agent。镜像因此把产物放进 `/srv/agent-seed/agent/<version>/`（`FOBE_AGENT_SEED_DIR`，非卷路径），服务端启动时**复制进 DL 卷**并把 `agent/latest` 指向自己这一版。这跟 §9.5 的 sing-box 缓存是两件事：sing-box 的产物由服务端自己联网下载，agent 的产物只能来自镜像（服务端不会自己编译 agent）。
- **前端产物在镜像里，不挂载**：`Dockerfile.server` 的 node 阶段产出 `web/dist` 并 `COPY` 到 `/srv/web`。改前端 = 重新 `docker compose build`，不存在"改了源码忘了构建/挂载路径写错"这类事故。
- **`/api/*` 永远不吃 SPA 兜底**（实现修订 2026-09-15）：静态处理器是最后的兜底（`mux.HandleFunc("/", s.handleStatic)`），未匹配的 `/api/...` 现在直接回 `404 {"error":{"code":"unknown_endpoint"}}`，不再吐出 API-only/`index.html` 那一页。原因是这个组合会造成一个很难查的假象：**旧代码的 server**（没重启的 dev 进程、没重建的镜像、没重启的容器）遇到新前端调用的新接口，会以 `200 text/html` 应答，前端 `resp.json()` 解析失败——而 `apiErrorMessage` 把任何非 `ApiError` 都当成 `network_error`，于是面板报"网络错误,无法连接服务器"，把人往 DNS/防火墙方向带，实际连接完全正常。现在同类情况会明确说是"接口不存在(服务端可能是旧版本)"。前端侧也补了 `bad_response`：2xx 但非 JSON 的响应单独报错，不再伪装成网络故障。（副作用：因为有 `/` 兜底模式，Go 1.22 ServeMux 的自动 405 在 `/api` 下不会触发，方法/路径不匹配统一落到这个 404。）
- 注意：即使你从宿主机 `127.0.0.1` 发起请求，容器内看到的源地址通常是 Docker 网关（如 `172.17.0.1`），所以 `FOBE_TRUSTED_PROXIES` 默认包含 Docker 私网段。

- **构建管线**：`Dockerfile.server` 是多阶段构建——`node:22-alpine` 阶段构建 React 前端 → `golang` 阶段构建 `server` 与 `agent`（agent 交叉编译 `linux/amd64`，`CGO_ENABLED=0`）→ 运行镜像里同时含：server 二进制、`/srv/web`（前端产物）、`/srv/dl/agent/<version>/`（agent 产物 + 带 sha256 的 `manifest.json`，安装脚本与面板版本选择都读它）。`scripts/build.sh` 提供同一套产物的本地/CI 构建，供不进容器的开发方式使用。
- **备份**：每日 `VACUUM INTO` 快照到 `./data/backup/fobe-YYYYMMDD.db`（保留 14 份），另提供面板导出/导入 JSON（不含凭据明文）。

---

## 18. 里程碑

| 阶段 | 交付 | 验收标准 |
|---|---|---|
| **M1 骨架** | 多阶段镜像（含 React 前端产物）+ compose + SQLite + 登录（含黑名单+CLI）+ agent 注册/心跳 + 节点列表 + **README 外部 nginx 接入文档** | 一条安装命令能在 Debian 上装出节点并出现在面板；按 README 配好外部 nginx 后可从公网访问；本地 `go run` + `npm run dev` 跑通同一套接口 |
| **M2 监控** | 指标采集 + 实时面板 + 7 天明细 + 日流量表 + 四模式配额 + 重置锚点 + 缴费周期 | 面板 CPU/内存/磁盘/流量数字与 `top`、机房账单对得上 |
| **M3 sing-box** | 安装/更新/回滚/启停 + anytls 自签 + 证书 pinning + 订阅与模板 | 从零到"手机能导入订阅并连通"；故意写坏配置能自动回滚 |
| **M4 运维面** | Web 终端（agent 本地 PTY）+ IP/国旗 + 延迟测量 + 告警 | 浏览器里能上探针改配置；离线节点 90s 内告警到 Telegram |
| **M5 AI** | 工具集 + 确认弹窗 + 审计 + Kill Switch + 熔断 | 用自然语言完成"看这台为什么负载高"和"把 sing-box 升到 x.y.z" |
| **M6 打磨** | i18n + 明暗主题 + OpenWrt 实机验证 + 备份恢复 + 文档 | x86 OpenWrt 软路由上完整跑通 M2–M4 |

每阶段可独立验收，M3 结束即具备"最小可用产品"价值。

> 实现修订 2026-09-15：**agent 自更新（§5.5）** 是 M6 之后的运维增强项，不属于任何里程碑的验收前提——它只在"探针换上新 agent 之后"才开始生效（存量探针需人工重装一次，见 §20.8）。

---

## 19. 还需你拍板的默认项

以下我按默认写进了方案，**没有异议就照此实现**：

1. 离线探针的指令入队后 TTL **10 分钟**，超时标失败（避免迟到指令突然生效）。
2. 探针防火墙：agent **尝试自动放行** sing-box 端口（ufw / firewalld / nft / fw4），失败则把需要你手动执行的命令原文返回面板。
3. 备份：每日 `VACUUM INTO` 快照保留 14 份 + 面板导出/导入。
4. agent 自更新：与 sing-box 同样三道闸门 + 版本显式指定。**（实现修订 2026-09-15：此条已被 §5.5 取代——agent 没有 `check`/观察期可用，实际是"下载 + sha256 + 旁路自检 + 原子替换"，且不保留 `.prev`；"跟随服务端"改为双向（含降级），触发是系统行为、不需确认，Kill Switch on 时冻结。）**
5. anytls 端口默认随机高位端口，面板可改。
6. **接入层完全外部化**：fobe 不碰 nginx、不签发也不续期证书；README 提供可直接复制的 nginx 配置（单上游 + WS 升级 + 真实 IP + 超时 + 上传体量）。
7. 证书 pinning 替代 `insecure`：订阅里内嵌证书 PEM。
8. 日志默认**不进** AI 上下文，需手动开关。
9. 节点级 anytls 密码覆盖字段：**数据模型预留**，v1 UI 不暴露。
10. v1 不做 TOTP（按你的选择），但 `users` 表预留 `totp_secret` 字段。
11. 首次启动密码：未设置 `FOBE_ADMIN_PASSWORD` 时生成一次性初始密码并打印到服务端日志，登录后强制修改；忘记密码用 `fobe-server admin reset-password`。**（实现修订 2026-09-14：`FOBE_ADMIN_PASSWORD` 从"仅首次生效"升级为密码准绳——每次启动都同步为该值，变更时吊销全部旧会话；不设置则不动现有密码。）**
12. 前端形态：**React + TS + Vite**；生产镜像内置编译产物，本地开发用 Vite dev server + `go run`（`FOBE_WEB_DIR` 为空时 server 进 API-only 模式）。

---

## 20. 已接受的已知风险（签字区）

1. ⚠ **AI 默认放行 → 间接提示注入可静默执行任意 root 命令**（§12.4）。缓解路径已设计，改一个配置项即可收紧。
2. ⚠ **登录面只有密码 + IP 黑名单**，无第二因子。黑名单依赖你自备的 nginx 正确传递 XFF、且 `FOBE_TRUSTED_PROXIES` 与实际拓扑一致；**接 CDN 后忘记同步回源网段 = 要么封不到人，要么把真实用户全封了**。
3. ⚠ **共享 anytls 密码**：无法按订阅吊销代理访问，泄漏只能全局轮换（会打断所有客户端）。
4. ⚠ **指标只存 7 天**：7 天以外的曲线不可得（月曲线依赖永久日表，可信；但"上月某天下午的 CPU"查不到）。
5. ⚠ **不做自动停服**：配额超标不会自动止损，完全依赖告警通道可达。
6. ⚠ **agent 自更新不留 `.prev`**（§5.5）：没有本地回滚。旁路自检把"坏产物"挡在提交之前，但**自检过、`-run` 起不来**（只有 run 路径才用到的内核特性/配置）这种残余情形只能 SSH 上去跑面板给的重装命令。你选了省下 OpenWrt overlay 上的 10MB，代价就在这里。
7. ⚠ **Kill Switch on 期间探针不跟随**："一直跟着服务端走"有例外——冻结是全局止损闸，优先级高于跟随。恢复跟随要你手动关掉它。
8. ⚠ **存量探针必须人工重装一次**：今天已装的 agent 二进制里没有自更新代码，服务端下发 target 它也不认识（Go 忽略未知字段，照常跑）。面板按能力位把它标成"需人工重装"，不会自动跟上。
9. ⚠ **服务端版本号成了对外契约**：随便打一个版本号（含把 `VERSION` 改成别的时间戳）就等于让**全部探针换一次二进制**，而降级路径是自动化测试里最容易缺的那条。发版前想清楚这个数字。
10. ⚠ **混版窗口**：升级/降级过渡期一定是混版。承诺是"同大版本内双向兼容（新增字段可选、未知帧忽略）"，**不保证行为等价**。
11. ⚠ **DL 卷是产物单点**：`/srv/dl` 没挂成独立卷（被重建容器清空）或换了机器，全部探针会停在原地并告警——fail-closed 不会把探针搞砖，但也绝不会跟上，直到你把产物补齐。
