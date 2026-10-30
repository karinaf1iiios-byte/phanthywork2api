# phanthywork2api

> 许可证：AGPL-3.0。包含来自 [new-api](https://github.com/QuantumNous/new-api) 的 relaykit / tokenkit 组件（AGPL-3.0），详见 LICENSE 与 THIRD-PARTY-LICENSES.md。

PhanthyWork 桌面端（macOS 2.43.0 / 内核 2.1.92）协议逆向的产物：一套 Go 写的多账号推理网关
＋ 一组 Python 运维脚本，能自动完成每日签到、onboarding 1500 分、邀请奖励与账号池调度。

> **免责声明**：本项目仅用于协议学习与个人自用研究。请遵守目标服务的用户协议，
> 不要用于商业转售、批量注册或任何绕过服务端风控的用途。所有协议结论均来自
> 公开可得的客户端二进制（dmg）静态分析 + 自己账号的线上实测，不含任何服务端代码。
> 仓库里**不含**任何真实 token / 私钥 / 邀请码。

---

## 能做什么

| 能力 | 入口 | 状态 |
|---|---|---|
| 每日签到（09:00 / 21:00 两趟，幂等不漏） | `scripts/claim_daily.py`、`cmd/activity -action claim` | 实测到账 +300（连签第 2 天 +600） |
| Onboarding 1500 分（5 个任务、真实推理触发） | `scripts/onboard_driver.py` | 实测 `0/1500 → 1500/1500` |
| 邀请（开工搭子）码 / 档位 / 关系 | `cmd/activity -action referral*`、`scripts/invite_flow.py`、`scripts/invite_link.py` | 端点全通；**关系只在注册表单建立，事后 bind 永久 409** |
| 推理网关（Anthropic + OpenAI 双入口） | `cmd/server`：`/v1/messages`、`/v1/chat/completions` | 实测 200，SSE 透传 |
| 设备画像 / 会话粘性 / 缓存透传 / 出站净化 | `internal/{fingerprint,session,sanitize}` | 见 `docs/05` |
| 模型目录与透传（`/v1/models` 本地合成，未知模型原样透传） | `internal/upstream/models.go`、`config.models` | `/v1/models` 69 个 id；网页列出的 13 个模型实测 **13/13 放行** |
| 选号权重（快过期积分优先） | `internal/server/weight.go` | 单测 6 例；抽签命中 ≈74% |
| 统一调度（互不重叠 + 目录锁） | `scripts/pool.sh` + `scripts/install_schedule.sh` | launchd 9 个 job |
| 自动登记装机 | 读摘要前先 `ensureRegistered`（含 401 自愈与 10 分钟失败冷却），新号登录即刻显示真实数据 |
| usage 透传 | 流式/非流式共用一套映射；真实数字取 `message_delta`（`message_start` 的 `input_tokens` 是早报）；上游 `billing_usage` 明细原样带上（`reasoning_tokens` 等） |
| 只有 thinking 的一轮 | 网关侧兜底（`upstream.reasoning_only=promote`）：补成正文再交付；流式补 `content` delta、原生 `/v1/messages` 补 `text` 块 |
| 流式工具调用 | `content_block_start(tool_use)` + `input_json_delta` → OpenAI `tool_calls` 增量，`finish_reason` 按 `stop_reason` 映射 |
| 面板调度 | `config.schedule.jobs` 进程内定时（签到/回读/补做），看板可见上次结果并可一键运行 |
| 定时补做 | `sweep.py` 扫 onboarding 进度并补做（9:40 / 21:40）；`invite` 任务（8:50 / 20:50）刷新带码注册链接 |
| 账号去重 | 登录按 `accountScope` 收敛，同号覆盖不新建；启动自动合并历史重复（`.trash/`） |
| 积分看板（用量统计 / 积分总览 / 账号池） | `internal/server/ui.go` + `web/`：`/ui/` | 实测渲染正常，数据来自上游 summary/usage 接口 |
| 可视化登录（PKCE 两步，浏览器内完成） | `/ui/login.html`、`POST /api/ui/login/*` | 起链接 → 粘授权码 → 落盘并立刻入池 |
| 主动续期（keepalive，60s 检查 / 10min 阈值） | `internal/server/handler.go` | 修掉"1h 间隔 + 5min 阈值"导致的 55 分钟空窗 |

## 快速开始

需要 Go（开发用 1.27）与 Python 3（标准库即可，零第三方依赖）。

```bash
git clone <this-repo> && cd phanthywork2api

# 1) 登录，拿到 auths/phanthy-<ts>.json
./scripts/login.sh                 # 交互式：生成链接 → 浏览器授权 → 粘授权码
./scripts/login.sh --ui            # 或打开可视化登录页（需网关在跑）
./scripts/login.sh --code='<码>'    # 非交互

# 2) 看板 + 终端速览
./scripts/dashboard.sh             # 起网关并打开 http://127.0.0.1:7863/ui/
./scripts/credit.sh                # 积分日报（表格 + 汇总行）
./scripts/acct.sh list             # 账号与装机身份对照

# 3) 回读状态（不花积分）
python3 scripts/probe_models.py --from-config   # 13 个模型实测是否放行（每次约 1 积分）
python3 scripts/claim_daily.py --status
python3 scripts/onboard_driver.py --status

# 4) 真实领取 + 补齐 onboarding
python3 scripts/claim_daily.py
python3 scripts/onboard_driver.py

# 5) 网关
cp config.example.json config.json          # 改 upstream / 端口 / pool 参数
go build -o p2a ./cmd/server && ./p2a -listen :7863
curl -s localhost:7863/status | python3 -m json.tool

# 6) 定时任务（macOS launchd）
./scripts/install_schedule.sh --install
./scripts/install_schedule.sh --status
```

脚本都支持只读模式（`--status` / `--plan` / `--dry-run`），确认无误再真跑。

## 看板

```bash
./scripts/dashboard.sh        # 起网关并打开 http://127.0.0.1:7863/ui/
```

`/ui/` 是纯静态页面（无前端构建链，`go:embed` 打进二进制），四块内容：积分总览卡、
7/14/30 天用量图、按模型的积分分布、账号池明细（积分 / 套餐 / 签到 / 引导任务 / 邀请 / 权重 / token 寿命）。
右上角「登录新账号」进 `/ui/login.html`：浏览器完成 OAuth 授权 → 粘回授权码 → 服务端落盘并立刻入池。

访问控制：默认只允许回环与内网地址；要远程看就在 `config.json` 里设 `ui.access_token`，
地址带 `?token=…`。实现细节与实测见 `docs/11`。

## 目录

```
cmd/login/        OAuth（PKCE）：-step=url 打印授权链接，-step=exchange 换 token
cmd/activity/     账号池运维 CLI：status/provision/claim/reconcile/probe/rotate/referral
cmd/server/       推理网关主程序

internal/upstream/     协议层：sign.go（两条 canonical）、installation.go（三把钥匙 + di_ 推导）、
                       activities.go（register/summary/claim/rewards/reconcile/usage）、
                       inference.go（desktop-inference-v2 证明头）、pool.go、referral.go
internal/fingerprint/  按身份种子派生设备画像（UA / cli-device / metadata.user_id）
internal/session/      会话→账号粘性（TTL 2h、上限 20000、落盘、命中统计）
internal/sanitize/     出站净化（丢 SDK 头、beta 白名单、剥离中转站痕迹）
internal/server/       网关：双入口、加权选号、缓存透传、/v1/phanthy/*、定时领取、
                       主动续期（keepalive）、内置看板 UI（ui.go + web/ 静态资源，go:embed 打包）

scripts/claim_daily.py     签到全链（register → summary → claim → rewards → reconcile）
scripts/inference.py       desktop-inference-v2 证明 + /v1/messages
scripts/onboard_driver.py  onboarding 真实用量驱动（按 action_kind 造用量）
scripts/onboard_new.py     只给「onboarding 未满」的新号补做（进池时一次性）
scripts/invite_flow.py     邀请：老号取码 → 新号自己签名 bind（已证伪，仅作协议参考）→ 回读档位
scripts/invite_link.py     邀请：挑可用邀请人 → 产出「带码注册」链接（data/invite_candidate.json）
scripts/login.sh           登录包装：构建 cmd/login → 生成链接 → 交换 token → 打池子状态
scripts/credit.sh          积分日报（表格 + 汇总行，--json / --quiet）
scripts/checkin.sh         批量签到（薄包装 cmd/activity claim，汇总 ok/already/warn/fail）
scripts/acct.sh            账号运维：list / show / rotate / rm / path
scripts/dashboard.sh       起网关并打开 /ui/ 看板（--stop / --no-open）
scripts/pool.sh            统一调度入口（claim/verify/bind/onboard/invite，目录锁）
scripts/install_schedule.sh launchd 安装/查看/卸载
scripts/ed25519_min.py     零依赖 Ed25519（RFC8032 TEST1 自证）

docs/    协议与实测记录
auths/   凭据目录（空，内容不入库）
data/    运行态（空，内容不入库）
```

## 文档

| 文档 | 内容 |
|---|---|
| `docs/00-endpoints.md` | 端点清单与两条签名链 |
| `docs/01-api-test-report.md` | 自动化实测报告（签到链） |
| `docs/02-spec.md` | 规格说明书 v2 |
| `docs/03-desktop-installation.md` | 装机注册协议、`di_` 推导、错误码表 |
| `docs/04-inference-and-pool.md` | 推理证明协议、onboarding 判据、账号池身份管理 |
| `docs/05-fingerprint-session-cache.md` | 指纹 / 粘性 / 缓存 / 净化的实测数据 |
| `docs/06-referral.md` | 邀请端点、绑定限制、池调度口径 |
| `docs/07-schedule.md` | 定时任务：时间表、锁语义、坑 |
| `docs/08-weighting.md` | 三因子选号权重（快过期优先） |
| `docs/09-onboarding-recipe.md` | 1500 分正确配方（`action_kind` + `start` + 真实用量） |
| `docs/10-ref-architecture.md` | 参考架构提炼（workbuddy2api） |
| `docs/11-ui-and-scripts.md` | 看板 / 可视化登录 / 五个实用脚本的实现与实测 |

## 协议速查

活动签名（签到 / 邀请 / summary 等 `/api/oauth/*`）：

```
canonical = v1 \n METHOD \n /api/oauth/<path> \n ts \n nonce \n b64url(sha256(body)) \n idempotency-key
头：x-desktop-installation-id / -timestamp / -nonce / -signature（identity 钥匙签）
```

推理证明（`/v1/messages`）：

```
canonical = desktop-inference-v2 \n METHOD \n pathname \n ts_ms \n nonce \n
            b64url(sha256(body)) \n b64url(sha256(authorization)) \n
            b64url(sha256(desktop_inference_token)) \n
            x-request-id \n x-claude-code-session-id \n x-phanthy-expert-id \n x-phanthy-skill-names
头：x-phanthy-desktop-session / -proof-timestamp / -proof-nonce / -proof-signature（session 钥匙签）
```

细节、反例与错误码见 `docs/03`、`docs/04`。

## 已知限制

- **一账号一装机**：`rotate` 只替换不并存；多装机共用设备的积分口径未验证。
- **邀请不能补填**：`POST /referrals/bind` 对已产生用量的老号返回 409 `existing_user_ineligible`，
  邀请关系只能在注册当时建立。
- **缓存命中不稳定**：命中率取决于上游节点本地缓存，`/status` 的实测值才算数。
- **onboarding 是一次性账号任务**（`program_key=desktop_onboarding_2_16_0`），已满 1500 的号只读不重跑。
- 所有写操作都没有失败回滚；批量操作前先看 `--status`。

## License

MIT，见 `LICENSE`。

## 依赖

- Go 1.26+（协议层依赖 new-api relaykit v0.3.5 / tokenkit；go.mod 已锁定）
