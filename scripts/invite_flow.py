#!/usr/bin/env python3
"""邀请关系自动化：新账号入库 → 挑一个老号 → 拿老号的邀请码 → 新账号自己补填。

正确顺序（服务端认的流程）：
  1. 新账号先正常注册、正常入库（登录流程落 auths/phanthy-*.json）；
  2. 在池子里找邀请人：默认「注册时间最早、前两档还没满」的老号（--random 改成随机）；
  3. 取老号的邀请码 + 分享页里的 referral_state；
  4. **用新账号自己的 token 和它自己的装机签名** POST /api/oauth/referrals/bind，body {"state":"rs1.…"}；
  5. 回读：老号 referrals/me 的 tier_granted_count +1（5,000 / 8,000），relations 里出现新账号。

老账号不能补填：服务端 409 existing_user_ineligible（"Only a new verified phone account can
bind."），「新」是账号侧属性，不是调用时机。脚本把 409 原文打出来，不重试、不绕过。

用法：
  python3 scripts/invite_flow.py --bindee <新账号scope>             # 主流程：新账号补填
  python3 scripts/invite_flow.py --bindee <scope> --inviter <老号>   # 指定邀请人
  python3 scripts/invite_flow.py --auto --max-age-days 3            # 近 3 天注册的账号自动补填
  python3 scripts/invite_flow.py --verify                           # 只读：全池档位表
  python3 scripts/invite_flow.py --pick                             # 备选：生成「注册时带码」的链接
"""
import argparse
import contextlib
import io
import json
import os
import random
import re
import sys
import time
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import claim_daily as cd

SHARE_BASE = "https://code.phanthy.com"


@contextlib.contextmanager
def quiet():
    """claim_daily 的 helper 会往 stdout 打 [identity] loaded；输出要干净（尤其 --json）。"""
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        yield buf


# ---------------------------------------------------------------- 采集
def accounts(auth_dir=None):
    d = auth_dir or cd.AUTH_DIR
    return sorted(
        (os.path.join(d, f) for f in os.listdir(d)
         if f.startswith("phanthy-") and f.endswith(".json")),
        key=os.path.getmtime)


def identity_path(scope):
    return os.path.join(cd.AUTH_DIR, f"installation-{scope}.json")


def identity(scope):
    p = identity_path(scope)
    if not os.path.exists(p):
        return {}
    with open(p, encoding="utf-8") as f:
        return json.load(f)


def patch_identity(scope, **fields):
    p = identity_path(scope)
    if not os.path.exists(p):
        return
    with open(p, encoding="utf-8") as f:
        doc = json.load(f)
    doc.update(fields)
    tmp = p + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=2)
    os.chmod(tmp, 0o600)
    os.replace(tmp, p)


def survey(auth_path):
    """单个账号：token → profile → 装机身份 → referrals/me。失败带 error。"""
    out = {"auth": auth_path, "uid": "", "scope": "", "tier": 0, "eligible": False, "next": 0,
           "total": 0, "relations": 0, "inviteCode": "", "shareUrl": "", "createdAt": "",
           "boundInviter": "", "bindHttp": 0, "error": ""}
    try:
        doc = cd.load_auth(auth_path)
        token = cd.access_token(doc, auth_path)
        st, prof = cd.http("GET", "/api/oauth/profile", {"Authorization": "Bearer " + token})
        if st != 200:
            out["error"] = f"profile http {st}"
            return out
        acct = prof["account"]
        uid = acct["uuid"]
        out["uid"] = uid
        out["scope"] = uid
        out["createdAt"] = acct.get("created_at") or acct.get("createdAt") or ""
        _, ident = cd.load_or_create_identity(uid)   # register 503 不影响只读
        st, me = cd.signed_call(token, ident, "GET", "/referrals/me", None)
        if st != 200:
            out["error"] = f"referrals/me http {st}: {str(me)[:120]}"
            return out
        r = me["referral"]
        out.update(tier=r.get("tier_granted_count", 0),
                   eligible=bool(r.get("eligible_to_invite")),
                   next=r.get("next_reward_points", 0),
                   total=r.get("total_granted", 0),
                   relations=len(r.get("recent_relations") or []))
        idoc = identity(uid)
        out["inviteCode"] = idoc.get("referralInviteCode", "")
        out["shareUrl"] = idoc.get("referralShareUrl", "")
        out["boundInviter"] = idoc.get("referralBoundInviter", "")
        out["bindHttp"] = int(idoc.get("referralBindHttp") or 0)
    except Exception as e:                             # noqa: BLE001 —— 单账号失败不中断全池
        out["error"] = f"{type(e).__name__}: {e}"
    return out


