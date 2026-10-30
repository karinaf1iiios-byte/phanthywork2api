#!/usr/bin/env python3
"""全池状态扫描：谁还缺 onboarding、邀请这一步到底该怎么做、下一步动作是什么。

**邀请补填这条路已经被证伪（2026-10-28 定论）**
上游的邀请关系只在「注册表单提交」那一刻建立：注册页有
`<input name="inviteCode">`，从 `/r/<code>` 进来时会被预填，服务端在"新手机号
验证注册"的瞬间写入关系并发前两档 5000 / 8000。账号一旦创建完成，
`POST /referrals/bind` 永远 409 `existing_user_ineligible`
（"Only a new verified phone account can bind."）——判定的是"这不是新号"，
不是"你填的码对不对"。注册 1 天、onboarding 已做满的号照样被拒。

所以本脚本**不再尝试 bind**（旧版的 `bind` / `bindwatch` / `--retry` 全部下线），
只做两件仍有价值的事：
  1. 报事实：每个号的 onboarding 进度、邀请档位、能不能当邀请人；
  2. 给动作：缺 onboarding 的补 onboarding；想要邀请奖励就用带码注册链接
     （`scripts/invite_link.py` 产出，看板也能看）。

用法：
    python3 scripts/sweep.py             # 只扫描（默认，不花积分、不提交）
    python3 scripts/sweep.py --apply     # 扫描 + 给缺 onboarding 的号补做
    python3 scripts/sweep.py --json      # 机器可读
"""
import argparse
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import claim_daily as C          # noqa: E402
import inference as I            # noqa: E402
import onboard_new as ON         # noqa: E402
import invite_flow as IF         # noqa: E402
import invite_link as IL         # noqa: E402

CANDIDATE = os.path.join(os.path.dirname(C.AUTH_DIR), "data", "invite_candidate.json")


def account_paths():
    d = C.AUTH_DIR
    if not os.path.isdir(d):
        return []
    return sorted(
        (os.path.join(d, f) for f in os.listdir(d)
         if f.startswith("phanthy-") and f.endswith(".json")),
        key=lambda p: os.path.getmtime(p),
    )


def phone_of(prof):
    acc = (prof or {}).get("account") or {}
    s = (acc.get("email") or acc.get("display_name") or "").strip()
    return s.replace("+86", "").replace(" ", "") or "?"


def scan():
    rows = []
    for path in account_paths():
        row = {"auth": os.path.basename(path), "scope": "", "phone": "?", "age_days": None,
               "onboarding": {"earned": 0, "max": 1500, "pending": []},
               "inviter": {"tier": 0, "budget": 2, "eligible": False, "next_reward": 0},
               "actions": [], "note": ""}
        try:
            doc = I.Desktop(auth_path=path)
        except SystemExit as e:
            row["note"] = f"读取失败: {e}"
            rows.append(row)
            continue
        except Exception as e:                        # noqa: BLE001
            row["note"] = f"读取失败: {type(e).__name__}: {e}"
            rows.append(row)
            continue

        row["scope"] = doc.ident.get("accountScope", "")
        try:
            st, prof = C.http("GET", "/api/oauth/profile",
                              {"Authorization": "Bearer " + doc.token})
            if st == 200:
                row["phone"] = phone_of(prof)
        except Exception:                             # noqa: BLE001
            pass
        row["age_days"] = round(ON.age_days(doc), 2)

        earned, mx, pending, _ = ON.progress(doc)
        row["onboarding"] = {"earned": earned, "max": mx, "pending": pending}

        # 邀请只跟自己相关：能不能当邀请人、前两档用了几档
        rec = IF.survey(path)
        if not rec["error"]:
            row["inviter"] = {
                "tier": int(rec.get("tier") or 0),
                "budget": IF.budget_left(rec),
                "eligible": bool(rec.get("eligible")),
                "next_reward": int(rec.get("next") or 0),
            }
        else:
            row["note"] = f"参考 /referrals/me 失败: {rec['error']}"

        if earned < mx or pending:
            row["actions"].append("onboard")
        if row["actions"]:
            row["note"] = row["note"] or f"缺 onboarding 用量（{earned}/{mx}）"
        elif not row["note"]:
            row["note"] = "已完成"
        rows.append(row)
    return rows


