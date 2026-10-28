#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""workbuddy-plus 演示用「假上游」。

用途：在没有真实账号的前提下，把面板的成长任务链路（扫描 → accept → 动作 →
轮询 → 领奖 → 余额变化）跑通，用来演示「积分效益 ROI 闸门」的实际效果。

**不连接任何真实服务，也不含任何真实凭据。**

行为约定（对面板可见的部分）：
  - GET  /v2/activity/growth/tasks            任务列表（带 X-Client-Platform: miniprogram 时追加 mp 专属任务）
  - POST /v2/activity/growth/tasks/accept     报名（幂等）
  - POST …/tasks/<code>/claim 与 …/tasks/reward/claim   领奖（返回该任务的奖励积分）
  - POST /v2/chat/completions                 SSE 对话：**每次扣 UNIT_CHAT_COST 分**
  - POST /v2/billing/meter/get-user-resource  余额查询（面板据此实测「对话花了多少分」）
  - POST /portal/operation-platform/market/expert/list  专家市场列表（按 expert_type 返回不同专家）
  - 其它 /v2/report、/activity/... 一律 200 空 data（遥测类动作零成本）

进度口径（尽量贴近真实上游的「按行为事件推进度」）：
  - 计费任务：按 chat 请求自身携带的信号归属（X-Expert-Id / 提示词 / agent 字段），
    攒够该任务需要的对话次数才点亮——**跳过不跑的任务不会阻止后面的任务点亮**。
  - 零成本任务：每个动作周期（一次任务列表请求）最多点亮一个，多出来的事件丢弃，
    避免「一个动作点亮一串任务」的假象。

