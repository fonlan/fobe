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
| 3 | 流量口径 | 整机网卡 `/proc/net/dev`，agent 自动探测默认路由出口并上报全部网卡；面板可选其一 | 配额含系统更新与其他服务流量；需计数器回绕/重启检测 |
| 4 | 使用率语义 | 分母 = 周期配额；4 模式决定取哪个方向 | 每节点必须落库：配额值、流量周期类型、下次重置时间；时区以 agent 上报为准 |
| 5 | Web 终端 | 浏览器 → server → agent 本地 PTY（不依赖 sshd、不存凭据）（2026-09-15 修订：原 SSH + 凭据托管方案废弃） | agent 需要 PTY 权限；协议保留 `mode` 字段仅为滚动升级兼容 |
| 6 | 平台 | x86 Linux + x86 OpenWrt（v1 硬需求） | procd/init.d、musl 静态、flash 写最小化 |
| 7 | 到期/超量 | 只提醒，不自动停服 | 需要告警通道，且没有自动止损 |
| 8 | AI 模型 | OpenAI 兼容 API，自填 base_url + key，服务端加密存储 | key 在服务端；服务端需能出网 |
| 9 | AI 执行权 ⚠ | **默认放行**：AI 自评有风险才弹确认 | 见 §12，间接提示注入可静默拿到探针 root |
| 10 | 构建形态 | server / agent 分开编译；前端**不嵌入 Go 二进制**但**编进生产镜像**；agent 产物进服务端镜像 | 共享 protocol 模块 + 产物分发链路；镜像构建多一个前端 stage；**挂载卷会遮蔽镜像内的 agent 产物，服务端启动时得把它补进卷**（§5.5） |
| 11 | 数据库 | SQLite（WAL），文件挂载在容器外 | 单写者；指标靠保留期控盘 |
| 12 | 订阅模型 | 单用户 + 多订阅，每订阅独立节点集与模板文件 | 模板管理 + 双格式引擎 |
| 13 | 探针凭据 | 每个入站一份密码：服务端在创建它时现生成（16 位随机字母数字 / VLESS 用 UUID），此后跟着该入站的配置走、不轮换（**2026-09-17e 修订：取消全局共享密码**，见 §10.1） | 面板没有"一键全场轮换"；换密码只影响那一个节点 |
| 14 | 流量重置 | 独立周期类型（无 / 按月 / 按年）+ 下次重置时间；填一次自动滚动 | 需处理月末、闰日边界、秒级时间与探针时区 |
| 15 | 缴费周期 | 周期类型（无 / 按月 / 按天 / 按年，2026-09-15 增按年）+ 周期长度 + 下次到期日 + 费用，手动改；**周期长度单位随类型（天/月/年），类型只作记账口径、不参与任何**自动**到期计算**（2026-09-17 修订：唯一例外是编辑页费用输入框右侧新增的「已续费」按钮——一键按当前周期顺延下次到期日，原到期日已过或未填则从今天起算、以原到期日为基准整周期滚到未来，避免月末/闰日钳制逐次漂移；只改表单字段，保存才落库）；**无续费历史**。费用即 `node_billing.note`（2026-09-16 由「缴费备注」更名），自由文本、只展示：编辑页可填，概览卡片贴成标签 | 查不到"上期什么时候交的" |
| 16 | 指标保留 | 明细只存 7 天；另存永久「按天流量」表 | 7 天以外的曲线不可得（月曲线靠日表） |
| 17 | 延迟测量 | 探针**主动**测面板配置的目标；本地测量频率由设置项 `latency.interval_seconds` 控制（默认 5s）、60s 批量上报；ICMP + TCP 握手两种 | 拿不到"用户→探针"的真实延迟 |
| 18 | 登录加固 | 失败 3 次拉黑 IP（持久化）+ CLI 解封；**不做 2FA** | 黑名单依赖 XFF 信任链；无第二因子 |
| 19 | 告警 | Telegram Bot + 通用 Webhook + **飞书**（2026-09-16 增：应用机器人 / 群自定义机器人） | 一条告警要么进 Telegram、要么进飞书或你自己的 Webhook——通道各自独立，配一个是一个 |
| 20 | 接入层 | **不在 fobe 内实现**：外部 nginx 提供 TLS 与反代，证书自备自管 | fobe 不碰证书；你必须让 nginx 传对 XFF 与 Upgrade 头（见 §3 与 README） |
| 21 | agent 自更新 | **一直跟随服务端**：版本不一致就切（含降级），agent 自发、免确认；Kill Switch on 时冻结（§5.5） | 服务端版本号成为对外契约；不保留 `.prev` ⇒ **没有本地回滚**；存量探针必须人工重装一次才进入自动跟随 |

---

## 1. 范围

**v1 做**：单用户面板；探针注册与心跳；硬件/网络指标；流量配额与四种口径；缴费与重置周期；sing-box 安装/更新/回滚/启停；anytls 自签证书服务端；订阅渲染（sing-box / Clash 模板）；Web 终端；延迟测量；IP 与国旗；Telegram/Webhook/飞书 告警；中英双语；明暗主题；AI 助手；**外部接入文档（README 给出可直接复制的 nginx 配置）**。

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
                                        │  ├ /dl → /data/dl│  agent 与 sing-box 产物直出
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
- **server 只绑定回环端口**（`127.0.0.1:8080`），对外唯一入口是你自备的 nginx；fobe 仓库内不含任何反代组件，也不生成/管理证书。**（实现修订 2026-09-15：compose 默认端口映射改为 `8080:8080`——面板明文 HTTP 直达宿主机全接口；要回到"nginx 唯一入口"的形态，把 compose 的 ports 改回 `127.0.0.1:8080:8080` 或用防火墙限制来源。容器内 `FOBE_LISTEN` 恒为 `0.0.0.0:8080`，绑定收敛只在端口映射层做。）**
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
- **实现修订 2026-09-17（凭据重发：节点绑定的注册 token）**：上面那条"凭证不符 → 拒绝注册"有个死角——探针的 `config.json` 一旦被覆盖、清空或随机器搬迁丢失，服务端只剩旧 secret 的哈希，**没有任何入口能把凭据发回去**（明文只存在探针盘上）。真机踩过：一次验证把生产探针的 config 覆盖成测试凭据，节点掉线，唯一出路"删掉节点再添加"还会连带丢掉挂在 `node_id` 上的 7 条端口转发与 1 条账单。现在：
  - `reg_tokens` 增加 `node_id`（`SchemaVersion 12`，增量迁移）：`''` = 通用"添加节点"token（行为不变）；非空 = **绑定该节点**的 token，只由 `POST /api/nodes/{id}/agent/reinstall-command` 签发（面板上是「重装 / 重发凭据」按钮，二次确认），TTL 30 分钟、单次有效。
  - 注册判定：绑定 token 且该节点 `machine_id` 相符 → **不需要旧 secret**，换发新凭据、复用同一节点（审计 `node_reissued`）。`machine_id` **未注册**（探针连 `machine-id` 一起丢了、或换到新主机）→ 同样换发凭据并**接管新 `machine_id`**（审计 `node_reissued`，命令里记 `machine=<new>`）：面板从不暴露 `machine_id`，若在这里拒绝，节点就彻底没有回来的路。`machine_id` 已属于**别的**节点 → `409 machine_mismatch`；被绑定的节点已被删除 → `404 node_not_found`（绑定 token **永不**新建节点）。通用 token 的两条老路径（旧 secret 有效 → `node_rebound`；无凭据 → `409 duplicate_machine`）原样保留。
  - **代价（有意接受）**：绑定 token 就是"该节点的一次注册授权"（bearer）——拿到它的人可以顶掉该节点的凭据并接管其身份（通用 token 做不到，它只会撞 `duplicate_machine`；被占用的 `machine_id` 也仍然拦得住）。因此入口必须二次确认、签发（`agent_reinstall_token`）与使用（`node_reissued`）都写审计，并沿用 30 分钟 TTL + 单次有效；这是"凭据丢了还能救"的对价。
- agent 侧默认落盘：`/etc/fobe-agent/{config.json,machine-id,update-state.json}`（0600，root）。OpenWrt 上 `/etc` 是 overlay，重启保留。**实现修订 2026-09-16（非特权模式）**：`machine-id` 与 `update-state.json` 一律从 `-config` 所在目录派生；因此 `--unprivileged` 的 `/opt/fobe-agent/config.json` 会把全部探针本地状态收进同一个用户可写目录。

### 4.3 登录失败黑名单

- 连续 3 次失败 → 拉黑该 IP；持久化到 SQLite（重启不丢）。
- 真实 IP 来源：`FOBE_TRUSTED_PROXIES` 白名单内上游传来的 `X-Forwarded-For` 最左侧地址；上游不在白名单内 → **忽略 XFF，改用 socket 源地址**（防伪造头绕过或嫁祸）。
- **防自锁**（三条硬规则）：
  1. 回环与私有网段（含 Docker 网段、`X-Forwarded-For` 里的内网地址）**永不加黑**；
  2. 上游代理地址与 `FOBE_TRUSTED_PROXIES` 网段永不加入黑名单；
  3. 黑名单表带 `expires_at`，CLI 可无条件清空。
- 面板提供"当前封禁列表 + 一键解封"页面，不逼你非进容器不可（CLI 是兜底）。

### 4.4 密钥托管

- 主密钥 `FOBE_MASTER_KEY`（32 字节，环境变量 / Docker secret）。用它 AES-GCM 加密：AI API Key、Telegram Bot Token、全局 anytls 密码（§10.1）、订阅模板中的敏感段。（2026-09-15：不再加密任何 SSH 凭据——Web 终端已改走 agent 本地 PTY，见 §11。）
- 未设置主密钥时，服务端**拒绝启动**并打印生成命令（不静默降级为明文）。**（实现修订 2026-09-15：镜像入口脚本 `deploy/docker-entrypoint.sh` 在「未设置」时先行兜底，优先级 env > `FOBE_MASTER_KEY_FILE` > 生成随机 32 字节落盘到数据卷 `/data/.master_key`（0600，重启复用）。服务端的 fail-closed 语义不变——生成失败（如 `/data` 不可写）容器直接退出，绝无明文回退；admin CLI 跳过密钥解析，逃生口永不被堵。代价是密钥与密文同卷，见 §20.12；要分开就显式设置 `FOBE_MASTER_KEY`。）**
- 所有审计写 `audit_logs`：谁、何时、对哪个节点、什么动作、命令原文、来源 IP。

---

## 5. 探针 agent

### 5.1 形态

- 单个静态二进制（`CGO_ENABLED=0`，`linux/amd64`，musl 兼容）——**OpenWrt 与常规发行版同一份产物**。
- **不调用外部命令做采集**：CPU/内存/磁盘/网络全部读 `/proc`、`/sys`、`statfs`。原因：OpenWrt 是 busybox，字段与工具集都可能缺。
- 默认 root 安装（安装 systemd unit/procd 服务、写 `/etc`、ICMP raw socket）；常规 systemd Linux 可选「一次性 root 引导、日常非 root」模式（§5.3）。ICMP 依次尝试 raw socket（root / `CAP_NET_RAW`）与内核 ping socket（`SOCK_DGRAM`，受 `ping_group_range` 控制）；两者皆不可用时才降级 TCP-only，并上报能力位。

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

`--unprivileged` 是可选的 systemd-only 分支：引导阶段仍需 root/sudo，脚本创建 `fobe-agent` 系统用户、将二进制与本地状态装入 `/opt/fobe-agent/`（0750，用户所有），生成 `User=fobe-agent` 且仅带 `CAP_NET_RAW` ambient capability 的 `fobe-agent.service`。已有 root 安装切换时复制 config/machine-id/update-state 以保留同一节点，并停用旧 sing-box 单元（`one-sing.service` 与改名前的 `fobe-singbox.service` 都停，非特权 agent 自己没有 systemctl 权限去清理后者），避免它和非特权 fallback child 抢端口；procd/fallback 明确拒绝此模式。

> **实现修订 2026-09-16（OpenWrt 实机首装暴露，三处）**：① 落盘一律走 `put_file`（`rm -f` + `cp` + `chmod`）而不是 `install -m`——实测 Kwrt 的 busybox 没编 `install` applet，首装在第 3 步直接 `sh: install: not found`；先 unlink 还避开重装时的 `ETXTBSY`（`cp` 原地写正在运行的二进制会被内核拒绝；unlink 让活进程留在旧 inode，由随后的服务 restart 换入新二进制，与上文 enable/restart 陷阱同一语义）。② procd 分支曾把**二进制**误装到 `/etc/init.d/fobe-agent`（`$TMP/fobe-agent` 手误，写出来的 initd 脚本是死代码）——真机后果是拿 ELF 覆盖 init 脚本、`enable`/`restart` 变成带怪参数跑 agent、procd 服务管理整体失效。已改为安装 `$TMP/fobe-agent.initd`，heredoc 去引号让 initd 里的 agent 路径跟随 `$BIN_DIR` 探测结果，不再可能与它分叉。③ procd 分支收尾 `restart` 在**首装**时会打印 `Command failed: Not found`——rc.common 的 restart 就是 stop+start，而 procd 从没见过这个服务，stop 步骤的 `ubus call service delete` 回 `NOT_FOUND`（退出码仍是 0，纯化妆性噪音，agent 实际正常上线）。改为 `stop >/dev/null 2>&1 || true` + `start`：首装输出干净，start 的真实错误仍然可见，重装时依旧真的停掉旧进程。三分支已在 busybox 容器（stub procd/systemd/fallback）里做过首装+重装实跑验证。

### 5.3 服务管理抽象

| 平台 | 检测 | 服务形态 |
|---|---|---|
| 常规 Linux | 存在 `/run/systemd/system` | systemd unit（`fobe-agent.service` / `one-sing.service`，后者与 one-sing.sh 同名，§9.3） |
| x86 OpenWrt | 存在 `/sbin/procd` 或 `/etc/rc.common` | `/etc/init.d/fobe-agent`、`/etc/init.d/sing-box`（procd，`USE_PROCD=1`） |
| 兜底（容器/WSL） | 两者皆无 | 前台进程 + pidfile + 看门狗 |

`sing-box` 的服务文件由 agent 自己写（面板只下发期望状态），这样才能保证不同平台一致。

> **实现修订 2026-09-15（检测谓词用错，systemd 全被误判为 fallback）**：`service.Detect()` 用 `fileExists("/run/systemd/system")` 检测第一行，而同一个包里的 `fileExists` 语义是"存在**且不是目录**"（它对 unit 文件/二进制/配置是对的）——`/run/systemd/system` 恰恰是 systemd 自己建的**目录**，于是**每一台 systemd 机器（所有常规 Debian/Ubuntu VPS）都返回 `KindFallback`**。这不是"这台没有 supervisor"，是压根没检测到 supervisor，症状分散在三处、互相看起来无关：
> - 当时的 `hello.Caps` 报 `systemd=false / fallback=true`，旧版 §5.5 又把自更新错误绑定到 supervisor，于是 `self_update=false`，面板把这台"其实一直跑在 systemd 下"的探针标成"不支持自更新 / no service manager to restart the agent"；2026-09-16 改为 `exec` 后能力位已只看 Linux、可解析自身路径和二进制目录可写；
> - sing-box 落到 **spawn 兜底分支**（agent 自己 fork + reap）而不是自己写的 systemd 单元（当时的 `fobe-singbox.service`，2026-09-16 起叫 `one-sing.service`，见 §9.3），于是它不受 systemd 管、开机不自动起；
> - 面板的 sing-box 启停/重启（`commands.go` 与 §9.x 动作）一律回 `no service manager detected`。
>
> 修法：检测存在性用新加的 `pathExists()`（`os.Stat` 成功即可，文件或目录都算），`fileExists()` 保持"文件"语义并只用于它该用的地方；`Detect()` 拆出 `detectAt(root)` 以便用假根目录单测（`internal/agent/service/detect_test.go` 钉住"目录必须算存在"）。**同批修掉被这次修正才第一次走到的 native 分支的两个潜伏问题**：① `singboxManager.start` 改用 `restart`（`systemctl start` 对已在跑的服务是 no-op，改了 config 也不会重载，而闸门③只看到"端口有人应答"；这与 §5.5 里 install.sh 的 `enable --now` 陷阱是同一类）；② 迁移到 native 前用 `dropOrphan()` 清掉 fallback 分支遗留的孤儿 sing-box——它被 reparent 到 init 后仍占着入站端口，会让新 unit 起不来、`Restart=always` 反复重启，闸门③却去怪新版本。另外 `InstallSingbox` 补上 `systemctl enable`：unit 文件里的 `WantedBy` 不会自己创建 `multi-user.target.wants` 符号链接。
>
> **实现修订 2026-09-16（非特权运行）**：`euid != 0` 的 agent 不能写 `/etc/systemd/system`，也不能经 system bus 管理 unit（会被 polkit 拒绝），因此 sing-box **显式强制**走既有 fallback 分支（agent `spawn` + watchdog），而不是把宿主的 `caps.systemd=true` 错当成自己可用的管理权限。面板端口本来就限制为 10000–60000（§9.3），非 root 可直接绑定；工作目录从 `-config` 同级的 `one-sing/` 派生，`FOBE_SINGBOX_HOME` 可显式覆盖，服务端下发 JSON 里的 `/etc/one-sing` 证书前缀在 agent 写盘、哈希比对时同步重定位。root 的 legacy 布局迁移与 `one-sing.service` 接管跳过：前者只会产生 EACCES，后者无权执行；已有 root 服务占端口时由闸门③如实失败上报。`install.sh --unprivileged` 是此形态的唯一受支持引导路径（§5.2）。

### 5.4 OpenWrt 专项

- 二进制与配置统一放 `/etc/one-sing/`（`sing-box` + `config.json` + `cert/`），与 one-sing.sh 同一套路径（实现修订 2026-09-15，见 §9.3）；`/etc` 在 OpenWrt 上同样是 overlay 持久。
- **写放大控制**：指标与日志不落盘（内存环形缓冲），仅上报；错误日志按行数上限写 `/tmp`（tmpfs）。
- 内存：agent 目标常驻 < 30MB；采集周期在低内存设备上可从 60s 放宽（面板可配）。
- 首次运行检测 overlay 剩余空间，低于阈值时拒绝安装 sing-box 并给出提示（避免把路由器写满）。**实现修订 2026-09-16（阈值必须随产物大小走）**：原先只查一个平坦的 64 MiB，而 agent 要写的 sing-box 二进制自 1.14 起已近 90 MiB（见 §9.2 实现修订）——闸门等于放行一个必然写满 overlay 的安装。现在分两道：收敛前的粗筛仍查 64 MiB / 100 inode（此时还不知道产物多大），拿到响应头后按 `64 MiB + 产物` 复核；**替换已有二进制时再加一份**，因为它要被留成 `.prev` 供回滚（与 §5.5 的 2× 规则同源，只是首装没有那份 `.prev`，不该为不存在的副本拒绝）。空间扫描不到时一律放行（fail open）。**实现修订 2026-09-17e（inode 半闸只对"有 inode 预算"的文件系统生效）**：btrfs 等动态分配 inode 的文件系统 `statfs` 上报 `f_files = f_ffree = 0`（`df -i` 显示 0 0，是"无预算"不是"用尽"），原判定把 8 GiB 空闲的 btrfs 数据卷判成 0 inode 可用而拒绝安装（面板报 `insufficient disk space on /etc/one-sing: 8375 MiB / 0 inodes free`）。现在 statfs 一并取总 inode 数：为 0 视为该文件系统不跟踪 inode、跳过 inode 半闸（字节闸不变），错误信息也不再展示无意义的 inode 数字；真实耗尽的 ext4/f2fs（总 inode > 0 且空闲 < 100）照旧拒绝。

### 5.5 agent 自更新（跟随服务端）（实现修订 2026-09-15）

> 目标：**探针 agent 一直跟着服务端走**——服务端换了版本，探针就换成与之匹配的那一版，**不看高低**（降级与升级共用同一套逻辑）。本节取代 §9.2 里"agent 自更新走同一条链路"那句：自更新**不复用 sing-box 的三道闸门**（sing-box 有 `check` 与 30s 观察期，agent 没有可比的"先验后启"手段），换来的是**旁路自检 + 原子提交**，且**不保留 `.prev`**。

