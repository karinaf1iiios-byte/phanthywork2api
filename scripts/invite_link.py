#!/usr/bin/env python3
"""挑一个还能领邀请奖励的老号，产出「带码注册」链接，落到 data/invite_candidate.json。

为什么是链接而不是补填：上游的邀请关系只在**注册表单提交那一刻**建立。
注册页有 `<input name="inviteCode">`，从 `/r/<code>` 进来时会被预填；
服务端在"新手机号验证注册"的瞬间写入关系并发前两档 5000 / 8000。
账号创建完成之后，`POST /referrals/bind` 永远 409 `existing_user_ineligible`
（"Only a new verified phone account can bind."）——跟码对不对无关，只跟"是不是新号"有关。
所以正确流程是：**先拿链接 → 新手机号从链接进注册页 → 关系在注册时建立**。

规则（沿用既有约定）：一个邀请人最多领前两档，第 3 档起要人工审核，所以只用到 2。

用法：
    python3 scripts/invite_link.py             # 选人 + 生成/复用链接，写 data/invite_candidate.json
    python3 scripts/invite_link.py --json      # 只输出 JSON（不写文件）
    python3 scripts/invite_link.py --who <uid> # 指定邀请人（scope/uid 都认）
"""
import argparse
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import claim_daily as C          # noqa: E402
import invite_flow as IF         # noqa: E402

OUT = os.path.join(os.path.dirname(C.AUTH_DIR), "data", "invite_candidate.json")


def account_paths():
    d = C.AUTH_DIR
    if not os.path.isdir(d):
        return []
    return sorted(
        (os.path.join(d, f) for f in os.listdir(d)
         if f.startswith("phanthy-") and f.endswith(".json")),
        key=lambda p: os.path.getmtime(p),
    )


def normalize(doc):
    """兼容历史 schema（invite_flow.py 早期写的是 inviterBudgetLeft / shareUrl 等驼峰键）。"""
    if not doc:
        return None
    out = {
        "at": doc.get("at") or doc.get("createdAt") or "",
        "inviter": doc.get("inviter") or "",
        "phone": doc.get("phone") or "",
        "tier": int(doc.get("tier") if doc.get("tier") is not None else (doc.get("inviterTier") or 0)),
        "budget": int(doc.get("budget") if doc.get("budget") is not None
                      else (doc.get("inviterBudgetLeft") or 0)),
        "next_reward": int(doc.get("next_reward") if doc.get("next_reward") is not None
                           else (doc.get("inviterNextReward") or 0)),
        "invite_code": doc.get("invite_code") or doc.get("inviteCode") or "",
        "share_url": doc.get("share_url") or doc.get("shareUrl") or "",
        "register_url": doc.get("register_url") or doc.get("registerUrl") or "",
        "note": doc.get("note") or doc.get("hint") or "关系只在注册表单建立；已建好的号无法补填",
    }
    if not out["share_url"] and not out["register_url"]:
        return None
    return out


def load_candidate():
    try:
        with open(OUT, encoding="utf-8") as f:
            return normalize(json.load(f))
    except (OSError, ValueError):
        return None


def save(doc):
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    tmp = OUT + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=2, ensure_ascii=False)
    os.chmod(tmp, 0o600)
    os.replace(tmp, OUT)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", action="store_true", help="只输出 JSON，不写文件")
    ap.add_argument("--who", default="", help="指定邀请人 scope/uid")
    a = ap.parse_args()

    recs = [IF.survey(p) for p in account_paths()]
    ok = [r for r in recs if not r["error"]]
    failed = [r for r in recs if r["error"]]
    for r in failed:
        print(f"[skip] {os.path.basename(r['auth'])}: {r['error']}", file=sys.stderr)

    rec = IF.pick_inviter(ok, forced=(a.who or None))
    if not rec:
        print("没有可用邀请人：前两档已用满，或没有 eligible_to_invite 的号。", file=sys.stderr)
        return 3

    try:
        code, share = IF.ensure_code(rec["auth"], rec)
    except (RuntimeError, SystemExit) as e:
        print(f"生成邀请码失败: {e}", file=sys.stderr)
        return 4

    doc = {
        "at": time.strftime("%Y-%m-%dT%H:%M:%S"),
        "inviter": rec["scope"],
        "phone": "",
        "tier": int(rec.get("tier") or 0),
        "budget": IF.budget_left(rec),
        "next_reward": int(rec.get("next") or 0),
        "invite_code": code,
        "share_url": share,
        "note": "关系只在注册表单建立；已建好的号无法补填",
    }
    if doc.get("register_url") is None:
        doc["register_url"] = ""
    # 手机号掩码只用于人读
    try:
        st, prof = C.http("GET", "/api/oauth/profile",
                          {"Authorization": "Bearer " + C.access_token(C.load_auth(rec["auth"]), rec["auth"])})
        if st == 200:
            acc = prof.get("account") or {}
            doc["phone"] = (acc.get("email") or "").replace("+86", "").replace(" ", "")
    except Exception:                                # noqa: BLE001
        pass

    if a.json:
        print(json.dumps(doc, ensure_ascii=False))
        return 0

    save(doc)
    print(f"邀请人 {doc['phone'] or doc['inviter']} · 已用 {doc['tier']}/2 档 · 下一档 +{doc['next_reward']}")
    print(f"带码注册链接（新手机号必须从这里进入注册页）：\n  {doc['share_url']}")
    print(f"已写入 {OUT}（0600）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
