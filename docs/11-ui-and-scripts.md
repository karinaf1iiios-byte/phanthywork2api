# 11 看板与实用脚本：/ui、登录页、login/credit/checkin/acct/dashboard

## 结论先行

- **看板 `/ui/`**：积分总览 + 用量图 + 模型分布 + 账号池表格 + 邀请入口 + 定时任务卡，数据全部来自上游真实接口
  （`/api/oauth/activity/summary`、`/api/oauth/usage/summary`、`/api/oauth/referrals/me`），
  不做任何本地推算。默认只允许本机/内网访问。
- **可视化登录 `/ui/login.html`**：浏览器里走完 PKCE 两步（生成授权链接 → 粘授权码），
  服务端换 token 并落盘，立刻并进池子。
- **脚本**：`login.sh` / `credit.sh` / `checkin.sh` / `acct.sh` / `dashboard.sh`。
  签到、状态、批量操作全部是**薄包装**——真正的逻辑在 `cmd/login`、`cmd/activity`，
  脚本只负责构建二进制、把输出整理成人能看的表。
- 设计语言抄的是 `code.phanthy.com` 的 CSS 变量（浅色、白面板、`#2f7bff` 主色、蓝→青品牌渐变），
  没有引入任何前端依赖，`go:embed` 直接打进二进制。

## 看板

启动：

```bash
./scripts/dashboard.sh          # 后台起服务 + 开浏览器（已在跑就复用）
./scripts/dashboard.sh --stop   # 停掉（只停它自己起的，不动 launchd 的实例）
./scripts/dashboard.sh --no-open   # 服务器上只起服务
```

打开 `http://127.0.0.1:7863/ui/`。页面四块：

1. **KPI 卡**：可用积分合计 / 账号数 / 今日已签到 N-M / 最近套餐到期。
   套餐到期是硬约束——**积分整包随套餐作废**，所以卡片上直接标"还剩几天"。
2. **积分消耗**：按天柱状（积分）+ 折线（请求数），7/14/30 天可切，空白日留一条淡痕，
   鼠标悬停显示当天 `积分/请求/Token`。
3. **模型分布**：按积分占比排序的横向条，出自服务端 `usage_by_day_and_model.model`。
4. **邀请入口**：当前邀请人（手机掩码 + uid）、已用档位 `0/2`、下一档奖励、邀请码、可复制的
   带码注册链接，以及「重新挑邀请人」按钮（就是异步跑一次 `invite` 任务）。
   这张卡存在的理由：邀请关系**只在注册表单提交那一刻**建立，`POST /referrals/bind`
   对已创建账号永久 409，所以看板给的是注册入口而不是补填按钮。
5. **定时任务**：每条任务的计划时间（09:00 · 21:00 这种）、下次触发、上次运行（相对时间+钟点）、
   耗时、结果（成功/失败 rc/运行中）、「立即运行」按钮；点整行可展开上次运行的输出尾巴。
   数据来自进程内的 `jobRunner`（`config.schedule.jobs`），落盘 `data/jobs.json`。
6. **账号池表**：手机号掩码（`138****0000`）当主标题、uid 作副标题，加积分、套餐、今日签到、引导任务进度、邀请两档、权重、token 寿命、冷却状态。

头部两个动作按钮直接打真接口：**立即签到**（`POST /api/ui/claim`，服务端幂等）、
**续期全部 token**（`POST /api/ui/refresh`）。

### 访问控制

| 配置 | 行为 |
|---|---|
| `ui.enabled=false` | `/ui`、`/api/ui/*` 全部不挂载 |
| `ui.access_token=""`（默认） | 只允许回环与内网地址（`127.0.0.0/8`、`10/172/192.168`） |
| `ui.access_token="xxx"` | 非本机请求必须带 `?token=xxx` 或 `X-UI-Token: xxx` |

没有复用网关的 `api_key`：浏览器直接访问时带自定义头很别扭，而看板如果暴露在公网又必须先有鉴权，
所以单开一套。**默认配置下公网 IP 是打不开的**，`TestUIAllowed` 锁了这个行为。

### 后端接口

| 路径 | 作用 |
|---|---|
| `GET /api/ui/overview` | 账号池总览（20 秒缓存，避免页面轮询把上游打爆） |
| `GET /api/ui/usage?days=7\|14\|30` | 按天/按模型/按账号的用量聚合 |
| `POST /api/ui/claim` | 全池签到（走 `claimOne`，与定时任务同一套逻辑） |
| `POST /api/ui/run` | body `{"job":"invite\|claim\|verify\|sweep"}`，异步触发一条定时任务，返回 202 |
| `POST /api/ui/refresh` | 强制续期所有 token |
| `POST /api/ui/login/start` | 生成 PKCE 授权链接，返回 `{id, url}` |
| `POST /api/ui/login/exchange` | `{id, code}` → 换 token → 落盘 `auths/phanthy-*.json` |

`/api/ui/login/*` 的 verifier 只活在本进程内存里，10 分钟自动清；进程重启 = 这次登录作废，
重新点一次「生成授权链接」即可。

## 脚本