- **判据只有一个：服务端版本号**。服务端在 `hello_ack` 里下发 `agent_target_version`，agent 拿它和自己的 `internal/agent.Version` 比，不等就切换（含降级）。两端版本由构建时同一个 `$VERSION` 注入（`cmd/server` 的 `main.version` 与 `internal/agent.Version`），所以"服务端版本"就是"该配哪一版 agent"。
- **发布门槛（fail-closed）**：仅当 ① 服务端版本是发布形态（非空、非 `dev`）且 ② `<FOBE_DL_DIR>/agent/<version>/{linux-amd64,linux-amd64.sha256}` 都在时，才下发 target。任一不满足 → 不下发，面板显示停用原因。理由：dev 构建没有"发布"概念，而"下发一个取不到的版本"只会让每台探针反复重试 404。
- **触发 = agent 自发，不需要任何人确认**：agent 每次握手看到不一致就自己动手。这是**系统行为**，不属于 §12.3 的"元操作"清单；**Kill Switch on 时服务端不下发 target**，因此"永远跟随"在这段时间让位给止损闸。
- **错峰由服务端编排**：服务端重启会让全部探针同时重连、同时发现不一致，因此 `hello_ack` 里带 `agent_update_after`（unix 秒），服务端按 `hash(nodeID + target)` 在 0–5 分钟内给出确定性偏移（同一 node+target 稳定，target 变则重排），并把**锚点**写进 `nodes.agent_update_planned_at`（它是 `StaleAgentUpdates` 的"分发后 15 分钟"判据与跨重连稳定性的依据）。⚠️ **面板显示的「计划时刻」= 锚点 + 该节点偏移（即真正允许开始的时刻），不是锚点本身（实现修订 2026-09-16）**：锚点是服务端第一次发现不一致的那一刻，重启后**对每台探针都是同一秒**，照它显示会让正确的错峰读起来像"卡在计划中 / 集体迟到"；节点视图因此新增 `agent_update_after`（`agentupdate.PlannedStart`），前端只渲染它，`agent_update_planned_at` 留在接口里仅供诊断（与审计时间线对表）。**这不是概率问题**：不错峰就是 N 台同时拉 10MB，而那一刻服务端刚起来。
- **执行链**（agent 侧，收到 target 且已过 `update_after`）：
  1. 下载到**目标二进制所在目录**里的临时文件（同目录才能 `rename`，跨文件系统会 `EXDEV`）；目标路径由 `os.Executable()` 解析（跟随符号链接）——不猜 `BIN_DIR`，systemd 装的是 `/usr/local/bin`、OpenWrt 是 `/usr/bin`；
  2. 与 `linux-amd64.sha256` 比对 sha256，不符即 `terminal` 失败；
  3. 以 `-selfcheck` 跑一次**新二进制自己**：用同一份 config 连服务端，`hello` 带 `selfcheck:true`，hub 只回 `hello_ack` 后立即关闭——**不注册、不落库、不顶掉线上连接**（`hub.go` 的重复注册会 `old.close()`：天真地握手会把正在跑的 agent 踢下线，还会让面板显示一个并没生效的版本号）。自检还必须校验 `hello_ack.agent_target_version`：非空且不等于自己的 `Version` 即失败，挡住"目录名与二进制内版本错位"这类产物错放（对方为空说明服务端此刻已不再下发 target，那是环境变化而不是这个产物的问题，不作为失败）；超时 60s；
  4. 自检通过 → `rename` 覆盖目标二进制 → **`exec` 原地换入**新版本。PID 不变，systemd/procd 若存在只负责崩溃恢复；Go 默认 CLOEXEC 关闭旧 socket，`main()` 重新初始化连接与状态。**不保留 `.prev`**：不留就没有本地回滚，代价是"自检过但 `-run` 起不来"这种残余情形只能 SSH 重装（§20）；换来的是 OpenWrt overlay 上少 10MB 常驻占用；
  5. 自检或下载失败 → 线上二进制**一动不动**。
- **fallback 也参与**（实现修订 2026-09-16）：`exec` 不依赖 supervisor，`nohup`/容器 fallback 只要二进制所在目录可写就能跟随；若 exec 真失败（ENOEXEC/权限），才回退旧语义 `exit(0)`，交给可能存在的 supervisor。
  ⚠️ **`unsupported` 是能力判定，不是失败（实现修订 2026-09-15）**：它 `attempts=0`、什么都没试过，所以服务端**不为它建失败告警**（并顺手 recover 掉该节点既有的失败/环境告警，否则一条永远无法自愈的 high 风险告警会长期挂在面板上），`audit_logs` 的 risk 记 `low`；面板把它渲染成中性提示（`agent_update_reason`）而不是红字"上次错误"，并按「目录不可写/无法解析自身路径」与「旧二进制未上报能力位」给出不同的重装提示文案。`caps.fallback` 仅描述宿主的服务管理环境，不再决定自更新能力。
- **能力位与失败分类（两本账）**（实现修订 2026-09-16）：`hello.Caps.self_update` 不再判「是否发现 supervisor」，而是判「linux + 可解析自身二进制 + 该目录可写」（`rename` 看目录权限，不看二进制文件自身 mode）；`unsupported` 原因如实报告「无法定位自身二进制 / 目录不可写」。
  - `terminal`（不再重试，等 target 变更或人工重试）：sha256 不符 / 无法 exec / 自检报版本不符；
  - `transient`（退避 1m → 1h 封顶，不计入熔断）：连不上服务端、`/dl` 404/5xx、同目录剩余空间 < 2×产物、目标不可写。
- **熔断双记账**：agent 本地 `update-state.json`（默认 `/etc/fobe-agent/`，非特权模式与 `-config` 同目录）记 `target / attempts / last_error / class`（跨 re-exec 有效），**同一 target 连续 3 次 `terminal`** 就停手并上报；服务端 `nodes` 表同样记一份（面板可见 + 人工解锁），target 变化时两边计数清零。本地那份是唯一能在"替换无效、反复重启"时救命的账，服务端那份负责可见性——**只留一边都会在某个场景下失效**。
- **状态与面板**：`nodes` 增 `agent_target_version / agent_update_state / agent_update_attempts / agent_update_error / agent_update_planned_at / agent_update_done_at`（`migrateAdditive`，幂等）；节点页显示 当前版本 / 期望版本 / 计划时刻（= `agent_update_after`，锚点 + 偏移，见上一条修订） / 上次结果与原因；`POST /api/nodes/{id}/agent/retry` 清计数（人工解锁）。每次尝试写 `audit_logs`（`actor=system`）。
- **告警三档**：`terminal` 立即；`transient` 连续 3 次；分发后 15 分钟仍未收敛的节点**汇总一条**（与 §9.5 同一口径与去重窗口）。
- **存量探针只能人工重装一次**：今天已装的 agent 二进制里没有这段代码，服务端下发 target 它也不认识（Go 忽略未知 JSON 字段，它会照常跑）。判定靠**能力位而非版本号猜测**：新 agent 在 `hello.Caps` 里报 `self_update=true`，不报的一律在面板标"需人工重装（不支持自更新）"并给出重装命令。**不许假装它会自动跟上。**
- **产物供给**：镜像里**不能**把 agent 产物放在 DL 目录——compose 把 `./data` 挂到 `/data`，`FOBE_DL_DIR=/data/dl` 在卷内，挂载在容器启动时就已生效，镜像里 COPY 进卷的东西**根本不可见**（不是"被覆盖"，是读不到）。因此镜像把产物放在卷外的 `/srv/agent-seed/agent/<version>/`，服务端启动时**复制进 DL 卷**（缺什么补什么，已有则不覆盖；`FOBE_AGENT_SEED_DIR` 可改，置空即关闭；与 §9.5 的 sing-box 缓存互不影响）。同一个启动步骤还会把卷里的 `agent/latest` 指向**服务端自己这一版**——否则升级容器后 `/install.sh` 会继续装上一版的 agent（`latest` 是本地脚手架留下的真实目录时不动它）。不这么做，"跟随服务端"在标准 compose 部署里默认就是 404。
- **混版承诺**：同一大版本内双向兼容（新增字段一律可选、未知字段/帧忽略并记日志）。升级/降级过渡期必然是混版，"版本不匹配就拒绝"会把探针直接锁死。`protocol.Version` 保持 `1`。
- **开关**：`settings.agent.auto_update`（默认开，面板可关）。关掉或 Kill Switch on = 不下发 target。AI 侧**不新增任何工具**（§12.2）：状态随既有节点查询返回，触发/冻结/解锁/重试都只能由人在面板操作。
- **面板与接口**（全部走会话鉴权，`GET /api/agent/update` 集群状态、`POST /api/nodes/{id}/agent/retry` 人工解锁、`POST /api/nodes/{id}/agent/reinstall-command` 生成一次重装命令）。节点视图新增 `agent_target_version / agent_update_state / agent_update_attempts / agent_update_error / agent_update_planned_at / agent_update_done_at` 与能力位 `agent_self_update`（外加 `agent_caps_seen`：没握过手的节点不算"不支持"，否则新建节点会被误标重装）；`agent_update_state` 取 `planned|downloading|verifying|committed|failed|transient|suppressed|unsupported`。
- **"重试"要能推动在线的探针**：`hello_ack` 之外的载体是普通 `desired` 帧（`DesiredState` 里带上同样两个字段），因此解锁不必等下一次握手；服务端由同一个函数同时填两个载体，不可能填出不一致的两份。
- **两个必须失败关闭的点**（都在实现里显式处理）：① 临时文件必须落在**目标二进制同目录**再 `rename`（跨文件系统 `EXDEV`；`open+truncate` 覆盖正在运行的文件会 `ETXTBSY`）；② 同目录可用空间 < 2×产物即判 `transient` 拒绝，宁可不动也不能写半个二进制。
- **dev 环境怎么让"跟随"真的可验证（实现修订 2026-09-15）**：设计上 `dev`/`compose` 不是发布形态，而两端版本号又恒等（判据是"相等"，不是"新旧"）⇒ 测试环境**永远判已收敛**，这条链路在 dev 里一次都跑不到；"把 `IsReleaseVersion` 对 dev 放开"是死路——等值判定在门槛之前，两端都是 `dev` 照样什么都不发生，还会让面板把陈旧探针显示成已收敛。`scripts/dev.sh` 因此自己产 agent 产物、并给自己一个**内容寻址**的版本号：先用固定占位版本 `dev-pending` 交叉编译一份 agent，对产物取 sha256 前 12 位作为 `dev-<fp>`（全字母时补一位数字，保证过 `IsReleaseVersion`），再用这个号正式编一次（Go 对相同输入确定性：实测同 flags、不同输出路径两次构建哈希一致）。产物落 `data/agent-seed/agent/<版本>/`（`FOBE_AGENT_SEED_DIR`），由 server 启动时的 `Seed` 复制进 DL 卷——和容器同一条路径，顺带覆盖"挂载卷遮蔽"。
  **为什么不是时间戳**：AGENTS.md 要求"改了后端必须重启 `dev.sh`"，时间戳会让**每次后端热改都触发全网探针下一次 10MB 并重启一次**，`data/dl/agent/<版本>` 还会每次多一个 ~10MB 目录。内容寻址把触发条件收紧成"agent 真正编出来的东西变了"（改 `internal/protocol` 这类两端共用包也会换号，这是正确的）；**纯注释/格式改动不换号**——二进制没变，探针本来就不该动（实测：改一个日志字符串换号，还原后回到原号）。
  **语义后果**：版本号是**占位构建**产物的哈希，不是交付产物的哈希——它与 `linux-amd64.sha256`（交付产物的摘要）没有推导关系，别互相校验。`FOBE_VERSION=<旧号>` 可显式钉住版本号 ⇒ 降级演练就是"用旧号重跑 dev.sh"（旧产物还在 DL 卷里，`Seed` 不覆盖）。
  **compose 本地构建同样跟进（实现修订 2026-09-16）**：`--build` 形态此前缺省注入恒定的 `compose`，同样过不了 `IsReleaseVersion`，且等值判定下两端都叫 `compose` 永远"已收敛"——本地构建部署的探针**永不**跟随服务端。`Dockerfile.server` 的 golang 阶段现在对占位 VERSION（空/`dev`/`compose`）做与 dev.sh 同样的事：先用 `<base>-pending` 编一份占位 agent、取 sha256 前 12 位（全字母时前置 `0`），得到 `compose-<fp>`（不带 build-arg 直接 `docker build` 时是 `dev-<fp>`）；显式传入的真实版本（发布管线传 tag）原样通过。于是"改 agent 代码 ⇒ 重新 `--build` ⇒ 换号 ⇒ 探针自更新"在容器形态也成立；agent 源码没变的重 build 不换号、不折腾探针。
  **占位号不再带前缀（实现修订 2026-09-17）**：面板把版本号当徽章挂在品牌标题旁之后，`compose-`/`dev-` 前缀成了纯噪声——本地构建的占位号现在是**裸 `fp`**（不带 build-arg 的直接 `docker build` 同样）。`IsReleaseVersion` 判据不变（只要求含数字，`0` 前置规则保证这一点），§5.5 跟随行为照旧；占位构建仍嵌 `<base>-pending`，所以同一份源码的指纹与带前缀时代一致——升级只换版本字符串，探针做一次自更新就收敛。dev.sh 的 `dev-<fp>` 未动（它有自己的构建脚本，前缀仍用于在告警/日志里区分来源）。
  ⚠️ **一次性动作**：`data/dl/agent/latest` 若是**真实目录**（本地手工 staged 的产物），`ownedLatest` 判定"不是我们的布局"、永不重指，而 `/install.sh` 固定取 `dl/agent/latest/linux-amd64`（重装命令走的也是它）——于是"重装"会把你装回那份旧二进制，人却在困惑它为什么永远不跟随。删掉它，让 `pointLatest` 建符号链接布局。
- **错峰可参数化（`FOBE_AGENT_UPDATE_STAGGER`）**：默认仍是 5 分钟；1–2 台探针的测试机设 `1s` 即"立即"（偏移取 `hash%span`，span=1s 时恒为 0）。理由是"还没到计划时刻"在面板上与"功能没生效"长得一模一样。≤0 视为未设——`agentupdate.New` 会把非正值回落到默认 5 分钟，静默吃掉 `0` 本身就是一个陷阱。生产不设。
- **重装命令必须让新二进制真的上场（2026-09-15 修订）**：`install.sh` 的 systemd 分支原本收尾是 `systemctl enable --now`，procd 分支是 `/etc/init.d/fobe-agent start`——**对已经在跑的 agent 两者都是 no-op**。而面板的「重装命令」恰恰是为"已经在跑、只是版本旧"的节点准备的（§5.5 存量探针路径），于是它只把磁盘上的文件换掉，进程仍跑着被替换掉的旧 inode：`strings $(command -v fobe-agent)` 显示新代码（用户以为装好了），`systemctl status` 的 `Active since` 却远早于这次安装，节点永远报旧版本、期望版本箭头永不消失。改成 `enable` + `restart`（`restart` 对未运行的单元等同启动，首次安装同样覆盖），procd 侧同样用 `restart`；fallback（`nohup`）分支则先 `pkill -f "^$BIN_DIR/fobe-agent"` 再起，否则重装会留下两个 agent 抢同一个节点凭据（hub 会把先连上的那个顶掉）。排查口径：`systemctl status fobe-agent` 的 `Active since` 是否是刚刚；与 `/proc/$(systemctl show -p MainPID --value fobe-agent)/exe` 里的版本号对照磁盘上的二进制。

---

## 6. 数据模型（SQLite，WAL）

| 表 | 关键字段 | 说明 |
|---|---|---|
| `users` | id, password_hash | 单行 |
| `sessions` | id, created_at, last_seen, ua, ip, revoked | 可批量吊销 |
| `ip_blacklist` | ip, reason, fail_count, created_at, expires_at | 持久化 |
| `settings` | key, value, encrypted | 全局 anytls 密码（服务端自动生成，§10.1）、AI 配置、Telegram、保留期、延迟测量频率（`latency.interval_seconds`，默认 5）等 |
| `reg_tokens` | token_hash, note, expires_at, used_at | 单次 |
| `nodes` | id, name, machine_id, node_secret_hash, status, last_seen, agent_version, os, arch, kernel, distro_id, distro_version, cpu_cores, primary_ip, country_code, sub_name, tz；**自更新（§5.5）**：agent_target_version, agent_update_state, agent_update_attempts, agent_update_error, agent_update_planned_at, agent_update_done_at | 探针主表；`tz` 与发行版（os-release 自动探测，§16）由 agent 上报，只读；`sub_name` 是 §10.2 的订阅展示名 |
| `node_ips` | node_id, ip, family, scope, is_primary, manual_primary | 多 IP 全量上报 |
| `node_interfaces` | node_id, name, is_default, updated_at | agent 上报的可选网卡清单与默认路由标记 |
| `node_network` | node_id, iface, mode(in/out/both/max), quota_bytes, cycle_type(none/month/year), next_reset_at | 流量口径、配额与独立流量周期；空 `iface` 表示 agent 自动选择默认路由 |
| `node_billing` | node_id, cycle_type(none/month/day/year), cycle_days(单位随 cycle_type), next_due_at, note | 缴费周期 |
| `traffic_counters` | node_id, iface, direction, last_raw, last_ts, period_start, period_used | 回绕/重启检测 |
| `traffic_daily` | node_id, date, rx_bytes, tx_bytes | **永久**，月曲线与配额靠它 |
| `metrics_samples` | node_id, ts, cpu, mem_used, mem_total, disk_used, disk_total, net_rx_rate, net_tx_rate, uptime | 保留 7 天 |
| `latency_targets` | id, name, kind(icmp/tcp), host, port | 面板配置的目标 |
| `node_latency_targets` | node_id, target_id | 每探针选用的目标集 |
| `latency_samples` | node_id, target_id, ts, icmp_ms, tcp_ms, loss | 保留 7 天 |
| `subscriptions` | id, name, token_hash, format, template_id, enabled, ua_filter | 多订阅；`format` 空 = 自动（2026-09-16 实现：见 §10 修订），`token_enc` 见 §10 |
| `subscription_nodes` | subscription_id, node_id | **旧直连投影**（§10.2）：`subscription_entries` 里 `relay_node_id=''` 且启用的那半边，双写以保降级可读 |
| `subscription_entries` | subscription_id, node_id, relay_node_id, proto, src_port, iface, alias, enabled | §10.2 的订阅绑定单位 = 入口；`relay_node_id=''` 为直连，`enabled=0` 是**墓碑**（取消勾选，reconcile 不再加回） |
| `templates` | id, name, format(singbox/clash), content | 完整配置模板 |
| `node_singbox` | node_id, version, desired_version, desired_uninstall, config_hash, status, last_error, cert_pem, cert_sha256, port | sing-box 期望/实际状态；`desired_uninstall` 是面板的卸载意图（§9.2 实现修订 2026-09-16），探针回报 `absent` 后清零 |
| `commands` | id, node_id, kind, payload, status, created_at, sent_at, finished_at, result | 指令队列 |
| `audit_logs` | ts, actor, node_id, action, command, risk, source_ip, ai_session_id | |
| `alerts` | id, kind, node_id, payload, created_at, delivered_at | |

索引要点：`metrics_samples(node_id, ts)`、`latency_samples(node_id, target_id, ts)`、`traffic_daily(node_id, date)`。

**保留策略**：定时任务每 10 分钟删除 `metrics_samples`/`latency_samples` 中超过 7 天的行，并 `PRAGMA incremental_vacuum`。

> **Schema 兼容策略（实现修订 2026-09-16，起因是宿主机重启导致主库页级损坏、settings 全丢的事故）**：
> - **只做增量是铁律**：schema 变更只允许「新表 / 带默认值的新列 / 新索引」（`schema.sql` 的 `IF NOT EXISTS` + `migrateAdditive`，幂等）。所有查询**显式列名**，这样新表/新列对旧二进制是惰性的——旧版本直接打开新库也能服务，**降级不需要迁移**。
>   **唯一的例外（2026-09-16）**：删掉 `node_singbox.rollback_version`。它从来没有任何写入方（面板渲染的「可回滚到 v」永远不可能出现），留着一个死字段比破一次铁律更糟。代价写清楚：跨过这条线**降级不安全**——旧二进制按名字 SELECT 这一列会直接报错，而 `SchemaVersion` 3→4 的 bump 保证每个现有库在迁移前先拍一张快照（§19.3），操作员要回退就用那张。`migrateAdditive` 里删列单独成段并注明原因，别把它当常规手段。
> - **版本追踪**：`PRAGMA user_version` ↔ `store.SchemaVersion`，改 schema 必须同时 bump。`db < build`（升级）：先自动快照再跑迁移、写版本号；`db > build`（降级，§5.5 钉版本演练是真实场景）：只打 WARN、**不迁移、不回写版本号**——本次构建不知道新版本做过什么，重置版本号会让半知的升级重跑。
> - **打开即 `quick_check`**：损坏的库拒绝启动（容器进入重启循环、日志给出恢复路径），而不是像事故里那样全站随机 500、每次写入把洞挖深。
> - **`synchronous=FULL`**（原 NORMAL）：WAL+NORMAL 是常规推荐，但库文件在 Docker bind mount 上，fsync 顺序只与宿主文件系统一样可靠（OrbStack/virtiofs 等）。FULL 每提交一次 fsync，本面板写频是秒级批次、代价测不出来；换来崩溃时最多丢最后一次提交而不是文件完整性。
> - **快照默认开启**：启动即拍 + 每 24h 一轮 + 迁移前必拍（时间戳命名，与每日快照共用 3 份池，§19.3 复核 2026-09-16：库体量随节点数增长，池子刻意压小）；`FOBE_BACKUP_DIR` 缺省 `<db 目录>/backup`，`off` 显式关闭。事故复盘：备份循环此前"配置着"但用 24h ticker、首次触发要等满一天，部署 churn 下容器从未活满——备份一次没跑过，坏的是「有机制、无快照」这种假安全感。

---

## 7. Agent ↔ Server 协议

传输：单条 WSS（`/ws/agent`），JSON 信封，版本化：

```json
{ "v": 1, "type": "metrics", "id": "uuid", "ts": 1730000000, "payload": { } }
```

**核心设计：声明式期望状态（desired state）**。服务端只下发"我要什么"（sing-box 版本、配置、anytls 参数、延迟目标集、终端开关），agent 负责收敛并回报实际状态。好处：agent 离线重连后自动补齐，服务端不需要重放命令序列。

