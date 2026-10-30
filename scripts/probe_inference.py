#!/usr/bin/env python3
"""真实用量探针：确认桌面推理通道（desktop_inference_token）能不能用，
以及一次真实对话能否让 onboarding 的 recommended_model_success 结算。

用法: python3 scripts/probe_inference.py [--model NAME] [--send]
不带 --send 只打印 summary 里的 onboarding 状态与可用 token 概况。
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import claim_daily as C


def models(token):
    for path in ("/v1/models", "/api/oauth/models"):
        st, raw = C.http("GET", path, {"Authorization": "Bearer " + token})
        print(f"[{path}] {st} {str(raw)[:300]}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="")
    ap.add_argument("--send", action="store_true")
    ap.add_argument("--prompt", default="ping")
    args = ap.parse_args()

    auth_path = I.pick_auth()
    doc = C.load_auth(auth_path)
    token = C.access_token(doc, auth_path)
    _, ident = C.load_or_create_identity("acct_5f3d2c1b0a9e8d7c6b5a4f3e")
    C.register(token, ident)
    st, summ = C.summary(token, ident)
    print("[summary]", st)
    inf = summ.get("desktop_inference_token")
    print("[inference_token] len=", len(inf or ""), "(值不打印)")
    ob = summ.get("onboarding") or {}
    print("[onboarding]", json.dumps({k: ob.get(k) for k in ("status", "completed_count", "earned_points", "max_points")}, ensure_ascii=False))
    for t in ob.get("tasks") or []:
        print("   -", t["task_key"], t["status"], t["reward_points"], json.dumps(t.get("action_config"), ensure_ascii=False))

    if not args.model:
        models(token)
        return

    body = json.dumps({
        "model": args.model,
        "max_tokens": 16,
        "stream": False,
        "messages": [{"role": "user", "content": args.prompt}],
    })
    print("[inference body]", body)
    if not args.send:
        print("(dry) 加 --send 才真的发")
        return
    hdr = {
        "Content-Type": "application/json",
        "anthropic-version": "2023-06-01",
        "anthropic-beta": "oauth-2025-04-20",
        "x-app": "cli",
        "x-phanthy-client-source": "desktop",
        "Authorization": "Bearer " + (inf or token),
    }
    st, raw = C.http("POST", "/v1/messages?beta=true", hdr, body, timeout=120)
    print("[messages]", st, str(raw)[:600])

    st, rec = C.reconcile(token, ident)
    print("[reconcile]", st, json.dumps(rec, ensure_ascii=False)[:600])


if __name__ == "__main__":
    main()
