<p align="center">
  <h1>Traework2API Panel</h1>
</p>

<p align="center">
  <b>把 Trae 免费对话通道包成 OpenAI / Anthropic 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  三协议入站 · 多账号轮转 · 自动签到 · Token 保活 · 会话粘性 · 单二进制 · 零第三方依赖
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26.10-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI%20%7C%20Anthropic-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

---

> **本仓的源头 [Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api) 已下线，原仓库不再可访问**；本仓为其延续分支，接手后独立演进。

> [!IMPORTANT]
> **本仓没有可跟随的上游基线仓**：源头仓库已下线，Trae 协议事实**取自公开实现，并结合作者对本人账号的抓包分析印证**，**无官方支持与后续保障**。
>
> **维护方式披露**：本仓的 issue / PR 处置、代码审查、测试与修复实现由 **Ekko Studio + Hermes Agent**（AI 编码 agent）辅助完成，合并与发布决策由维护者本人作出。作者是学生，因家庭经济困难，周末需兼职，业余还有摄影、公交等自己的兴趣——**本项目兴趣驱动，更新随缘，不代表作者有全部精力投入维护**，也可能出现较长时间没有提交，请勿当作有 SLA 的商业项目。
>
> 风险自评：**上游随时可能收紧导致通道不可用**；因使用非官方客户端导致的账号风控 / 封禁，由使用者自行承担。

> [!WARNING]
> **郑重提醒**——这段话针对两类人：**现实中与作者同校的使用者**，以及**拿本项目搭建对外中转站、做批量小号 / 售卖额度等恶意滥用的人**。
>
> 作者无法追踪、也不会去追踪任何人的行为，不对任何人行使监控。但必须把话说清楚：上述用法**严重违背上游项目作者与维护者的初衷**，也是消耗本仓作者维护动力的最大来源——本项目免费、开源、由一名学生用课余时间维护，不是给任何人拿去转卖的资源。
>
> 如果哪一天这个项目**确确实实让作者心力憔悴、不再有动力维护，本仓也会停止维护**，且不另行通知。上游的今天，可能就是本仓的明天，请自重。
>
> 同样的底线也适用于**同校使用者**：若有人出于一己私利，或以作者无法理解、也不知情的行为，把本项目的情况反映到**学校信息中心**、作者所在的**二级学院**，乃至**学院院长与校方其他上级领导**，藉此换取奖学金等个人好处，**本仓也会同时停止维护**。这不是赌气——接受核查、说明与后续处理所占用的时间与心力，已经超出本仓能承受的维护成本。

## 项目简介

Trae 的 OpenAI 兼容反向代理。把 Trae 免费对话通道
（`llm_utils_chat` + `function=solo_work_lite`）包装成统一服务，支持多账号轮转、自动签到、
token 自动刷新。入站三种协议：`chat_completions`（`POST /v1/chat/completions`）、
`codex_responses`（`POST /v1/responses`）、`anthropic_messages`（`POST /v1/messages`，
同样接受 `POST /messages`）。

纯 Go 标准库，零第三方依赖。

## 核心能力

- **三种入站协议**：`POST /v1/chat/completions`（OpenAI chat）、`POST /v1/responses`
  （Codex；`wire_api="chat"` 已在 Codex 0.84+ 移除，只认 Responses）、`POST /v1/messages`
  与 `POST /messages`（Anthropic Messages，含 `x-api-key` 鉴权）；都是先转成 chat 请求再走
  **同一条链路**（切号、冷却、会话粘性、提示词改写、出站改写），上游仍是 Trae chat SSE
- **`GET /v1/models`**：模型列表
- **多账号池**：积分加权挑选（闲置补偿 + 快过期优先），在途限额，熔断/降权/计划冷却，
  401 连续 3 次才禁用（一次抖动不杀号），请求级轮转
- **流内错误分类**（HTTP 级与 `event:error` 共用一张表）：1005 权益不足 → 12h 计划冷却；
  4008/限流文案 → 软冷却退避；401/429/404/5xx 各归各类；**4001「model config is empty」
  一类模型/参数问题不罚号**——它不是账号的问题，记账只会把好号推向熔断
- **自动签到**：多时点定时签到 + 周期余额刷新 + 手动批量签到（`signin.sh`）
- **积分查询**：全账号/指定账号日报（`credit.sh`）
- **Token 保活**：多时点**无条件**全账号刷新 token（不看剩余有效期），refreshToken 轮换落盘
- **会话粘性**：同一会话的多轮请求粘同一账号（命中上游 prompt 缓存）
- **调用用量台账**：按「时间片 × 账号 × 模型」记请求数/失败数/token/延迟，面板可查
- **系统提示词改写**：passthrough / custom / append 三态，出站前替换或追加
- **登录闭环**：`login.sh` 自生成登录链接 → 浏览器登录 → 粘贴回调链接 → 换 token 落盘（面板「添加账号」同一流程，Trae 只认它自己的本机回调，见下）

## 快速开始

### Docker

```bash
# 1. 准备凭证目录（放 trae-*.json）
mkdir -p auths data

# 2. 配置（Bearer 鉴权用的 api_key 在 config.json；面板「配置」页可看可改）
cp config.example.json config.json
# 编辑 config.json，给 api_key 填一个自己的随机密钥（留空 = 不鉴权，仅本机可访问）

# 3. 启动
docker compose up -d --build

# 4. 验证
curl http://127.0.0.1:7864/healthz          # → ok
curl http://127.0.0.1:7864/v1/models         # → 模型列表
curl http://127.0.0.1:7864/status            # → 账号状态

# 5. 对话
curl -X POST http://127.0.0.1:7864/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $KEY" \            # KEY=$(jq -r .api_key config.json)，或在面板「配置」页复制
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}]}'

# Codex Responses（流式，Codex CLI 走这条）
curl -N http://127.0.0.1:7864/v1/responses \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","input":"你好","stream":true}'

# Anthropic Messages（官方 SDK 用 x-api-key）
curl http://127.0.0.1:7864/v1/messages \
  -H "x-api-key: $KEY" -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"你好"}]}'
```

## 登录流程

Trae 登录页强制回调 `127.0.0.1`，但浏览器与服务器不需要同机：
`login.sh` 自生成登录链接，登录成功后你把地址栏回调链接粘回去即可。
（同一件事在面板里也能做：点「登录账号」→ 粘贴回调 → 换票落盘，见「管理面板」。）

```bash
# 1. 在服务器（或任意机器）运行 login.sh
./login.sh
#    → 打印登录链接（带 127.0.0.1 回调 + 新的 machine/device id）

# 2. 用浏览器打开链接登录（手机号/验证码）
#    登录成功后浏览器跳到打不开的 127.0.0.1 地址

# 3. 复制浏览器地址栏的完整回调链接，粘贴到 login.sh
#    → 解析 refreshToken/userInfo → ExchangeToken 换 token → GetUserInfo 拿 uid
#    → 落盘 auths/trae-{uid}.json → 自动签到 + 查积分 → 重启容器加载新账号
```