| 方向 | type | 说明 |
|---|---|---|
| agent → server | `hello` | machine_id、版本、os/arch、时区、发行版（os-release 的 `distro_id`/`distro_version`，2026-09-16）、网卡清单与能力位（含 `self_update`，§5.5） |
| | `agent_update` | 自更新逐次上报：phase / class(terminal\|transient) / error / attempts（§5.5） |
| | `metrics` | 60s 一次；面板打开详情页时服务端可请求 5s 实时流 |
| | `traffic` | 60s 一次：选定网卡的累计计数 + 日增量 |
| | `latency` | 60s 一次批量；本地采样点数量按 `latency.interval_seconds`（默认 5s）变化 |
| | `state` | 节点信息、IP/网卡清单、sing-box 实际状态、nftables 端口转发清单（§21，`forwards`，**旧 agent 不带该字段**） |
| | `cmd_result` | 指令执行结果（stdout/stderr/exit code，截断） |
| | `terminal` | 终端输出/关闭 |
| server → agent | `hello_ack` | 期望状态全量下发（含流量网卡选择、`agent_target_version` / `agent_update_after`，§5.5） |
| | `desired` | 增量下发期望状态（流量网卡、sing-box 版本/配置/端口/密码/证书要求、以及 `singbox.uninstall` 卸载声明，§9.2 实现修订 2026-09-16） |
| | `cmd` | 一次性命令（AI 执行、面板操作、§21 端口转发编辑：`nft_forwards`，结果为 stdout 里的 `ForwardsResult` JSON） |
| | `terminal_open/input/resize/close` | 终端会话 |
| | `probe_metrics` | 请求临时高频指标采集 5s |
| | `latency_config` | 立即更新本地延迟测量频率（全局设置变更时广播）；带 `targets` 数组时替换该节点的测量目标（面板编辑/删除延迟测量后即时下发，§13 实现修订 2026-09-17k） |

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
| 发行版 | `/etc/os-release`（回退 `/usr/lib/os-release`）的 `ID` + `VERSION_ID`；`ID_LIKE` 含 `openwrt` 或存在 `/etc/openwrt_release` 时归入 openwrt 家族（iStoreOS/ImmortalWrt 等衍生版改写 `ID`，靠这两处识别）（2026-09-16，§16） |

网卡通过 `/proc/net/dev` 与 `/proc/net/route` 探测；agent 将完整可选清单及默认路由出口随 `hello`/`state` 上报。面板的「自动」不猜网卡，始终由 agent 选择默认路由；人工选择只允许该探针已上报的网卡，并以 `desired` 即时下发到 agent。时区同样由 agent 自动上报到 `nodes.tz`，面板只读显示，不允许人工覆盖。

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
- **流量周期独立于缴费周期**（2026-09-16 修订）：每节点设置 `none|month|year` 与「下次重置时间」（Unix 秒，表单精确至秒）。`none` 不隐式回退到自然月，按累计流量计算；月/年模式将已到期的下次重置时间依探针时区向前滚动，保留设置时的日、时、分、秒。月末和 2 月 29 日落到不存在日期时钳制到该月最后一天，下一次仍以原始日期规则计算。缴费周期仅用于到期提醒，与流量统计互不驱动。

---

## 9. sing-box 生命周期与 anytls

### 9.1 期望状态

服务端为每个节点生成完整 `config.json`（inbound + log + 必要的出站），存 `config_hash`；面板改任何参数 → hash 变化 → 下发 → agent 应用。

> **实现修订 2026-09-16（生成的配置要能自己追上新模板）**：每台被管理的节点，其 `config.json` 只在**变更时**生成一次，然后作为 `singbox_config:<node_id>` 缓存在设置里，之后没有任何东西重新推导它。生成器稳定时这没问题，但**生成器本身改了**（§9.4 那次：地址式 DNS 段必须去掉）就出问题了——存量节点会永远收到那份陈旧字节，而 agent 只比 hash（磁盘 == 期望）所以没有任何理由重写文件，**唯一出路是操作员再点一次"安装"**。因此服务端启动时多一步 `SyncSingboxConfigs()`：遍历所有 `desired_version` 非空的节点，用当前模板重新生成，**只有字节不同才**写回 setting + 刷新 `config_hash` + 下发（写 `audit_logs`，actor=system）；幂等，普通重启一个字节都不动。端口非法（未分配/越界）的节点跳过并记 WARN——那不是这一趟的事；密码缺失也不再是跳过理由（没有就现生成，见 §10.1 实现修订）。

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

> **实现修订 2026-09-16（下载闸门的尺寸预算 + "安装中"不再卡住）**：起因是一次真实的安装失败——告警里是 `download 1.15.0-alpha.4: artifact exceeds 67108864 bytes (rolled back)`。
>
> - **产物上限必须按真实产物定**：`/dl/singbox/<version>/linux-amd64` 直出的是**解包后的单文件二进制**（不是 33 MB 的 tar.gz），而官方 linux-amd64-musl 构建自 1.14 起已近 90 MiB（1.14.1 = 91,891,552 B、1.15.0-alpha.4 = 92,895,232 B）。agent 侧的 64 MiB 上限因此**否掉了当时每一个版本**，而且是在**把整个文件传完之后**才拒绝——探针白付一次带宽、再回滚，面板上只看到"安装中 → 异常"。上限提到 256 MiB（与 §9.5 服务端 `defaultMaxDownloadBytes` 对齐），并**先读响应头再决定要不要接收**（`Content-Length` 触顶直接拒绝，不再下载一遍）。整包下载预算 5 min → 15 min：5 分钟等于要求 90 MiB 在 >2.5 Mbit/s 的链路上完成，远程探针达不到时的现象与"产物被拒"一模一样（下载错 → 回滚）。
> - **空间闸门随产物大小**（§5.4）：见该节的实现修订。
> - **失败原因在上报里是粘性的**：退避窗口（5 min）内的周期性 `reportOnly` 原本把 `last_error` 报成空串，而服务端只在 `running` 或"本次带错误"时改 `status` ⇒ 节点长期停在 `degraded` 却没有任何原因可看。现在 agent 在**尚未健康**（进程未在跑，或版本仍不是期望版本）期间继续上报上一次的失败原因，跑起来且版本正确、或期望版本变更时才清空（新的失败自然覆盖旧的）。"版本对了"不等于收敛——闸门①拒绝时二进制已经落盘，只看版本会把这个错误当成历史清掉（真机上就是这么又丢了一次原因）。
> - **门后面的坑不在门上**：修完尺寸上限，同一台探针立刻在闸门①倒下——生成的 config 用了 1.14.0 已移除的地址式 DNS 段，见 **§9.4 实现修订 2026-09-16**。这条链路的失败是一个接一个暴露的，别看到第一个错误消失就收工。
> - **面板不能对着一次快照发呆**：节点编辑页的 sing-box 卡片原先只在挂载与操作后各读一次，而收敛是异步的（agent 收到期望状态、执行、再上报）⇒"安装中 + 期望状态已下发,等待探针应用并回报"会一直挂在屏幕上，哪怕探针几秒后就已经报回失败。现在 `status=installing` 且探针在线时每 5s 重读一次（离线时改为如实显示"探针离线,期望状态已记录"，因为状态不可能变），收敛或失败后自动清掉"已下发"的提示。

- 版本**必须显式指定**（面板展示可选版本，来自服务端 release 清单），不追 latest。一键批量更新里的 `latest` 只在**点击那一刻**解析成具体版本号并固化后下发，不变量不被破坏（见 §9.5）。
- 产物来源：优先面板 `/dl/singbox/<version>/linux-amd64`（服务端缓存，规避国内拉 GitHub 的问题），失败回退官方地址。**缓存由服务端进程自己填充**（启动时/手动重试，见 §9.5）——agent 侧只拉面板、不补官方回退。
- agent 自更新**不走**这条链路（实现修订 2026-09-16）：agent 没有 `check` 与 30s 观察期可用，改用「旁路自检 + 原子提交 + `exec` 原地换入」，且**不保留 `.prev`**——详见 **§5.5**。
- 每次变更写 `audit_logs`，并在面板节点页显示"当前版本 / 期望版本 / 上次操作结果"。

> **实现修订 2026-09-16（闸门①在所有"要启动 sing-box"的路径上都跑）**：闸门①原本只在**变更路径**（`apply`）里执行；"版本与配置都已就位、只是没在跑"那条路径直接 `start` 就进闸门③。真机上这被证明是个诊断陷阱：一份 sing-box **拒绝加载**的配置（§9.4 的地址式 DNS）留在盘上，文件永远不变 ⇒ 变更路径永不进入 ⇒ `check` 永不执行 ⇒ 每一轮都报闸门③的 `sing-box process exited during observation window`，而真原因（`config check: …`）一次都没出现过。现在 `checkConfig()` 是两条路径共用的独立闸门；那条路径上没有任何东西被替换，所以失败**不需要回滚**，如实上报即可。
>
> 这一条与 §9.1 的启动同步是同一次故障的两半：同步负责把陈旧的配置**换掉**，闸门负责在配置**换不掉**时把真话说出来。

> **实现修订 2026-09-17（闸门③核对配置里的监听，而不是记账端口；只要求"别把好的弄坏"）**：多入站编辑器上线后，闸门③仍然只验**一个**端口——期望帧里的 `node_singbox.port`（记账字段，卸载/重装回路特意让它粘住不变），找不到才退回配置的第一个 `listen_port`。真机故障：`node_singbox.port=22039` 指向的监听已不在配置里（或从绑定起就没通过本机 TCP 检测），于是**这台节点上的任何一次编辑**（哪怕只是删一条出站）都在闸门③干等 30 秒 → `verify: port 22039 not reachable within 30s (rolled back)` → 回滚，且期望状态不撤、5 分钟后原样再来。修法分两层：
>
> - **核对对象换成新配置自己声明的监听**（`gate3Ports`，全部去重 `listen_port`）：`node_singbox.port` 从此只服务防火墙放行与卸载端口保留，不参与健康判定——记账端口指向一个配置里已不存在的监听，不再有资格否决一次 apply。
> - **基线语义**：apply 前进程**已在跑**时，只要求"改前本机 TCP 可连的监听、改后仍然可连"（重启前先探测一遍作为基线）；改前就不可连的监听不在要求集合里——它连不连与这次编辑无关，面板侧本就由 `effective_inbound_ports` 把它如实显示为「添加中」，不该反过来让一条卡死的监听封锁这台节点的所有编辑。进程**没在跑**（首装/停机后启动）时没有基线可用，配置承诺什么就要求什么：全部监听必须可连。进程存活检查不变——绑定失败会直接退出，仍被"进程退出"抓住。
> - **代价（说清楚）**：基线路径下，一个从未可连的监听永远不参与健康判定——sing-box"活着但一条监听都连不上"的节点会照常通过闸门③。这是有意接受的：这类节点改前就是这个状态，闸门③的职责是"别把能用的改坏"，不是审计存量问题；存量问题由「添加中」状态与 `singbox_down` 告警负责。

> **实现修订 2026-09-16（卸载 sing-box：声明式卸载 + 批量更新跳过）**：面板的 sing-box 卡片新增「卸载 sing-box」按钮（二次确认）。它是**期望状态**而不是一次性命令：`node_singbox.desired_uninstall`（新增列，`SchemaVersion=5`）承载意图，帧里是 `SingboxDesired.Uninstall`。
>
> - **为什么不用命令**：操作员可能在探针离线时点击，而 `commands` 队列的 TTL 是 10 分钟——过期即丢，探针会带着还在跑的 sing-box 变成"未纳管"却再也收不到卸载。期望状态由 `hello_ack` 在每次重连时全量重发，天然幂等（§7 声明式），不需要任何重放。
> - **谁清空什么**：服务端写下意图时只清 desired 半边（`desired_version=''`、`config_hash=''`），**上报半边（版本/证书/状态）原样保留**——面板不会在探针真删掉之前就宣称"已卸载"。探针删完上报 `{running:false, version:""}` 且无错误时，服务端才置 `status=absent`、清掉 version/证书/config_hash 与 `desired_uninstall`。订阅（§10）要求"端口 > 0 **且**证书非空"，所以节点恰好在这一刻从订阅里消失。
> - **探针删什么**：停服务 → 删服务定义（含改名前的 `fobe-singbox.service`；`UninstallSingbox(kind)` 显式接收 kind，非特权探针**绝不**碰 systemctl，同 §5.3 的坑）→ 删二进制（连同 `.prev`/`.download`）、配置（连同 `.prev`）、自签证书目录 → 仅当工作目录已空时删掉它（`/etc/one-sing` 可能是 one-sing.sh 的）。整条路径**不做回滚**：操作员要的就是"东西没了"，没有值得恢复的上一态。**代价（说清楚）**：证书一并删除，重新启用会生成新证书，按 SHA256 pinning 的客户端必须重拉订阅；换来的是"卸载后探针上不留 fobe 放过的东西"。幂等：没有任何残留时整步短路（否则 `systemctl stop` 一个不存在的单元会被当成失败，意图就永远清不掉）。
> - **端口保留**：卸载帧里带 `Port`，探针在 `absent` 上报里把它带回来；服务端对 `port=0` 的上报**不回写**。于是"卸载 → 重装"仍是原来那个入站端口（与它的防火墙规则），而不是重新随机一个。
> - **批量更新跳过**：`ListSingboxTargets()` 增加 `desired_uninstall = 0` 过滤，§9.5.4 的发布、影响面与 15 分钟收敛复查都以它为准。`desired_version=''` 本来就已经让节点落选，多这一列是为了让"待卸载"与"从未纳管"可区分（前者要重发、要在面板显示"卸载中"，并且**在探针离线时也不丢**）。待卸载期间改端口会被拒（`uninstall_pending`）——那一下写盘会把卸载意图顺手取消。
> - **旧 agent 的窗口**：不认识 `Uninstall` 的 agent 会把它当"空版本 = 不纳管"忽略掉，面板就一直显示"卸载中"。这正是期望行为：探针跟不上时如实展示未完成，而不是假装删掉了；agent 按 §5.5 自动跟随服务端，窗口是短暂的。

> **实现修订 2026-09-15（服务端产物缓存 + 一键批量更新）**：本节原本假定"版本清单里总是有货"，但从没有一条链路负责**把货取回来**——服务端只直出 `/dl` 目录里已有的文件，agent 也只会从面板拉取。本次补齐这条链路，并且**不新增任何安装路径**：
>
> - **服务端产物缓存**：server 在启动时（仅当 `<FOBE_DL_DIR>/singbox` 里没有任何有效版本）后台下载"当时最新稳定版"到 `<FOBE_DL_DIR>/singbox/<version>/`，已有缓存则**完全不联网**；可用 `FOBE_SINGBOX_AUTO_DOWNLOAD=0` 关闭。下载校验 **fail-closed**，失败只记 WARN + 设置页状态 + 手动重试，不阻塞启动。
> - **一键批量更新**：设置页点「更新 sing-box」→ 先展示受影响节点名单 → 二次确认 → 把目标版本写进每个已启用 sing-box 节点的 `desired_version`（保持端口与配置不变）→ 复用 agent 现有的三道闸门与 `hello_ack` 收敛。异步 job + SSE 进度 + 每节点结果表；分发后 15 分钟未收敛的节点汇总成一条告警。
> - 详细规则、接口与错误码见 **§9.5**；目录与卷见 **§17**。

### 9.3 anytls 与自签证书

> **实现修订 2026-09-15（探针侧布局与服务管理对齐 one-sing.sh）**：入站配置与服务管理参考 `one-sing.sh`（同类需求的成熟实现），落地三件事，**协议与订阅语义不变**：
> - **目录/证书布局（root 模式）**：探针上统一为 `/etc/one-sing/{sing-box,config.json,cert/{cert.crt,private.key}}`（原 `/usr/local/bin/sing-box`、`/etc/sing-box/{config.json,cert/{cert.pem,key.pem}}`）。同名同路径意味着**已经用 one-sing.sh 管起来的机器可以直接被接管**，不必重下二进制或重签证书。证书文件名由服务端写进 `config.json`，两侧必须一致，`internal/server/singbox` 有一条跨包测试盯着（`TestCertPathsMatchAgentLayout`）。
> - **anytls `padding_scheme`**：采用 one-sing.sh 的方案（`stop=6` / `0=30-30` / `1=80-120` / `2=350-550,c` / `3=900-1400` / `4=250-600` / `5=250-600`），不再用 sing-box 默认值。它进 `config.json`，因此**改这一项等于改 `config_hash`，会在下一次收敛时把全部节点重新下发一遍**——有意的、一次性的代价。
> - **systemd 单元对齐**：单元补上 `CapabilityBoundingSet` / `AmbientCapabilities`、`ExecReload=/bin/kill -HUP $MAINPID`、`LimitNOFILE=infinity`、`RestartSec=10s`（保留 `User=root`、`NoNewPrivileges=true`）。**单元名（2026-09-16 修订）＝ `one-sing.service`，即 one-sing.sh 自己的名字**：二进制与配置路径本来就与那个脚本同名同路径（`/etc/one-sing/{sing-box,config.json}`），单元名却是 fobe 自有的，结果是两套工具各自管一个单元、互相 disable——面板按下启动，脚本那边看到的还是"服务已停"。改名后两边指向同一个单元，脚本的 `systemctl {restart,status}` 直接作用于 fobe 的 sing-box。**代价（镜像是旧的也是真的）**：单元从此是共享物，谁最后写谁的内容生效（fobe 每次 apply 都会重写它），卸载 fobe 会删掉脚本也认得的单元；反过来，脚本 `create_systemd_service` 只在二进制缺失时才写，而 fobe 保证二进制在，所以那条路平时不会触发。procd 侧同时从 `/etc/init.d/sing-box` 改为 `/etc/init.d/one-sing`——OpenWrt 上 `sing-box` 这个名字常年属于发行版自己的包，fobe 此前一直在覆盖别人的启动脚本。
>   **同名的边界（别指望混用协议配置）**：共享的是**单元**，不是**配置文件**。`one-sing.sh` 用 `jq` 往 `/etc/one-sing/config.json` 里追加 SS2022/VLESS/Socks5 入站，而那份 config.json 是 fobe 的期望状态产物——下一次收敛（最多 60s）发现 hash 不符就会**整份重写**，脚本加的入站会消失（fobe 的闸门②也随之重启服务）。要用脚本的那些协议，**要么先接管**（2026-09-17 修订，见下；**2026-09-17d 起接管是自动的**——上报即认定，操作员什么都不用点），要么就别让 fobe 管这台机器的 sing-box；二者共享单元名的收益是"面板与脚本对同一个服务生效"，不是"配置可以混着改"。
>   **改名要带迁移**：改名前的 `fobe-singbox.service` / `/etc/init.d/sing-box` 仍然 enable 且在跑，和 `one-sing.service` 抢同一个二进制、配置与入站端口 ⇒ agent 启动时（root）`RetireLegacySingboxUnit()` 停掉、disable 并删掉旧单元；procd 侧**只删自己写的那个**（脚本内容含 one-sing 布局才算我们的，`ownsLegacyInitScript`），发行版的 `/etc/init.d/sing-box` 一律不碰。旧的 `AdoptForeignSingboxService`（把 one-sing.service 停掉并 disable）随之删除——它现在会 disable 掉 fobe 自己刚建的单元。`install.sh --unprivileged` 两个名字都停：非特权 agent 没有 systemctl 权限，只能靠引导脚本收尾。
> - **迁移是幂等的**：agent 启动时 `MigrateSingboxLayout` 只在「目标不存在且源存在」时搬文件（二进制连同 `.prev`/`.download`、配置连同 `.prev`、证书改名 `cert.pem→cert.crt`、`key.pem→private.key`），搬完删空的旧目录；搬不动只记 WARN，收敛循环照样能把缺的东西重新下载回来。
> - **`config.json` 是 fobe 独占的**：同一台机器上又跑 one-sing.sh 又由 fobe 托管，两边会互相覆盖同一份 `config.json`（one-sing.sh 加的协议会消失）。**2026-09-17 起有了正解**：先在面板接管（见本节末的修订），脚本那些入站成为期望状态的一部分，此后由面板统一管理；接管之后仍继续用脚本往里追加，是唯一还会被覆盖的用法。**2026-09-17b/c 两次修订把这条改写掉了**：文件本身就是真相，面板只读只编辑，脚本追加的入站下一轮上报就出现在面板与订阅里，没有"接管"这个动作，也不存在"整份重写"。

