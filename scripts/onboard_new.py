#!/usr/bin/env python3
"""新号入场助手：只给「onboarding 还没满 1500」的账号跑那 5 个一次性任务。

为什么单独一个脚本：
  - 5 个任务是一次性的（服务端 onboarding.tasks 一旦 completed 就不会再给分），
    所以不该每天对所有账号跑，而是「某账号首次进池、且进度不满」才补一次；
  - 只在真实推理下结算：脚本发的是真实 /v1/messages 请求，再让 reconcile 按服务端证据结算，
    不构造 usage、不改 billing（onboard_driver 的既有原则）。

用法：
  python3 scripts/onboard_new.py --plan     # 只列「谁会跑、差哪几项」，不发推理不花积分
  python3 scripts/onboard_new.py            # 给所有 pending 账号补做（幂等：已完成项跳过）
  python3 scripts/onboard_new.py --only acct_1a2b3c4d5e6f7a8b9c0d1e2f
  python3 scripts/onboard_new.py --skip acct_1a2b3c4d5e6f7a8b9c0d1e2f
  python3 scripts/onboard_new.py --force      # 连「等补填邀请」的新号也一起做（默认不动它们）
"""
import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import claim_daily as C
import inference as I
import onboard_driver as OD


def accounts():
    d = C.AUTH_DIR
    return sorted((os.path.join(d, f) for f in os.listdir(d)
                   if f.startswith("phanthy-") and f.endswith(".json")), key=os.path.getmtime)


def identity_path(scope):
    return os.path.join(C.AUTH_DIR, f"installation-{scope}.json")


# 说明：这里原本有个「等补填邀请码、先别做 onboarding」的保护，2026-10-28 已删除。
# 邀请关系只在注册表单提交那一刻建立，账号建好之后 POST /referrals/bind 永久 409，
# 所以「等补填」等于永远等下去（老版本会白跳过新号 3 天，把 1500 分一直拖到过期窗口外）。


def age_days(doc):
    """账号注册天数（profile.account.created_at；summary 里没有这个字段）。拿不到当老号。"""
    try:
        st, prof = C.http("GET", "/api/oauth/profile", {"Authorization": "Bearer " + doc.token})
        raw = (prof.get("account") or {}).get("created_at") or ""
    except Exception:                                  # noqa: BLE001
        raw = ""
    if not raw:
        return 9999.0
    s = raw.replace("Z", "")[:19]
    for fmt in ("%Y-%m-%dT%H:%M:%S", "%Y-%m-%d %H:%M:%S", "%Y-%m-%d"):
        try:
            return (time.time() - time.mktime(time.strptime(s[:len(fmt) + 4], fmt))) / 86400.0
        except ValueError:
            continue
    return 9999.0


def earned_peek(doc):
    ob = (doc.summary or {}).get("onboarding") or {}
    return int(ob.get("earned_points") or 0)


def progress(doc):
    """返回 (earned, max, pending_task_keys)。onboarding 缺字段时按 0/1500 处理。"""
    ob = (doc.summary or {}).get("onboarding") or {}
    earned = int(ob.get("earned_points") or 0)
    mx = int(ob.get("max_points") or 1500)
    pending = [t["task_key"] for t in ob.get("tasks") or [] if t.get("status") != "completed"]
    return earned, mx, pending, ob


def mark(scope, earned, mx, pending):
    p = identity_path(scope)
    if not os.path.exists(p):
        return
    doc = json.load(open(p, encoding="utf-8"))
    doc["onboardingEarned"] = earned
    doc["onboardingMax"] = mx
    doc["onboardingPending"] = pending
    doc["onboardingCheckedAt"] = int(time.time())
    if earned >= mx and not pending:
        doc.setdefault("onboardingCompletedAt", int(time.time()))
    tmp = p + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=2)
    os.chmod(tmp, 0o600)
    os.replace(tmp, p)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--plan", action="store_true", help="只列计划，不跑任务")
    ap.add_argument("--only", help="只处理这个 accountScope")
    ap.add_argument("--skip", action="append", default=[], help="跳过这个 accountScope（可多次）")
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()

    rows = []
    for path in accounts():
        try:
            doc = I.Desktop(auth_path=path)          # 内部会 register + summary（register 重复报 503 不影响）
            scope = doc.ident.get("accountScope", "")
            if a.only and scope != a.only:
                continue
            if scope in a.skip:
                rows.append({"scope": scope, "status": "skipped-by-flag"})
                continue
            earned, mx, pending, _ = progress(doc)
            mark(scope, earned, mx, pending)
            if earned >= mx and not pending:
                rows.append({"scope": scope, "status": "already-full", "earned": earned, "max": mx})
                continue
            if a.plan:
                rows.append({"scope": scope, "status": "pending", "earned": earned,
                             "max": mx, "tasks": pending})
                continue
            print(f"=== {scope} {earned}/{mx} 缺 {len(pending)} 项：{','.join(pending)}")
            OD.run_tasks(doc, {t["task_key"]: t.get("status") for t in
                               ((doc.summary or {}).get("onboarding") or {}).get("tasks") or []})
            earned2, mx2, pending2, _ = progress(I.Desktop(auth_path=path))
            mark(scope, earned2, mx2, pending2)
            rows.append({"scope": scope, "status": "ran", "earned": earned2, "max": mx2,
                         "pending": pending2})
        except SystemExit as e:
            rows.append({"auth": os.path.basename(path), "status": "error", "error": str(e)})
        except Exception as e:                        # noqa: BLE001
            rows.append({"auth": os.path.basename(path), "status": "error",
                         "error": f"{type(e).__name__}: {e}"})

    if a.json:
        print(json.dumps({"accounts": rows}, ensure_ascii=False))
        return 0
    for r in rows:
        name = r.get("scope") or r.get("auth", "?")
        if r["status"] == "pending":
            print(f"{name:<28} 待补 {r['earned']}/{r['max']}  缺: {','.join(r['tasks']) or '-'}")
        elif r["status"] == "already-full":
            print(f"{name:<28} 已满 {r['earned']}/{r['max']}")
        elif r["status"] == "waiting-referral":
            print(f"{name:<28} 等补填邀请，跳过 onboarding")
        elif r["status"] == "ran":
            print(f"{name:<28} 跑完 {r['earned']}/{r['max']}  剩余: {','.join(r.get('pending') or []) or '-'}")
        else:
            print(f"{name:<28} {r['status']} {r.get('error','')}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