**`ClientID` 与账号强绑定，别改**（2026-10-15 实测）：`refresh token` 是**按 `ClientID` 铸**的，
换票时两者必须同档，跨档一律 `10101 无效参数`（2×2 全交叉矩阵在两个 host 上、正序倒序各跑一遍，
结果一致；`BoundDeviceID` 也分档）。本仓所有账号都是 `en1oxy7wnw8j9n`（对话档）铸的，
所以 `upstream.ClientID` / `login.sh` 的 `CLIENT_ID` / 登录页 `client_id` **三处必须保持一致**，
任意一处漂到 IDE 档的 `ono9krqynydwx5` 都会让**全部已有账号**换票失败。
该档位本身健在：`GetPCAuthCode` 用对话档与 IDE 档都返回 200，只有伪造的 client_id 才
`400 Invalid client.`（对照有区分力）。

### 本机运行（非 Docker）

```bash
# 依赖 Go 1.22+
go build -o tw2api ./cmd/server
./tw2api          # 监听 :7864，auths/ 目录读取凭证；密钥在 config.json 的 api_key
```

`config.json`（参考 `config.example.json`）**首次运行自动生成**，字段分组与
`workbuddy2api-panel` 对齐（同段同键，两个面板可以对着读）：

| 段 | 作用 |
|---|---|
| `listen` / `api_key` / `auth_dir` / `state_file` | 服务与凭证位置 |
| `default_model` | 请求缺 `model` 时的默认模型（Trae 扩展，wb 无此键） |
| `cooldown` | `plan_credit`（1005 硬冷却，Trae 扩展）、`soft_rate` + `soft_rate_max`（限流指数退避封顶） |
| `schedule` | `checkin_hours` / `keepalive_hours` 时点数组 + `*_enabled` 开关，`balance_refresh_*` 周期刷余额 |
| `upstream` | 超时三元组（短请求 / 首字节 / 流内空闲）+ `user_agent` / `client_version` 覆盖 |
| `pool` | 在途限额、熔断、降权、闲置补偿、快过期窗口 |
| `session_sticky` | 会话粘性开关 / TTL / GC 周期 |
| `prompt` | 系统提示词改写模式与文件 |

全部项可用 env 覆盖：`TW2A_LISTEN` / `TW2A_AUTH_DIR` /
`TW2A_STATE_FILE` / `TW2A_DEFAULT_MODEL` / `TW2A_PLAN_CREDIT` / `TW2A_SOFT_RATE` /
`TW2A_SOFT_RATE_MAX` / `TW2A_CHECKIN_HOURS` / `TW2A_KEEPALIVE_HOURS` /
`TW2A_TIMEOUT_SECONDS` / `TW2A_HEADER_TIMEOUT_SECONDS` / `TW2A_IDLE_TIMEOUT_SECONDS` /
`TW2A_USER_AGENT` / `TW2A_CLIENT_VERSION` / `TW2A_MAX_IN_FLIGHT` / `TW2A_EXPIRING_SOON` /
`TW2A_PROMPT_MODE` / `TW2A_PROMPT_FILE`。非空才覆盖。**`api_key` 不在 env 覆盖之列**：
它只来自 `config.json`，单一来源——面板「配置」页显示的就是文件里的原值，改完当场生效
（不像其它 env 那样存在"界面显示 A、实际用 B"的错位）。首次运行生成的 `config.json`
里 `api_key` 是随机 `sk-…`（默认监听 `0.0.0.0`，空 key 等于裸暴露），启动日志会打印一次。
**已存在的配置不会被改写**（`O_EXCL`），老配置里空着就继续空着，改文件或在面板里填都行。

**与 workbuddy2api 的配置面差异**（有差异的地方都是"Trae 上游没有这个能力"）：
`schedule.travel/activity/blackcat_*`（活动任务）、`global`（国际域路由）、`upstash`
（共享状态）、`features.sanitize_blacklist_fingerprints`、`pool.max_in_flight_global`、
`pool.cost_explore_interval`（成本分层探索）、`upstream.device_token*/cli_version` 未提供——
写成空壳字段只会让人以为改了有效果。

## 发版

版本号是上海时间，形如 `v2026.9.28.1530`（`2026.9.28` 日期，月日不补零；`1530` 是 15:30，
时不补零、分两位、不带秒）。发版就是打标签并推送，工作流看到 `v*` 才建 Release：

```bash
VERSION=$(TZ=Asia/Shanghai date +%Y.%-m.%-d.%-H%M)
git tag "v$VERSION" && git push origin "v$VERSION"
```

- **`go-binaries.yml`**：push main / PR → `go test ./...` + linux(amd64/arm64)、windows(amd64)
  交叉编译，产物只进 artifact；打 `v*` tag → 同一个版本号注入 `-X main.appVersion=`（面板
  「概览」显示它），打包成 `traework2api-panel-v<版本>-<系统>-<架构>.zip|tar.gz`（含
  `config.example.json` 与 README）挂到 Release，另附 `checksums.txt`。
- **`docker-ghcr.yml`**：多架构（amd64/arm64）镜像推 `ghcr.io/misakano7545/traework2api-panel`，
  main 推 `latest` / `main` / `sha-xxxxxx`，PR 只构建不推送。
- 源码里 `appVersion` 默认 `dev`（本地 `go build` 与 Docker 镜像里都是 dev），只有 CI 发版会
  注入真实版本号；不要手改它。

## 运维

```bash
./signin.sh             # 批量签到（全账号，自动 refresh 过期 token）
./credit.sh             # 积分日报（美化）
./credit.sh -json       # 积分日报（JSON）
./credit.sh <uid>       # 指定账号
```

**积分口径**：池与面板的「剩余/总额」都来自 `ide_user_ent_usage`（`user_entitlement_pack_list[]` 里
`quota.credits_limit>0` 的包求和，`usage.credits_amount` 为已用）。这条通路上**没有**独立的 Trae 额度字段：
2026-10-11 抓整条对话流实测，事件只有 `done / extra_info / metadata / output / timing_cost / token_usage`，
没有第三方笔记记的 `notify_usage`，整条流里 credit/billing/quota/remain 字段 0 条 —— 复跑
`TW2A_PROBE_CHAT=1 go test ./internal/upstream -run TestProbeLiveBilling -v`。
该笔记另称 ent usage 聚合的包**含 work 包**（即「显示 remain=2000 其实是 work_credits」）：本仓**未复核**
（接口只给汇总，`./credit.sh` 拿不到包级数值），所以别据此改选号口径。