> **实现修订 2026-09-17（本机已有 sing-box 的发现与接管）**：上一版把「共享的只是单元，不是配置」写成代价，真机上就撞上了——一台 one-sing.sh 起了 anytls + VLESS 的机器，装上 agent 后面板**一个字都看不到**（`State.Singbox` 只在声明过期望版本后才上报，未接管时 `singboxManager.state == nil`），而一旦在面板点安装，生成器整份重写 `config.json`，脚本加的入站当场消失。现在补上「识别 → 接管」两段：
> - **探针只读探测**：agent 每个收敛周期（60s）外加启动时扫一遍本机布局（二进制/配置/单元/进程），把 `config.json` **原文**随每个 `state` 帧上报（`State.SingboxLocal`）。它**不受期望状态开关限制**——恰恰是没有期望状态的那台机器需要被看见。读不到就照实报（文件不存在、读不动、超 1 MiB 不传），不静默。**解析放在服务端**（`internal/server/singbox/local.go`）：sing-box 配置知识本来就在 §9.1/§9.4，这样新增一种协议是服务端改动，不用给每台探针推一次 agent。进程存活用既有 `findProcByExe`（不需要权限），单元状态另问 systemd（非特权探针 `unit_known=false`，如实说明"问不到"而不是"没在跑"）。
> - **入库加密**：快照整体经 `security.Cryptor`（AES-GCM）落 `node_singbox.local_config`（列名 `enc:v1:` 前缀自描述，见 `store.EncryptSettingValue`）。存的是**操作员自己写的文件**，里面可能有 SS 密码、VLESS UUID、REALITY 私钥——`/api/nodes/{id}/singbox` 只返回**摘要**（类型/端口/tag/有无凭据），凭据永不出库，只在生成下发的配置里出现。已接管的入站另存 `extra_inbounds`（同样加密）。
> - **接管**：`POST /api/nodes/{id}/singbox/adopt` 把选中的入站写进期望状态。**端口优先沿用**：脚本的 anytls 端口直接成为面板入站端口（面板自己的 anytls 就是这个端口的监听者，用全局密码），其余入站作为 `extra_inbounds` 与模板生成的 anytls 一起进 `config.json`——**同一台机器、同一批入站，不重写、不重签**。选中的入站按 `(type,port)` 匹配（tag 被手改也不会错认），第二次接管是**追加**而不是覆盖；`desired_uninstall` 挂起时拒绝接管。版本：优先探针正在跑的那个（服务端缓存里有就用，免下载），否则用缓存里最新的；**缓存空则拒绝接管**（`no_cached_version`）——那一步之前不写任何东西，探针不受影响。
> - **订阅**：每台被接管节点除面板 anytls 外，按 `extra_inbounds` 展开成多条出站（anytls / vless+reality / shadowsocks / socks），**各用自己的凭据**（anytls 保留脚本的密码）。REALITY 的公钥由私钥现场推导（`crypto/ecdh` X25519，替代脚本里那份手写 PKCS#8 DER 的 openssl 花活）。仅有已接管入站、没有面板证书的节点也算「可渲染」（`subRenderable` 看 `ExtrasPresent`）。
> - **生成配置必须过 sing-box 的严格解码**：`BuildNodeConfigWithInbounds` **重建**入站对象而不是整体 marshal，只放行 `allowedInboundKeys`/`allowedTLSKeys`/`allowedRealityKeys` 白名单内的键。这不是洁癖：闸门①是 live 探针上唯一的防线，而真机验证时踩了两个**服务级**的坑——`ExtraInbound.Password` 被赋成用户级密码，生成的 vless 入站多了个顶层 `password`（`inbounds[1].password: unknown field`）；以及 `reality.public_key` 是**客户端**字段，进服务端配置同样被拒。两次都是 agent 停服→写盘→`check` 失败→回滚（回滚后服务与脚本入站完好，已核对），但代价是操作员的服务被重启一次。**结论**：新增任何进配置的字段，先想清楚它是不是 sing-box 认的键，并跑 `TestGeneratedInboundCarriesNoUnknownKeys`。
> - **代价**：① 白名单意味着 fobe 还不建模的入站选项（例如某个新协议的字段）会被**静默丢弃**——明知而选，因为配置被 sing-box 拒绝等于服务中断；② 快照是操作员文件的加密副本，换主密钥/恢复旧库后解不开会**报错**而不是当成"没报过"（同 §10.1 的取向）；③ 接管后的 `config.json` 仍由 fobe 独占，脚本再 `jq` 追加的入站会在下一次收敛被移除。

> **实现修订 2026-09-17b（编辑器模型：文件是真相，fobe 退成编辑者）**：上一条修订把「识别 → 接管」做成了"接管一次、之后由 fobe 整份重写"，实战反馈是**这个代价不该由操作员承担**——他继续用 `one-sing.sh` 的 `jq` 加一个入站，下一次收敛就把它抹了；而"接管"这个词本身也在暗示一件他没打算做的事（交出配置所有权）。本修订**推翻"期望状态是配置真相"在 sing-box 上的适用性**：
> - **`/etc/one-sing/config.json` 是唯一真相**。agent 每轮（60s + 启动 + 面板"从探针刷新"命令）只读扫描，文件变了就把**原文**上报；服务端解析、加密存档，**不再按模板重写**。面板显示的就是探针盘上那一份。
> - **编辑 = 读-合并-写**：`PUT /api/nodes/{id}/singbox/config` 携带 `reported_hash`（渲染时的文件指纹）+ `add/update/delete`。哈希对不上直接 `409 config_changed` —— 撞上"最后写赢"比"基于陈旧副本静默合并"好。合并**只碰被编辑的字段**：`sniff`、`multiplex`、`padding_scheme` 这类 fobe 不建模的选项原样保留，缺省的凭据**继承文件里的值**（面板回显脱敏字段时绝不清空密码）。合并后的整份文档写进 `settings.singbox_config:<id>` 并作为期望态下发；agent 看到字节不同就 `check` → 写盘 → `systemctl restart one-sing.service`（还是那三道闸门）。
> - **手工加的东西立刻可见**：订阅从"最近上报的文件"渲染——`jq` 加一个 VLESS，下一轮上报后订阅里就有它，不需要任何"接管"。`POST /api/nodes/{id}/singbox/refresh` 让操作员改完文件不用等一轮。每个监听都以自己的协议、端口、凭据和证书渲染。名称不带 `协议:端口` 后缀（**2026-09-17j 删除了这里引入的后缀**：同机多条入站重名交给渲染器的 `#2`/`-2` 兜底，见 §9.3 的 17j 修订）。
> - **接管退化成一次普通编辑**：勾选 = 一个 credential 留空的 update（服务端写入面板自己那份全局任何 anytls 密码，前端拿不到它），端口与其余字段照旧。没有"接管后别的东西会被删"这种语义。**2026-09-17d 把这一步也删了**：勾选框与「接管」按钮不复存在，见下一条修订。
> - **证书要文件里的字节**：config.json 只写 `certificate_path`，而客户端 pin 的是 PEM。agent 顺带上报每个 anytls 入站的证书内容（按端口索引，≤64 KiB、必须含 `BEGIN CERTIFICATE`），否则那些监听**永远进不了订阅**（本项目不做 `insecure=true`，§9.3；**2026-09-17j 把该原则收窄为"sing-box 渲染永不 insecure"**——Clash 渲染无法 pin，见 §9.3 的 17j 修订）。
> - **agent 侧三处配套（都是真机上抓出来的）**：① 无版本的期望帧**不是**"未管理"——`SetDesired` 只有在 `Version=="" && ConfigJSON==""` 时才算 unmanaged，否则面板对脚本节点的编辑会被静默丢弃；② 无版本时**绝不去装内核**（`needInstall` 要求 `d.Version != ""`），否则 agent 会去下 `/dl/singbox//linux-amd64` 拿到 404、回滚整次 apply，日志里只有 `download : get sha256: 404 Not Found`；③ `hub.buildDesiredState` / `pushDesired` 在**只有 config、没有 desired_version** 时也要发帧（`ConfigHash != ""` 即算有期望态）。
> - **`SyncSingboxConfigs()` 跳过被面板编辑过的节点**（`settings.singbox_edited:<id>` 标记）：模板变化只修复"从头由面板装、且从未被编辑"的节点。改了模板就想把操作员的文件再覆盖一遍，正是这次要消灭的行为。
> - **代价（写清楚）**：① 订阅最长滞后一个上报周期（agent 变即推，通常数秒；可手动刷新）；② 两个写者抢同一文件时最后写赢，面板不保证等于盘上——它显示的是"最近上报"，且拒绝基于陈旧指纹的写入；③ 面板读得到凭据（它是操作员的配置，必须能改），仍经 Cryptor 加密落库、审计只记动作与端口。

> **实现修订 2026-09-17d（取消接管动作）**：17b 把「接管」降级成一次编辑之后，面板上仍留着一个勾选框加一个「接管勾选的入站」按钮，而它要做的事完全可以从探针上报的文件里读出来。本修订删掉这个动作：`POST /api/nodes/{id}/singbox/adopt` 与编辑器请求里的 `adopt` 标志一并删除；要改入站就在表格里读-合并-写，或在探针上改完点「从探针刷新」。`extra_inbounds` / `BuildNodeConfigWithInbounds` 仍保留为 0.1.1 及更早节点的存量读方，避免安装或改端口时丢掉 VLESS/SS/Socks。

> **实现修订 2026-09-17g（入口规则平等）**：一个节点可以有多条服务端入站，因此不再由探针上报挑选最后一个 anytls 写入 `node_singbox.port`，也不再给任何行显示「本节点入口」、锁定删除或使用无后缀订阅名。`LiveProxyNodes` 对每条可渲染规则统一按 `协议:端口` 加后缀（**2026-09-17j 已删除该后缀**，见下），并只使用该端口随本机配置上报的证书与凭据；缺少某条 anytls 的证书 PEM 时跳过那一条，不会借用别的入口的证书。编辑页移除了旧的单一「修改端口」控件，端口变更统一在规则表逐行完成；仍保留至少一条入站的保护，防止生成空配置。

> **实现修订 2026-09-17f（入站状态由探针确认）**：入站表新增状态列，状态不是沿用 `node_singbox.status` 的进程级结果。面板「新增入站」时先把 `(node_id, port)` 记为 `pending`，因此规则下发后、探针尚未回报前，新行就会立即显示「添加中」；agent 成功应用配置后马上重扫本机文件，并对每个已配置监听作 `127.0.0.1:port` TCP 检测，随 `State.SingboxLocal.effective_inbound_ports` 上报。服务端只在该端口同时出现在上报配置与有效端口集合中时改为 `running`（「运行中」），绝不由全局 sing-box 进程在跑来推断；新 agent 会额外声明检查能力，因此某次检查后端口不在集合内会保持/回落为 `pending`。旧 agent 没有该能力标记时保留已有状态（新入口仍是「添加中」），避免假阳性；探针离线或端口没绑定成功也同样不会被显示为运行。每入口状态持久化在 `node_singbox_inbounds`（SchemaVersion 13），使页面刷新和异步上报之间不丢刚新增的行。

> **实现修订 2026-09-17h（删除中的入站读「删除中」，不再借「添加中」的壳）**：17f 的状态机只有 `pending|running` 两态，而删除入站时服务端**当场删掉**生命周期行——探针应用并重报之前，GET 仍从"最近上报的文件"里渲染出那行，找不到状态行就落回默认的 `pending`，于是**点完删除，一条刚才还「运行中」的监听变成「添加中」**，直到下一轮上报才消失。修法：
>
> - **编辑时不再删行，改标 `deleting`**（`node_singbox_inbounds` 新状态值，`(node_id, port)` 主键不变，SchemaVersion 不动）：探针上报里**不再出现该端口**才算删干净——那时既有的"已上报但消失"清理会顺手把行删掉。上报里**还有**该端口时（apply 未到、或 apply 失败回滚），reconcile 保留 `deleting` 不回写 `running`——回滚之后那行仍在服务，但面板的删除意图仍未兑现，如实显示「删除中」，配合节点级「最近错误」解释为什么；期望状态会按 §9.2 的退避持续重试。
> - **两个边角**：`reported=0` 的行（刚新增、探针还没见过）没有可删的东西，编辑时直接物理删行，不留「删除中」的幽灵；探针从没上报过的活监听（生命周期行为空）也补一行 `deleting`，让删除可见。
> - **前端**：状态 chip 三态渲染（运行中/删除中/添加中），存在 `pending` 或 `deleting` 行时维持 5 秒快速轮询；zh/en 各加一条 `sb_inbound_deleting`。API 只新增一个状态字符串值，wire 协议不动。
> - **为什么不用期望文档对账**：曾考虑在 GET 时拿 `singbox_config:<id>` 与上报文件做差集来推导"删除中"，但 one-sing.sh 直接往探针文件里追加的入站同样"不在期望文档里"，会被误标成删除中。操作意图只有编辑动作自己知道，落在状态行里最诚实。

> **实现修订 2026-09-17i（发现即已识别：面板语义不再与发现通道打架）**：17g 让监听平等之后，「面板管理的 sing-box」与「探针上实际跑的 sing-box」成了两个事实，但面板的好几处仍只回答前一个：编辑页 sing-box 卡片对纯 one-sing.sh 节点显示「未安装」（管理态 `status=absent`），读起来像"这台机器上没有 sing-box"；§10.2 的中转候选与落账判定还在读 `node_singbox.port`/`cert_pem`——而 17g 之后这两列对发现型节点**永远是 0 和空**，于是「A 的转发指向 B 的 anytls 入站」这条中转入口在选择器里**根本不出现**（渲染端本来是读上报文件的，选择了也渲染得出来——判定与渲染打架，正是 `nodeRenderable` 当初要消灭的那类裂缝）。修法：
> - **候选与落账改读上报的文件**：中转落点端口 = 上报配置里的 anytls 入站端口集合（`liveAnytlsPorts`），管理态 `port` 只作为"文件还没上报过"的兜底；可渲染/可中转判定（`targetReadinessOf`）与订阅渲染同源（`liveNodesFor`），中转额外要求存在**带证书的 anytls 入站**（TLS 终结在 B，没有证书就没有可 pin 的落地）。自动落账的门槛同步替换。
> - **中转出站按规则的目的端口挑入站**：入口身份只存 `src_port`，渲染时重读跳板的 nft 规则取 `dst_port`，pin **那条规则落点入站**自己的证书——监听平等后同一节点多条 anytls 各带各的证书，一律 pin 第一条会把客户端 pin 到永远握不上手的证书上；规则已删时回退第一条可渲染 anytls（旧行为）。
> - **编辑页状态 Tile**：管理态沉默（无 desired_version 且无 version）而发现通道在场（`local.present`）时，状态显示「已识别（本机实例）」（`sb_status_local`），「尚未安装 sing-box」提示不再出现；管理态有任何话要说（安装中/运行中/异常）时仍以管理态为准。发现卡片里的运行/停止细节照旧。
> - **代价**：发现型节点的直连/中转入口进订阅仍滞后一个上报周期（不变）；「已识别」不区分运行与停止——那是发现卡片里状态行的职责，Tile 只回答"这台机器有没有 sing-box"。

> **实现修订 2026-09-17j（订阅名不再带 `协议:端口` 后缀；Clash 渲染的 anytls 改 `skip-cert-verify: true`）**：两条实战反馈，都出在订阅渲染器上：
> - **名字去后缀**：17g 给每条出站名追加 `协议:端口`，实战读起来就是"订阅名称字段不生效"。后缀删除，客户端看到的就是回退链算出的基础名（入口 alias → `nodes.sub_name` → `nodes.name` → 节点 id）；直连入口的 alias 现在真正逐字生效（此前 `NameSuffix` 未清，alias 后面仍会被追加一次后缀，后续入站甚至双重后缀）。同一节点多条入站同名时交给渲染器既有的兜底去重（Clash ` #2`、sing-box tag `-2`）——**代价（说清楚）**：`-2` 不含端口信息，光看名字分不清是哪条入站；要区分就用 alias 或别在一台机器上开同名入站。这与 2026-09-16 的 tag 修订是同一款取舍："拒绝静默加后缀"的口径从来只约束显式 alias（`alias_conflict`），自动名一直允许 `-2`。
> - **Clash 渲染放弃 pinning**：mihomo 的 anytls 出站 schema 没有 `ca-str` 字段（写了无效），`skip-cert-verify: false` + 内嵌证书在 Clash 客户端里根本没有落地方式；没有校验可言时 `sni` 也无从起作用。anytls 在 Clash YAML 里改写 `skip-cert-verify: true`，不再输出 `sni` 与 `ca-str`。**这是明确接受的降级**：Clash 侧的自签 anytls 暴露给能动路由的中间人，换取"Clash 客户端能连"；sing-box JSON 渲染保持 `tls.certificate` 内嵌 PEM pinning 不变，"不做 insecure" 的原则收窄为"sing-box 渲染永不 insecure"。证书上报（§9.3）仍是渲染门槛：没上报 PEM 的 anytls 入站照旧不进订阅，两种格式一致。

> **实现修订 2026-09-18（改端口是一次搬移：面板立刻显示新端口，不必手动刷新）**：17f/17h 把「新增」和「删除」的意图落在状态行里，**唯独改端口没有落**——`update` 只改文档，不动 `node_singbox_inbounds`。而 GET 渲染的是**最近上报的文件**，探针要跑完自己的应用窗口（§9.2 闸门②/③，最长 30s）才重扫上报，于是现象是：保存后表格里仍是**旧端口**（还顶着「运行中」），新端口**根本不在页面上**；又因为没有任何 `pending`/`deleting` 行，前端的快速轮询不启动，只有 30s 的发现轮询会碰巧读到，操作员于是以为"没保存"、手动刷新才看到新端口。修法（意图仍然只由编辑动作自己记录，不做期望文档对账——理由同 17h）：
> - **一次改端口 = 一次新增 + 一次删除**：`update` 里端口变了就同时落两行——新端口 `pending`（探针还没见过它）、旧端口 `deleting`（探针仍在服务它），GET 立刻把新端口渲染成「添加中」、旧端口渲染成「删除中」；探针应用并重报后，旧行被既有的"已上报但消失"清理删掉、新行升为「运行中」。请求里的 `add` / `delete` 与这两半走**同一套状态**，没有新状态值、wire 协议不动。
> - **保持端口的编辑（改 tag/凭据）会取消先前的搬移**：新文档仍然服务这个端口，就把行还原成上报蕴含的状态（探针确认过 TCP 可连＝运行中，否则添加中）——不还原的话它会一直挂着「删除中」，而探针**不会**上报任何新东西（它的文件没变），没有任何机制能把它清掉。
> - **面板自己的乐观行以面板最新推的文档为准**：推成功后清掉 `reported=0 && status='pending'` 里端口已不在文档中的行（连续改两次端口、加完又撤）。这类行是面板自己造的、探针从没上报过，reconcile 的"已上报但消失"清理永远扫不到它们，会永久停在「添加中」。`reported=1` 的行绝不在此列——它们镜像探针的文件，文档里没有的监听是 one-sing.sh 的，不是陈旧意图（17h 的原则不变）。
> - **写入顺序与撤销**：意图行在**下发期望态之前**写入（在线 agent 可能立刻应用并回报，后写会覆盖真实确认；17f 的同一条理由），每一条都带自己的 undo，写入中途失败或 `pushConfigDocument` 失败时按逆序撤回——一次没离开服务端的编辑不能留下"已搬移/已退役"的假象。
> - **代价（说清楚）**：① 搬移窗口内该监听在页面上占**两行**（旧端口「删除中」+ 新端口「添加中」，后者只读，与 17f 新增的那行同形），探针在线时通常不到 5 秒就收敛成一行「运行中」；探针离线时它会一直这么显示——这恰好是"期望态已改、探针还没应用"的真实状态，正是 17h 对「删除中」的定义。② 窗口内再编辑仍以**最近上报的文件**为合并基准（§9.3 读-合并-写不变），所以第二次编辑会以那份基准覆盖第一次的结果；乐观行随之以最新文档为准（见上一条），不会留下幽灵行。③ 前端在**任何一次动作成功后**加一分钟的 5 秒轮询：改 tag/凭据这类不产生在飞行行的编辑同样要等探针重报才可见，不这样它就得等 30s 发现轮询——那正是"我改了但页面没变"的观感（`watchUntil`，纯前端，不新增接口）。

> **实现修订 2026-09-16（非特权目录重定位）**：`--unprivileged` 不可写 `/etc`，故布局为 `dirname(-config)/one-sing/`（安装路径即 `/opt/fobe-agent/one-sing/`，`FOBE_SINGBOX_HOME` 优先）。服务端仍生成 root 布局的绝对证书路径，agent 在写盘与 config hash 比对时同步替换前缀；否则每次 60 秒收敛都会误判配置变化并重启。非特权模式不迁移 root 的旧布局，也不接管 `one-sing.service`。

- **私钥永不离开探针**：首次启用时由 agent 用 Go 标准库 `crypto/x509` 现场生成自签证书（不依赖 openssl——OpenWrt 常常没有），存 `/etc/one-sing/cert/{cert.crt,private.key}`（0600）。密钥算法保持 ECDSA P-256（而非 one-sing.sh 的 RSA-4096）：探针是小机器、密钥在设备上现场生成，而客户端是按 SHA256 指纹 pinning 的，算法对客户端不可见。
- agent 上报**证书 PEM + SHA256 指纹 + 有效期**给面板（不含私钥）。
- 订阅渲染时把证书 PEM 写进客户端的 `tls.certificate` 字段做 **pinning**，而不是让客户端 `insecure: true`。这样自签也不会被中间人。（**2026-09-17j：Clash 渲染是唯一例外**——mihomo 的 anytls 没有 `ca-str`，只能 `skip-cert-verify: true`，见上方 17j 修订。）
- 入站口令 = **该入站自己的密码**：服务端在创建它时现生成 16 位 `[A-Za-z0-9]`，此后每次重新生成配置都从节点自己的 `config.json` 读回来（见 §10.1 实现修订 2026-09-17e；2026-09-17e 之前是全局共享的 `settings.anytls_password`）。
- 端口：默认随机高位端口（10000-60000），面板可改；改端口时 agent 尝试自动放行防火墙（ufw / firewalld / nft / OpenWrt fw4），失败则返回需要你手动执行的命令原文。