def budget_left(rec):
    return max(0, min(2, 2 - int(rec.get("tier", 0) or 0)))


def age_days(rec):
    """账号注册天数；拿不到 created_at 返回 9999（当老号，绝不误判成新号）。"""
    raw = rec.get("createdAt") or ""
    if not raw:
        return 9999.0
    s = raw.replace("Z", "")[:19]
    for fmt in ("%Y-%m-%dT%H:%M:%S", "%Y-%m-%d %H:%M:%S", "%Y-%m-%d"):
        try:
            return (time.time() - time.mktime(time.strptime(s[:len(fmt) + 4], fmt))) / 86400.0
        except ValueError:
            continue
    return 9999.0


def pick_inviter(recs, forced=None, mode="oldest"):
    """在「资格为真且前两档未满」里挑邀请人。默认老号（注册最早）；mode=random 随机。"""
    cands = [r for r in recs if not r["error"] and r["eligible"] and budget_left(r) > 0]
    if forced:
        for r in cands:
            if forced in (r["scope"], r["uid"]):
                return r
        return None
    if not cands:
        return None
    if mode == "random":
        return random.choice(cands)
    return sorted(cands, key=age_days, reverse=True)[0]


# ---------------------------------------------------------------- 邀请码 + state
def ensure_code(auth_path, rec):
    """确保邀请人有专属码；没有就现生成（POST /referrals/code 必须带幂等键）。"""
    if rec["inviteCode"] and rec["shareUrl"]:
        return rec["inviteCode"], rec["shareUrl"]
    doc = cd.load_auth(auth_path)
    token = cd.access_token(doc, auth_path)
    _, ident = cd.load_or_create_identity(rec["uid"])
    idem = "refcode-" + cd.b64url(os.urandom(12))
    st, cs = cd.signed_call(token, ident, "POST", "/referrals/code", "{}", idem)
    if st != 200:
        raise RuntimeError(f"referrals/code http {st}: {str(cs)[:160]}")
    ident["referralInviteCode"] = cs["invite_code"]
    ident["referralShareUrl"] = cs["share_url"]
    cd.save_identity(identity_path(rec["uid"]), ident)
    rec["inviteCode"], rec["shareUrl"] = cs["invite_code"], cs["share_url"]
    return rec["inviteCode"], rec["shareUrl"]


def states_from_share(share_url):
    """/r/<code> 页面里的 referral_state：返回 (registration_state, login_state)。"""
    req = urllib.request.Request(share_url, headers={"User-Agent": cd.UA})
    html = urllib.request.urlopen(req, timeout=25).read().decode("utf-8", "replace")
    pairs = re.findall(r'href="(/(?:register|login)\?[^"]*?referral_state=(rs1\.[^"&]+))', html)
    reg = next((s for path, s in pairs if path.startswith("/register")), "")
    login = next((s for path, s in pairs if path.startswith("/login")), "")
    return reg, login


# ---------------------------------------------------------------- 主流程：新账号补填
def submit_bind(bindee, state, dry=False):
    """用 bindee 自己的 token + 装机签名提交 {"state": ...}。返回 (status, body)。"""
    if dry:
        return 0, {"dry": True}
    with quiet():
        doc = cd.load_auth(bindee["auth"])
        token = cd.access_token(doc, bindee["auth"])
        _, ident = cd.load_or_create_identity(bindee["uid"])
    body = json.dumps({"state": state}, separators=(",", ":"))
    idem = "refbind-" + cd.b64url(os.urandom(12))
    return cd.signed_call(token, ident, "POST", "/referrals/bind", body, idem)