def print_table(rows):
    if not rows:
        print("auths/ 里没有账号凭证")
        return
    print(f"{'手机/昵称':<14} {'scope':<26} {'注册':>6} {'onboarding':>13} {'可邀请':<12} {'动作':<9} 说明")
    for r in rows:
        ob, iv = r["onboarding"], r["inviter"]
        age = "-" if r["age_days"] is None else f"{r['age_days']:.0f}天"
        acts = ",".join(r["actions"]) or "-"
        inv = f"{iv['tier']}/2档 余{iv['budget']}" if iv["eligible"] else ("不可邀请" if iv["tier"] else "-")
        print(f"{r['phone']:<14} {(r['scope'] or r['auth'])[:26]:<26} {age:>6} "
              f"{ob['earned']:>5}/{ob['max']:<7} {inv:<12} {acts:<9} {r['note']}")
    todo = [r for r in rows if r["actions"]]
    print()
    if todo:
        print(f"共 {len(rows)} 个号，{len(todo)} 个要补 onboarding："
              + "、".join(f"{r['phone']}（{r['onboarding']['earned']}/{r['onboarding']['max']}）"
                          for r in todo))
    else:
        print(f"共 {len(rows)} 个号，onboarding 全部做满。")


def candidate_info():
    """读 data/invite_candidate.json（旧 schema 也认；没有就现算一次，不写文件）。"""
    got = IL.load_candidate()
    if got:
        return got
    try:
        p = subprocess.run([sys.executable, os.path.join(HERE, "invite_link.py"), "--json"],
                           capture_output=True, text=True)
        if p.returncode == 0 and p.stdout.strip():
            return json.loads(p.stdout)
    except Exception:                                 # noqa: BLE001
        pass
    return None


def run(cmd):
    print(f"$ {' '.join(cmd)}")
    p = subprocess.run(cmd, cwd=os.path.dirname(HERE), capture_output=True, text=True)
    out = "\n".join(l for l in (p.stdout or "").splitlines() if "[identity]" not in l)
    if out.strip():
        print(out)
    if p.returncode != 0:
        err = "\n".join(l for l in (p.stderr or "").splitlines()[-6:] if "[identity]" not in l)
        print(f"（退出码 {p.returncode}）{err}")
    return p.returncode


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--apply", action="store_true", help="真的执行（默认只扫描）")
    ap.add_argument("--json", action="store_true", help="输出 JSON")
    a = ap.parse_args()

    rows = scan()
    inv = candidate_info()
    stamp = time.strftime("%Y-%m-%dT%H:%M:%S")

    if a.json:
        print(json.dumps({"at": stamp, "accounts": rows, "invite": inv}, ensure_ascii=False))
    else:
        print(f"== 扫描 {stamp}（onboarding 1500 / 邀请只能带码注册）")
        print_table(rows)
        print()
        if inv:
            print(f"带码注册链接（邀请人 {inv.get('phone') or inv.get('inviter')}，"
                  f"已用 {inv.get('tier')}/2 档，下一档 +{inv.get('next_reward')}）：")
            print(f"  {inv.get('share_url')}")
            print("  新手机号必须从这个链接进注册页；已建好的号无法补填。")
        else:
            print("没有可用邀请人（前两档已用满，或没有 eligible_to_invite 的号）")

    if not a.apply:
        if not a.json:
            print("\n（只扫描。加 --apply 才会真的补 onboarding）")
        return 0

    if not any(r["actions"] for r in rows):
        if not a.json:
            print("\n没有需要执行的动作。")
        return 0

    rc = run([sys.executable, os.path.join(HERE, "onboard_new.py")])
    print("\n== 执行后复查")
    print_table(scan())
    return rc


if __name__ == "__main__":
    sys.exit(main())