### 9.4 sing-box 入站模板（生成物示意）

> **实现修订 2026-09-16（DNS 段必须用 1.12+ 的 type 形式）**：下发的是**完整 config.json**（log + dns + inbounds + outbounds），而这份 dns 段一直用 `{"tag":"local-dns","address":"local","detour":"direct"}`——地址式 DNS server **在 1.12.0 弃用、1.14.0 移除**，于是任何一次现行版本的安装都在闸门①原地失败（`config check: exit status 1: .servers[0]: legacy DNS server formats … removed in sing-box 1.14.0`），日志与告警里看起来像"配置坏了"，实际是模板过期。现在写 `{"type":"local","tag":"local-dns"}`（`local` 就够了：入站只有 anytls、出站是 `direct`，解析交给系统 resolver；`detour` 是**远程** server 的 dialer 选项，这里没有意义）。**代价**：这份模板的兼容下限被钉在 sing-box ≥ 1.12（type 形式 1.12 才有），面板的版本列表仍列出更老的 release，装 <1.12 会反过来在闸门①失败。已用真实二进制（1.14.1 / 1.15.0-alpha.4）对模板跑过 `check`。**注意 config_hash 因此变了**：所有节点会在下一次收敛时重新下发一遍配置（与 §9.3 改 padding_scheme 同性质，一次性）。

```json
{
  "type": "anytls",
  "tag": "anytls-in",
  "listen": "::",
  "listen_port": 23456,
  "users": [{ "name": "default", "password": "<该入站自己的密码>" }],
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
- **改写期望状态而非发命令**：对每个目标节点写 `desired_version`（**保持既有端口与配置不变**），然后对在线节点 `pushDesired`；离线节点什么都不发，等它重连时由 `hello_ack` 全量下发自然收敛（§7 声明式期望状态）。目标集合 = `desired_version` 非空**且未被卸载**（`desired_uninstall = 0`，见 §9.2 实现修订 2026-09-16）——否则"一键更新"会把操作员刚卸载掉的 sing-box 又装回去。
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
- 格式决策（2026-09-16 修订）：`?format=singbox|clash` 显式指定 > 订阅自身的 `format` 固定值 > 自动（**绑定模板的格式**；未绑定模板时按 UA 嗅探 `clash`/`mihomo` → Clash、`sing-box` → sing-box）> 默认 `singbox`。`surge` 从嗅探列表移除：fobe 没有 Surge 输出格式，给 Surge 客户端发一份它读不了的 Clash 配置，比发默认 JSON 更糟。
- 输出 = **模板渲染**：模板是每格式一份完整配置文件（Clash YAML / sing-box JSON），使用 `{{nodes}}` 占位符（2026-09-16b：`{{rules}}` 占位符已删除，分流规则直接写在模板里）；同一模板可被多个订阅复用。
- **模板中禁止出现可手填的凭据**：节点数据统一由 `{{nodes}}` 注入，密码来自全局设置。这样"轮换密码"才不会漏改。
- 面板功能：模板编辑器（含语法校验 + 预览渲染结果）、订阅的节点多选、**绑定模板**、UA 过滤、一键轮换 token、**链接随时复制**、访问日志（时间/IP/UA/**结果与拒绝原因**）。
- 节点渲染为 anytls outbound：`server`（探针主 IP 或你指定的域名）、`server_port`、`password`、`tls.certificate`（内嵌 PEM pinning）、`tls.server_name`；**出站 tag = `fobe-<节点名>`**（节点名为空则用节点 id，重名依次加 `-2`/`-3`，2026-09-16 修订，见下）。**2026-09-16c 起绑定的单位是「入口」而不是节点，且名称支持自定义（入口 alias / 节点订阅名 / 中转命名格式）——见 §10.2。**

> **实现修订 2026-09-16（订阅链接随时可复制；`{{rules}}` 有了配置界面）**：补齐两处"规格与实现对不上"的地方，并补上一直缺的模板绑定入口。
> - **链接不再只显示一次**：`subscriptions` 增列 `token_enc`（Cryptor AES-GCM 密文，密钥边界同 §4.4），hash 仍只用于 `/sub/<token>` 查询；新增 `GET /api/subscriptions/{id}/link` 返回 `{token,url}`，列表返回派生字段 `link_available`。**代价（说清楚）**：库里多一份密文，所以"库中只存 hash"这句话不再成立——单用户自托管下主密钥与库同处一地，单拿 `token_enc` 并不比单拿 hash 更危险，但**拿到整个 `data/` 就等于拿到全部订阅链接**；换来的是"想复制就复制"。本修订前建的订阅 `token_enc=''`、`link_available=false`，面板提示只能轮换 Token 换新链接（迁移只加列，不改老数据）。订阅行按钮「复制订阅链接」一次点击直接写剪贴板并 toast 反馈；明文不可恢复时 toast 提示轮换，两条剪贴板路径都被拒时才展开链接面板供手动选中。**复制一律走 `format.copyText` 而不是 `navigator.clipboard`**：面板通常跑在局域网明文 http 上，异步剪贴板 API 在非安全上下文里根本不存在，直连它会让按钮"点了没反应"。
> - **`{{rules}}` 从"原样保留"改为"注入"**：校验器一直强制模板包含 `{{rules}}`，渲染器却从不替换它 ⇒ 任何带该占位符的手写模板都会输出含字面量 `{{rules}}` 的坏配置。现由两个设置填充：`sub.rules_singbox` / `sub.rules_clash`（按输出格式各一份，因为订阅格式由客户端请求决定，同一订阅可能两种格式都被拉），订阅页「路由规则」卡片编辑。片段与 `{{nodes}}` 同规格：**自带列表符号与缩进**，模板只负责外层的键；空值展开为空。`PUT /api/settings` 做基本校验（sing-box 要求包进 `[]` 后是合法 JSON 对象数组，Clash 要求每个非空行以 `-` 开头），不合格回 `bad_rules`；设置页之外的写入路径（§17 导入）同样受 `allowedKeys` 约束。
> - **订阅绑定模板**：`template_id` 一直由 `PUT /api/subscriptions/{id}` 支持，但页面没有入口；现于订阅行加「模板」下拉（内置默认 / 指定模板）。渲染选择逻辑未变：模板格式与请求格式不一致时回退内置默认模板。
> - **被拒的请求也进访问日志（带原因）**：`sub_access_logs` 增列 `reason`（`''` = 已下发，否则 `ua_mismatch` / `disabled` / `render_error`）。此前只有放行的请求才记，而所有拒绝都回同一个 404（§10 的不泄露存在性原则），于是"客户端为什么拉不到"在面板上完全不可见——运维只能靠猜。**未知 token 不记**：它不属于任何订阅，访问日志按订阅展示，没有地方挂；不记也维持了"不透露该 URL 是否存在"。给客户端的响应体逐字不变（同原因不同原因的 404 完全一致，已有测试钉住）。
> - **sing-box 出站 tag 改由节点名派生**：原为 `fobe-<随机节点 id>`，模板里不可能预先写出这些 tag ⇒ selector / urltest 组与 `route.final` 全都写不出来（Clash 侧一直是节点名，所以只有 sing-box 半残）。现在 tag = `fobe-<节点名>`（空名回退节点 id，重名加 `-2`），模板终于能写分组。**代价**：已经在自己配置里 pin 过旧 tag 的客户端需要重新拉订阅——订阅本来就是干这个的，且旧 tag 是随机 id，实际几乎不可能有人写过。
> - **输出格式成为订阅自己的属性**：§6 的 `subscriptions.format` 列从前只有文档、没有实现——格式一律按请求嗅探，于是"绑了 Clash 模板的订阅被一个 UA 不认识的客户端一拉"就拿到 sing-box 内置默认模板的 JSON，操作员的模板完全没生效（这正是被报上来的现象）。现补齐该列（`''` = 自动）与解析顺序（见上），订阅页「模板」面板里多出「输出格式」下拉（自动 / 固定 sing-box / 固定 Clash）；`PUT /api/subscriptions/{id}` 接受 `format`（非法值回 `bad_format`），§17 快照也带上它（快照里缺字段视为自动，不清空已存的固定值）。**自动的语义是有意这样定的**：绑模板本身就是"这条订阅是给哪类客户端用的"的声明，UA 嗅探只在没有模板时兜底。
> - **节点多选只列"能出现在输出里"的节点**：判据收敛为服务端唯一函数 `subRenderable`（主 IP + 入站端口 + 已上报证书），同时用于渲染与节点列表的新派生字段 `singbox_ready`——挑中一个节点必然改变输出，不会再出现"勾了却没进订阅"的迷惑。已绑定但**当前**不可渲染的节点（典型：正在重装、证书还没重新上报）不隐藏：以虚线条目单独列在下方、仍可取消勾选，避免操作员保存一次就把绑定关系悄悄删掉。

> **实现修订 2026-09-16b（删除 `{{rules}}` 占位符与「路由规则」卡片）——取代上一条里的 `{{rules}}` 注入部分**：上一版刚把 `{{rules}}` 从"原样保留"改成"由 `sub.rules_singbox` / `sub.rules_clash` 注入"，用起来才发现这层间接是多余的：规则本来就是每份模板各自的文本，两个按格式分家的设置只让"改规则"离模板更远。现整体删除——占位符不存在了，规则直接写在模板里。
> - **渲染器只剩 `{{nodes}}` 一次替换**；模板校验从"必须同时含 `{{nodes}}` 与 `{{rules}}`"收成"必须含 `{{nodes}}`"，并且**明确拒绝**含 `{{rules}}` 的新建/编辑模板（`obsolete_placeholder`）：规则不能一边删掉占位符、一边让手滑粘回来的模板静默下发一份带字面量 `{{rules}}` 的坏配置。
> - **两个设置项一并作废**：从 `allowedKeys` 移除（`PUT /api/settings` 回 `unknown_key`，§17 导入同样跳过、导出也不再带这两个键），订阅页那张「路由规则」卡片删除。
> - **存量数据一次性升级**：启动时 `MigrateLegacyRules()` 把仍含 `{{rules}}` 的模板按各自格式替换成原设置里的片段——替换结果与旧渲染器的输出**逐字一致**，然后删掉这两个设置键；只要有一份模板替换失败就保留片段，下次启动重试，不会把规则替换成空。**代价**：模板内容被就地改写一次（要回退得手动改回），换来的是升级前后订阅下发的内容完全相同。默认模板不受影响（内置 sing-box / Clash 模板本来就把规则静态写在里面）。
> - **§17 导入走同一个函数**：旧快照里的模板可能仍带占位符，导入时同样内联（片段若已被启动迁移消费掉，则展开为空——老备份恢复出来的是"规则空但语法合法"的模板，不会是带字面量占位符的坏配置）。

### 10.1 入站口令：创建入口时现生成、跟着入口走（2026-09-17e 起；此前是共享密码）⚠

> **本节的"共享"部分已是历史**：2026-09-17e 取消了全局密码，每个入口自己一份，见本节末的修订。下面的原文保留，用来说明当初为什么这么做、以及那条取舍的代价。

所有订阅共用一个 anytls 密码 → **订阅 token 只能吊销 URL，吊销不了代理访问**。真泄密时的补救链路是：

```
面板（AI 工具 `set_anytls_password`，留空即重新生成）→ 更新 settings → 批量下发所有节点 desired state
                    → agent 重载 sing-box（不断开现有连接，新连接用新密码）
                    → 重新渲染所有订阅 → 客户端重新拉订阅
```

面板必须在轮换对话框里写明："此操作会要求所有客户端重新拉取订阅，旧密码立即失效"。建议也顺手实现"节点级密码覆盖"字段，方便你以后想隔离时不必重构数据模型（v1 UI 可以先不暴露）。

> **实现修订 2026-09-16（密码改由服务端生成，取消操作员输入）**：这个凭据过去要人手填——不填，面板就拒绝在任何节点上安装 sing-box（`anytls_password_unset`），设置页的「代理」卡片是唯一的输入口与轮换口。但它的消费者全是机器产物（下发给 agent 的 `config.json`、客户端拉取的订阅），**没有任何一条路径需要人读出它**，于是要求操作员自己发明、记住并保管一个密码纯属净负担。现在服务端全权持有：
> - **生成时机 = 使用时机**：安装/改端口、渲染订阅、启动时发现已有被管理节点（`SyncSingboxConfigs`），三处任一处发现没配就现生成一份。全新实例若从不碰 sing-box，则**不会**凭空多出一个凭据。
> - **形态**：16 位 `[A-Za-z0-9]`（≈95 bit）。选这个字符集不只是为了够随机——同一个串要原样走进 JSON 字段、第三方客户端的 URI auth 位与 Clash YAML 标量，字母数字是唯一在三种上下文里都不需要转义的集合。用拒绝采样而非 `%` 取模（62 不整除 256，取模会让字母表前 8 个字符偏多）。
> - **落库与留痕**：AES-GCM 密文存 `settings.anytls_password`（§4.4），生成时写一条 `audit_logs`（actor=`system`、action=`anytls_password_generated`、command=`[redacted]`，值永不入审计/日志）。
> - **代价一（可见性）**：面板**没有**任何地方回显这个密码，设置页的输入卡片随之删除（连同 `sec_proxy` 与「代理」相关文案）。要用它手配客户端，就从订阅渲染结果里取。订阅页留了一行说明"密码由面板自动生成、无需配置"，否则"哪儿能改密码"会变成一个必然会被问到的问题。**实现修订 2026-09-16（订阅页文案精简）**：这一行已按"面板文案只留必要说明"的口径删除，同一页的「中转入口」描述段也一并删掉、命名格式提示只保留占位符清单——"哪儿能改密码"的答案改由 README 与本节承担，面板不复述设计理由。
> - **代价二（轮换入口变了）**：§10.1 的补救链路仍然成立，但触发点从设置页换成 **AI 工具 `set_anytls_password`（`password` 可留空 = 重新随机生成，属 §12.3 元操作、强制确认）** 与 **`PUT /api/settings`（`{"anytls_password":""}` 亦表示重新生成）**；两者都会立刻重推所有已启用 sing-box 的节点（不再是"等到下次重启才由 `SyncSingboxConfigs` 补上"）。**这张取舍本身没变**：全局共享仍意味着无法按订阅吊销代理访问。
> - **不变量（别把它改回去）**：密文存在但**解不开**（换过主密钥、恢复了旧库）时**绝不当作"未配置"**去生成新密码——那等于在操作员正在处理密钥事故时静默轮换，把还能用旧密码的客户端全部打断。此时安装/配置同步直接失败并保留原值（与服务端缺主密钥时拒绝启动同一种态度）。

> **实现修订 2026-09-17e（取消全局密码：创建入口时现生成一份，跟着入口走）**：上一版把密码做成服务端持有的**单一全局值**（`settings.anytls_password`），每个面板安装的节点都烤进同一个串。这条取舍本身有问题：一个凭据泄漏就要全场轮换（打断所有客户端），而"某个节点的密码"与那个节点毫无关系。现在密码**属于入站**：
> - **生成时机 = 创建入口**：服务端创建一个入口时现生成一份 16 位 `[A-Za-z0-9]`——安装节点（创建面板自己的 anytls 入站）、编辑器里「新增入站」留空凭据（密码类协议生成密码，vless 生成 v4 UUID）。字符集与拒绝采样的理由不变（同一个串要原样进 JSON 字段、第三方客户端 URI auth 位与 Clash YAML 标量，字母数字是唯一在三种上下文里都不用转义的集合）。一个节点上新增第二个入站就多一个独立凭据，泄漏面按入口切分。
> - **它住在节点自己的配置里**：`singbox_config:<node_id>`（面板写的那份文档）与探针上报的 `local_config`。每次重新生成配置（改端口、重装、启动时的模板同步、渲染订阅兜底路径）都**从那里读回来**，绝不新生成——轮换只发生在"这个节点还没有入口"的时候。`node_singbox.password_override`（§19.9）退化成最后的兜底：面板不再有写入方，但手工设过的值仍优先于新生成。
> - **老节点零迁移**：0.1.1 及更早的节点，密码本来就烤在它们自己的 `config.json` 与面板存的那份文档里（那个值是当年的全局密码）——读取路径从"读全局设置"换成"读文档"之后，它们照旧服务同一个密码，不需要迁移步骤，升级也不会触发轮换。
> - **删掉的入口**：`settings.anytls_password`（`PUT /api/settings` 不再接受这个键）与 AI 工具 `set_anytls_password`（§12.3 表同步删除）。**代价（有意）**：面板不再有"一条命令轮换全场"的杠杆——那种杠杆的对价就是全场客户端同时失效。要换某个节点的密码，在编辑器里改它那个入站（重启一次 sing-box，只有这个节点的客户端需要重拉订阅）。
> - **不变量（与旧版同源，别改回去）**：① 密文存在但**解不开**（换过主密钥、恢复了旧库）时**绝不当作"没有凭据"**去新生成——`node_singbox.local_config` 读不出来时安装直接失败并保留一切（`TestUnreadableReportIsNotMintedOver`）；② 只有"创建入口"这条路径会生成，配置同步与订阅渲染都必须复用读到的值，否则等于静默轮换；③ 生成动作写 `audit_logs`（actor=`system`、action=`anytls_password_generated`、command=`[redacted]`，现在带 `node_id`），值永不入审计/日志。

### 10.2 入口（entry）：中转拓扑与订阅命名（2026-09-16c 新增）

**需求**：节点 A 上的一条 nftables 转发的目标正好是节点 B 的 anytls 入站端口（`dst_ip ∈ B 的 IP`、`dst_port == B 的入站端口`、`tcp`）时，这两个节点在订阅里应产出 **3 个入口**：A 直连、B 直连、**B 经 A**（入口是 A 的 `ip:转发源端口`）；并且每个节点在订阅里的名称要能自定义。

- **订阅绑定的单位从「节点」变成「入口」**：身份 `(node_id, relay_node_id, proto, src_port, iface)`，`relay_node_id=''` 即直连。渲染时一个入口 = 一个 anytls outbound：

| 入口 | server | port | 证书 pin |
|---|---|---|---|
| A 直连 | `A.primary_ip` | A 的入站端口 | A 的自签证书 |
| B 直连 | `B.primary_ip` | B 的入站端口 | B 的自签证书 |
| B 经 A | `A.primary_ip` | 那条转发规则的 `src_port` | **B 的自签证书** |

  DNAT 在 L4、TLS 仍终结在 B，所以中转入口 pin 的仍是 B 的证书、SNI 仍是全局 `www.bing.com`——`singbox.ProxyNode` 本来就是 `(server, port, cert)` 三元组，**渲染器零改动**，新增的只是「入口装配」这一层。

- **推导规则（服务端，无新 wire、无新探针状态）**：`node_forwards` 快照里 `proto=tcp`、`dst_port == B.singbox.port`、`dst_ip ∈ B 上报的 IPv4 集合`（`node_ips` 里 `family=4`，不限于 `primary_ip`——入口渲染才一律用 `primary_ip`）的每条规则，就是 B 的一个中转入口。排除自环（`A == B`，或 `dst_ip` 落回 A 自己）。§21.1 说规则集才是事实，所以转发一改，入口跟着改——那是这个设计的重点，不是副作用。
- **落账 + 墓碑（自动纳入，2026-09-16c 修订）**：新表 `subscription_entries`（SchemaVersion 9→10）。当某订阅里**已启用**某节点 B 的直连入口时，B 的所有**可渲染**中转候选自动落成 `enabled=1` 的行并落审计 `subscription_entry_auto_added`；操作员取消勾选写 `enabled=0` 的**墓碑**而不是删行——reconcile 不会把它加回来。`sub.relay_auto_include`（布尔式设置，默认 on）关掉就退化成「只作候选、手动勾选」。触发点：服务端启动、探针上报新的 forwards 快照（hub 观察者）、面板读候选列表。
  - **代价（说清楚）**：订阅输出会在转发规则变化后**自己变**（这正是本需求要的），所以自动纳入必须可见——入口在面板上带「由转发规则发现」标记、审计里有记录、取消能长期生效。反过来：不可渲染的候选（B 还没有证书/端口）**不落账**，免得表里堆一批渲染不出东西的行。
  - **候选消失不删行**：转发规则被删、探针离线导致快照失真时，入口标 `unavailable` 并在渲染时跳过，但**仍留在面板上可取消**——不静默消失，否则客户端的节点集合会毫无提示地变。此时 B 直连入口还在，客户端的降级是合理的。
- **候选范围（§10 的限制继续生效，且按入口逐条判定；2026-09-17i 修订：判定与订阅渲染同源，读上报的文件而不是管理态列——发现型（one-sing.sh）节点的直连与中转入口照常进选择器，落点端口 = 上报配置里的 anytls 入站集合）**：选择器只**列出会出现在输出里的入口**。直连入口要求该节点可渲染（主 IP + 上报文件可产出出站，或管理态端口+证书）；中转入口还额外要求**落地节点**有带证书的 anytls 入站——中转是"借道"，最终 TLS 会话仍然终止在 B，所以 B 必须是可用的 anytls 节点；**跳板 A 自己不需要装 sing-box**（它只转发包），但必须有主 IP 才谈得上入口。已绑定但**当前**不可渲染的入口仍然列出（灰显 + 原因、可取消勾选），否则操作员保存一次就会把绑定关系悄悄丢掉——这与 §10 原口径完全一致。
- **命名（每个节点可自定义）**：
  - 直连入口：`alias` → `nodes.sub_name`（新增列，面板「编辑服务器 → 订阅名称」）→ `nodes.name` → 节点 id。
  - 中转入口：`alias` → `sub.relay_name_format`（默认 `{name} · {relay}:{port}`；占位符 `{name}{relay}{host}{port}{proto}{iface}`，必须含 `{name}`）。`{name}` = 目标节点的最终基础名，`{relay}` = 跳板节点名。默认带 relay + port 是必需的：多条规则指向同一 B、多个中转指向同一 B 时默认名不能撞车。
  - 出站 tag 仍由最终名派生 `fobe-<name>`（重名 `-2` / `#2` 留作兜底），但**显式 alias 撞名在保存时就拒**（`alias_conflict`）：静默加后缀会毁掉写模板的人的预期。alias 1–64 字符、禁控制字符；两个渲染器都走 escape，别名只出现在带引号的标量里，没有注入面。
  - **代价**：改 alias/名字 = 改 tag，模板里 pin 过 `fobe-<旧名>` 的 selector/urltest/final 会失配——重拉订阅即恢复（订阅本来就是干这个的，同 §10 实现修订 2026-09-16 对 tag 的取舍）。