def do_bind(bindee_rec, inviter_rec, args, echo=True):
    with quiet():
        code, share_url = ensure_code(inviter_rec["auth"], inviter_rec)
    state_reg, _ = states_from_share(share_url)
    if not state_reg:
        return {"bindee": bindee_rec["scope"], "ok": False,
                "reason": "分享页里没找到 /register 的 referral_state（页面结构可能变了）"}

    if args.dry_run:
        if echo:
            print(f"[dry-run] 新账号 {bindee_rec['scope']} ← 邀请人 {inviter_rec['scope']}"
                  f"（码 {code}，档位 {inviter_rec['tier']}/2）→ 会提交 POST /referrals/bind")
        return {"bindee": bindee_rec["scope"], "inviter": inviter_rec["scope"],
                "inviteCode": code, "ok": False, "reason": "dry-run：未提交"}

    st, resp = submit_bind(bindee_rec, state_reg)
    out = {"bindee": bindee_rec["scope"], "inviter": inviter_rec["scope"],
           "inviteCode": code, "ok": st == 200, "http": st}
    if st == 200:
        with quiet():
            after = survey(inviter_rec["auth"])
        out.update(inviterTierAfter=after["tier"], inviterTotalAfter=after["total"],
                   inviterRelationsAfter=after["relations"])
        patch_identity(bindee_rec["scope"], referralBoundInviter=inviter_rec["scope"],
                       referralBoundAt=int(time.time()), referralBindHttp=200)
        patch_identity(inviter_rec["scope"], referralGrantedCount=after["tier"])
    else:
        err = (resp or {}).get("error", {}) if isinstance(resp, dict) else {}
        out.update(error=err.get("code") or str(resp)[:120], message=err.get("message", ""))
        patch_identity(bindee_rec["scope"], referralBindHttp=st,
                       referralBindError=f"{err.get('code')}: {err.get('message','')}"[:200],
                       referralBindAt=int(time.time()))
    if echo:
        print(f"新账号 {out['bindee']}  ←  邀请人 {out['inviter']}（码 {code}）")
        if out["ok"]:
            print(f"绑定成功：邀请人档位 {inviter_rec['tier']} → {out['inviterTierAfter']}，"
                  f"累计 {out['inviterTotalAfter']} 分，关系数 {out['inviterRelationsAfter']}")
        else:
            print(f"绑定失败 http {st} {out.get('error','')} {out.get('message','')}")
    return out


# ---------------------------------------------------------------- 到账确认
def watch(inviter_scopes, seconds):
    deadline = time.time() + seconds
    base = {}
    for p in accounts():
        r = survey(p)
        if r["scope"] in inviter_scopes:
            base[r["scope"]] = (r["tier"], r["total"])
    while time.time() < deadline:
        for p in accounts():
            r = survey(p)
            b = base.get(r["scope"])
            if b and r["tier"] > b[0]:
                print(f"[watch] {r['scope']} 到账：tier {b[0]} → {r['tier']}，累计 {r['total']}")
                return r
        time.sleep(20)
    print(f"[watch] {seconds}s 内没有档位变化")
    return None


