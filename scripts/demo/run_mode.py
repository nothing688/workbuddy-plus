#!/usr/bin/env python3
"""把演示环境按指定 task_mode 跑一遍并打印前后账。

用法（在 scripts/demo 目录下，假上游与二进制已在跑）：
    python3 run_mode.py roi|free|all

它会：
  1) 改写 config.json 的 schedule.task_mode（面板只在启动时读配置，所以随后重启进程）；
  2) 复位假上游账本（余额回 3000、花费/已领清零）；
  3) 读一次 GET /panel/api/accounts/<uid>/tasks/plan（纯预览，不发任何动作请求）；
  4) POST .../tasks/auto_all 跑完整一键队列，轮询到结束；
  5) 打印计划预估与实测花费/到账/净收益，便于横向对比三种模式。
"""
import json
import os
import re
import subprocess
import sys
import time
import urllib.request

MODE = sys.argv[1] if len(sys.argv) > 1 else "roi"
UID = os.environ.get("DEMO_UID", "demo01")
HERE = os.path.dirname(os.path.abspath(__file__))
PANEL = os.environ.get("DEMO_PANEL", "http://127.0.0.1:7863")
MOCK = os.environ.get("DEMO_MOCK", "http://127.0.0.1:18080")
KEY = os.environ.get("DEMO_KEY", "demo-key")
H = {"Authorization": "Bearer " + KEY}


def set_mode(mode):
    path = os.path.join(HERE, "config.json")
    with open(path) as f:
        s = f.read()
    s = re.sub(r'"task_mode":\s*"[a-z]+"', '"task_mode": "%s"' % mode, s)
    with open(path, "w") as f:
        f.write(s)


def get(url):
    return json.load(urllib.request.urlopen(urllib.request.Request(url, headers=H)))


def post(url, body=None):
    req = urllib.request.Request(url, data=json.dumps(body or {}).encode(),
                                 headers={**H, "Content-Type": "application/json"}, method="POST")
    return json.load(urllib.request.urlopen(req))


def restart_panel():
    subprocess.run(["pkill", "-x", "workbuddy-plus"])  # 精确匹配进程名，避免误杀别的 shell
    time.sleep(2)
    subprocess.Popen([os.path.join(HERE, "..", "..", "workbuddy-plus")],
                     stdout=open(os.path.join(HERE, "panel.log"), "w"),
                     stderr=subprocess.STDOUT, start_new_session=True, cwd=HERE)
    for _ in range(30):
        time.sleep(1)
        try:
            get(PANEL + "/panel/api/tasks/queue")
            return
        except Exception:
            pass
    raise SystemExit("面板没起来，看 scripts/demo/panel.log")


def main():
    set_mode(MODE)
    restart_panel()
    urllib.request.urlopen(MOCK + "/__demo/reset").read()

    plan = get(PANEL + "/panel/api/accounts/%s/tasks/plan" % UID)["plan"]
    keys = ("mode", "run_count", "claim_count", "skip_count", "chats",
            "projected_cost", "projected_reward", "projected_net")
    print("PLAN", MODE, json.dumps({k: plan[k] for k in keys}, ensure_ascii=False))

    post(PANEL + "/panel/api/accounts/%s/tasks/auto_all" % UID)
    t0 = time.time()
    job = {}
    for _ in range(120):
        time.sleep(4)
        job = get(PANEL + "/panel/api/accounts/%s/tasks/job" % UID)["job"]
        if job["status"] in ("finished", "interrupted", "failed"):
            break
    st = json.load(urllib.request.urlopen(MOCK + "/__demo/state"))
    from collections import Counter
    print("JOB", job["status"], job.get("completed"), "/", job.get("total"),
          "%.0fs" % (time.time() - t0),
          dict(Counter(r.get("status") for r in job.get("results") or [])))
    print("STATE", json.dumps({k: st[k] for k in ("balance", "spent", "chat_calls",
                                                  "claimed_credit", "net", "chats_by_code")},
                              ensure_ascii=False))


if __name__ == "__main__":
    main()
