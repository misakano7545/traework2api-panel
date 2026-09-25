# traework2api

TRAE Work (SOLO CN) 的 OpenAI 兼容反向代理。把 TRAE SOLO 免费对话通道
（`llm_utils_chat` + `function=solo_work_lite`）包装成标准的
`/v1/chat/completions` + `/v1/models` 接口，支持多账号轮转、自动签到、token 自动刷新。

纯 Go 标准库，零第三方依赖。

## 功能

- **OpenAI 兼容 API**：`POST /v1/chat/completions`（流式/非流式）、`GET /v1/models`
- **多账号池**：积分加权挑选（闲置补偿 + 快过期优先），在途限额，熔断/降权/计划冷却，
  401 禁用，请求级轮转
- **自动签到**：多时点定时签到 + 周期余额刷新 + 手动批量签到（`signin.sh`）
- **积分查询**：全账号/指定账号日报（`credit.sh`）
- **Token 保活**：多时点预刷新（默认过期前 24h 内），refreshToken 轮换落盘
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

## 管理面板

浏览器打开 `http://127.0.0.1:7864/panel/`。页面和 `app.js` 不用密钥；`/panel/api/*` 与 `/v1`
共用 `config.json` 里的 `api_key`（「配置」页可见可改，保存后两边同时换新）。没设密钥时这些接口只接受本机连接。

「模型」页按上游 `get_detail_param` 原值列出模型的 `capability`（`model_capability`）与思考
相关字段（`model_extra_config` 里的 `Thinking.Type`、`reasoning_effort_config` 原文），不做解释和换算。

客户端自己带的 `reasoning_effort` 之类字段是**原样透传**上游的，不认的也一个不吞（`PrepareBody` 只改
`stream`/`function`/`config_name`/`model`/`tools`）。实测 `glm-5.2` 全档各 3 次，`reasoning_tokens`
中位数：不发 331 / `none` 345 / `minimal` 337 / `low` 314 / `medium` 313 / `high` 330 —— 全在 ±5%
抖动带里，连 `none`、`minimal` 都没把思考关掉，也就是**上游不按这个字段调档，静默忽略**（各档内单次
波动 ±20%，大于档位间差异）。非法值、错类型、未知键都不会让请求失败。复跑：
`TW2A_PROBE_CHAT=1 go test ./internal/upstream -run TestProbeLiveEffortAB -v`（吃额度，19 次约 5 积分；
`TW2A_PROBE_ROUNDS=8` 加样本，`TW2A_PROBE_MODEL=` 换模型）。

可签到、刷新剩余积分、禁用、解除冷却、移除，以及粘贴登录回调后立刻写入 `auths/trae-{uid}.json` 并进池，不用重启。

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
internal/scheduler/ 定时签到 + token 预刷新
internal/server/  OpenAI 兼容路由
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
