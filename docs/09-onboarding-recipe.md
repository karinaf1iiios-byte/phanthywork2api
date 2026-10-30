# 09 Onboarding 1500 分的正确配方（0 → 1500 实测打通）

## 结论先行

`reconcile` 只负责「按服务端判据核对用量」，本身不能造分；而判据全部写在
`GET/POST /api/oauth/activities/summary` 的 `onboarding.tasks[].action_kind` +
`action_config` 里。按 `action_kind` 去造真实用量，再 reconcile，就能结算。

2026-10-28 实测：老号从 `0/1500` 打到 `1500/1500`，credits `872 → 2320`，五个任务逐项结算：
`recommended_model_success +200`、`follow_up_success +200`、`expert_success +300`、
`skill_success +300`、`complete_workflow +500`。

脚本：`scripts/onboard_driver.py`（`--status` 只看进度，`--only <task_key>` 只补一项）。

## 关键更正：`signed_session_match` 不是门槛

白天一度出现过 reconcile 返回 `signed_session_match=false` 且一项都不结算，当时误判成
「服务端收紧了，必须有真桌面会话」。重做验证后确认：

```
{"billing_status":"settled","client_source":"desktop","installation_match":true,
 "desktop_verified":true,"has_user_message":true,"output_tokens_positive":true,
 "signed_session_match":false}          ← 5 个任务全部 completed 的那几次，它也是 false
```

它只是「这条用量能否对上一个由桌面端签过的会话」的观测值，不参与结算。
真正卡住的是**任务前置条件**（下面那张表），以及一个容易忽略的点：**每个任务都要先 start**。

## 五个任务的判据（实测）

| task_key | action_kind | 需要的真实用量 | 实测细节 |
|---|---|---|---|
| `recommended_model_success` | `new_session` | 新会话里一次有效对话 | `phanthy-pro` 即可 |
| `follow_up_success` | `recent_session` | 同一会话第二轮要「长出结果」 | 第二轮 `max_tokens` 给 1500+，被截断的那轮不算 |
| `expert_success` | `expert_market` | 请求带真实专家 id | `phanthy.research-analyst` / `phanthy.data-analyst` |
| `skill_success` | `skill_picker` | 请求带真实 skill 名 | `deep-research` / `academic-paper` / `understand-chat` |
| `complete_workflow` | `workflow` | 同一会话「专家 + Skill」两轮完整交付 | `max_tokens=3000`，两轮都带 expert 与 skill |

## 三个坑

1. **`action_config.recommended_models` 是陷阱**
   summary 写着 `["Iris-1.0","Apollo-2.0"]`，但体验版发这两个模型直接
   `403 model_not_allowed`；判据并不校验模型名，用 `phanthy-pro` 一样结算。
2. **专家 id / skill 名必须是真目录里的值**
   臆造值（如 `expert-product-consultant`、`weekly-review`）不报错、也正常 200，
   但任务**不动**。真 id 形如 `phanthy.research-analyst`（sidecar 里的专家目录
   `x1({id:"phanthy.xxx", slug:...})`），skill 名形如 `deep-research`。
3. **不 start 就 reconcile 一定不动**
   `POST /activities/onboarding/tasks/<key>/start` 先落 `started_at`，再 reconcile 才会评这一项。
   `POST .../complete` 对 `new_session` / `recent_session` / `workflow` 三类返回
   `400 task_action_not_supported`（"This task is not completed by a direct action."），
   对 `expert_market` / `skill_picker` 返回 200 但也**不涨分** —— 别拿它当捷径。

## reconcile 的扫描半径

`reconcile` 的 `billing` 段是 `{"scanned":1,"settled":1}`：只为**最近一条**结算记录出证据。
所以顺序很重要——先 start 任务，紧接着做它的用量，马上 reconcile；中间插别的请求会盖掉证据。

## 顺带纠正的接口用法

`POST /api/oauth/usage/requests` 的 body 键是 **`requestIds`（驼峰）**，不是 `request_ids`：

```json
{"requestIds":["req_..."]}   → {"requests":[{"request_id":"...","status":"settled","cost_points":2}]}
```

传 `request_ids` 会稳定 `400 {"error":"invalid_request_ids"}`（格式正确与否都报同一个错，很容易误判）。
单条查询是 `GET /api/oauth/usage/requests/<request_id>`，返回 `status` 与 `cost_points`。
