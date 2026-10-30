# 07 定时任务：统一调度、互不重叠、签到不漏

## 结论先行

所有定时任务只有一个入口 `scripts/pool.sh`，由 macOS 原生 launchd 叫醒；
所有子命令共用一把目录锁 `data/run.lock`，所以任何两个任务都不可能同时跑。
签到固定 09:00 与 21:00 各一趟（接口幂等，任意一趟成功即可），漏一趟不会丢分。

```
08:50  invite  09:00  claim   09:30  verify   09:40  sweep
20:50  invite  21:00  claim   21:30  verify   21:40  sweep
```

## 为什么这样排

| 时间 | 子命令 | 做什么 | 花额度 |
|---|---|---|---|
| 09:00 / 21:00 | `claim` | 全池领每日签到（真实签名调用） | 不花，只加分 |
| 09:30 / 21:30 | `verify` | 只读回读档位/邀请额度，写 `data/pool_status.json` | 不花 |
| 08:50 / 20:50 | `invite` | 挑「前两档没用满」的邀请人，产出带码注册链接（`data/invite_candidate.json`，0600） | 0 |
| 09:40 / 21:40 | `sweep --apply` | 只扫 onboarding 进度，给没满 1500 的号补做（邀请补填已证伪，不再尝试） | 约 5 次短请求/账号 |

**邀请没有"补填"这一步**：上游只在注册表单提交时建立关系，已建好的账号
`POST /referrals/bind` 永久 409 `existing_user_ineligible`（"Only a new verified phone account
can bind."）。所以 `bind` / `bindwatch` 已删除，改为 `invite` 产出带码注册链接，
新号必须从链接进注册页。详见 `docs/06-referral.md` 的「定论」一节。
2026-10-28 又补了一次实测：注册 **2 天**、onboarding 已满 1500 的号去补填，仍然 409 ——
所以「新」的判据不是注册时长，而是**这个号还没产生过完成态用量**。结论不变、要求更硬：
新号入库后必须**先补填、再做任何工作**，`onboard_new.py` 会跳过「近 3 天注册、邀请还没补填」
的账号（`--force` 可绕过）。

## 资格扫描：scripts/sweep.py

一次性任务（onboarding 1500、邀请补填）不能靠"每天刷一遍所有号"来保证——它们有窗口、
有前置条件，而且做错顺序就永久失去资格。`sweep.py` 把"谁有资格、为什么"显式打出来：

```bash
python3 scripts/sweep.py            # 只扫描（不花积分、不提交任何东西）
python3 scripts/sweep.py --apply    # 扫描 + 给缺 onboarding 的号补做
python3 scripts/sweep.py --json      # 机器可读
./scripts/pool.sh sweep --apply      # 定时任务实际调用的入口（带目录锁）
```

输出示例（真实数据，2026-10-28）：

```
手机/昵称        scope                       注册   onboarding  邀请      动作    说明
138****0000    acct_5f3d2c1b0a9e8d7c6b5a4f3e   2天   1500/1500    0/2档 余2    -
138****0000    acct_1a2b3c4d5e6f7a8b9c0d1e2f   75天  1500/1500   未绑定    -       已完成（无需动作）
```

判定规则：

| 动作 | 触发条件 | 说明 |
| --- | --- | --- |
| `onboard` | `earned < 1500` 或还有 pending 任务 | 真用量触发，一次性的 1500 分 |
| `onboard` | `onboarding.earned_points < max_points` 或还有 pending 任务 | 真实推理，约 5 次短请求/账号 |
| 无动作 | 已满 1500 且已绑定 / 老号 409 | 表里给出原因，不重复尝试 |

邀请关系不在这张表里判定：它只能在注册表单提交时建立，事后无论如何都是 409，
所以扫描表只显示「可邀请几档」和当前带码注册链接。

- **签到两趟是为了不漏**：服务端按自然日计，第二趟落在同一自然日时返回 `already_granted`，无副作用。
  Mac 休眠错过 09:00 时，launchd 会在唤醒后补跑一次日历任务。
- **onboarding 只补 pending**：5 个任务是一次性的（`onboarding.tasks[].status=completed` 后不再给分），
  所以不是每天对全池跑，而是每次先看服务端进度，已满 1500/1500 的账号只做一次 summary 就跳过。
  新号进池后，最迟下一趟（10:00 或 22:00）会被自动补满。
- **不重叠**：即使某个任务跑久（比如 20 个账号 × 20s 间隔），21:00 那趟拿不到锁会直接退出 0 并记日志，
  不会出现两个 `claim` 同时打同一批账号。下一趟 21:00/次日 09:00 会补上。

## 装 / 拆 / 查

```bash
./scripts/install_schedule.sh            # 干跑：只打印 6 个 launchd 日历任务
./scripts/install_schedule.sh --install   # 写入 ~/Library/LaunchAgents 并加载
./scripts/install_schedule.sh --status    # 每个任务 loaded / missing
./scripts/install_schedule.sh --uninstall # 全部卸载
launchctl kickstart -k gui/$(id -u)/com.phanthywork.pool.verify9   # 手动触发一次（实测可用）
```