### agent 域名是上游下发的（换域名时先看这里）

**上游会换 agent 域名，而且面板看不出征兆**：官方客户端从不写死具体后端，它请求一个**逻辑主机名**
（国服 `trae-api-cn.mchost.guru`，国际 `coresg-normal.trae.ai`），由 TTNet 的 dispatcher 按 TNC 下发的
域名表在**传输层**改写成 `api<N>-normal[-<region>].mchost.guru` 再发出去。所以同一个客户端，`referer`
里写着旧域名，实际打的是新域名 —— 2026-10-15 国服那轮改动就是这么发生的（`trae-api-cn` →
`api5-normal`，客户端 `referer` 至今还是旧名）。

本仓是纯 Go HTTP、不走 TTNet，默认**自己把那表拉回来解析**（`internal/upstream/domains.go`）：

1. `GET https://tnc0-aliec2.zijieapi.com/get_domains/v5/`（国服）/ `https://tnc-boot.trae.ai/...`（国际）
   —— 无需鉴权。注意 `cronet_version` / `ttnet_version` 两个参数**不能省**，省了返回 0 条规则。
2. 从 `data.ttnet_dispatch_actions[].param.host_group[]` 里挑出候选：国服 = 无区域段
   （`api5-normal.mchost.guru`），国际 = 带区域段（`api22-normal-alisg.mchost.guru`）。
   同表的 `trae-api-cn.mchost.guru` 之类**不是**分片后端，会被排除。
3. 逐个探活（POST `/api/ide/v1/get_detail_param` 发最小报文，**拿到任何 HTTP 响应即算活** —— 401 也
   证明路由存在），取第一个可用的；全部失败就沿用现有值，最终回落 `constants.go` 的常量。
   发现机制自己坏掉**不影响出流**。

配置与开关：

| 键 | 默认 | 说明 |
|---|---|---|
| `upstream.host_discovery` | `true` | 关掉 = 只用 `agent_host` / 内置常量 |
| `upstream.host_discovery_hours` | `6` | 刷新周期。改了**需重启**（ticker 启动时定） |
| `upstream.agent_host` | `""` | 显式钉死国服基址，非空则优先于自动发现（热生效） |
| `upstream.agent_host_intl` | `""` | 同上，国际版 |

排查时先看两个地方：

```bash
# 1) 面板首屏 JSON（/panel/api/overview）——agent_host_source 是 auto/内置，看它是不是「自动发现」
curl -s http://127.0.0.1:7864/panel/api/overview | python3 -c 'import json,sys;d=json.load(sys.stdin);print({k:v for k,v in d.items() if "agent_host" in k})'

# 2) 启动日志：每次发现成功/失败/换域名都有一行 [domains]
grep '\[domains\]' <面板日志>
```

`agent_host_source` 长期是「内置常量/显式配置」而不是「自动发现」，或者日志里反复出现
「域名表获取失败」，说明 TNC 接口或 boot 主机也变了 —— 那时候要改的是 `domains.go` 里的
`tncBootHosts` / `domainQuery`，而不是 `constants.go` 的常量。

已实测（2026-10-15，两区真 token）：两条域名下的 agent 基址**同时可用**，模型表逐字一致，
所以自动发现是「跟着上游走」而不是「不换就坏」；换域名那次也是旧域名仍返回 401 而非 404
（属于遗留入口，给运维留了观察窗口）。

## Max 上下文（1M 档）

客户端模型选择器里每个模型都有一个「Max 上下文」开关（关着时窗口 116k/200k/256k，开着 1M）。
面板把这条通道做出来了，**默认关闭**——与客户端一致（客户端开着时也会提示「更多积分消耗」）。

### 怎么开

两种写法等价，任选：

```bash
# 1) 请求体加一个字段（OpenAI 客户端忽略未知字段，不会报错）
curl -s http://127.0.0.1:7864/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "cn:glm-5.2", "max_context": true,
  "messages": [{"role":"user","content":"hi"}]}'

# 2) 模型名带上游内部后缀（客户端原生写法）
curl ... -d '{"model":"cn:glm-5.2__max","messages":[...]}'
```

`/v1/models` 里每个模型会如实带上可用性，客户端可据此决定要不要开：

| 字段 | 含义 |
|---|---|
| `context_length` | 默认档窗口（`__dev` 实际请求口径） |
| `context_length_max` | 上游**声明**的 Max 口径 |
| `max_available` | `true` = 这个模型真能开 Max（有 `__max` 变体**且**窗口确实变大） |
| `max_model` | 出站要用的变体名（形如 `glm-5.2__max`） |

当前国服 32 个模型里 **20 个** `max_available=true`。`Doubao-Seed-2.1-Turbo` 这类上游给了
`__max` 名字但窗口没变大的，**报 false** —— 开了也不增长，报 true 就是骗客户端。

### 为什么要换 `function`

Max 变体只在部分上游面上**声明**了更大的窗口（2026-10-15 全表实测 782 条明细）：

| function 面 | 带 `__max` 条目 | 其中窗口更大 | 默认档 `dev` |
|---|---|---|---|
| `solo_work_lite`（面板默认面） | 12 | **0** | 200000 |
| `solo_coder` | 36 | **0** | 200000 |
| **`chat_v3`**（Max 面，默认） | 20 | **20** | 116000 |
| `solo_agent` / `_lite` / `_remote` | 54/39/31 | 44/36/36 | 200000 |

所以开 Max 时出站 `function` 会换到 `chat_v3`（`upstream.max_function` 可改，热生效）。
换面不影响可调用性 —— 本仓 2026-10-04 已实测 function 不是约束（4 模型 × 4 轴 16/16 全 200）。

想要「200k 默认档 + 1M Max」的话把 `max_function` 设成 `solo_agent` 或 `solo_agent_lite`。

### 上游没给 Max 时

如实忽略并留一行 `WARN: [max] <模型> 没有更大的 Max 窗口…`，**不换 model、不换 function、不报错**，
请求照常成功。宁可少一个开关，不硬发一个上游认不出的名字。

### 一个坑：变体名必须从上游表里读，不能拼

`model = config_name + "__max"` 是错的。全表 417 条带后缀的变体名里 **99 条不满足**这个规律：

```
search_agent_qwen_fast → search_agent_model__max   （多个 config 共用一个变体名）
glm-5.2_advisor_doubao → glm-5.2__dev             （advisor 槽位复用基础模型的变体名）
custom_model_kimi      → custom_model_placeholder__dev
```

拼出来的名字上游认不出，失败形态是「静默按 dev 行为跑」—— 客户端以为开了 1M，实际还是 116k。
代码只存**读到的原值**（`upstream.variantNames`），取不到就是空串，绝不猜。
回归测试用的是真抓包夹具 `internal/upstream/testdata/batch_max_variants.json`。

