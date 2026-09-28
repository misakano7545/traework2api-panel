# traework2api

TRAE Work (SOLO CN) 的 OpenAI 兼容反向代理。把 TRAE SOLO 免费对话通道
（`llm_utils_chat` + `function=solo_work_lite`）包装成统一服务，支持多账号轮转、自动签到、
token 自动刷新。入站三种协议：`chat_completions`（`POST /v1/chat/completions`）、
`codex_responses`（`POST /v1/responses`）、`anthropic_messages`（`POST /v1/messages`，
同样接受 `POST /messages`）。

纯 Go 标准库，零第三方依赖。

## 功能

- **三种入站协议**：`POST /v1/chat/completions`（OpenAI chat）、`POST /v1/responses`
  （Codex；`wire_api="chat"` 已在 Codex 0.84+ 移除，只认 Responses）、`POST /v1/messages`
  与 `POST /messages`（Anthropic Messages，含 `x-api-key` 鉴权）；都是先转成 chat 请求再走
  **同一条链路**（切号、冷却、会话粘性、提示词改写、出站改写），上游仍是 SOLO chat SSE
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
- **登录闭环**：`login.sh` 自生成登录链接 → 浏览器登录 → 粘贴回调链接 → 换 token 落盘（面板「添加账号」同一流程，TRAE 只认它自己的本机回调，见下）

## 快速开始（Docker）

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

TRAE 登录页强制回调 `127.0.0.1`，但浏览器与服务器不需要同机：
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

## 本机运行（非 Docker）

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
| `default_model` | 请求缺 `model` 时的默认模型（trae 扩展，wb 无此键） |
| `cooldown` | `plan_credit`（1005 硬冷却，trae 扩展）、`soft_rate` + `soft_rate_max`（限流指数退避封顶） |
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

**与 workbuddy2api 的配置面差异**（有差异的地方都是"trae 上游没有这个能力"）：
`schedule.travel/activity/blackcat_*`（活动任务）、`global`（国际域路由）、`upstash`
（共享状态）、`features.sanitize_blacklist_fingerprints`、`pool.max_in_flight_global`、
`pool.cost_explore_interval`（成本分层探索）、`upstream.device_token*/cli_version` 未提供——
写成空壳字段只会让人以为改了有效果。

## 运维

```bash
./signin.sh             # 批量签到（全账号，自动 refresh 过期 token）
./credit.sh             # 积分日报（美化）
./credit.sh -json       # 积分日报（JSON）
./credit.sh <uid>       # 指定账号
```

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
`input_tokens_details.cached_tokens`，messages 出 `cache_read_input_tokens`（都取自 SOLO
`usage.cache_read_input_tokens`，没有就不写，不拿 0 冒充"没命中"）。
客户端自己带的 `reasoning_effort` 之类字段是**原样透传**上游的，不认的也一个不吞（`PrepareBody` 只改
`stream`/`function`/`config_name`/`model`/`tools`）。

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
下发不同的模型表与门控，`X-Ide-Version` 字符串本身不影响结果（`0.1.43`/`0.1.50`/`1.0.0` 同码同表）。

| 版本码 | 模型表 | k3 的 `access.identity_list` | 结果 |
|:--|:--|:--|:--|
| `20260716`（旧） | 39 条 | `[2,3,100]` | 免费档被挡，请求回 `1005 plan:2` |
| `20260811`（当前，见 `constants.go`） | 42 条 | `[0,5,1,2,3,100]` | 免费档**可用**，实测出流正常 |
| `20260814` / `20260821` / `20260927` / `20261231` | 42 条 | `[0,5,1,2,3,100]` | **与 `20260811` 逐字一致**（含故意超前的未来码） |

- 可见集合在 `20260811` 换过一茬：多出 `kimi-k3` / `glm-5.3` / `step-5-preview` /
  `deepseek-v4.1-flash`，少掉 `glm-5` / `DeepSeek-V4-Flash` / `DeepSeek-V4-Pro`（后三个在新版客户端里
  已 `is_invisible_to_user`）。可见集合本身还会随灰度漂移（同一版本码下 `qwen*` 时有时无），
  所以**动态拉取才是权威**，静态表只是拉不到时的兜底快照。
- **官方版本号在更新日志里**：`https://docs.trae.cn/work_changelog`（TraeWork 最新 `v0.1.52`，
  2026-08-21 那批；IDE 侧另见 `ide_changelog`）。客户端自带的更新接口
  `log.snssdk.com/service/2/app_alert_check/` 实测只回 `{"message":"success"}`，取不到版本。
- **冒充最新版能请求到模型，但没有收益**：用 `0.1.52 / 20260821` 的身份发 `kimi-k3` 拿到
  `HTTP 200` 出流，可模型表与门控跟 `20260811` 逐字一致；上游也不校验版本码（`1.0.0 / 20261231`
  照收）。所以 `constants.go` 保持 `0.1.50 / 20260811`，不追官方版本号。
- 复跑矩阵（只读，6 个请求 + 一发 k3）：
  `TW2A_PROBE_VERCODE=1 go test ./internal/upstream -run TestProbeLiveVersionCode -v`
  —— 哪天阵容出现 `+`/`−` 变化，才需要动 `constants.go`（`TestWorkChannelIdentityFrozen` 会拦住改动）。

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
实测 TRAE 只认它自己 IDE 的这一条，换任何别的地址（面板自己的路径、别的端口、公网隧道域名）授权页
直接「登录失败 / 网络错误，请刷新页面重试」，登录都进行不下去。所以回跳必然落在浏览器那台机器的
`127.0.0.1:18080` 上（那里没人监听，页面打不开是正常的），只能把地址栏整段粘回面板换票。
想免粘贴就得在**浏览器那台机器**上跑个监听 18080 的本机中继（面板在服务器上时它够不着服务器的回环地址）。

保存配置后**立即生效**：冷却/熔断/降权/在途/闲置/快过期、会话粘性开关与 TTL、提示词模式与文件、
默认模型、超时与出站身份。**需重启**：`listen`、`auth_dir`、`state_file`、整个 `schedule`
（时点与开关）、`session_sticky.gc_interval`——面板保存时会明确列出，不会假装已生效。

「用量」视图：按窗口（24h/72h/7 天/30 天/全部）汇总请求数、失败数、输入/输出 token、
平均延迟与吐字速率，可按模型、按账号、按时序下钻。失败尝试也计入请求数——重试放大正是
靠这一列才看得见。数据落在 `data/usage.json`（与 `state_file` 同目录，30 秒防抖落盘）。

## 目录结构

```
cmd/server/       HTTP 服务（config + main）
cmd/signin/       批量签到工具
cmd/credit/       积分查询工具
internal/auth/    auth 文件解析/原子写回
internal/upstream/ SOLO 上游客户端 + SSE 转换
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