装完后的 job 标签：`com.phanthywork.pool.{invite8,claim9,verify9,sweep9,invite20,claim21,verify21,sweep21}`。

`sweep` 取代了原来的 `bind`/`onboard` 两个日历任务（`bind` 后来被证伪、整个删除）：

1. **先判定、再执行**。原来看不出「谁有资格、为什么没跑」；现在 `scripts/sweep.py` 输出一张表
   （手机掩码 / scope / 注册天数 / onboarding 进度 / 邀请绑定状态 / 待执行动作 / 原因），
   `--apply` 才真的动手，顺序固定为「先补邀请、再补 onboarding」。
2. **状态可见**。9:40/21:40 的 sweep 结果会落进 `logs/pool-*.log` 和看板的任务卡，
   谁还没满 1500 一眼能看到。

`invite` 只读 + 必要时生成一次邀请码，成本可忽略；`data/invite_candidate.json` 不存在时
看板会现算一次（不写盘），所以新部署不需要先跑任务才能看到链接。

## 手动用法（同一把锁，手动跑也不会撞上定时任务）

```bash
./scripts/pool.sh claim                 # 领签到（幂等）
./scripts/pool.sh verify                # 回读状态
./scripts/pool.sh invite                # 产出带码注册链接（新号必须从链接注册）
./scripts/pool.sh onboard --plan        # 只看谁还差 5 个任务，不发推理不花积分
./scripts/pool.sh onboard               # 给 pending 账号补做
./scripts/pool.sh invite                # 随机选邀请人，生成注册链接 → data/invite_candidate.json
```

额外参数会透传，例如 `./scripts/pool.sh claim -delay 30s`。

## 文件约定

| 路径 | 内容 | 权限 |
|---|---|---|
| `data/run.lock/` | 互斥锁（目录 + `pid`），进程死亡后下次自动回收 | - |
| `data/pool_status.json` | 上次 verify 的全池快照（含邀请档位） | 0600 |
| `data/invite_candidate.json` | 上次 invite 选中的邀请人 + 邀请码 + 注册链接 | 0600 |

| `logs/pool-<cmd>-<ts>.log` | 每次运行的完整输出，30 天自动清理 | - |
| `logs/launchd-*.log` | launchd 捕获的 stdout/stderr | - |

凭据、邀请码、`referral_state`、登录链接一律不进日志正文：`invite` 只把「邀请人 + 档位 + 下一档分数」写日志，
完整链接落在 0600 的 `data/invite_candidate.json`。

## 与网关内建调度的关系

`internal/server` 里有一套 `claim_hours` 定时器（`config.go` / `activities.go`），和 launchd 调度是两套。
**二选一，别同时开**：`config.example.json` 已把 `claim_enabled` 设为 `false`，交给 `scripts/pool.sh`。
如果要改回网关内建调度，就把 `claim_enabled` 设 `true` 并卸掉 launchd 任务（`--uninstall`），否则两边会重复领。

## 坑

- macOS 没有 `flock`，互斥用 `mkdir`（原子）+ `pid` 存活判断；残留锁会在下一次运行时自动回收。
- 中文全角字符紧跟在 `$var` 后面会被 bash 当成变量名的一部分（`$pid）` → `pid?: unbound variable`），
  变量统一写成 `${pid}`。
- launchd 的 PATH 很短，`pool.sh` 里显式把 `/Users/lwj/.local/go/bin` 放到最前面，否则 `go run` 找不到。（需要 go 1.26+：协议层依赖 new-api relaykit/tokenkit）
- `launchctl load` 已废弃，用 `bootstrap gui/<uid> <plist>` / `bootout gui/<uid>/<label>`。
- 改时间只需要改 `install_schedule.sh` 的 `JOBS` 数组再 `--install`（会先 bootout 再 bootstrap）。

## 主动续期（keepalive）：旧实现有个真 bug，已修

**旧实现**（2026-10-28 之前）：

```go
for {
    time.Sleep(time.Hour)                                  // ① 先睡一小时
    for _, a := range s.auths {
        if !a.NeedsRefresh(5 * time.Minute) { continue }    // ② 阈值 5 分钟
        ...refresh...
    }
}
```

① 和 ② 的组合是矛盾的：**检查间隔 1 小时，判断窗口只有 5 分钟**，而 token 寿命 30 分钟。
空闲账号必然在两次 tick 之间过期，最长要烂 55 分钟才被下一次 tick 捞起来。

实测（把号 `phanthy-<ts>` 的 `expiresAt` 改成 now+120s，起服务后什么都不做）：

```
06:40:50  服务启动（expiresAt = 06:42:50）
06:45:00  expiresAt 已过期 74 秒，日志里没有任何 keepalive/refresh 记录，auth 文件 mtime 仍是 06:40:50
```