### 已实测 / 未实测（如实分列）

已实测（2026-10-15，真 token）：

- 出站报文：`config_name` 留裸名、`model` 填变体名、`function` 换 `chat_v3`，日志有 `[max]` 行。
- 面 × 变体 6 格矩阵（`solo_work_lite`/`chat_v3` × `__dev`/`__max`/裸名）**全部 200 且正文正确**。
- 面板端到端 13 次调用（默认档 6 次 / Max 档 7 次，含带与不带 `max_tokens`），除下述两次异常外全部正常。

**未实测：1M 窗口本身没有被验证。** 上游表里 `context_window_tokens.max = 1000000` 是**声明**，
客户端侧看不到实际生效窗口。要证实得发一段超过默认档（116000 token）的输入，约需 100 积分 ——
没做，别把声明当实测。

**观察到的异常（未复现，留档）**：首轮面板 E2E 里两次 Max 调用出现一次英文胡话、一次正文为空
且烧掉 16000 个推理 token（正好是 Max 档输出上限）。随后按同样条件重跑 3 次 dev + 3 次 max 全部正常，
2×2 上游矩阵也全正常，**复现不出来**。原因未明，可能是上游瞬时状态或满预算的思考跑飞，继续观察。

## 保活（防掉线）

`schedule.keepalive_hours`（默认 `[3]`）到点对**全账号无条件**刷一遍 token，不看剩余有效期：
access token 活 14 天，只在「过期前 24h」刷等于 refresh token 链十天半个月没人碰，上游一旦让它过期
就只能重新登录（掉线）。每天摸一次，链不会冷。刷新 token 不吃积分，只换凭证并原子落盘 `auths/`。
面板「全部保活」= 同一件事的手动入口（`POST /panel/api/keepalive`）。

session 失效（上游 `1001` / `20101`，refresh token 也救不回来）**连续 3 次**才永久禁用：一次 401 多是
上游抖动，见到就杀号会让可用账号凭空下线。期间任何一次刷新成功都清零（`ClearSessionDead`），
误判的号自己回来；真被判死的号用面板「启用」（`POST /panel/api/accounts/{uid}/enable`）清掉禁用标记、
计数与冷却直接复池，不必再「移除 + 重登」。计数与禁用状态都落 `state_file`。

## 管理面板

浏览器打开 `http://127.0.0.1:7864/panel/`。页面和 `app.js` 不用密钥；`/panel/api/*` 与 `/v1`
共用 `config.json` 里的 `api_key`（「配置」页可见可改，保存后两边同时换新）。没设密钥时这些接口只接受本机连接。

「模型」页按上游 `get_detail_param` 原值列出模型的 `capability`（`model_capability`）与思考
相关字段（`model_extra_config` 里的 `Thinking.Type`、`reasoning_effort_config` 原文），不做解释和换算。

三种入站协议在 handler 层先归一成一份 chat 请求（`responses.go` / `messages.go`），再进上面这条
管线，所以切号/冷却/会话粘性/台账对三条路一样生效。`codex_responses` 不保存
`previous_response_id` / `store`（Codex 每轮自带全量 input）；`anthropic_messages` 不回传
thinking（上游没有 signature）。缓存命中各按客户端口径回：responses 出
`input_tokens_details.cached_tokens`，messages 出 `cache_read_input_tokens`（都取自 Trae
`usage.cache_read_input_tokens`，没有就不写，不拿 0 冒充"没命中"）。
客户端自己带的 `reasoning_effort` 之类字段是**原样透传**上游的，不认的也一个不吞（`PrepareBody` 只改
`stream`/`function`/`config_name`/`model`/`tools`）。

**Codex 工具契约**（`/v1/responses`，2026-10-08 实测）：Codex 的 wire 契约跟 OpenAI chat 不是一回事，
网关侧补齐三件——① `type:"custom"`（freeform，如 `apply_patch`）降级成单 `input` 参数的 function 送出，
回程还原成 `custom_tool_call` + `response.custom_tool_call_input.delta/done`，载荷严格解包（多键/非字符串/
半截 JSON 一律按不可执行下发 `status:"incomplete"`，避免客户端拿脏载荷去改文件）；② `type:"namespace"`
工具组（Codex 0.160 的 `multi_agent_v1` 就是这种）展开成扁平 function 出站，回程补回 `namespace`，组内/
与顶层重名时加稳定后缀（`read_file_2`）并双向映射，客户端按自己声明的名字回传结果能对上；③ 无 `call_id`
的 `function_call_output`（别的任务的回执）与 `agent_message` 注入成用户指令，不伪装成工具结果（空
`tool_call_id` 到上游只会撞配对失败）；带图时整段保留 parts。实测：Codex 0.160 一次 `apply_patch` 建文件
全程走通，`multi_agent_v1.list_threads` 调用回程带 `namespace`。

**usage 口径**：`input_tokens`/`input_tokens_details.cached_tokens` 按各自协议出（Anthropic 口径的
`input_tokens` **不含**缓存读/写，扣掉 `cache_read_`/`cache_creation_input_tokens`；Responses 出
`cached_tokens`）；上游没报就不写，不拿 0 冒充「没命中」。

**工具结果里的图片**（`/v1/responses` 的 `function_call_output`、`/v1/messages` 的 `tool_result`）：
图不能留在 `tool` 消息里（Chat 的 `tool` 角色只接受文本，带 `image_url` 会被 OpenAI 直接 400），
也不能整段拍平成 JSON 文本（base64 按正文分词 ≈ 字符数×0.69，一张 400KB 截图十几万 token，而且模型
**看不到图**）——所以拆成「文本进 tool 消息 + 图片攒到本轮工具结果**全部**落地之后补一条 user 消息」。
不能插在中间：一轮里多条工具结果是连续的 tool 消息，中间插任何消息都会让后面的 `tool_call_id`
失去应答（严格上游判 11148），并行工具调用必然踩中。只有图片时 tool 消息留占位文案指路。

**上游承载不了的工具一律留痕剔除**：`tools[]` 里没有 `function` 键的（Codex 每轮都发
`type:"web_search"`、Anthropic 的 server tool）在上游只承载函数工具，剔除时按名字记一条
`WARN: [upstream] 剔除 N 个…`，并连带删掉点名了它们的 `tool_choice`（名字对不上，上游只当参数非法）。
静默丢会让客户端以为自己能联网搜索，而日志里一个字都没有。