- **接口**：`GET /api/subscriptions/{id}/entries`（候选 + 已绑定，含 `selected/alias/auto_name/available/reason/warning/discovered`）；`PUT /api/subscriptions/{id}/nodes` 接受 `entries`（**`node_ids` 保留为兼容路径**：等价于只维护直连入口，中转行不动）；`PATCH /api/nodes/{id}` 接受 `sub_name`。§17 快照带上入口与 alias（老快照只带 `node_machine_ids` → 导入为直连入口，与旧行为等价）。
- **v1 边界**：只做单跳（`C→A→B` 不合成「B 经 C」，组合爆炸且每跳 masquerade 语义不同）；入口身份里的 `proto/src_port/iface` 与 §21 的规则身份逐项对齐，且一律用 `''` 而不是 NULL（SQLite 唯一索引允许多个 NULL，复合主键里塞 NULL 会漏掉去重）。遮蔽（§21.9）只在出口侧告警，不在转发侧拒绝。

---

## 11. Web 终端（2026-09-15 修订：单一 Agent PTY，SSH 模式与凭据托管移除）

- 前端 xterm.js ↔ `/ws/terminal?node=<id>` ↔ server 转发 ↔ agent；agent 收到 `terminal_open` 一律用 `creack/pty` 起本地 shell，**不再内置 SSH 客户端、不依赖探针 sshd、不在面板保存任何 SSH 凭据**。适用于禁密码登录的 VPS、dropbear 行为怪异的 OpenWrt。
- 代价绑定：探针必须能起 PTY（容器内挂载 devpts）；无法再"借道"探针上已有的 sshd 配置。
- 兼容：`TerminalOpen.Mode` 字段保留在 v1 wire 上（server 恒写 `pty`）；旧浏览器发来的 `ssh` 由 server 规范化为 `pty`，旧 agent 收到 `ssh` 也只起本地 PTY。旧 `node_ssh` 凭据表在服务端启动迁移时删除。
- 会话初始化时记审计：操作者、节点、来源 IP、会话 ID、开始/结束时间。浏览器帧限 1 MiB，会话 ID 由 server 生成并覆写，浏览器不能伪造。
- 终端与 AI 执行共用 agent 指令通道 → 审计口径统一。
- 终端页三个盒子（2026-09-15 修订）：卡片头一行（左标题「Agent Web 终端」，右会话 ID +「重连」+ 连接状态点）→ `.terminal-shell`（深色屏幕框，8px padding 让文字不贴边）→ `.terminal-host`（FitAddon 的量测盒）。卡片自身用默认面板底色，屏幕是页面上唯一的深色面；页面不再另起标题。
- **padding 不能放在 `.terminal-host` 上**：xterm 的 `.xterm` 是绝对定位盒，绝对定位子元素相对宿主 **padding box** 定位，`inset: 0` 会把字形区直接铺满 padding、把留白盖掉（这就是"padding 设了却依然贴边"的成因）。所以外框（padding/border/深色底）与量测宿主必须是两个元素。
- `.terminal-host` 高度**必须由卡片所在的网格行决定、不能被 xterm 内容撑高**：宿主 `flex: 1 1 auto`（不写内容高度）、内部 `.xterm` 绝对定位填满，否则每次 fit 都会把外框的 padding+border 折成行数加回去，现象是终端每帧长高一行；fit 只由 rAF 防抖的 `ResizeObserver` 触发（`onResize` 里调 fit 会同步递归），宿主不可见（宽高 ≤0）时跳过 fit，避免把活着的 PTY 缩成 2×1。
- v1 不做 PTY 全量录制（体积与隐私成本高），但保留 `session_id`，便于后续开启录制。

---

## 12. AI 助手

### 12.1 形态

- 服务端代理到 OpenAI 兼容 API：`base_url`、`api_key`（AES-GCM 加密存 `settings`）、`model` 均由面板配置；支持流式输出。
- 三键缺一即视为**未配置**（2026-09-15 修订）：判定收敛为服务端唯一函数，`GET /api/settings` 额外返回派生字段 `ai_configured`（不是 setting，不受 `allowedKeys` 影响），终端页只在它为 `true` 时渲染 AI 侧栏——否则侧栏每次提交都只会拿到 503 `ai_not_configured`，不如不显示，终端独占整宽。
- 上下文注入（默认）：节点列表、当前指标、流量与配额、延迟、sing-box 版本与状态、最近告警。
- **默认不注入原始日志**：需要时由你在对话里显式打开"附带日志（最近 N 行）"开关。这既减少 token，也显著缩小注入面——**但不改变你选的默认放行语义**。

### 12.2 工具集

| 工具 | 类型 | 说明 |
|---|---|---|
| `list_nodes` / `get_metrics` / `get_traffic` / `get_latency` | 只读 | 结构化查询 |
| `get_singbox_status` / `tail_logs` | 只读 | 需显式开关日志 |
| `restart_singbox` / `stop_singbox` / `start_singbox` | 变更 | 走同一闸门 |
| `install_singbox` / `update_singbox` | 变更 | 指定版本 |
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
  - `icmp`：ICMP echo RTT。优先 raw socket（root 或 `CAP_NET_RAW`）；其次 Linux 内核 ping socket（`SOCK_DGRAM`，仅当 `net.ipv4.ping_group_range` 放行当前组）；两者皆不可用才跳过并标记（实现修订 2026-09-16）。
  - `tcp`：TCP 三次握手 RTT（连接目标 host:port 后立即关闭）。
- 每台探针选择自己用哪些目标（多选）。
- 采集：**探针本地按全局设置 `latency.interval_seconds` 测量，默认每 5s 一次**（允许 1–3600 秒），本地保留 60s 窗口并**每 60s 批量上报**。设置变更立即推送给在线探针，离线探针会在下一次 `hello_ack` 收到；不是逐点上报，否则 10 探针会出现 2 次/秒的常驻写入，且网络抖动会污染测量本身。
- 存储：`latency_samples` 保留 7 天。
- 数据量估算（默认 5s）：5s × 7d = 120,960 点 / (探针×目标) 对；10 探针 × 5 目标 ≈ 600 万行，SQLite 可承受（每行 ~40B，约 250MB 量级）。更短的频率会按比例增加数据量。**图表必须降采样**：1h 视图取原始点，24h/7d 视图按 5min/30min 桶取平均 + P95。
- 前端：折线图 + 丢包率；每探针一张"多目标对比"图。
- **设置入口（实现修订 2026-09-16）**：全局频率 `latency.interval_seconds` 的输入面板从「基础设置」页移到设置子页 **延迟测量**（`/settings/targets`，原页名「延迟目标」改为「延迟测量」）——"多久测一次"与"测哪些目标"是同一件事的两半，现在同页；该卡片自带 settings 草稿状态（页面是 Settings 的 route 子节点，借不到父级的 draft map），保存与默认值兜底（空值落回 5s）语义不变。
- **目标变更即时下发（实现修订 2026-09-17k）**：测量目标此前只随 `hello_ack` 下发，面板改完 `latency_target_ids` 后对在线探针**什么都不推**——稳定长连接的探针要等下一次 WS 重连（可能永远不会发生）才换目标列表，表现为"给已有节点加了延迟测量，详情页永远不出新折线"（前端对没有样本的目标不画线）。现在编辑保存后立即对该节点下发携带目标列表的 `latency_config`（`hub.PushLatencyTargets`，与 `hello_ack` 同源读 `TargetsForNode`，两个载体不会不一致）；删除全局目标同样即时推送所有仍选中它的节点（先查 `NodeIDsForLatencyTarget` 再删，FK 级联会抹掉线索）；离线探针由下次 `hello_ack` 兜底。字段语义：`latency_config.targets` 为 `null`（全局频率广播的形态）＝目标不变；非 null 空数组＝清空、真的停测——所以该字段**不能加 `omitempty`**，否则清空永远到不了 agent。旧 agent 忽略未知字段不受影响，自更新后自然跟上。**同日补齐显示侧（17k 的另一半）**：下发修好后真机仍"看不到新折线"——目标是防火墙拦 ICMP 的地址，每个样本都 `icmp_ms=-1, loss=1`，前端对没有有效 RTT 点的目标不画线，于是「在测但全丢」与「没在测」在面板上无法区分。现在详情页对画不出线的目标显式标注：有样本标「全部丢包 · 目标无回应」（提示换 tcp 类型），无样本标「暂无样本 · 探针离线或尚未开始」。

---

## 14. IP 检测与国旗

- agent 首次启动、网络变化（每 5 分钟检查 IP 集合是否有变化）、以及面板手动触发时，枚举本机地址（`net.Interfaces`，排除 loopback/link-local）→ **全量上报**。
- 服务端判定：
  1. 本地 GeoLite2-Country MMDB（离线，面板可上传更新，**也会自动下载**——见 §14.1）；
  2. 未命中且可配置在线 API（ip-api / ipinfo，可选配 key）作为回退。
- 国旗 = ISO 3166-1 alpha-2 → emoji/图标资源（前端内置，不依赖 CDN）。
- 主 IP 选择：默认第一个公网 IPv4，否则第一个公网 IPv6；面板可手动指定（`node_ips.is_primary` + `manual_primary`），订阅渲染使用主 IP（或你填的域名）。**手动主 IP 的口径（修订 2026-09-15）**：`nodes.primary_ip` 是列表/卡片/订阅实际读取的字段，手动选择必须落在它上面——`ReplaceNodeIPs` 在每次全量上报时若手动选择仍在上报集内，就把 `nodes.primary_ip` 同步写回该地址（同事务）；hub 只在当前 primary 为空或不在上报集内时才重新指认（含 agent 未标 primary 的回退分支）。此前实现只在 `node_ips` 里保住标志、不同步 `nodes.primary_ip`，被旧版覆盖逻辑带偏的值（仍在上报集内）永远不会自愈，表现为列表一直显示 agent 自选地址。
- **手动国旗（修订 2026-09-16）**：geoip 对 CDN/中转/隧道后的机器经常判错，且主 IP 一变（换网、手动换主 IP）旗子就跟着错，所以国旗要能手动改。编辑服务器页新增「国家 / 地区」字段，走 `PATCH /api/nodes/{id}` 的 `country_code`：填两位字母即钉住（`nodes.country_manual=1`，镜像 `manual_primary` 的先例），此后主 IP 变化/重新指认**不再**回退为 IP 库结果——`countryFor` 遇 manual 直接短路，且 `SetNodePrimaryIP` 在 SQL 里 `CASE WHEN country_manual=1` 保留原值，"手动优先"是行本身的性质，不依赖每个调用方记得判；清空提交即取消钉住并**当场**用当前主 IP 重查一次（`GeoIPResolver` 未命中则保留现值），"回到自动"不用等下次 IP 变化。校验只认 `[A-Z]{2}`，其余回 `invalid_country_code`。schema 加列 `nodes.country_manual`（SchemaVersion 2→3）。
- **国旗跟随主 IP 的口径（实现修订 2026-09-16b）**：国别判定原本只在 `onState` 的「重新指认主 IP」那一个分支里做，另外三条路径全是哑的，于是"根据主 IP 判国旗"最常见的表现就是**永远一个灰点**：
  1. **hello 路径从不解析国别**（直接把 `n.CountryCode` 原样写回），而探针的 `primary_ip` 常常正是被首次 hello 写下的；此后每条 state 都命中「主 IP 已在上报集内」的守卫，再也不会重算 ⇒ 新装的探针一辈子没有旗子。
  2. **手动主 IP**（`PUT /api/nodes/{id}/primary-ip` → `SetManualPrimary`）只改地址，不重算国别。
  3. **查不到就保留旧值**的规则（本身是对的：数据库缺失不该抹掉旗子）叠加上面两条，就变成"主 IP 换成内网地址后旗子还留着上一个地址的国别"。
  现在：hello 与 state **共用 `hub.recordIPs`**（一份「地址集入库 + 主 IP 指认 + 国别判定」的代码，两条路径不可能再走偏）；国别**每次上报都重算**（命中才写、未命中保留旧值，因此写放大被 `code == 已存值` 挡掉）；判定顺序 = **主 IP → 上报集里的公网地址（IPv4 优先，再 IPv6）**——这是操作者把主 IP 手动选成 `192.168.123.x` 这类内网地址（想显示可直连地址）时仍能出旗的原因，纯内网、又不带任何公网地址的探针仍然只能靠手动钉。`country_manual=1` 照旧短路一切重算。面板侧两处同步：手动主 IP 与「清空国家」都立刻调 `hub.RefreshCountry`（不等下一次最长 5 分钟的 state）；编辑页**只在操作者真的动过国家输入框时**才提交 `country_code`，否则保存任何其它字段都会把自动识别到的值静默钉住（此前 3 台节点全被这么钉成了手动，自动识别就此"失效"）。


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

通道：**Telegram Bot** + **通用 Webhook**（JSON POST，HMAC 签名头）+ **飞书**（2026-09-16 修订）。

- 同类告警去重合并（同一节点同一类型 1 小时内只发一次，恢复时补一条 recovery）。
- 面板内"通知中心"保留全部历史，`alerts` 表为权威。

### 15.0 渠道与事件开关（2026-09-16 修订：通知独立成页）

设置里**通知不再是基础设置页上的一张卡片**，而是与「服务器」同级的一个子页（`/settings/notifications`，§16）：页面自上而下是**渠道**（Telegram / 飞书 / 通用 Webhook，同级并列，各自带开关、配置与"发送测试消息"）→ **事件开关** → **流量阈值**。

- **渠道开关**：`notify.telegram_enabled` / `notify.webhook_enabled` / `notify.feishu_enabled`。关掉只停推送、**不清空配置**（重新打开不用重填凭据），也不影响"发送测试消息"——测试走 `Deliver`，开关只管调度。`Configured()` 语义保持"有配置"，与开关正交。
- **事件开关**：`notify.event.<组>`，组是按操作者心智划分的（不是按 kind 一一对应）：`node_status`（探针上线下线，含恢复）、`traffic`（流量阈值：warn/crit）、`billing`（缴费到期：7/3/1 天与逾期）、`singbox`（sing-box 异常：退出/回滚）、`updates`（sing-box 批量更新未收敛、探针自更新失败/落后）、`counter_reset`（计数器重置）。未知 kind 归入 `updates`，保证新告警类型天然可关。
- **两侧都默认开**：设置项缺省（或值不可解析）= 开，否则一次升级就会静默掉已经生效的通道/事件。
- **关掉 ≠ 攒着**：被开关拦下的告警**直接出队**（`scheduler.deliverOne` 对"没有任何渠道接受"返回已处理）。取舍：重新打开开关不会补推关闭期间的历史——把静音期当成"待发队列"会在重新打开时一次性倒灌，那不是"别打扰我"的意思；`alerts` 表仍然是全量权威记录，面板告警页照常可见。某个渠道失败（非开关原因）仍然留在队列里下轮重试。
- 流量阈值（`alert.traffic_warn_pct` / `alert.traffic_crit_pct`，默认 80/100）也落在这一页——它们只被 `traffic` 事件使用，放在别处等于让用户满仓库找旋钮。

### 15.1 飞书通道（2026-09-16 修订）

两条子通道，共用 `internal/server/notify` 里的一个 `Feishu` 通知器（**应用模式优先于群机器人模式**，两者都配时只走应用）：

1. **应用机器人（推荐，支持"直接扫码接入"）**：面板调用 `POST https://accounts.feishu.cn/oauth/v1/app/registration`（RFC 8628 设备授权风格）拿 `device_code` + `user_code`，把 `verification_uri_complete` 渲染成二维码（前端用 `qrcode.react`，自托管、不引 CDN）。用户用飞书 App 扫码 → 在**自己的租户里确认创建一个自建应用**（`archetype=PersonalAgent`，预填应用名/描述，并经 URL 上的 gzip+base64url `addons` 预授权 `im:message:send_as_bot`）→ 面板轮询 `action=poll` 拿到 `client_id/client_secret` 与扫码人的 `open_id`，加密落库并把**扫码人本人**设为接收者。此后告警由机器人以私聊形式送达：`POST /open-apis/im/v1/messages`（`tenant_access_token` 进程内缓存，过期前 60s 刷新，token 失效自动换新重试一次）。若轮询响应里 `user_info.tenant_brand=lark`，后续请求整体切到 `accounts.larksuite.com` / `open.larksuite.com`（与官方 SDK 一致），域名记在 `notify.feishu_domain`。
- 代价：这一步**需要一个飞书账号并联网访问飞书的域名**（面板自己出网，不需要公网入站/回调地址，因此局域网部署也能用）；扫码确认页由飞书托管，**面板无法替用户跳过"创建应用"那一次确认**；会话是进程内状态（重启即失效，重新扫一次即可，不影响已绑定的配置）。手动填写 `App ID/App Secret/接收者 ID` 是同一条发送路径的备选入口（老应用、或因组织策略不便扫码的场景），接收者 ID 按前缀判定类型（`ou_` 用户 / `oc_` 群 / `on_` union），也可手填 `chat_id` 把告警发进群。
- 安全：`app_secret`、群机器人 webhook URL（路径里自带 token）与加签密钥都按 §4.4 经 `security.Cryptor` 加密存储、GET 不回读；解除绑定是**删除**设置行（空值仍会被当作"已配置"，见 store 的 `DeleteSetting`）。
2. **群自定义机器人**：在飞书群里添加"自定义机器人"拿到的 webhook 地址（+ 可选加签密钥）直接粘贴即可，不需要应用凭据；POST `{msg_type:"text", content:{text}, timestamp, sign}`，`sign` 为 HMAC-SHA256（**密钥是 `"<unix秒>\n<加签密钥>"`、消息为空**）、base64 编码，与 Telegram/通用 Webhook 的签名方案不同，别混用。

---

## 16. 前端