# ---------------------------------------------------------------- main
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bindee", help="主流程：新账号 scope（由它去补填邀请码）")
    ap.add_argument("--auto", action="store_true", help="对池里「近期注册且没绑定过」的账号自动补填")
    ap.add_argument("--max-age-days", type=float, default=3.0, help="--auto 的注册时间窗口（天）")
    ap.add_argument("--inviter", help="指定邀请人（默认注册最早且前两档未满的老号）")
    ap.add_argument("--random", action="store_true", help="邀请人随机挑，而不是老号优先")
    ap.add_argument("--pick", action="store_true", help="备选：生成「注册时带码」的链接")
    ap.add_argument("--verify", action="store_true", help="只读：打印全池邀请档位")
    ap.add_argument("--open", action="store_true", help="--pick 时用浏览器打开注册链接")
    ap.add_argument("--watch", type=int, default=0, help="绑定后轮询到账秒数")
    ap.add_argument("--retry", action="store_true", help="--auto 时连之前被拒过的也重试")
    ap.add_argument("--dry-run", action="store_true", help="只算不提交")
    ap.add_argument("--auths", help="凭证目录（默认仓库 auths/）")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args()

    if args.auths:
        cd.AUTH_DIR = os.path.abspath(args.auths)

    with quiet():
        recs = [survey(p) for p in accounts()]
    ok = [r for r in recs if not r["error"]]

    # --- 只读
    if args.verify or not (args.bindee or args.auto or args.pick):
        rows = sorted(ok, key=lambda r: -age_days(r))
        print(f"{'account':<28}{'age':<7}{'tier':<6}{'left':<6}{'next':<7}{'total':<7}{'rel':<5}"
              f"{'bound':<28}code")
        for r in rows:
            age = f"{age_days(r):.0f}d" if age_days(r) < 9999 else "?"
            print(f"{r['scope']:<28}{age:<7}{r['tier']:<6}{budget_left(r):<6}{r['next']:<7}"
                  f"{r['total']:<7}{r['relations']:<5}{r['boundInviter'] or '-':<28}"
                  f"{r['inviteCode'] or '-'}")
        for r in recs:
            if r["error"]:
                print(f"[skip] {r['auth'].split('/')[-1]}: {r['error']}", file=sys.stderr)
        if args.json:
            print(json.dumps({"accounts": recs}, ensure_ascii=False))
        return 0

    # --- 主流程：新账号补填
    if args.bindee or args.auto:
        if args.bindee:
            bindees = [r for r in ok if args.bindee in (r["scope"], r["uid"])]
            if not bindees:
                print(f"找不到账号 {args.bindee}", file=sys.stderr)
                return 1
            if bindees[0]["boundInviter"]:
                print(f"{bindees[0]['scope']} 已绑定邀请人 {bindees[0]['boundInviter']}，跳过")
                return 0
        else:
            # 已经被服务端拒过（如 409 existing_user_ineligible）的不再自动重试，除非 --retry
            bindees = [r for r in ok if age_days(r) <= args.max_age_days
                       and not r["boundInviter"] and (args.retry or not r["bindHttp"])]
            if not bindees:
                print(f"没有需要补填的账号（近 {args.max_age_days:g} 天注册且未绑定：0 个）")
                return 0
            bindees.sort(key=age_days)

        results = []
        for b in bindees:
            inviter = pick_inviter([r for r in ok if r["scope"] != b["scope"]],
                                   args.inviter, "random" if args.random else "oldest")
            if inviter is None:
                results.append({"bindee": b["scope"], "ok": False,
                                "reason": "没有可用邀请人（前两档都满了）"})
                continue
            results.append(do_bind(b, inviter, args, echo=not args.json))
        if args.json:
            print(json.dumps({"results": results}, ensure_ascii=False))
        if args.watch > 0:
            watch({r["inviter"] for r in results if r.get("ok") or args.dry_run}, args.watch)
        bad = [r for r in results if not r["ok"] and r.get("reason") != "dry-run：未提交"]
        return 0 if not bad else 1

    # --- 备选：生成「注册时带码」的链接
    cand = pick_inviter(ok, args.inviter, "random" if args.random else "oldest")
    if cand is None:
        print("没有可用邀请人：全池前两档（5,000 / 8,000）都已到账或不可邀请")
        return 1
    try:
        with quiet():
            invite_code, share_url = ensure_code(cand["auth"], cand)
        state_reg, state_login = states_from_share(share_url)
    except Exception as e:                             # noqa: BLE001
        print(f"[error] {e}", file=sys.stderr)
        return 1
    if not state_reg:
        print("[error] 分享页里没找到 /register 的 referral_state", file=sys.stderr)
        return 1
    reg_url = f"{SHARE_BASE}/register?next=%2Faccount%2Freferrals&referral_state={state_reg}"
    login_url = (f"{SHARE_BASE}/login?next=%2Faccount%2Freferrals&referral_state={state_login}"
                 if state_login else "")
    payload = {"inviter": cand["scope"], "inviterTier": cand["tier"],
               "inviterBudgetLeft": budget_left(cand), "inviterNextReward": cand["next"],
               "inviteCode": invite_code, "shareUrl": share_url,
               "registerUrl": reg_url, "loginUrl": login_url}
    if args.json:
        print(json.dumps(payload, ensure_ascii=False))
    else:
        print(f"邀请人      {cand['scope']}（档位 {cand['tier']}/2，下一档 {cand['next']} 分）")
        print(f"邀请码      {invite_code}")
        print(f"分享页      {share_url}")
        print(f"注册链接    {reg_url}")
        if login_url:
            print(f"备用登录页  {login_url}")
    if args.open:
        os.system(f'open "{reg_url}"')
    return 0


if __name__ == "__main__":
    sys.exit(main())