**与参考实现对拍**（`internal/upstream/vectors_check_test.go`，`TW2A_VECTORS=<dir>` 才跑、只报告不判红；答案卷来自
另一套独立实现 `shadyrispy/cpa-multi-plugins` 的 `plugins/trae`，只当对拍基线，不进仓）：chat 路 18 个
伪装头**键集逐键一致**（已固化成 `TestSOLOHeadersKeySet`），出站 body 与聚合口径除我们有意的分歧
（不发 `config_name`、`max_tokens` 上限、未知字段透传、空流报错好换号）外全对。

对拍查出的 4 条 classify 差距已修（`client.go` 的 `Classify` 现在与 `event:error` 共用
`soloSpecificKind` 那张具体判据表，README 早先写的「HTTP 级与流内共用一张表」名副其实）：
`400/4001`、`413`、`400 prompt is too long` 都归**调用方问题不罚号**（与 4026 同一类，见
`context_overflow_test.go`），`403/4008` 归软冷却。探针按**效果**比对（参考的
`model_unavailable`/`input_too_large` 等于本仓的 `ErrNone`），跑完只剩 2 条，都是有意为之：
① 参考实现把 4008 当 `plan_limit`（12h 硬冷却），本仓按软冷却 —— 4008/quota 是可自愈的限流
而不是权益不足，判据钉在 `TestClassify`；② `{"error":"This input is too large"}` 参考归
`client`（降权），本仓归不罚号 —— 调用方发太大，罚号没有意义（他们自己的标记表漏了这句）。
传输层信号仍优先于 body 里的业务码（500 里带 quota 文案按服务端故障算）。

**全模型全档实测**（不发 + 10 档 × 3 轮，取 `reasoning_tokens` 中位数，`max_tokens=2000`）：

| 模型 | 不发 | none | minimal | lowest | low | medium | high | max | xhigh | ultra | highest | 档间极差 | 档内波动 | ρ(档位序) |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| `Doubao-Seed-Evolving` | 128 | 77 | 149 | 105 | 113 | 68 | 128 | 111 | 124 | 119 | 120 | 81 | ±58 | +0.28 |
| `Doubao-Seed-2.1-Pro` | 109 | 117 | 140 | 73 | 108 | 78 | 223 | 173 | 64 | 78 | 70 | 159 | ±132 | -0.40 |
| `Doubao-Seed-2.1-Turbo` | 160 | 137 | 186 | 136 | 118 | 114 | 120 | 181 | 166 | 140 | 180 | 72 | ±63 | +0.22 |
| `step-5-preview` | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | ±0 | — |
| `glm-5.3` | 123 | 139 | 112 | 148 | 136 | 121 | 83 | 124 | 123 | 124 | 123 | 65 | ±72 | -0.26 |
| `glm-5.2` | 356 | 340 | 321 | 340 | 349 | 326 | 337 | 346 | 340 | 325 | 351 | 30 | ±54 | +0.25 |
| `deepseek-v4.1-flash` | 77 | 74 | 58 | 55 | 58 | 68 | 80 | 72 | 64 | 61 | 67 | 25 | ±17 | +0.14 |
| `DeepSeek-V4-Flash-Official` | 67 | 78 | 94 | 78 | 82 | 83 | 78 | 77 | 25 | 84 | 81 | 69 | ±30 | -0.14 |
| `DeepSeek-V4-Pro-Official` | 115 | 112 | 133 | 109 | 100 | 108 | 120 | 117 | 104 | 92 | 109 | 41 | ±22 | -0.40 |
| `kimi-k3` | 80 | 157 | 113 | 120 | 137 | 175 | 158 | 126 | 104 | 167 | 160 | 71 | ±78 | +0.31 |
| `kimi-k2.7-code` | 114 | 116 | 80 | 79 | 107 | 140 | 106 | 84 | 100 | 103 | 86 | 61 | ±53 | -0.09 |
| `kimi-k2.6` | 164 | 197 | 203 | 148 | 156 | 177 | 152 | 141 | 201 | 146 | 160 | 62 | ±50 | -0.36 |
| `minimax-m3` | 82 | 110 | 120 | 103 | 136 | 132 | 54 | 80 | 86 | 83 | 90 | 82 | ±42 | -0.58 |
| `qwen3.8-max` | 109 | 104 | 108 | 133 | 97 | 110 | 100 | 99 | 109 | 110 | 114 | 36 | ±23 | +0.25 |
| `qwen-3.7-plus` | 143 | 142 | 152 | 144 | 143 | 158 | 144 | 143 | 144 | 147 | 138 | 20 | ±20 | -0.12 |

`档间极差` = 各档**中位数**的极差；`档内波动` = 同一档 3 轮的平均极差；`ρ(档位序)` = 中位数与档位序号的
Spearman 相关系数（真按档位调档应该接近 `+1`）。14 个会思考的模型**没有一个**呈单调关系：ρ 落在 −0.58~+0.31，
档间极差基本没超过档内波动，连 `none`/`minimal` 也关不掉思考（`Thinking.Type=enabled` 的 `kimi-k2.6`
亦然）——**上游不按这个字段调档，静默忽略**；非法值、错类型、未知键都不会让请求失败。
`step-5-preview` 那一行全 0 是模型特性不是失败：它 33 发全程 `reasoning_tokens=0`（`completion` 80~109 正常出答案）。
本轮 495 发（15 模型 × 11 列 × 3 轮，版本码 20260811）零失败。

**`kimi-k3` 的开关是「客户端版本码」，不是账号档位**（实测 2026-09-27）：上游按 `X-Ide-Version-Code`
下发不同的模型表与门控，`X-Ide-Version` 字符串本身不影响结果（`0.1.43`/`0.1.50`/`1.0.0` 同码同表；
2026-10-11 复核补测 `0.1.52`/`0.1.61` 仍逐行一致）。

| 版本码 | 模型表 | k3 的 `access.identity_list` | 结果 |
|:--|:--|:--|:--|
| `20260716`（旧） | 39 条 | `[2,3,100]` | 免费档被挡，请求回 `1005 plan:2` |
| `20260811` | 42 条 | `[0,5,1,2,3,100]` | 免费档**可用**，实测出流正常 |
| `20260924`（当前，见 `constants.go`） | 48 条 | 同上 | 与 `20260811` 的模型名**逐字相同**，只是表变长（实测 2026-10-30） |
| `20260814` / `20260821` / `20260927` / `20261231` | 42 条 | `[0,5,1,2,3,100]` | **与 `20260811` 逐字一致**（含故意超前的未来码） |

- 可见集合在 `20260811` 换过一茬：多出 `kimi-k3` / `glm-5.3` / `step-5-preview` /
  `deepseek-v4.1-flash`，少掉 `glm-5` / `DeepSeek-V4-Flash` / `DeepSeek-V4-Pro`（后三个在新版客户端里
  已 `is_invisible_to_user`）。可见集合本身还会随灰度漂移（同一版本码下 `qwen*` 时有时无），
  所以**动态拉取才是权威**，静态表只是拉不到时的兜底快照。