- 技术：**React + TypeScript + Vite**；构建产物输出到 `web/dist`。**生产镜像在构建阶段把产物烤进镜像**（`/srv/web`），由 server 直出（`FOBE_WEB_DIR`）——不嵌入 Go 二进制（避免体积膨胀），也不再依赖宿主机挂载前端目录。若想让外部 nginx 直接吐静态文件，见 §3 末尾的替代做法。
- **本地开发不走容器**：前端 `npm run dev`（Vite dev server，把 `/api`、`/ws`、`/sub`、`/install.sh`、`/dl` 代理到 `http://127.0.0.1:8080`，**WebSocket 代理必须开 `ws: true`**），后端 `go run ./cmd/server`。此时把 `FOBE_WEB_DIR` 留空 → server 进 **API-only 模式**：`/` 返回一句"请访问 Vite dev server"的提示（不 404、不白屏），其余接口行为与生产一致。
- **`scripts/dev.sh` 与自更新（2026-09-15 修订）**：dev.sh 现在除 server 外还交叉编译 linux/amd64 的 agent 产物，并给两端注入同一个内容寻址版本号 `dev-<哈希>`（§5.5 实现修订），所以本机 dev 也能真跑"探针跟随服务端"；`go run ./cmd/server` 没有 `-ldflags`、版本仍是 `dev`，不会下发目标。降级演练用 `FOBE_VERSION=<旧号> scripts/dev.sh`；改后端必须重启 dev.sh 这条老规矩不变，但版本号只在 agent 源码真的变了才变——重启本身不再惊动探针。
- 页面：登录 / 概览（卡片墙）/ 节点详情（**只读监控**：指标 + 图表 + 流量 + 延迟历史）/ 订阅与模板 / 延迟测量 / 告警 / 终端（全屏）/ AI 助手（侧栏）/ 设置（**通知**、AI、GeoIP、保留期、主密钥状态、**服务器**、**延迟测量**、**审计日志**）。（2026-09-15 修订；2026-09-16 修订：详情页「命令」卡片移除，见下；2026-09-16 修订：通知从基础设置页独立为子页，见 §15.0；2026-09-16 修订：延迟目标页更名延迟测量并收编测量频率面板，见 §13）
- **配置入口收敛（2026-09-15；2026-09-16 列完善；2026-09-16 修订：添加节点按钮随之下沉到本页）**：设置里新增「服务器」页，表格列出已接入的服务器，明确显示文字状态、主 IP、过期时间、版本，行尾为编辑/删除图标按钮；未配置缴费周期时过期时间留空，已配置时显示距离 `next_due_at` 的剩余或逾期时间。「编辑服务器」页集中承载该服务器的全部配置：节点设置（名称/备注/**只读时区**，2026-09-16 修订：时区从「流量周期」区块上移——它约束的是全部按日切分的记账，不属于某一个周期）、流量（agent 上报的网卡下拉/统计模式/配额）、**独立的流量周期**（无/按月/按年与秒级下次重置时间）、缴费周期（类型/长度/下次到期日/费用）、延迟测量端点选择、IP 列表（含手动主 IP）、sing-box 服务端配置（版本/启停/端口）。节点详情页不再承载配置表单与删除按钮——监控与配置分离，删除服务器统一走设置页（带确认）。**「添加节点」（注册 token + 安装命令弹窗）原先挂在概览页页头，现移到本页页头**：注册探针是接入/配置动作而非监控动作，概览页因此退回纯只读卡片墙；空态文案（`no_nodes` / `servers_empty`）同步指向新位置。
- **顶栏与表格的窄屏适配（2026-09-16）**：顶栏全部按钮带图标，退出/语言切换补上图标后与其他按钮一致；手机竖屏（≤640px 且 portrait）顶栏按钮只显示图标（文案保留在 `title`/`aria-label`），actions 禁止换行，sticky topbar 高度不被按钮文字撑高。设置→服务器列表的状态列（圆点+文字）`nowrap` 保证单行；编辑服务器页 IP 列表的超长 IPv6 在单元格内折行（`overflow-wrap: anywhere`，只有它会压缩 min-content 宽度），不再把 auto 布局的表格顶出卡片。基础设置页与订阅页里「卡片内裸表格」统一包进 `.table-wrap`（`overflow-x: auto`）——卡片既不裁剪也不滚动，宽行（审计日志、UA、mono 值）原本会画出卡片外，现在横向滚动由容器接管。**编辑服务器页的两张规则表按同一条规矩补齐（实现修订 2026-09-17c）**：sing-box 入站编辑器（`.sb-inbound-table`）的控件此前没被任何面板输入样式匹配（`.field input` 只管 `.field` / `.cmd-input-row` 里的控件），落回浏览器默认外观，而六列在 390px 竖屏下把表格顶出卡片右侧 119px（页面 `scrollWidth` 493 > 390）；现在控件自带面板样式（密集高度 32px）+ 表格包进 `.table-wrap`，页面横向溢出归零。端口转发规则表则相反，原先被"压缩换行"顶高：短字段一律 `nowrap`、备注列单行省略号 + `title`、行内按钮改 `.row-actions`（`flex-wrap: nowrap` 且**按钮自身也要 `white-space: nowrap`** —— flex 默认 `flex-shrink: 1` 会把中文按钮文案压成两行，单元格照样 56px 高），规则行从 240/123px 降到 45px。代价：两张表在手机竖屏上要在卡片内横向滚动才能看到最后一列（与上面 §16 的取舍一致）。**「表格即卡片」的四页补上滚动容器（实现修订 2026-09-17d）**：设置→服务器列表、审计日志、告警、延迟目标这四页的 `<table class="table card">` 把卡片外观直接放在 `<table>` 上，其 `overflow-x: auto` 从未生效——overflow 不适用于表格盒，宽行（mono IP/版本 + nowrap 操作列）在 390px 竖屏下把表格顶到 min-content 宽（实测页面 `scrollWidth` 587 > 390），整页随之左右平移。现在卡片与滚动都由外层 `.card.table-card` 容器接管（表格只留 `.table`），页面横向溢出归零、桌面端无滚动条；代价与前同——竖屏要在卡片内横向滚动看最后一列。
- **详情页「命令」卡片移除（实现修订 2026-09-16）**：面板不再提供对探针的任意 shell 下发入口——前端命令卡片与 `POST /api/nodes/{id}/commands` 路由一并删除。指令队列本体（`commands` 表、`enqueueCommand`、离线排队与幂等）保留：AI 执行（§12）与面板动作（sing-box 启停/重启）仍走同一条队列与审计；`GET /api/nodes/{id}/commands` 保留，供 AI 面板轮询执行结果。取舍：普通面板用户少一个"顺手敲 shell"的危险面，命令执行的入口收敛到 AI 确认流（§12.3）。
- **基本信息新增「发行版」（2026-09-16）**：agent 读 `/etc/os-release`（回退 `/usr/lib/os-release`，仍只做文件读取，遵守不调外部命令的约定）取 `ID` + `VERSION_ID`，随 `hello` 与注册请求上报，落 `nodes.distro_id / distro_version`（增量迁移，`SchemaVersion=2`）；详情页基本信息显示「Debian 13」式标签与自绘发行版徽标（内联 SVG、自托管，不引 CDN）。OpenWrt 衍生系统（iStoreOS、ImmortalWrt 等）改写 `ID` 但保留 `ID_LIKE="lede openwrt"`，按 `ID_LIKE` 归一化为 openwrt，并在 os-release 缺失时回退 `/etc/openwrt_release`。没有 `VERSION_ID` 的滚动发行版（如 Arch）只显示名称；未知 ID 回退首字母徽标，旧 agent 未上报时显示 `-`。
- **详情页「AnyTLS 端口」卡片（2026-09-16）**：基本信息网格在 **sing-box 已启用**（`desired_version` 非空）时多一张 `AnyTLS 端口` 卡片，显示入站端口（副标题为当前版本），数据来自 `GET /api/nodes/{id}` 已在返回的 `singbox` 对象——不额外发请求。未启用、或已被卸载（`desired_version=''`）时不渲染：那时端口字段不再指向任何在跑的东西。编辑服务器页的 sing-box 卡片同步新增「卸载 sing-box」按钮（二次确认，见 §9.2 实现修订）：卸载下发后探针在线时每 5s 重读一次状态（与"安装中"同一条轮询），离线则如实提示"卸载已记录,重连后执行"。
- **端口转发卡片（实现修订 2026-09-16，§21）**：编辑服务器页新增「端口转发（nftables）」卡片，列出探针 `ip nat prerouting` 里所有 DNAT 规则——**包括 nfpf.sh 或手工命令加的**——并可增删改。列表默认读服务端快照（打开页面不阻塞探针、离线也能看），「从探针刷新」按钮才走一次实况往返；改动手感是同步的：在线探针一次往返内返回新规则集，离线/超时则如实提示「已入队，探针上线后执行（10 分钟内有效）」并每 5s 重读快照直到探针回答。规则带面板不建模的匹配条件（源地址、计数器、端口范围）时只允许查看与删除，编辑按钮禁用并给出原因提示。备注（comment）可写，写在 DNAT 规则上、位置与 nfpf.sh 相同；双引号在 nft 字符串语法里无法表示，因此被拒绝（见 §21.3）。保存后若备注为空，agent 会回报 `comment_not_applied`，卡片按 §21.6 给出"agent/后端是旧二进制"的红字提示，不静默。

- **审计日志独立成页（2026-09-16）**：审计从基础设置页的卡片移出，成为设置的子页 `/settings/audit`（子导航入口「审计日志」），表格结构与 `GET /api/audit` 的其余字段不变。节点列显示节点名称而非 ID：`ListAudit` 读取时 `LEFT JOIN nodes` 带出 `node_name`（`COALESCE` 成空串，避免无节点记录的 NULL 扫描错误），改名后的历史记录显示当前名称，节点已删除时前端回退显示原始 ID；审计表本身不回写，无 schema 变更。
- 实时（2026-09-15 修订）：`/ws/events` 只覆盖状态类变化（节点增删改、sing-box、订阅、设置、GeoIP）——**常规指标上报不产生任何事件**，所以数据新鲜度必须靠「轮询 + 高频上报」两条腿：
  - **概览**：挂载且标签页可见期间，对每个在线节点打开 §16 的 5s 探测流，并 5s 拉一次列表。打开探测流是必要的：不打开就只能等 60s 基线节奏，卡片墙看起来像「不自动更新」。
  - **详情**：5s 拉节点快照（与探测流对齐），30s 拉图表 / 流量 / 延迟曲线（重查询）。
  - **标签页隐藏时全部交还 60s 基线并停轮询**（`visibilitychange`），避免一个忘记关闭的标签页把每个 agent 永久钉在 5s。离线节点 409 直接忽略；节点重新上线后会重新打开探测流。
- 节点卡片展示：名称、国旗、IP、CPU%、内存%、磁盘%、CPU 核数、流量使用率（按所选模式）、今日上下行、**费用标签**（2026-09-16 新增）、到期倒计时、延迟摘要。费用的取值就是缴费周期里那行自由文本（`node_billing.note`，随 `GET /api/nodes` 的 `billing_note` 一起下发，省掉逐节点详情的额外请求），非空即贴，位置固定在卡片头、发行版徽标左侧（“这台多少钱”挨着“这台是什么”），用 accent 色与状态色区分；不解析币种与周期，也不要求缴费周期已配置——它是操作者写给自己看的一行账。**连接状态改由卡片自身样式表达（实现修订 2026-09-16）**：卡片头右侧的位置改为发行版徽标（复用详情页的自绘 SVG，未知/未上报 ID 回退首字母徽标），不再画在线状态小圆点；离线卡片红边框 + 底色红色斜纹（`--red-weak`，明暗主题各有一档），状态文字保留在卡片 `title` 提示里。**到期倒计时对过期节点直接贴「已过期」标签（实现修订 2026-09-17）**：到期日存本地零点，过期之后 `Math.ceil(负的零点几)` 得到 `-0`，而 `-0 >= 0` 为 true，卡片曾把已过期的节点显示成「0 天后到期」；现在直接比较时间戳，非未来一律「已过期」（红色 chip，未过期仍是 amber 倒计时）。设置→服务器列表的到期列本来就是「已逾期 <时长>」，不受影响。
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
├─ docker-compose.yml         # 仓库根：默认拉 GHCR 预构建镜像，--build 落回本地构建；零必填环境变量
├─ deploy/
│  ├─ Dockerfile.server       # 多阶段：node 构建前端 → go 构建 server/agent → 运行镜像（不含 nginx）
│  └─ docker-entrypoint.sh    # 主密钥解析（env > KEY_FILE > 自动生成进 /data 卷），admin 子命令直通
├─ .github/
│  └─ workflows/release.yml   # v* tag → gofmt/vet/test + 前端构建 → 镜像 → ghcr.io/<owner>/<repo>
├─ scripts/
│  ├─ build.sh                # 本地/CI 三件套构建（镜像内自行多阶段构建，不依赖它）
│  ├─ pgo.sh                  # 采/并 CPU 剖面 → cmd/server/default.pgo（§17 PGO）
│  └─ install.sh.tmpl         # 安装脚本文本模板（服务端注入 token 后下发）
├─ data/                      # 宿主单一数据目录（gitignore；compose 挂为容器 /data）
│  ├─ fobe.db
│  ├─ .master_key             # 未显式设置主密钥时由入口脚本生成（§4.4）
│  ├─ geoip/                  # GeoLite2-Country.mmdb（§14.1，与 fobe.db 同卷）
│  ├─ dl/                     # 容器内 /data/dl：agent 产物（§5.5）+ sing-box 缓存（§9.5）
│  │  ├─ agent/<version>/     # 二进制 + .sha256 + manifest.json（首次启动由 seed 目录补进卷）
│  │  └─ singbox/<version>/   # 服务端自动下载或手动投放的 sing-box 产物
│  └─ backup/                 # 每日 VACUUM INTO 快照（容器内 /data/backup）
└─ docs/design.md
```

`docker-compose.yml` 要点（实现修订 2026-09-15：从 `deploy/` 移到仓库根，默认拉 GHCR 预构建镜像，本地构建作 `--build` 兜底；主密钥由镜像入口脚本自动生成进 `/data` 卷（§4.4）；同日把 `/data`、`/backup`、`/srv/dl` 三个挂载**合一为单一 `./data:/data` 卷**，DL 与备份成为卷内子目录——compose 里不再需要任何环境变量，镜像 `ENV` 自带全部固定路径）：

```yaml
services:
  server:
    image: ghcr.io/fonlan/fobe:latest   # v* tag 时由 .github/workflows/release.yml 推送
    build:
      context: .
      dockerfile: deploy/Dockerfile.server
      args: { VERSION: "${FOBE_VERSION:-compose}" }  # 本地构建专用；占位缺省会被镜像内换成内容寻址的裸哈希（无前缀，见下）
    ports:
      - "8080:8080"                # 默认全接口映射（明文 HTTP）；要收敛改回 127.0.0.1:8080:8080 或防火墙限来源
    volumes:
      - ./data:/data               # 唯一数据卷：db、.master_key、geoip/、dl/、backup/
    environment:
      FOBE_ADMIN_PASSWORD: "${FOBE_ADMIN_PASSWORD:-}" # 唯一保留的可选项；主密钥/镜像源等要设就在这里照样式加
```

- **没有 nginx 服务，也没有证书卷**：接入层完全外部化（见 §3 与 `README.md`）。
- **单一 `/data` 数据卷**（实现修订 2026-09-15：原先 `/data`、`/backup`、`/srv/dl` 三个挂载合一，DL 与备份成为卷内子目录，镜像 `ENV` 定为 `FOBE_DL_DIR=/data/dl`、`FOBE_BACKUP_DIR=/data/backup`；`VOLUME` 只声明 `/data`）。**DL 目录不能落在容器可写层**：那里的下载在升级/重建容器时全部丢失。挂载自检（`singboxcache/mount.go`）的判据是「`FOBE_DL_DIR` 的覆盖挂载（mountinfo 里最长的父挂载点）是否只是 `/`」——挂在 `/data` 卷下的 `/data/dl` 算通过；落在可写层才写 WARN 并置 `singbox.dl_mount_ok=false`（设置页标红）。症状与处置见 `README.md` 排障表。
- **agent 产物必须放在卷外的 seed 目录**（实现修订 2026-09-15）：compose 把 `./data` 挂到 `/data`，`FOBE_DL_DIR=/data/dl` 在卷内，而挂载在容器启动时就生效——镜像里 `COPY` 进卷的东西**运行时根本读不到**，所以标准部署下 `/dl/agent/<version>/linux-amd64` 默认 404，`/install.sh` 也拉不到 agent。镜像因此把产物放进 `/srv/agent-seed/agent/<version>/`（`FOBE_AGENT_SEED_DIR`，非卷路径），服务端启动时**复制进 DL 卷**并把 `agent/latest` 指向自己这一版。这跟 §9.5 的 sing-box 缓存是两件事：sing-box 的产物由服务端自己联网下载，agent 的产物只能来自镜像（服务端不会自己编译 agent）。
- **本地构建的 VERSION 缺省不再是恒定的 `compose`**（实现修订 2026-09-16）：占位值（空/`dev`/`compose`）会在 golang 阶段被换成**内容寻址**的 `compose-<哈希>`（不带 build-arg 直接 `docker build` 时是 `dev-<哈希>`）——先编一份占位 agent、取产物 sha256 前 12 位，机制与 `scripts/dev.sh` 完全一致（§5.5）。此前 `--build` 部署版本恒为 `compose`：既过不了 `IsReleaseVersion`，等值判定下也永远"已收敛"，探针**永不**跟随服务端；现在改 agent 代码后重新 `--build` 会真的换号并触发自更新，agent 源码没变的重 build 不换号。显式 `FOBE_VERSION=<真实版本>`（发布管线传 tag）原样注入，不受影响。（2026-09-17 修订：占位号去掉 `compose-`/`dev-` 前缀、只留裸哈希——面板把它当徽章显示，前缀是噪声；`IsReleaseVersion` 只要求含数字，跟随行为不变。）
- **前端产物在镜像里，不挂载**：`Dockerfile.server` 的 node 阶段产出 `web/dist` 并 `COPY` 到 `/srv/web`。改前端 = 重新 `docker compose build`，不存在"改了源码忘了构建/挂载路径写错"这类事故。
- **`/api/*` 永远不吃 SPA 兜底**（实现修订 2026-09-15）：静态处理器是最后的兜底（`mux.HandleFunc("/", s.handleStatic)`），未匹配的 `/api/...` 现在直接回 `404 {"error":{"code":"unknown_endpoint"}}`，不再吐出 API-only/`index.html` 那一页。原因是这个组合会造成一个很难查的假象：**旧代码的 server**（没重启的 dev 进程、没重建的镜像、没重启的容器）遇到新前端调用的新接口，会以 `200 text/html` 应答，前端 `resp.json()` 解析失败——而 `apiErrorMessage` 把任何非 `ApiError` 都当成 `network_error`，于是面板报"网络错误,无法连接服务器"，把人往 DNS/防火墙方向带，实际连接完全正常。现在同类情况会明确说是"接口不存在(服务端可能是旧版本)"。前端侧也补了 `bad_response`：2xx 但非 JSON 的响应单独报错，不再伪装成网络故障。（副作用：因为有 `/` 兜底模式，Go 1.22 ServeMux 的自动 405 在 `/api` 下不会触发，方法/路径不匹配统一落到这个 404。）
- 注意：即使你从宿主机 `127.0.0.1` 发起请求，容器内看到的源地址通常是 Docker 网关（如 `172.17.0.1`），所以 `FOBE_TRUSTED_PROXIES` 默认包含 Docker 私网段。

- **Go module path = `github.com/fonlan/fobe`**（实现修订 2026-09-16）：`go.mod` 的 module path 自首次实现提交起写着 `github.com/fobe-panel/fobe`——那个 org 并不存在（HTTP 404），是脚手架期的占位名，而仓库实际发布在 `github.com/fonlan/fobe`。现已全量对齐（`go.mod` + 全部 import 前缀 + 构建脚本里的 `-X`）。**代价**：module path 在 `deploy/Dockerfile.server`、`scripts/dev.sh`、`scripts/build.sh` 里被当字面量抄了 5 遍（`-X …/internal/agent.Version=$VERSION`），而 **`-X` 指向不存在的符号时 `go build` 不报错、静默忽略** ⇒ `internal/agent.Version` 留在 `dev`，§5.5 的发布门槛又是 fail-closed，于是探针自更新被**静默停用**——编译/测试/CI 全绿却少一个功能。改模块名必须同步这 5 处；验证也不能靠 `strings`/`go version -m`（它们命中的是 buildinfo 里记录的 `-ldflags` 原文，必然命中，是假阳性），要临时 `main` 打印 `agent.Version` 真跑出来。
- **构建管线**：`Dockerfile.server` 是多阶段构建——`node:22-alpine` 阶段构建 React 前端 → `golang` 阶段构建 `server` 与 `agent`（agent 交叉编译 `linux/amd64`，`CGO_ENABLED=0`）→ 运行镜像里同时含：server 二进制、`/srv/web`（前端产物）、`/srv/agent-seed/agent/<version>/`（agent 产物 + 带 sha256 的 `manifest.json`，启动时播种进 DL 卷，安装脚本与面板版本选择都读它）。`scripts/build.sh` 提供同一套产物的本地/CI 构建，供不进容器的开发方式使用。**两端的编译口径不同且是有意的**（agent 求体积、server 求速度，见下两条）。
- **agent 为体积编译**（实现修订 2026-09-16）：agent 要下到每一台探针（§5.5 每次自更新都重下一遍）并占着路由器闪存，所以除 `-s -w` 外再加 `-trimpath` 与空 `-buildid=`，并且构建链里有 upx 时用 `upx --best --lzma` 压一道——**实测 7,184,568 → 2,262,820 字节（31.6%）**，压过的产物在容器里实跑过（`-h`/无参启动正常出 usage）且 upx 输出是确定性的（同输入两次打包 md5 相同）。`FOBE_AGENT_UPX=0`（Docker 侧 `--build-arg AGENT_UPX=0`）关掉打包。三条硬约束：①**压缩必须发生在写 `.sha256`/`manifest.json` 之前**，否则"一个版本号"对应两份字节，探针的 sha256 校验必然失败；②**占位构建（内容寻址版本号的那次）也走同一套打包**，于是 upx 版本或开关一变版本号就变，"版本号 ↔ 字节"始终一一对应；③**agent 的 `GOAMD64` 永远钉 v1**——探针的 CPU 是什么我们不知道，v2/v3 在老机器上是 SIGILL，也就是探针掉线。代价要认：packed 二进制可能被 VPS 的安全扫描当成可疑文件；启动时多一份约 7MB 的瞬时内存（upx 在内存里解包）；开发机没装 upx 时产物不压缩，与发布镜像形态不同（版本号也随之不同，探针会重下一次）。**副作用**：packed 之后 `go version -m` 直接报 `not a Go executable`、`strings | grep` 也不再命中 buildinfo——验证 agent 版本只能真跑（无参启动会打印 `fobe-agent <version>`），这反而堵死了 AGENTS.md 里那个"`strings` 假阳性"的坑。
- **server 为速度编译**（实现修订 2026-09-16）：编译期真正的杠杆只有两条。①**PGO**：`cmd/server/default.pgo` 提交进仓库，`-pgo=auto`（go 的默认行为，三处构建入口都显式写出来，好让"这份剖面真的被用上"在脚本里看得见）自动把它吃进编译；`scripts/pgo.sh` 默认用测试套件采一份（不需要起服务），`--url` 从跑着的面板上采真实热点，热点漂移后重采再提交。收益是几个百分点（工作量相关，不是常数），**代价**是这份剖面只对采它那版代码最有效，代码大改后收益衰减（Go 官方口径是不具代表性的剖面也很少造成回退）。②`FOBE_SERVER_GOAMD64`（Docker 侧 `--build-arg SERVER_GOAMD64`，默认 v1）：v2/v3 用新指令集换几个百分点，代价是老 CPU 上直接非法指令。server **故意不 strip**：保留 DWARF，pprof 才出得了符号，而 `-s -w` 对运行速度没有任何影响（只影响体积与排障能力）。验证 PGO 真的生效别看编译器输出（`grep -i pgo` 会命中 `prepGoExitFrame` 这类假阳性），看 `go version -m <binary> | grep -- -pgo`——它会打印实际吃进去的剖面路径。
- **`FOBE_PPROF`：只读诊断口**（实现修订 2026-09-16，默认关）：设成回环地址（如 `127.0.0.1:6060`）即暴露标准 `/debug/pprof/*`，用来回答"面板慢在哪"并给 `scripts/pgo.sh --url` 供剖面；**非回环地址直接拒绝并记 error 日志**——heap profile 就是一份内存快照，而这个进程内存里躺着主密钥、AI key 与 bot token，"临时开一分钟"的代价是这些凭据可能落进日志或对象存储。容器里采集**不需要发布任何端口**：`docker compose exec server wget -qO- 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30' > cpu.pprof`；远程主机走 `ssh -L`。
- **发布管线**（实现修订 2026-09-15）：推 `v*` tag 触发 `.github/workflows/release.yml`——先过 `gofmt`/`go vet`/`go test ./...`/前端 `tsc+vite build`，全绿才用 buildx 构建 `linux/amd64` 镜像推送到 `ghcr.io/<owner>/<repo>`，标签 `{version, v<tag>, latest}`；`$VERSION` = tag 去掉 `v` 前缀，同时注入 server 与 agent。镜像默认单平台、关 provenance：探针产物本就只有 linux/amd64（§10），attestation manifest list 会让旧 docker 引擎匿名拉取失败。compose 的 `image:` 指向它，`docker compose up -d` 即用预构建镜像；首次发布是 GHCR 私有包，转公开或 `docker login` 后再 `up`。
- **备份**（实现修订 2026-09-16，默认开启）：启动即拍 + 每 24h 一轮 + 数据库迁移前必拍，`VACUUM INTO` 快照到 `./data/backup/fobe-<时间戳>.db`，保留 3 份；`FOBE_BACKUP_DIR` 换位置、`FOBE_BACKUP_DIR=off` 关闭。另提供面板导出/导入 JSON（不含凭据明文）。损坏恢复流程见 `README.md` 排障表。

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
3. 备份：每日 `VACUUM INTO` 快照保留 3 份 + 面板导出/导入。（实现修订 2026-09-16：由 14 压到 3，库体量随节点数增长，池子刻意压小；启动即拍 + 迁移前必拍见 §6/§17。）
4. agent 自更新：与 sing-box 同样三道闸门 + 版本显式指定。**（实现修订 2026-09-15：此条已被 §5.5 取代——agent 没有 `check`/观察期可用，实际是"下载 + sha256 + 旁路自检 + 原子替换"，且不保留 `.prev`；"跟随服务端"改为双向（含降级），触发是系统行为、不需确认，Kill Switch on 时冻结。）**
5. anytls 端口默认随机高位端口，面板可改。
6. **接入层完全外部化**：fobe 不碰 nginx、不签发也不续期证书；README 提供可直接复制的 nginx 配置（单上游 + WS 升级 + 真实 IP + 超时 + 上传体量）。
7. 证书 pinning 替代 `insecure`：订阅里内嵌证书 PEM。
8. 日志默认**不进** AI 上下文，需手动开关。
9. 节点级 anytls 密码覆盖字段：**数据模型预留**，v1 UI 不暴露。（2026-09-16：全局密码改由服务端自动生成、面板不再有密码输入口；**2026-09-17e：全局密码整个取消**——每个入口创建时现生成一份、跟着该入口的配置走，覆盖字段退化成只读的兜底，见 §10.1 实现修订。）
10. v1 不做 TOTP（按你的选择），但 `users` 表预留 `totp_secret` 字段。
11. 首次启动密码：未设置 `FOBE_ADMIN_PASSWORD` 时生成一次性初始密码并打印到服务端日志，登录后强制修改；忘记密码用 `fobe-server admin reset-password`。**（实现修订 2026-09-14：`FOBE_ADMIN_PASSWORD` 从"仅首次生效"升级为密码准绳——每次启动都同步为该值，变更时吊销全部旧会话；不设置则不动现有密码。）**
12. 前端形态：**React + TS + Vite**；生产镜像内置编译产物，本地开发用 Vite dev server + `go run`（`FOBE_WEB_DIR` 为空时 server 进 API-only 模式）。
13. **主密钥零输入部署**（实现修订 2026-09-15）：镜像入口脚本在 `FOBE_MASTER_KEY` 与 `FOBE_MASTER_KEY_FILE` 都未设置时，自动生成随机 32 字节（base64）写进数据卷 `/data/.master_key`（0600）并复用——`docker compose up -d` 不再要求先填任何环境变量。要自己掌管密钥就显式设置 env；admin CLI 不经过密钥解析，逃生口不受影响。

---

## 20. 已接受的已知风险（签字区）

1. ⚠ **AI 默认放行 → 间接提示注入可静默执行任意 root 命令**（§12.4）。缓解路径已设计，改一个配置项即可收紧。
2. ⚠ **登录面只有密码 + IP 黑名单**，无第二因子。黑名单依赖你自备的 nginx 正确传递 XFF、且 `FOBE_TRUSTED_PROXIES` 与实际拓扑一致；**接 CDN 后忘记同步回源网段 = 要么封不到人，要么把真实用户全封了**。
3. ~~⚠ **共享 anytls 密码**：无法按订阅吊销代理访问，泄漏只能全局轮换~~ **已消除（2026-09-17e）**：口令改为每个入口一份、创建时现生成、跟着该入口的配置走（§10.1 实现修订）。泄漏面按入口切分，换某个节点的密码只影响该节点。遗留的代价是面板不再有"一键全场轮换"的杠杆——那正是被打断全场客户端的对价。
4. ⚠ **指标只存 7 天**：7 天以外的曲线不可得（月曲线依赖永久日表，可信；但"上月某天下午的 CPU"查不到）。
5. ⚠ **不做自动停服**：配额超标不会自动止损，完全依赖告警通道可达。
6. ⚠ **agent 自更新不留 `.prev`**（§5.5）：没有本地回滚。旁路自检把"坏产物"挡在提交之前，但**自检过、`-run` 起不来**（只有 run 路径才用到的内核特性/配置）这种残余情形只能 SSH 上去跑面板给的重装命令。你选了省下 OpenWrt overlay 上的 10MB，代价就在这里。
7. ⚠ **Kill Switch on 期间探针不跟随**："一直跟着服务端走"有例外——冻结是全局止损闸，优先级高于跟随。恢复跟随要你手动关掉它。
8. ⚠ **存量探针必须人工重装一次**：今天已装的 agent 二进制里没有自更新代码，服务端下发 target 它也不认识（Go 忽略未知字段，照常跑）。面板按能力位把它标成"需人工重装"，不会自动跟上。
9. ⚠ **服务端版本号成了对外契约**：随便打一个版本号（含把 `VERSION` 改成别的时间戳）就等于让**全部探针换一次二进制**，而降级路径是自动化测试里最容易缺的那条。发版前想清楚这个数字。
10. ⚠ **混版窗口**：升级/降级过渡期一定是混版。承诺是"同大版本内双向兼容（新增字段可选、未知帧忽略）"，**不保证行为等价**。
11. ⚠ **DL 目录是产物单点**：`data/` 没挂载成持久卷（被重建容器清空）或换了机器，全部探针会停在原地并告警——fail-closed 不会把探针搞砖，但也绝不会跟上，直到你把产物补齐。
13. ⚠ **面板整文件重写 `/etc/nftables.conf`（§21）**：端口转发的持久化沿用 nfpf.sh 的做法——`nft list ruleset` 的实况快照整个写进该文件，并按需 `systemctl enable nftables`、写 `net.ipv4.ip_forward=1`。手工维护这个文件的人要注意：下一次在面板里增删改转发规则时，文件会被实况覆盖（注释头写明来源）。不想被覆盖就别让面板管转发，或把持久化交给别的文件（面板不读该文件，只写）。
12. ⚠ **主密钥与密文同卷**（实现修订 2026-09-15，§4.4/§19.13）：零输入部署把自动生成的主密钥放在 `/data/.master_key`，与它加密的设置同一个挂载卷——能读卷的人（宿主机 root、备份文件拿到手的人）就能解密 AI key / Bot Token。换来的是不填任何变量即可启动。不接受这个代价：显式设置 `FOBE_MASTER_KEY`（env / secret），入口脚本就完全不碰磁盘。

---

## 21. nftables 端口转发（2026-09-16 新增）

**需求**：在「编辑服务器」页管理探针所在服务器的 nftables 端口转发，**与 [fonlan/nfpf](https://github.com/fonlan/nfpf) 的 `nfpf.sh` 兼容**，并且要能识别、编辑、删除**脚本或手工添加的既有规则**。

### 21.1 为什么是命令，而不是期望状态

全项目的主线是声明式期望状态（§7），这里刻意反过来：**探针的 ruleset 是唯一事实来源，面板只做一次性的定向事务**。

- ruleset 是**共享**的：nfpf.sh、手工 `nft`、别的面板都可能往里加规则。若把面板维护的清单当成"完整期望状态"去收敛，agent 就必须删掉自己不认识的规则——那正是"识别 nfpf.sh 规则"的反面。
- 因此：读 = 解析实况（`state` 帧随带上报，或面板主动要求一次 `list`）；写 = 一条 `nft_forwards` 命令（add / update / delete），agent 在一个 `nft -f -` 事务里改完，立即把新规则集回给面板与 `state` 通道。
- 命令仍是 §7 的离线队列：探针离线时入队（TTL 10 分钟），上线后执行；面板在此期间显示"已入队"，并每 5s 重读快照等它回答。旧 agent 不认识这个 kind，回 `unsupported command kind: nft_forwards`，面板据此提示"agent 版本过旧"（`forward_agent_unsupported`）。

> 顺带修掉一个老问题：`Hub.NotifyCommand` 原本往一个**没人读**的 `Conn.notify` 通道里塞信号（命令只能等 `PumpCommands` 的 2s 轮询），现在改成 hub 级 `wake` 通道，`PumpCommands` 的 `select` 立即被唤醒——面板动作（含本节的转发编辑）从"最多 2s 才送达"变成即时。

### 21.2 与 nfpf.sh 的兼容点（逐条对齐）

| 项 | nfpf.sh | 本节实现 |
|---|---|---|
| 表/链 | `table ip nat` + `prerouting`(hook prerouting, prio -100) + `postrouting`(hook postrouting, prio 100) | 完全相同；缺失时按同样参数自动创建（首次添加即初始化，等价于脚本的 `init_nftables`） |
| DNAT 规则 | `[iifname "X" ]<tcp\|udp> dport <src> dnat to <ip>:<port>` | 完全相同（同一事务里追加） |
| 回程规则 | `ip daddr <ip> <proto> dport <port> masquerade`，**每条转发各一条** | 完全相同：每条转发配一条；同目标被多条转发共用时**按条数**删除，不会删掉幸存转发还需要的那条（脚本的文本删除会一次删光同目标的所有 masquerade） |
| 持久化 | `nft list ruleset > /etc/nftables.conf` + `systemctl enable nftables` | 相同文件、相同内容形状（前面加两行注释头，仍是合法 `nft -f` 输入）；服务启用改为按需、幂等 |
| 内核转发 | `enable_ip_forward`：`/etc/sysctl.conf` 追加 `net.ipv4.ip_forward=1` + `sysctl -p` | 功能等价：直接写 `/proc/sys/net/ipv4/ip_forward` 立即生效，并在 `/etc/sysctl.conf` **没有生效行时**才追加（脚本会重复追加） |
| 注释 | `dnat to` 之后跟 `comment "…"`，经 `nft -f` 生效 | 完全相同的位置与写法；双引号无法表示（nft 字符串无转义）故拒绝 |
| 冲突语义 | 同 proto+src_port 冲突，除非两条规则各自指定了**不同**的 `iifname` | 完全相同（面板与脚本必须对"这个端口被占了"有一致答案） |
| 删除方式 | `nft flush ruleset` + 文本重写后整体重载 | **改按 handle 精确删除**：`flush ruleset` 会瞬时丢掉全部转发，还会抹掉别的工具（如 fw4/docker）的运行时状态——这是我们与脚本唯一的实质分歧，方向是更安全 |
| 识别既有规则 | `grep "dnat to"` + 正则 | 优先 `nft -j`（JSON，精确到表达式）；老 nft 无 JSON 时回退文本正则（只认上面那种行形状） |

### 21.3 注释（comment）：可写，但只有 `nft -f` 这条路

先说踩过的坑：**命令行形式 `nft add rule ... comment "x"` 会报语法错误**，因为引号是**调用它的 shell** 吃掉的，nft 收到的是 `comment x`（两个裸词）。nfpf.sh 把规则写进临时文件再 `nft -f`，引号原样到达 nft，注释完全合法——所以脚本的注释功能是有效的。（第一次验证时正是踩了这个坑，误判成"dnat 是终结语句、注释写不进去"，实测 nft 0.9.3 / 1.0.6 / 1.0.9 / 1.1.6 上 `... dnat to 1.2.3.4:80 comment "web"` 经 `nft -f` 一律成功。）

实现按这个事实来：

- 注释写在 DNAT 规则上，位置与 nfpf.sh 一致（`dnat to` 之后）；panel 的脚本只经 `nft -f -`（stdin）送进 nft，永不经 shell。
- **`"` 无法表示**：nft 的字符串字面量没有转义写法（`\` 也只是普通字符，原样存储、原样打印，实测确认），所以注释里出现双引号一律拒绝（`bad_comment` / `err_bad_comment`），而不是"帮用户转义"。
- 换行/制表/控制字符同样拒绝（会把脚本行拆断）；长度上限 128 字符（对齐 nfpf.sh 的 `validate_comment`）。
- 注释是 **rule 对象的字段**（`{"rule": {..., "comment": "x", "expr": [...]}}`），不是 expr 里的一项；解析以它为准，文本回退路径也认 `comment "…"` 后缀。

### 21.4 只读规则（`extra_match`）

带面板不建模的匹配条件（`ip saddr`、`counter`、端口范围/集合、`meta l4proto` 等）的 DNAT 规则**照常列出**（这是"识别既有规则"的一部分），但标 `extra_match`：面板拒绝编辑（改写会静默丢掉那些条件），允许删除（按 handle 精确删，不会误伤）。IPv6（`table ip6 nat`）、端口范围、多端口集合都不在 v1 范围——nfpf.sh 同样只做 IPv4 单端口。

### 21.5 数据模型与接口

- 快照表 `node_forwards`（每次上报整体替换；规则顺序保留）+ 状态表 `node_forward_status`（`supported/initialized/code/message/reported_at`）。`SchemaVersion` 8 → 9（增量，降级安全：老二进制按列名查，两张新表它根本不看）。
- 表只是**缓存**：探针的 ruleset 才是事实。所以删除只发生在本面板，且被删除的规则若仍在探针上，下一次上报会把它带回来——这是有意的（"面板不是权威，探针才是"）。
- 接口（全部 `requireSession`，错误码 snake_case，文案在 `i18n`）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/nodes/{id}/forwards` | 读快照；`?live=1` 则向探针要一次实况（离线/超时返回 `queued:true` 与已有快照，不报错） |
| POST | `/api/nodes/{id}/forwards` | `{rule:{proto,src_port,iface,dst_ip,dst_port}}` 新增 |
| PUT | `/api/nodes/{id}/forwards` | `{old,rule}` 替换（agent 侧一个事务完成 delete+add，失败则老规则原样保留） |
| DELETE | `/api/nodes/{id}/forwards` | `{rule}` 删除（含配对 masquerade） |

- 错误码映射：agent 的 `conflict/not_found/ambiguous/bad_*/need_root/nft_missing/chain_mismatch/nft_failed` → `forward_conflict`(409) / `forward_not_found`(404) / `forward_ambiguous`(409) / `bad_forward`(400) / `forward_need_root` / `forward_nft_missing` / `forward_chain_mismatch`(400) / `forward_failed`(502)。服务端只做廉价的形状校验（协议、端口范围、IPv4、接口名长度），**权威校验在 agent**——它才看得见探针上的实际规则。
- 每次执行都落审计：入队时 `cmd:nft_forwards`（含 payload），成功后另记 `forward_add/update/delete` + 人类可读的 `tcp/8080@eth0 -> 10.0.0.1:80`。
- 面板一条规则的身份是 `(proto, src_port, iface, dst_ip, dst_port)`，并**优先用 handle 定位**：ruleset 重载会重编号 handle，所以 handle 必须与其余字段同时匹配才生效（否则用元组兜底；元组命中了多条则拒绝猜测，回 `forward_ambiguous`）。

### 21.6 写后校验（2026-09-16 修订：不许静默丢字段）

"保存成功但备注是空的"这个症状查过一次，根因是**链路里有旧二进制**：面板后端或探针 agent 若来自加入备注字段之前的构建，Go 的 JSON 解码会**静默忽略**未知字段——规则照样写进去，备注凭空消失，任何地方都不报错（`decodeJSON` 不设 `DisallowUnknownFields`，这正是兼容旧前端的代价）。

既然"静默"本身是缺陷，agent 现在**校验自己的写入结果**：新增/编辑成功后，拿实况规则集与请求逐项比对，对不上就回报 warning（规则已生效，所以不是错误）：

| warning | 含义 | 面板 |
|---|---|---|
| `comment_not_applied` | 规则在、备注不在——探针 agent（或后端）太旧，不认识备注字段 | 红字提示"让探针跟随服务端更新或重装 agent 后重试" |
| `write_not_applied` | 实况里找不到这条规则 | 红字提示"点从探针刷新核对" |

未知 warning 按 `fw_warn: <原文>` 原样显示，不丢信息。

### 21.7 验证

- 单测：`internal/agent/forwards_test.go`（用 nft 1.0.6 在容器里跑出来的真实 JSON/文本输出做 fixture，覆盖解析、冲突、事务脚本、masquerade 计数、`ip_forward`）。
- 端到端（真 kernel，需 root + 可抛弃环境）：

```bash
docker run --rm --privileged -v "$PWD":/src -w /src golang:1.25 sh -c \
  'apt-get update -qq && apt-get install -y -qq nftables >/dev/null && \
   FOBE_NFT_TEST=1 go test ./internal/agent -run TestForwardsNftKernel -v'