积分口径：余额 = 初始余额 - 对话次数 × UNIT_CHAT_COST + 已领奖励。
"""
import json
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UNIT_CHAT_COST = 30  # 单次真实对话的积分成本（演示口径）
START_BALANCE = 3000

# 任务目录：顺序与面板 autoActions 一致；(code, reward, chats)
#   reward None = 上游没有下发 reward_credit（真实的未知奖励情形）
#   chats      = 该任务点亮需要几次真实对话（0 = 纯遥测上报，零成本）
# 奖励值取自仓库动作表里实证过的上游下发值。
CATALOGUE = [
    ("chat_5", None, 0),
    ("first_buddy", 300, 0),
    ("Model_chat_GLM5.2", None, 1),
    ("RichMeow_Chat", None, 0),
    ("Buddy_App", None, 0),
    ("Buddy_App_QQ", None, 0),
    ("automation_1", None, 0),
    ("Library_read", None, 0),
    ("template_5", None, 0),
    ("playbook_prompt", None, 0),
    ("create_canvas", 300, 0),
    ("expert_5", None, 5),
    ("Expert_team_use_3", None, 3),
    ("Hp_Appearance", None, 0),
    ("skill_1", None, 1),
    ("Expert_lighthouse", None, 1),
    ("WorkBuddy_Valorant", None, 1),
    ("expert_use_maidehao", 50, 1),
    ("expert_use_douyinyin", 50, 1),
    ("expert_use_shangjia_xuanchuan", 50, 1),
    ("expert_use_bowangwang", 50, 1),
    ("black_cat", None, 3),
    ("school_season", 100, 0),
    ("Sequential_Tasks_1", 100, 0),
    ("Sequential_Tasks_2", 200, 0),
    ("Sequential_Tasks_3", 300, 0),
    ("Sequential_Tasks_4", None, 0),
    ("Sequential_Tasks_5", None, 0),
    ("Sequential_Tasks_6", None, 0),
    ("Sequential_Tasks_7", 500, 0),
]
MP_ONLY = {"Sequential_Tasks_1", "Sequential_Tasks_2", "Sequential_Tasks_3",
           "Sequential_Tasks_4", "Sequential_Tasks_5", "Sequential_Tasks_6",
           "Sequential_Tasks_7"}
REWARD = {c: r for c, r, _ in CATALOGUE}
CHATS = {c: n for c, _, n in CATALOGUE}
BILLED = [c for c, _, n in CATALOGUE if n > 0]
FREE = [c for c, _, n in CATALOGUE if n == 0]

# 专家 id → 任务码（与面板 expertExperienceTasks / runExpertLighthouse 等静态表一致）
EXPERT_TASK = {
    "ex_2cvvUZQhDyeJ": "Expert_lighthouse",
    "ex_xQHHD1u5Jkua": "WorkBuddy_Valorant",
    "ex_GDl31WahyIH0": "expert_use_maidehao",
    "ex_FbY5nlWTqDod": "expert_use_douyinyin",
    "ex_Weo3x2EZuSDm": "expert_use_shangjia_xuanchuan",
    "ex_WZY136wV5aMt": "expert_use_bowangwang",
}
# 市场列表返回的专家：agent 类 5 个（对应 expert_5），team 类 3 个（对应 Expert_team_use_3）
AGENT_EXPERTS = [("ex_agent_%02d" % i, "演示专家%d" % i, "演示职业%d" % i) for i in range(1, 6)]
TEAM_EXPERTS = [("ex_team_%02d" % i, "演示专家团%d" % i, "演示团职业%d" % i) for i in range(1, 4)]

lock = threading.Lock()
state = {}  # uid -> dict


def uid_state(uid):
    st = state.get(uid)
    if st is None:
        st = {
            "balance": START_BALANCE,
            "accepted": set(),
            "done": set(),
            "claimed": set(),
            "prog": {},           # code -> 已攒对话次数
            "pending_report": 0,  # 待消费的遥测事件
            "chat_calls": 0,
            "claimed_credit": 0,
            "spent": 0,
            "by_code_chats": {},  # code -> 该任务实际消耗的对话次数
        }
        state[uid] = st
    return st


def first_incomplete(st, codes):
    for code in codes:
        if code not in st["done"] and code not in st["claimed"]:
            return code
    return None


def task_json(code, reward, st):
    n = CHATS[code]
    prog = st["prog"].get(code, 0)
    target = n if n > 0 else 1
    current = target if code in st["done"] else min(prog, target)
    item = {
        "task_code": code,
        "code": code,
        "title": code,
        "task_type": "single",
        "accept_status": "accepted" if code in st["accepted"] else "not_accepted",
        "status": "complete" if code in st["done"] else "in_progress",
        "progress": {"current": current, "target": target},
    }
    if reward is not None:
        item["reward_credit"] = reward
        item["has_reward"] = True
    return item


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # 静音
        pass

    # ── 基础工具 ─────────────────────────────────────────────
    def _uid(self):
        return (self.headers.get("X-User-Id") or self.headers.get("X-Auth-Uid")
                or self.headers.get("x-uid") or "demo01")

    def _send(self, obj, status=200, ctype="application/json"):
        raw = obj if isinstance(obj, bytes) else json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        # keep-alive 下如果某个分支没读完请求体，残留字节会被当成下一个请求行，
        # 上游就会回 400 / 501 —— 演示里表现为「莫名 query 失败」。直接关连接最稳。
        self.send_header("Connection", "close")
        self.close_connection = True
        self.end_headers()
        self.wfile.write(raw)

    def ok(self, data=None):
        self._send({"code": 0, "msg": "ok", "data": {} if data is None else data})

    def _body(self):
        if getattr(self, "_body_cache", None) is not None:
            return self._body_cache
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n > 0 else b""
        try:
            self._body_cache = json.loads(raw.decode("utf-8") or "{}") if raw else {}
        except Exception:
            self._body_cache = {}
        return self._body_cache

    # ── 路由 ─────────────────────────────────────────────────
    def do_GET(self):
        self.route("GET")

    def do_POST(self):
        self.route("POST")

    def do_PUT(self):
        self.route("PUT")

    def do_PATCH(self):
        self.route("PATCH")

    def do_DELETE(self):
        self.route("DELETE")

    def route(self, method):
        path = self.path.split("?")[0]
        uid = self._uid()
        st = uid_state(uid)

        if path.startswith("/__demo/"):
            return self.demo(path, st, uid)

        # 任务列表
        if path == "/v2/activity/growth/tasks":
            with lock:
                if st["pending_report"] > 0:
                    code = first_incomplete(st, FREE)
                    if code is not None:
                        st["done"].add(code)
                    st["pending_report"] = 0
                mp = (self.headers.get("X-Client-Platform") or "").lower() == "miniprogram"
                tasks = []
                for code, reward, _n in CATALOGUE:
                    if code in MP_ONLY and not mp:
                        continue
                    tasks.append(task_json(code, reward, st))
            return self.ok({"tasks": tasks})

        # 报名
        if path == "/v2/activity/growth/tasks/accept":
            body = self._body()
            with lock:
                for code in body.get("task_codes") or []:
                    st["accepted"].add(code)
            return self.ok()

        # 领奖（表单 /reward/claim 或 /<code>/claim）
        m = re.match(r"^/(?:v2/)?activity/growth/tasks/(?:reward/claim|([^/]+)/claim)$", path)
        if m and method == "POST":
            body = self._body()
            code = m.group(1) or body.get("task_code") or ""
            with lock:
                if code in st["claimed"]:
                    return self.ok({"already_claimed": True, "credit": 0, "energy": 0})
                reward = REWARD.get(code) or 0
                st["claimed"].add(code)
                st["done"].add(code)
                st["claimed_credit"] += reward
                st["balance"] += reward
            return self.ok({"already_claimed": False, "credit": reward, "energy": 0})

        # 余额
        if path.endswith("/billing/meter/get-user-resource") or path.endswith("/billing/meter/get-user-resource-summary"):
            with lock:
                bal = st["balance"]
            end_ms = int((time.time() + 86400 * 400) * 1000)
            return self.ok({"Response": {"Data": {"Accounts": [{
                "PackageName": "演示套餐",
                "DeductionEndTime": end_ms,
                "CycleEndTime": "2036-12-31 00:00:00",
                "CapacitySize": 50000,
                "CapacityRemain": bal,
                "CapacityUsed": 50000 - bal,
            }]}}})

        # 专家市场列表 / 搜索
        if "market/expert/list" in path:
            body = self._body()
            etype = (body.get("expert_type") or "").lower()
            src = TEAM_EXPERTS if etype == "team" else AGENT_EXPERTS
            experts = [{"expert_id": i, "expert_type": etype or "agent",
                        "display_name_zh": n, "profession_zh": p, "version": "1.0.0"}
                       for i, n, p in src]
            # 专家体验类任务的静态专家也放进列表，模拟「市场里确实存在」
            for i, code in EXPERT_TASK.items():
                experts.append({"expert_id": i, "expert_type": "agent", "display_name_zh": code,
                                "profession_zh": code, "version": "1.0.0"})
            return self.ok({"experts": experts})

        # 对话
        if path.endswith("/chat/completions"):
            return self.chat(st)

        if path.endswith("/chat/busy"):
            return self.ok({"busy": False})

        # 遥测上报类：零成本，只当作「行为事件」
        if path.startswith("/v2/report") or path == "/v2/plugin/event" or path.endswith("/appearance/set") \
                or path.startswith("/v2/activity/growth/buddy") or path.endswith("/plugin/auth/token/refresh") \
                or "/market/expert/list" in path or path.endswith("/enterprises/personal/models"):
            body = self._body()
            raw = json.dumps(body, ensure_ascii=False)
            # 伴随真实对话的 chat_activity 上报不重复计入（成本已由 chat 请求扣掉）
            if "glm-5.2" in raw.lower() or "GLM-5.2" in raw:
                return self.ok()
            with lock:
                st["pending_report"] += 1
            return self.ok()

        # 其它一律成功空 data（避免未知路径把动作判失败）
        return self.ok()

    # ── 对话 ─────────────────────────────────────────────────
    def classify_chat(self, body):
        eid = self.headers.get("X-Expert-Id") or ""
        if eid:
            if eid in EXPERT_TASK:
                return EXPERT_TASK[eid]
            if any(eid == i for i, _, _ in AGENT_EXPERTS):
                return "expert_5"
            if any(eid == i for i, _, _ in TEAM_EXPERTS):
                return "Expert_team_use_3"
            return "expert_5"  # 未知专家：按 5 个专家的任务记账，不影响成本核算
        if (self.headers.get("X-Client-Platform") or "").lower() == "miniprogram":
            return "mp"
        text = ""
        for m in body.get("messages") or []:
            if isinstance(m, dict):
                text += str(m.get("content") or "")
        if "hi，请回复一句话" in text:
            return "Model_chat_GLM5.2"
        if body.get("agent") == "cli":
            return "skill_1"
        return "black_cat"

    def chat(self, st):
        body = self._body()
        code = self.classify_chat(body)
        with lock:
            st["chat_calls"] += 1
            st["spent"] += UNIT_CHAT_COST
            st["balance"] -= UNIT_CHAT_COST
            st["by_code_chats"][code] = st["by_code_chats"].get(code, 0) + 1
            if code == "mp":
                target = first_incomplete(st, [c for c in MP_ONLY])
                if target is not None:
                    st["done"].add(target)
            elif code in CHATS:
                st["prog"][code] = st["prog"].get(code, 0) + 1
                if st["prog"][code] >= CHATS[code]:
                    st["done"].add(code)
            n = st["chat_calls"]
        # 服务端 requestId 的形状是 `cmb-` 前缀或裸 32 位十六进制（见 upstream/idRegex），
        # 自造 UUID 或 chatcmpl-xxx 这类别名不会被认，expert_actual_use 等 JOIN 事件就点不亮。
        rid = "cmb-%032x" % (n * 7919 + 1)
        chunk = {'id': rid, 'object': 'chat.completion.chunk',
                 'created': int(time.time()), 'model': 'mock',
                 'choices': [{'index': 0, 'delta': {'role': 'assistant', 'content': '2'}}]}
        done = {'id': rid, 'object': 'chat.completion.chunk',
                'choices': [{'index': 0, 'delta': {}, 'finish_reason': 'stop'}]}
        raw = ("data: " + json.dumps(chunk, separators=(",", ":"))
               + "\n\ndata: " + json.dumps(done, separators=(",", ":"))
               + "\n\ndata: [DONE]\n\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    # ── 演示观测接口 ──────────────────────────────────────────
    def demo(self, path, st, uid):
        if path == "/__demo/state":
            with lock:
                return self._send({
                    "uid": uid,
                    "balance": st["balance"],
                    "spent": st["spent"],
                    "chat_calls": st["chat_calls"],
                    "claimed_credit": st["claimed_credit"],
                    "net": st["claimed_credit"] - st["spent"],
                    "chats_by_code": st["by_code_chats"],
                    "claimed": sorted(st["claimed"]),
                    "billed_claimed": sorted(c for c in st["claimed"] if c in BILLED),
                    "free_claimed": sorted(c for c in st["claimed"] if c in FREE),
                })
        if path == "/__demo/reset":
            with lock:
                state.pop(uid, None)
                st = uid_state(uid)
            return self._send({"reset": True, "uid": uid, "balance": st["balance"]})
        return self._send({"error": "unknown demo path"}, status=404)


def main():
    srv = ThreadingHTTPServer(("127.0.0.1", 18080), Handler)
    print("mock upstream on http://127.0.0.1:18080  unit_chat_cost=%d start_balance=%d"
          % (UNIT_CHAT_COST, START_BALANCE), flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