- **官方版本号在更新日志里**：`https://docs.trae.cn/work_changelog`（Trae 最新 `v0.1.52`，
  2026-08-21 那批；IDE 侧另见 `ide_changelog`）。客户端自带的更新接口
  `log.snssdk.com/service/2/app_alert_check/` 实测只回 `{"message":"success"}`，取不到版本。
- **冒充最新版能请求到模型，但没有收益**：用 `0.1.52 / 20260821` 的身份发 `kimi-k3` 拿到
  `HTTP 200` 出流，可模型表与门控跟 `20260811` 逐字一致；上游也不校验版本码（`1.0.0 / 20261231`
  照收）。`constants.go` 现在跟官方客户端实测值走（`3.4.11 / 20260924`，2026-10-30 抓包），
  理由只有一个：**身份指纹一致性**，不是功能需要。
- 复跑矩阵（只读，6 个请求 + 一发 k3）：
  `TW2A_PROBE_VERCODE=1 go test ./internal/upstream -run TestProbeLiveVersionCode -v`
  —— 哪天阵容出现 `+`/`−` 变化，才需要动 `constants.go`（`TestWorkChannelIdentityFrozen` 会拦住改动）。

### 出站请求头：以客户端抓包为准（2026-10-30）

把两版客户端装在同一台机上，用本地 MITM（自签 CA + 系统代理）拦 Agent 模式的真请求，
逐项对拍后改了 `headers.go` / `identity.go` / `constants.go`：

| 面 | 客户端实测 | 旧实现 |
|---|---|---|
| User-Agent | `Mozilla/5.0 … TraeCN/1.107.1 Chrome/142.0.7444.235 Electron/39.2.7 Safari/537.36`（INTL 品牌段是 `Trae/`） | `Trae/0.1.50` |
| `Accept` | `*/*`（流靠 body `stream:true`） | `text/event-stream` |
| `x-app-version` | 国内 `3.4.11`；国际字面量 `default` | 恒 `default` |
| `x-ide-version` | `3.4.11` / `3.6.4`（= product.json appVersion） | `0.1.50` / `1.0.2` |
| `x-cloudide-token` / `x-uid` | **不发** | 发了 |
| aha 五件套 | `x-harness:1` / `x-agent-host-type:0` / `x-bridge-transport:aha` / `x-lgw-req-sdk-type:3` / `package-type` | 缺 |
| ug 族 | 国内 `stable_cn` + `x-lscbd-*` + `x-user-region:CN` + `X-SS-DP:787976`；国际 `stable_i18n` + `X-SS-DP:677332`（**无** lscbd / region） | 缺 |
| trace | `x-custom-trace-id` + `traceparent`(00-) + `x-flow-traceparent`(04-) + `x-request-id: req_<uuid>` | 缺 |

**坑（花了一次联调才定位）**：`x-request-pin` 与 `x-requested-at` 是客户端**加密载荷**的
伴随头 —— 客户端发 `llm_utils_chat` 时 body 是 X-Medusa/X-Helios 包着的 base64 密文。
本仓发明文 body，只要带上这两个里任意一个，上游立刻回
`400 {"error":"x-request-pin or x-requested-at..."}`（逐头二分实测）。所以这两个头
**必须不发**；`x-request-id` / `x-ahanet-timeout` 单独带没事。

明文 body 本身不受影响：客户端风格头 + 明文 JSON 打 `api5-normal.mchost.guru` 实测 200 出 SSE 正文
（`go test -tags wirecheck ./internal/upstream -run TestWireCheckLive -v`，需 `TW2A_CAP` 指向抓包 jsonl）。

实测输出（2026-09-27；`可见` 是 13 还是 15 随 `qwen*` 灰度浮动）：

```
===== 版本码矩阵（get_detail_param, function=solo_work_lite），账号 uid=253358232317424
  ver / code          表   可见  k3 门控                          阵容变化
  0.1.43 / 20260716    39   13  identity_list":[2,3,100]}        旧档
  0.1.48 / 20260811    42   13  identity_list":[0,5,1,2,3,100]} +[deepseek-v4.1-flash glm-5.3 step-5-preview]
                                                              −[DeepSeek-V4-Flash DeepSeek-V4-Pro glm-5]
  0.1.51 / 20260814    42   13  identity_list":[0,5,1,2,3,100]} （与上一档逐字一致）
  0.1.52 / 20260821    42   13  identity_list":[0,5,1,2,3,100]} （与上一档逐字一致）
  0.1.60 / 20260927    42   13  identity_list":[0,5,1,2,3,100]} （与上一档逐字一致）
  1.0.0  / 20261231    42   13  identity_list":[0,5,1,2,3,100]} （与上一档逐字一致）

===== 用最新档身份（0.1.52 / 20260821）真发 kimi-k3
  HTTP 200，流开头: event:metadata data:{"session_id":"e3841837-…","prompt_cache_pool_record":null}
```

复跑：`TW2A_PROBE_CHAT=1 go test ./internal/upstream -run TestProbeLiveEffortAB -v`（吃额度；档位清单
`TW2A_PROBE_EFFORTS=`、模型 `TW2A_PROBE_MODEL=`、样本 `TW2A_PROBE_ROUNDS=`、上限 `TW2A_PROBE_MAXTOKENS=`）。
换模型即换 `TW2A_PROBE_MODEL=`；矩阵是一条 shell 循环每个模型跑一次，逐模型累积日志。

可签到、刷新剩余积分、**全部保活**（立刻全账号刷 token，不吃积分）、禁用/启用、解除冷却、移除，
以及粘贴登录回调后立刻写入 `auths/trae-{uid}.json` 并进池，不用重启。

「登录账号」拿到的授权链接里，`auth_callback_url` 恒为 `http://127.0.0.1:18080/authorize`，**写死不做配置项**：
实测 Trae 只认它自己 IDE 的这一条，换任何别的地址（面板自己的路径、别的端口、公网隧道域名）授权页
直接「登录失败 / 网络错误，请刷新页面重试」，登录都进行不下去。所以回跳必然落在浏览器那台机器的
`127.0.0.1:18080` 上（那里没人监听，页面打不开是正常的），只能把地址栏整段粘回面板换票。
想免粘贴就得在**浏览器那台机器**上跑个监听 18080 的本机中继（面板在服务器上时它够不着服务器的回环地址）。

保存配置后**立即生效**：冷却/熔断/降权/在途/闲置/快过期、会话粘性开关与 TTL、提示词模式与文件、
默认模型、超时与出站身份。**需重启**：`listen`、`auth_dir`、`state_file`、整个 `schedule`
（时点与开关）、`session_sticky.gc_interval`——面板保存时会明确列出，不会假装已生效。