| 脚本 | 说明 | 示例 |
|---|---|---|
| `login.sh` | OAuth 登录（PKCE 两步），自动构建 `cmd/login`，成功后打池子状态 | `./scripts/login.sh` / `--code=XXX` / `--url` / `--ui` |
| `credit.sh` | 积分日报：表格 + 汇总行（未签到/异常单独列） | `./scripts/credit.sh` / `--json` / `--quiet` |
| `checkin.sh` | 批量签到，汇总 `ok/already/warn/fail`；服务端幂等，cron 多打几次安全 | `./scripts/checkin.sh` / `--dry` |
| `acct.sh` | 账号运维：`list` / `show` / `rotate` / `rm` / `path` | `./scripts/acct.sh list` |
| `dashboard.sh` | 起网关 + 开看板；`--stop` 停掉自己起的实例 | `PORT=8080 ./scripts/dashboard.sh` |

两个刻意的设计：

- **不在 bash 里重新实现 HTTP**。签到、状态、换机都调 `go run ./cmd/activity`，
  脚本只做二进制新鲜度判断和输出美化——省得同一套签名逻辑维护两份。
- **`acct.sh` 的 uid → scope 靠解 JWT 的 `sub`**。auth 文件里没有 `accountScope` 字段
  （装机身份文件名才有），早期脚本因此全部显示"电网身份 缺"。现在从 access token 的
  payload 里读 `sub`，不验签、只读 claim。

## 实测记录

```
$ ./scripts/credit.sh
账号                积分   今日签到  连签  引导任务    备注
------------------------------------------------------------------
phanthy-<ts>  3,592  已签          1  1500/1500   !
phanthy-<ts>  2,309  已签          2  1500/1500
------------------------------------------------------------------
汇总：2 个号 · 积分合计 5,901 · 未签到 0 · 异常 1
  异常：phanthy-<ts>: 活动服务临时不可用（503，不影响推理）

$ ./scripts/checkin.sh
! phanthy-<ts> 活动服务 503（稍后重试，不影响推理）
  ✓ phanthy-<ts> +0 分  连签 2 天  余额 2,309
汇总：total=2 ok=1 already=0 warn=1 fail=0 本次共 +0 分
```

`register` 的 `temporarily_unavailable` 是上游活动服务自身抖动，和凭证无关
（同一个号用来推理是正常的），脚本把它归到 `warn` 而不是 `fail`，退出码仍是 0。

## 坑

- 静态资源必须挂在 `"/ui/"`（带尾斜杠）上并 `http.StripPrefix("/ui", ...)`：
  挂 `"/ui"` 只匹配精确路径，`/ui/app.css` 会 404；`StripPrefix("/ui/")` 在
  `/ui/` 这个请求上会把路径切成空串，FileServer 会重定向成死循环。
- `go:embed web` 要求 `web/` 至少有一个文件，否则编译期就报 `pattern web: no matching files found`。
- JSON 字段名前后端必须对齐：写第一版时后端 tag 是 `completed_count`、前端读 `ob.done`，
  表格直接显示 `undefined/undefined`。前端不做类型检查，这类错只能靠打开页面看。
- 中文全角标点紧跟 `$var` 会被 bash 当成变量名的一部分（`$LOG：` → `LOG?: unbound variable`），
  脚本里统一写 `${LOG}。`。

## 账号身份：为什么显示手机号，为什么不会重复入池

池子里的"账号"由 `auths/phanthy-*.json` 决定，文件名是 uid。**uid 只是文件名，不是身份**——
真正的身份是 `/api/oauth/profile` 返回的 `account.uuid`（= JWT 的 `sub`，代码里叫 `accountScope`）。

早先登录用 `time.Now().Unix()` 当文件名（`phanthy-<ts>`），于是同一个手机号
重新登录就会多出一个文件，池子把同一个号算成两个：权重被摊薄、签到/邀请各跑一遍、
积分统计翻倍。这是真 bug，不是显示问题。

现在统一按 `accountScope` 收敛（`internal/auth/ident.go`）：

| 场景 | 行为 |
| --- | --- |
| 首次登录某号 | 新建 `phanthy-<scope>.json` |
| 同号再次登录（refresh 轮换、换机器） | **覆盖**已有文件，不新建（保留原文件名，装机身份/粘性对得上） |
| 磁盘上已有同 scope 的多份（历史遗留） | 登录时合并；服务启动时 `auth.DedupeDir()` 也会扫一遍，保留 **mtime 最新**（refresh_token 最新、最不可能失效）的那份，其余移进 `auths/.trash/` |

昵称（面板/表格里那一列）取 `profile.account.email`，上游只给掩码，所以显示成
`138****0000`；`account.nickname` 为空或是老的 `phanthy-1028` 时，`cmd/activity` 每次
status/claim 会顺手回填并落盘，所以老账号也会自愈。手机号完整值上游不给，拿不到也不用试。

看板渲染时还会**惰性回填**（`displayName`）：`account.nickname` 不是 `138****8888` 这种形态时
（老文件写的是 `phanthy-1028`，或者从别处拷过来缺字段），就顺手读一次 `/api/oauth/profile`
拿手机号掩码并写回文件，带 30 分钟内存缓存，不会因为看板轮询把 profile 打爆。
实测：把昵称改成 `phanthy-1028` 重启看板 → 页面显示 `138****0000`，文件也被改回来了。

`internal/auth/ident_test.go` 锁了这几种情况：同 scope 覆盖、遗留重复合并（唯一保留最新）、
scope 为空时不误合并、`DedupeDir` 幂等且不误伤别的号。
