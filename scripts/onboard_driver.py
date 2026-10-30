#!/usr/bin/env python3
"""Onboarding 真实用量推进器（2026-10-28 二次实测：5/5、1500/1500）。

原则不变：只发真实推理请求（真花积分、真产出 token），然后 reconcile 让服务端按自己的
判据结算。不构造 usage、不改 billing、不伪造 evidence。

服务端判据（`tasks[].action_kind` 直接写在 summary 里，这是权威口径）：

  recommended_model_success  action_kind=new_session     新会话里跑一次有效对话
  follow_up_success          action_kind=recent_session  同一会话里第二轮要**长出**（截断不算）
  expert_success             action_kind=expert_market   请求头必须带真实专家 id
  skill_success              action_kind=skill_picker    请求头必须带真实 skill 名
  complete_workflow          action_kind=workflow        专家 + skill 同会话多轮完整交付

每项都要先 `POST /activities/onboarding/tasks/<key>/start`，再用真实推理，再 reconcile；
不 start 直接 reconcile 只会得到 evidence 但任务不动。

实测要点（都是踩出来的）：

  * 体验版只放行 `phanthy-pro`；`Iris-1.0` / `Apollo-2.0`（summary 里写的推荐模型）
    直接 `403 model_not_allowed`，但它们**不是**判据——用 phanthy-pro 也能结算。
  * 专家 id 必须是真目录里的 id，形如 `phanthy.research-analyst` / `phanthy.data-analyst`
    （不是 `expert-product-consultant` 这种臆造值；带了假 id 不报错，但任务不结算）。
  * skill 名同理：`deep-research` / `academic-paper` / `understand-chat` 等。
  * follow_up 的第二轮 max_tokens 要给到 1500+，且回答要长（实测被 max_tokens 截断的
    那一轮不算"有效追问"）。
  * complete_workflow 要求同一会话里同时带 expert + skill，max_tokens 3000，跑两轮。
  * `reconcile.evidence.signed_session_match` 一直是 false，但它**不是**门槛：任务照样结算。
    别被这个字段误导（早期误判过，见 docs/04 的更正段）。

用法：
  python3 scripts/onboard_driver.py --status     # 只看进度，不发推理、不花积分
  python3 scripts/onboard_driver.py              # 补齐未完成项并结算
  python3 scripts/onboard_driver.py --auth auths/phanthy-<ts>.json
  python3 scripts/onboard_driver.py --only complete_workflow   # 只补某一项
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import claim_daily as C
import inference as I

MODEL = "phanthy-pro"           # 体验版唯一放行的模型
EXPERT = "phanthy.research-analyst"
SKILL = "deep-research"


def text_of(raw):
    if isinstance(raw, dict):
        return "".join(b.get("text", "") for b in raw.get("content") or [] if b.get("type") == "text")
    return ""


def tasks(doc):
    st, s = C.summary(doc.token, doc.ident)
    ob = s.get("onboarding") or {}
    print(f"[summary] earned={ob.get('earned_points')}/{ob.get('max_points')} "
          f"credits={(s.get('credits') or {}).get('available')}")
    for t in ob.get("tasks") or []:
        print(f"   {t['task_key']:26s} {t['status']:12s} {t['reward_points']:4d}  {t['title']}")
    return {t["task_key"]: t["status"] for t in ob.get("tasks") or []}


def start(doc, key):
    st, r = C.signed_call(doc.token, doc.ident, "POST",
                          f"/activities/onboarding/tasks/{key}/start", "{}",
                          f"ob-start:{key}:{int(__import__('time').time() * 1000)}")
    print(f"[start/{key}] {st}" + ("" if st == 200 else f" {str(r)[:120]}"))
    return st == 200


def settle(doc, tag):
    st, rec = C.reconcile(doc.token, doc.ident)
    done = rec.get("completed_task_keys") if isinstance(rec, dict) else None
    print(f"[reconcile/{tag}] {st} completed={done}")
    if isinstance(rec, dict) and rec.get("evidence"):
        print("   evidence:", json.dumps(rec["evidence"], ensure_ascii=False))
    return rec


def chat(doc, messages, session, expert="", skills="", max_tokens=400):
    st, raw = doc.chat({"model": MODEL, "max_tokens": max_tokens, "stream": False, "messages": messages},
                       session_id=session, expert_id=expert, skill_names=skills)
    if isinstance(raw, dict):
        u = raw.get("usage") or {}
        print(f"   -> {st} model={raw.get('model')} tokens={u.get('input_tokens')}/{u.get('output_tokens')} "
              f"stop={raw.get('stop_reason')}")
    else:
        print(f"   -> {st} {str(raw)[:160]}")
    return st, raw


# ---------------------------------------------------------------- 各任务流程
def t_recommended(doc):
    """new_session：新会话里一次有效对话。"""
    sid = I.new_session_id()
    print(f"[recommended] session={sid}")
    chat(doc, [{"role": "user", "content": "用两句话解释：为什么先写验收标准再动手。"}],
         sid, max_tokens=600)


def t_follow_up(doc):
    """recent_session：同一会话两轮，第二轮要长（不被 max_tokens 截断）。"""
    sid = I.new_session_id()
    print(f"[follow-up] session={sid}")
    msgs = []
    for prompt, mx in [
        ("请写一份三段式周报模板，说明每段写什么、常见错误是什么、怎么自查。", 1500),
        ("就你上面第二段的自查清单，再补三条能直接照做的动作，并说明每条针对哪个常见错误。", 1500),
    ]:
        msgs.append({"role": "user", "content": prompt})
        st, raw = chat(doc, msgs, sid, max_tokens=mx)
        body = text_of(raw)
        if st != 200 or not body:
            print("   多轮中断，停止")
            return
        msgs.append({"role": "assistant", "content": body})


def t_expert(doc):
    """expert_market：必须带真实专家 id（phanthy.* 目录 id）。"""
    sid = I.new_session_id()
    print(f"[expert] session={sid} expert={EXPERT}")
    chat(doc, [{"role": "user", "content":
                "作为市场研究分析师，给我一份「家用 NAS 二线城市购买决策因素」的访谈提纲："
                "8 个问题，每个说明要验证的假设，并给追问方向。"}],
         sid, expert=EXPERT, max_tokens=2000)


def t_skill(doc):
    """skill_picker：必须带真实 skill 名。"""
    sid = I.new_session_id()
    print(f"[skill] session={sid} skill={SKILL}")
    chat(doc, [{"role": "user", "content":
                "用 deep-research 的方法论，给我一份「国产 2api 网关选型」的检索与验证计划："
                "分步骤、每步产出什么、怎么判断可信。"}],
         sid, skills=SKILL, max_tokens=2000)


def t_workflow(doc):
    """workflow：同会话里 expert + skill，两轮完整交付。"""
    sid = I.new_session_id()
    print(f"[workflow] session={sid} expert={EXPERT} skill={SKILL}")
    msgs = []
    for prompt in [
        "作为数据分析师并用 deep-research 方法，针对「笔记本换 SSD 后续航下降」做一份完整排查报告："
        "现象、可能原因排序、每项验证命令、判定标准、结论模板。",
        "把报告压成一页可交付检查清单：按执行顺序编号，每步写预期输出和失败时的下一步。",
    ]:
        msgs.append({"role": "user", "content": prompt})
        st, raw = chat(doc, msgs, sid, expert=EXPERT, skills=SKILL, max_tokens=3000)
        if st != 200:
            return
        msgs.append({"role": "assistant", "content": text_of(raw)})


PROCS = {
    "recommended_model_success": t_recommended,
    "follow_up_success": t_follow_up,
    "expert_success": t_expert,
    "skill_success": t_skill,
    "complete_workflow": t_workflow,
}

# complete_workflow 依赖专家/Skill 的能力，放在最后跑，前面的会话能顺带满足部分判据
ORDER = ["recommended_model_success", "follow_up_success", "expert_success",
         "skill_success", "complete_workflow"]


def run_tasks(doc, status, only=()):
    """按服务端 status 补未完成项：start → 真实推理 → reconcile → 复核。"""
    for key in ORDER:
        if only and key not in only:
            continue
        if status.get(key) == "completed":
            print(f"[skip/{key}] 已完成")
            continue
        proc = PROCS.get(key)
        if not proc:
            continue
        start(doc, key)
        proc(doc)
        rec = settle(doc, key)
        got = {t["task_key"]: t["status"] for t in ((rec or {}).get("onboarding") or {}).get("tasks") or []}
        if got.get(key) == "completed":
            print(f"[ok/{key}] completed")
        else:
            print(f"[fail/{key}] 仍是 {got.get(key)}")
    print()
    return tasks(doc)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--status", action="store_true", help="只看进度，不发推理")
    ap.add_argument("--auth", help="指定 auths/phanthy-*.json")
    ap.add_argument("--only", action="append", default=[], help="只跑指定 task_key（可重复）")
    a = ap.parse_args()

    doc = I.Desktop(auth_path=a.auth)
    status = tasks(doc)
    if a.status:
        return
    run_tasks(doc, status, only=tuple(a.only))


if __name__ == "__main__":
    main()