「用量」视图：按窗口（24h/72h/7 天/30 天/全部）汇总请求数、失败数、输入/输出 token、
平均延迟与吐字速率，可按模型、按账号、按时序下钻。失败尝试也计入请求数——重试放大正是
靠这一列才看得见。数据落在 `data/usage.json`（与 `state_file` 同目录，30 秒防抖落盘）。

## 国际版（Trae 国际）

国内版与国际版是**同一套 API 的两套域 + 两套身份版本组**，所以入站协议、SSE 事件、`function=solo_work_lite`
全都不变，只有域和版本号不同（实测 2026-09-28，见下表）。账号按 `auth.domain` 自动分地区（`auth.Realm()`），
不需要开关：

| 面 | 国内版 | 国际版 |
|---|---|---|
| agent host（对话 / 模型表） | `api5-normal.mchost.guru`（2026-10-15 起；旧的 `trae-api-cn.mchost.guru` 仍活但已是遗留入口） | `coresg-normal.trae.ai`（2026-10-30 抓包更正；客户端逻辑域名，传输层 dispatch 到 `api5-normal-alisg.mchost.guru`。旧的 `a0ai-api-sg.byteintlapi.com` 仍活但已不是客户端在用的那台） |
| OAuth host | `api.trae.com.cn` | 登录回调里的 `host` 参数（本次实测 `api-sg-central.trae.ai`） |
| 登录页 | `www.trae.cn/authorization` | `www.trae.ai/authorization`（同参数集，`x_app_version=1.0.2`） |
| 模型名 | `glm-5.2` / `kimi-k2.6` … | `gpt-5.2` / `gpt-5.4` / `kimi-k3`（`gpt-6-sol`/`gpt-6-luna` 见下） |
| 签到 / 积分 | `/trae/api/v2/ug/*` | 签到**没有**（两个国际域上 404，国际账号跳过签到）；额度接口**有**，见下 |

### 国际版是**美元**计费，不是「免费无限」

> 2026-10-15 更正：本节此前写「国际版没有积分接口、跳过余额刷新」，**是错的**。
> 真相是国际账号**有**额度接口，只是口径与国内不是同一套——拿积分字段去读它，读出来是 0，
> 于是面板显示「无额度」，看起来像接口坏了。

上游在同一族接口上按账号返回两套计费体系，靠顶层 `is_dollar_usage_billing` 区分
（实测：国内 `false` + `is_credits_billing:true`；国际 `true`）：

| | 国内版 | 国际版 |
|---|---|---|
| 接口 | `api.trae.cn/trae/api/v2/pay/ide_user_ent_usage` | `ug-normal.trae.ai`（或账号 `api_host`）/trae/api/**v1**/pay/ide_user_ent_usage |
| 额度字段 | `quota.credits_limit` | `quota.basic_usage_limit` |
| 已用字段 | `usage.credits_amount` | `usage.basic_usage_amount` |
| 计量单位 | 积分 | **美元** |
| 免费档 | 有积分的几条，加总即余额 | `Free plan`：`basic_usage_limit=1` → **$1 免费额度** |

实测国际 Free plan 的完整额度报文（`ug-normal.trae.ai`，2026-10-15）：

```json
{"is_dollar_usage_billing": true,
 "user_entitlement_pack_list": [{
   "display_desc": "Free plan",
   "entitlement_base_info": {"quota": {"basic_usage_limit": 1, "credits_limit": 0,
                                       "advanced_model_request_limit": 1000,
                                       "premium_model_fast_request_limit": 10,
                                       "premium_model_slow_request_limit": 50}},
   "usage": {"basic_usage_amount": 0.00059, "credits_amount": 0}}]}
```

⇒ **免费档不是「无限免费」**：$1 额度，且另有请求数上限（高级模型 1000 次、
高级模型快速档 10 次 / 慢速档 50 次）。面板按美元显示这一列：

```
额度列（国际号）  $0.99941 / $1        已用 $0.00059 · 剩 99.9%
额度列（国内号）  5466 / 5900
```

两个口径**不可换算**，所以面板任何地方都不把它们相加：账号表分列、
KPI 里「积分合计」只算积分号、国际额度单列一张卡、到期分布图只画积分（国际号单列计数并注明）。

### 未证实：面板转发的调用会不会吃掉这 $1

**没验证成功，如实记着。** 用面板打了 4 次国际模型调用（共约 2300 token）之后：

- `basic_usage_amount` 仍是 `0.00059`，**没动**；
- 上游逐会话用量接口 `POST /trae/api/v1/pay/query_user_usage_group_by_session`
  （`usage_type:[5,6]`，近 7 天）**只返回 1 条** —— 是用户自己客户端 19:56 那次
  （`DeepSeek-V4.1-Flash`、`mode:"Max"`、in 13370 / out 8、$0.00005），
  **面板的调用一条都没登记**。

可能的原因（都还没证）：免费档按**请求数**而不是美元扣；或该台账只记客户端直连的用量；
或结算有较长延迟。**在弄清之前，不要把 $1 当成面板用量的硬约束**，
也别假设面板能把国际号用光。要确认只能在更长时间尺度上再对一次（面板跑一整天后比金额）。

顺带：那份逐会话台账带 `mode`/`use_max_mode` 字段，是 Max 模式被上游真实计量的独立佐证。

### 口径键名只准有一个（踩过的坑）

账号级口径在两个接口上必须用**同一个键名**（现统一为 `credit_unit`）。
踩过的坑：`/panel/api/overview` 发 `credit_unit`、`/panel/api/packages` 曾发 `unit`，
前端 `isUSD()` 只认前者 → 在「积分构成」视图里美元号被判成积分号，整张卡显示 0
（`0 · 共 0 · 1 个包 · 占最高 0%`），**不报错、不写日志、单测全绿** ——
因为单测喂的账号对象带的是 `credit_unit`，只有真浏览器渲染才看得出来。
回归网：`internal/panel/packages_unit_test.go::TestPackagesUnitKeyMatchesOverview`。



**`gpt-6-sol` / `gpt-6-luna`（官方门控 + 旁路）**：这两个在模型表里带
`display_contact_config.access.identity_list=[4,1,2,3]`，免费档（identity 0/5）不在内，直接发回
`1005 {"plan":4}`（要付费档；与客户端版本码无关，试过 6 个版本码门控一字不变）。**但同一批模型在租户
自定义槽位里能走**：`config_name=custom_model_gpt-5` + `model=openai/gpt-5.6-sol|luna` 实测出正文，
代理已给这两个名字挂了旁路（`intlSlotRoutes`），所以 `intl:gpt-6-sol` 直接可用。
槽位是租户配置，换账号可能没有——没有就照常报上游错误。其余 `custom_model_*` 槽位（gemini / kimi / minimax /
claude 系列）在这台账号上全回 `4023`，是租户没配那些 provider，不是模型不存在。

