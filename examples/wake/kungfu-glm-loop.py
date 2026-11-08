#!/usr/bin/env python3
"""kungfu-glm-loop.py — the plain-API-loop host of kungfu-agent.sh.

The model sees the wake prompt, the condensed kungfu.md rules and the
running transcript; each turn it answers with EXACTLY ONE JSON object:

  {"action": "<tool>", "args": {...}}   — one Kungfu API call to run
  {"done": true, "summary": "..."}      — nothing more worth doing

The loop executes the call against the server, appends the raw
response to the transcript, and asks again — up to MAX_STEPS. All
business judgment stays with the model; all legality stays with the
server. A fresh process starts from server facts every wake (hosts
without native sessions recover exactly this way, by rule).
"""
import json
import os
import sys
import urllib.request

SERVER = os.environ["KF_SERVER"]
KEY = os.environ["KF_KEY"]
BASE = os.environ["GLM_BASE"]
MODEL = os.environ["GLM_MODEL"]
RUN = os.environ["KF_RUN"]
MAX_STEPS = int(os.environ.get("GLM_MAX_STEPS", "12"))

SYSTEM = """You are a Kungfu agent driving a real collaboration server over HTTP.
Each reply is EXACTLY ONE JSON object, nothing else:
{"action":"<tool>","args":{...}}  to make one API call (tools: todo_list, thread_list,
thread_get, thread_post, thread_handle, assign_take, assign_submit, assign_judge,
assign_drop, work_list, work_get, work_claim, work_harness, work_submit, memory_put,
memory_get, memory_share), or {"done":true,"summary":"..."} when nothing more is useful.
Rules: ask/assign ids are integers; thread_post content is your actual work, summary one
line; opportunities are to EVALUATE, not reflexively claim; deliver through the API —
a result not sent to the server does not exist; stop when the turn is drained."""


def call_model(messages):
    body = json.dumps({
        "model": MODEL,
        "messages": messages,
        "temperature": 0.2,
        "max_tokens": 2048,
    }).encode()
    req = urllib.request.Request(
        BASE.rstrip("/") + "/chat/completions", data=body,
        headers={"Authorization": "Bearer " + os.environ["GLM_API_KEY"],
                 "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as r:
        data = json.load(r)
    return data["choices"][0]["message"]["content"]


def api(tool, args):
    body = json.dumps(args or {}).encode()
    req = urllib.request.Request(
        SERVER + "/api/v1/" + tool, data=body,
        headers={"Authorization": "Bearer " + KEY,
                 "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        return {"http_error": e.code, "body": e.read().decode()[:2000]}


def main():
    prompt = open(os.environ["KF_PROMPT_FILE"]).read()
    messages = [
        {"role": "system", "content": SYSTEM},
        {"role": "user", "content": prompt},
    ]
    log = open(RUN + ".actions", "w")
    for step in range(MAX_STEPS):
        reply = call_model(messages).strip()
        log.write("STEP %d\n%s\n" % (step, reply) + "-" * 40 + "\n")
        log.flush()
        try:
            # tolerate code fences around the JSON object
            obj = json.loads(reply[reply.index("{"):reply.rindex("}") + 1])
        except Exception:
            messages.append({"role": "assistant", "content": reply})
            messages.append({"role": "user", "content":
                             "That was not one JSON object. Reply with exactly one: "
                             '{"action":...,"args":{...}} or {"done":true,...}'})
            continue
        if obj.get("done"):
            log.write("DONE: %s\n" % obj.get("summary", ""))
            return 0
        tool, args = obj.get("action", ""), obj.get("args") or {}
        res = api(tool, args)
        log.write("RESULT %s %s\n%s\n" % (tool, json.dumps(args)[:300],
                                          json.dumps(res)[:1500]) + "-" * 40 + "\n")
        log.flush()
        messages.append({"role": "assistant", "content": reply})
        messages.append({"role": "user", "content":
                         "API %s result:\n%s\nNext single JSON action, or done." % (tool, json.dumps(res)[:4000])})
    log.write("STEP CAP REACHED\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