```

  它先按 nfpf.sh 的写法建表建链建规则，再验证面板能识别、能改目标、能删除其中一条而不动另一条要用的 masquerade，最后确认写出的 `/etc/nftables.conf` 能被 `nft -c -f` 接受。
- 面板链路：`internal/server/httpapi/forwards_test.go`（模拟 agent 的 WS 会话，覆盖 live/queued/旧 agent/错误码映射/离队入队）。

### 21.8 已知边界

- 只有 IPv4、单端口、tcp/udp（nfpf.sh 同）；IPv6 与范围转发留给后续。
- 探针 agent 必须先更新到带 §21 的版本，否则面板只能显示"版本过旧"（§5.5 自更新或重装）。
- 面板新增的规则**没有来源标记**：nftables 里没有可靠的"这是我加的"字段（注释是用户可见的备注，不该被面板征用），所以列表不区分来源。这是有意的——所有 DNAT 规则都可读可删，来源标记只会给出会过期的假信息。
- 未提供 AI 工具：端口转发会直接改变对外暴露面，v1 只走人工确认的面板路径（要开放给 AI 需按 §12.3 加确认与元操作门）。

### 21.9 与订阅中转的交叉点（2026-09-16c 新增）

- 一条转发的 `dst` 正好命中另一台探针的 anytls 入站端口时，它就是订阅里「B 经 A」入口的来源（推导与落账见 §10.2）。**转发侧不新增任何字段**：中转关系是推导出来的，不是声明出来的——否则同一件事会有两个事实来源。
- **遮蔽（shadowing）**：跳板 A 上某条转发的 `src_port` 若等于 **A 自己的** anytls 入站端口，发往 `A:<该端口>` 的包在 prerouting 就被 DNAT 走了，「A 直连」入口实际已不可用。§21 的冲突检测只看转发规则之间、不看与本机入站端口的关系，**这里刻意不在转发侧拒绝**（端口占用是探针的事实，面板不替它做决定），改由订阅侧把该入口标成 `shadowed` 告警。
- **转发变化会改变订阅输出**（自动纳入的中转入口，§10.2）：要停掉某个入口就取消勾选（写墓碑），不是删规则——两者语义不同，删规则是动探针，取消勾选只是这份订阅不发它。
- 流量口径不受影响：转发流量穿过 A 的网卡，A 的周期用量会跟着涨（配额按节点计）——面板在入口列表里写明「经 <A>」，让这个账对得上（不额外在节点页做跳板徽标：那会多一份会过期的派生状态）。