`work.trae.ai` 网页端列出的 11 个名字**不在同一通道上**，出站 `function` 现在跟着模型走
（`fnByModel`，客户端不用关心）：

| 网页端名字 | 内部模型 / 通道 | 状态 |
|---|---|---|
| Trae Auto Model | `auto` → 该地区旗舰（国际 `gpt-5.2`） | 可用 |
| GPT-6-Sol · GPT-6-Luna | `gpt-6-sol` / `gpt-6-luna`（工作通道，走槽位旁路） | 可用 |
| GPT-5.4 · GPT-5.2 · Kimi-K3 | 同名，工作通道（`solo_work_lite`） | 可用 |
| Gemini-3.1-Pro-Preview | `gemini-3.1-pro`，**coder 通道**（`solo_coder`） | 通道已接；本账号受上游速率/额度限制，忙时报 4011 |
| Gemini-3-Flash-Preview | `gemini-3-flash-solo`，coder 通道 | 同上 |
| MiniMax-M2.7 | `minimax-m2.7`，coder 通道 | 同上 |
| Kimi-K2.5 | `kimi-k2.5`，coder 通道 | 同上 |
| MiniMax-M3 | 这张账号的两张表里都没有（网页端列着，API 侧不下发） | 照实 400 |

coder 表怎么来的：同一台 agent 主机、同一个 `/api/agent/v3/llm_utils_chat`，只把 body 里的
`function` 换成 `solo_coder`。网页端（`work.trae.ai`）自己的模型接口
`GET /api/remote/v1/models` 用同一个票据就能读，列出的 `function=solo_coder` 组正是那 6 个名字
（`/api/remote/v1/*` 是它完整的会话/沙箱 API，反代只用到 `/models` 做对照，没接那套）。

**加账号**：面板「添加账号」里选「国际版」（链接自动切 `www.trae.ai`），或 `REALM=intl ./login.sh`。
回调链接里自带 `host`，换票与落盘按它写 `domain`/`apiHost`，不用手填；换票后 `apiHost` 也会随 token 轮换一起用。

**调模型**：模型名带地区前缀，`/v1/models` 列出的 id 就带（`cn:glm-5.2`、`intl:gpt-5.2`），原样回传即可；
不带前缀按国内版走（老客户端零变化），但名字在模型表里属于国际版的（如 `gpt-5.2`）会自动落到国际账号。
选号按前缀过滤——国际模型只发国际号，反之亦然，所以国内号不会被 `intl:` 请求误伤。

**别名层（客户端自带模型名）**：Claude Code / Cursor / Cline 把 `claude-*`、`gpt-4o` 写死在配置里，
这里照社区现成实现的做法把它们落到真模型上（`internal/server/model_alias.go`，别名表是唯一改动点；
口径参考 `ZedeX/trae-local-api` 的 `model-config.json` 与 `linqiu919/trae2api` 的 `convertModelName`）：
`claude-*`（含各种日期后缀）→ 该地区旗舰；`gpt-4o` / `gpt-4o-mini` / `gpt-4.1` / `gpt-3.5-turbo` →
国内 `glm-5.2`、国际 `gpt-5.2`。未知名字仍然 400，不会静默塞默认模型。

国际版的 `X-App-Version-Code` **必须非空**：缺了上游直接 `4001 missing required parameter`（实测），
代码里由 `identFor()` 按地区给。用量台账暂不区分地区（`workbuddy2api-panel` 有，按需再加一列）。

## 目录结构

```
cmd/server/       HTTP 服务（config + main）
cmd/signin/       批量签到工具
cmd/credit/       积分查询工具
internal/auth/    auth 文件解析/原子写回
internal/upstream/ Trae 上游客户端 + SSE 转换
internal/pool/    账号池（冷却/禁用/积分）
internal/scheduler/ 定时签到 + token 保活
internal/server/  三种入站协议（chat / responses / messages）+ 共用请求链路
internal/panel/   /panel/ 管理页（go:embed）
internal/session/ 会话粘性（会话 → 账号 绑定表）
internal/prompt/  系统提示词（内置默认 + 文件覆盖 + 改写）
internal/usage/   调用用量台账（分桶 + 持久化）
login.sh / signin.sh / credit.sh  运维脚本
auths/            （gitignored）trae-*.json 凭证
data/             （gitignored）state.json 池状态、usage.json 用量台账
```

## 脱敏说明

- 任何真实 token/key 一律 `********` 或 env 引用，绝不落盘 git。
- `auths/`、`data/`、`config.json`、`.env`、`*.key`、`*.pem` 全部 gitignored。
- `docs/` 不参与上传（gitignore + dockerignore）。
- 验证类文档（`VERIFICATION.md` 等）不入库。
- 日志/状态输出只显示 UID/Nickname/积分，不打印 token。
- 抓包夹具（`internal/upstream/testdata/`）是上游**响应报文原文**：入库前把 uid / 订单号 /
  设备号这类 12 位以上数字统一掩成 `1000…0`，计费与时间字段原样保留，
  见 `ent_usage_unit_test.go`、`domains_test.go`、`max_variant_test.go`。

## 安全与合规

- **非官方、未获授权**：本文与上游服务商无任何关系，仅用于**本人授权账号**在本机 / 私有环境下的协议互操作。
- **凭证**：`auths/*.json` 以 0600 明文保存，目录不得进入备份、同步盘或共享范围；仓库不含任何账号、token 或密钥。
- **鉴权**：`api_key`（或 `api_keys`）为空时 `/v1/*` 与 `/panel/api/*` 不鉴权，仅适合 `127.0.0.1` 或可信内网；公网部署必须设密钥并前置 HTTPS 反代与访问限流。
- **合规边界**：不得用于搭建对外中转站、共享他人账号、批量注册小号或售卖额度；作者不承担使用后果。

## 血统与致谢

- 本仓 fork 自 [`Sliverkiss/traework2api`](https://github.com/Sliverkiss/traework2api)（原始仓库已下线）。
  同血统的兄弟实现另见 `JeffHu0912/trae2api` 与 `connectedGraph/trae2api-web`（均 MIT）—— 模型表、版本码、
  错误码这些实测结论与它们的笔记互为参照；对方实现更小或更旧，**未直接合并其代码**。
- 只当对拍基线、不进仓的参考实现：`shadyrispy/cpa-multi-plugins`（答案卷）、`240xu/trae2api-more`、
  `star620/TraeTools`、`trae-workbuddy-switch`。
- 没有 license 的项目（如 `wefewe/trae2api`）只读参考，不取代码。

## License

MIT，见 [LICENSE](LICENSE)。