另一条更要命的路径：**推理请求自己不续期**。`desktopFor()` 直接拿 `a.AccessToken` 去调 summary，
token 过期就 401 → 换号 → 全池都过期就 502。空闲池子被叫起来干活时，正好撞在这个窗口上。

**修法**（`internal/server/handler.go`）：

| 项 | 旧 | 新 |
|---|---|---|
| 检查间隔 | 1 小时（硬编码） | 60 秒（`schedule.keepalive_interval_seconds`） |
| 续期阈值 | 5 分钟 | 10 分钟（`schedule.keepalive_threshold_seconds`） |
| 首轮时机 | 启动后 1 小时 | 启动立即扫一遍 |
| 并发保护 | 无 | 每号一把 `RefreshMu` 单飞锁（refresh_token 会轮换，多刷会把号刷废） |
| 推理前 | 不检查 | `desktopFor()` 里先 `refreshIfNeeded(2min)` |

约束：**阈值必须大于间隔（至少 3 倍）**，否则又会出现"两次检查之间错过窗口"。
`TestKeepaliveDefaultsSane` 把这个约束写成了测试，改配置改错了会直接红。

修完的实测（同样把 `expiresAt` 改成 now+120s 再起服务）：

```
06:53:27  [keepalive] 启动：每 1m0s 检查，剩余寿命 <= 10m0s 即续期（阈值必须大于间隔）
06:53:27  [refresh] phanthy-<ts> 已续期，剩余 30m0s
```

启动那一刻就续上了，auth 文件 `expiresAt` 变成 now+30min。

## 两条调度路线：进程内（推荐）与 launchd

同一批任务有两种装法，**不要同时开**（会互相抢，虽然 `pool.sh` 有目录锁也不会跑两遍，
但日志里会出现两套时间戳，排查时容易误导）：

| | 进程内 `config.schedule.jobs` | launchd `install_schedule.sh --install` |
| --- | --- | --- |
| 依赖 | 网关进程活着 | macOS + 本机登录会话 |
| 看板可见 | 有（计划/下次/上次/耗时/结果/输出尾巴 + 立即运行） | 只有日志文件 |
| 换机器 | 拷 config 就行 | 要重新 install |
| 适合 | 常开的小主机、容器、长期部署 | 只在 Mac 上、且不想让网关常驻 |

**推荐进程内**：看板上能直接看到「上次跑没跑、结果如何、输出了什么」，
不用再靠翻 logs/ 猜。

### 关于 go 路径

`scripts/pool.sh` 以前写死 `export PATH="/Users/lwj/.local/go/bin:$PATH"`，
在 Linux 容器里会打出 `/app/scripts/pool.sh: line 70: /Users/lwj/.local/go/bin/go: No such file or directory`
（有 `command -v go` 兜底，所以 rc 仍是 0，但错误日志很难看）。现在改成先探测
`$HOME/.local/go/bin`、`/usr/local/go/bin`、`/usr/lib/go/bin`，只在 `claim`/`verify`
真正需要 go 时检查；找不到就明确报「装 Go 或加 PATH」并 exit 127，
`sweep`/`invite`/`onboard` 只用 python，不会被 go 拦住。

### config 里怎么开

```json
"schedule": {
  "jobs": [
    {"name": "invite", "cmd": "invite",        "hours": [8, 20], "minute": 50, "enabled": true},
    {"name": "claim",  "cmd": "claim",         "hours": [9, 21], "minute": 0,  "enabled": true},
    {"name": "verify", "cmd": "verify",        "hours": [9, 21], "minute": 30, "enabled": true},
    {"name": "sweep",  "cmd": "sweep --apply", "hours": [9, 21], "minute": 40, "enabled": true}
  ],
  "keepalive_enabled": true,
  "keepalive_interval_seconds": 60,
  "keepalive_threshold_seconds": 600
}
```

`cmd` 的取值就是 `scripts/pool.sh` 的子命令（`claim` / `verify` / `sweep --apply` /
`invite` / `onboard`）；只有 `claim` 是走进程内原生调用（不 exec python），其余都是
exec `scripts/pool.sh`，协议逻辑仍然只有一份实现。

运行规则（`internal/server/jobs.go`）：

- 每 20 秒扫一次时钟，命中「今天该跑的时刻且还没跑过」就执行一次，用
  `job|YYYY-MM-DDTHH:MM` 去重，所以不会同一次计划跑两遍；
- **错过会补跑**：如果到 23:30 还没跑过 21:40 那次（Mac 睡了/进程重启了），起来就补一次；
- 同一时刻只允许一个任务在跑（进程内 `busy` + `pool.sh` 的 `data/run.lock` 双保险）；
- 结果写 `data/jobs.json`（0600），重启后看板仍能看到上次运行时间和输出尾巴。

老字段 `claim_enabled` / `claim_hours` / `onboarding_enabled` / `onboarding_hours` 只在
`jobs` 为空时才生效（向后兼容）。`onboarding_*` 从来没有被任何代码读取过 —— 之前
"以为配了其实没跑"就是这么来的，现在由 `sweep` 任务覆盖。
