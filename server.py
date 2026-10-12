#!/usr/bin/env python3
"""Zex local server: serves the web UI and relays requests to decision-model servers.

Most model servers (TypeSafe, Vercel, OpenJev, jeff) send no CORS headers for a page on
127.0.0.1, so the browser cannot call them directly. The relay at /zex/proxy forwards a
request to a decision-model API path, only when it comes from this page, and the server
binds to 127.0.0.1 so nothing else on the network can use it.

It also serves the local API (/api/sets/<set>), the same one the desktop app has: while it
is on, each saved question set answers POST requests that carry the token from Zex.

Usage:
    python3 server.py [--port 8765] [--open]

Standard library only. Python 3.9 or newer.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import hmac
import http.server
import json
import os
import re
import secrets
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from functools import partial

HERE = os.path.dirname(os.path.abspath(__file__))
WEB = os.path.join(HERE, "web")
TESTS = os.path.join(HERE, "tests")
# decisions and model lists, and on Ollaya the model store: details, downloads and removal
ALLOWED_PATH = re.compile(r"/(v1/(systemone|decisions|models)|api/(decide|tags|ps|show|version|pull|delete))/?$")
# Ollaya's catalog of models to download; the page reads it through /zex/catalog
CATALOG_URL = os.environ.get("ZEX_CATALOG_URL") or "https://ollaya.dev/search.json"
_catalog = {"at": 0.0, "data": b""}
LOOPBACK = {"localhost", "127.0.0.1", "::1"}
VERSION = "0.5.1"

# Local model servers never go through a system HTTP proxy; remote ones may need it.
DIRECT = urllib.request.build_opener(urllib.request.ProxyHandler({}))
DEFAULT = urllib.request.build_opener()


# ---------------------------------------------------------------- local API
# The page sends its sets and active connection to /zex/api-config while the API is on. They
# live in a config file only this user can read (the same file the desktop app uses).
def _config_dir() -> str:
    if os.environ.get("ZEX_CONFIG_DIR"):
        return os.environ["ZEX_CONFIG_DIR"]
    if sys.platform == "darwin":
        base = os.path.expanduser("~/Library/Application Support")
    elif os.name == "nt":
        base = os.environ.get("APPDATA") or os.path.expanduser("~/AppData/Roaming")
    else:
        base = os.environ.get("XDG_CONFIG_HOME") or os.path.expanduser("~/.config")
    return os.path.join(base, "Zex")


API_FILE = os.path.join(_config_dir(), "api.json")
API_LOCK = threading.Lock()
_api: dict | None = None


def api_config() -> dict:
    global _api
    if _api is None:
        try:
            with open(API_FILE, encoding="utf-8") as f:
                _api = json.load(f)
        except (OSError, ValueError):
            _api = {}
    return _api


def api_save(cfg: dict) -> None:
    os.makedirs(os.path.dirname(API_FILE), mode=0o700, exist_ok=True)
    tmp = API_FILE + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        json.dump(cfg, f, indent=2)
    os.replace(tmp, API_FILE)


def top_answer(a: dict) -> tuple[str, float]:
    """The top option and its probability, by the same rules as the page."""
    t = a.get("type")
    probs = a.get("probabilities") or {}
    if t == "noul":
        p = float(a.get("noul") or 0)
        return ("true", p) if p >= 0.5 else ("false", 1 - p)
    if t == "choice":
        label = str(a.get("choice"))
        return label, float(probs.get(label, a.get("confidence") or 0))
    if t == "score" and probs:
        k = max(sorted(probs), key=lambda x: probs[x])
        return str((a.get("legend") or {}).get(k, k)), float(probs[k])
    return "", 0.0


def decide(answers: dict, thresholds: dict, global_thr: float) -> tuple[dict, bool]:
    out, all_auto = {}, bool(answers)
    for k, a in answers.items():
        if not isinstance(a, dict):
            continue
        label, p = top_answer(a)
        t = (thresholds or {}).get(k) or global_thr or 0.8
        verdict = "auto" if p >= t else "review"
        all_auto = all_auto and verdict == "auto"
        out[k] = {"answer": label, "confidence": round(p, 4), "verdict": verdict, "threshold": t}
    return out, all_auto


class UpstreamError(Exception):
    pass


# ---------------------------------------------------------------- routes (cascades)
# A set can send its questions to a cheap model first, ask a stronger one only about what the
# first wasn't sure of, and leave the rest for a person. A stage that is down is skipped.
def run_route(cfg: dict, s: dict, model: str, state) -> dict:
    route = s.get("route") or [{"connection": cfg.get("connection") or {}, "model": model, "cost": 0}]
    questions = s.get("questions") or {}
    remaining = sorted(questions)
    res = {"answers": {}, "decisions": {}, "stages": [], "by": -1, "cost": 0.0,
           "baseline": float(route[-1].get("cost") or 0) / 1000, "model": model}
    start, last_err = time.time(), None
    for i, st in enumerate(route):
        conn = st.get("connection") or {}
        t0 = time.time()
        sl = {"name": conn.get("name") or conn.get("url", ""), "model": st.get("model", ""), "asked": len(remaining), "auto": 0}
        try:
            data = call_model(conn, st.get("model", ""), state, {k: questions[k] for k in remaining})
        except UpstreamError as e:
            sl.update(ms=int((time.time() - t0) * 1000), error=str(e))
            res["stages"].append(sl)
            last_err = e
            continue
        sl["ms"] = int((time.time() - t0) * 1000)
        sl["cost"] = float(st.get("cost") or 0) / 1000
        res["cost"] += sl["cost"]
        answers = data.get("answers") or {}
        dec, _ = decide(answers, s.get("thresholds") or {}, cfg.get("thr") or 0.8)
        res["model"] = data.get("model") or st.get("model", "")
        nxt = []
        for k in remaining:
            if k not in answers:
                nxt.append(k)
                continue
            d = dec[k]
            if len(route) > 1:
                d["by"] = f"{sl['name']} · {sl['model']}"
            res["answers"][k], res["decisions"][k] = answers[k], d
            if d["verdict"] == "auto":
                sl["auto"] += 1
            else:
                nxt.append(k)
        res["stages"].append(sl)
        remaining = nxt
        if not remaining:
            res["by"] = i
            break
    res["ms"] = int((time.time() - start) * 1000)
    res["all_auto"] = not remaining and bool(questions)
    if not res["answers"] and last_err:
        res["error"] = str(last_err)
    return res


# ---------------------------------------------------------------- request log and review queue
# Every API answer is recorded on this computer. Answers the model wasn't sure about wait for
# a person, and a share of the sure ones are set aside as spot checks; reviews become labels.
LOG_FILE = os.path.join(os.path.dirname(API_FILE), "requests.jsonl")
REVIEW_FILE = os.path.join(os.path.dirname(API_FILE), "reviews.jsonl")
LOG_KEEP = 10000
LOG_LOCK = threading.Lock()
_log: list | None = None
_log_by_id: dict = {}


def log_on(cfg: dict) -> bool:
    return cfg.get("log") is not False


def audit_rate(cfg: dict) -> float:
    a = cfg.get("audit")
    return 0.05 if a is None else max(0.0, min(1.0, float(a)))


def item_status(it: dict) -> str:
    if it.get("review"):
        return "reviewed"
    if it.get("error"):
        return "error"
    if not it.get("all_auto"):
        return "waiting"
    return "spot" if it.get("audit") else "auto"


def _append(path: str, obj) -> None:
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    with os.fdopen(fd, "a", encoding="utf-8") as f:
        f.write(json.dumps(obj) + "\n")


def _compact() -> None:
    global _log
    if len(_log) > LOG_KEEP:
        for it in _log[:-LOG_KEEP]:
            _log_by_id.pop(it["id"], None)
        _log = _log[-LOG_KEEP:]
    for path, rows in ((LOG_FILE, [{k: v for k, v in it.items() if k != "review"} for it in _log]),
                       (REVIEW_FILE, [{"id": it["id"], "review": it["review"]} for it in _log if it.get("review")])):
        os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
        fd = os.open(path + ".tmp", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.writelines(json.dumps(r) + "\n" for r in rows)
        os.replace(path + ".tmp", path)


def log_items() -> list:
    """The log, loaded once. Call with LOG_LOCK held."""
    global _log
    if _log is None:
        _log = []
        for path, is_review in ((LOG_FILE, False), (REVIEW_FILE, True)):
            try:
                with open(path, encoding="utf-8") as f:
                    for line in f:
                        try:
                            row = json.loads(line)
                        except ValueError:
                            continue
                        if is_review:
                            if row.get("id") in _log_by_id:
                                _log_by_id[row["id"]]["review"] = row.get("review")
                        elif row.get("id"):
                            _log.append(row)
                            _log_by_id[row["id"]] = row
            except OSError:
                pass
        if len(_log) > LOG_KEEP:
            _compact()
    return _log


def record(item: dict) -> None:
    with LOG_LOCK:
        log_items().append(item)
        _log_by_id[item["id"]] = item
        _append(LOG_FILE, item)
        if len(_log) > LOG_KEEP + LOG_KEEP // 5:
            _compact()


def new_id() -> str:
    return "r_" + format(int(time.time() * 1000), "x") + secrets.token_hex(6)


# ---------------------------------------------------------------- results sent back
# A set can have a webhook: Zex posts each final answer to it, right away when the model was
# sure, and after a person reviews it otherwise. A delivery that fails is tried again later.
HOOK_FILE = os.path.join(os.path.dirname(API_FILE), "deliveries.json")
HOOK_KEEP = 300
HOOK_LOCK = threading.Lock()
HOOK_WAKE = threading.Event()
HOOK_DELAYS = [10, 60, 300, 1800, 7200, 21600]  # seconds between tries: seven over about nine hours
_hooks: list | None = None
_hook_thread: threading.Thread | None = None


def valid_hook_url(u: str) -> bool:
    t = urllib.parse.urlparse(u or "")
    return t.scheme in ("http", "https") and bool(t.netloc)


def hook_list() -> list:
    """The deliveries, loaded once. Call with HOOK_LOCK held."""
    global _hooks
    if _hooks is None:
        try:
            with open(HOOK_FILE, encoding="utf-8") as f:
                _hooks = json.load(f) or []
        except (OSError, ValueError):
            _hooks = []
    return _hooks


def save_hooks() -> None:
    global _hooks
    _hooks = _hooks[-HOOK_KEEP:]
    os.makedirs(os.path.dirname(HOOK_FILE), mode=0o700, exist_ok=True)
    fd = os.open(HOOK_FILE + ".tmp", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        json.dump([{k: v for k, v in d.items() if k != "busy"} for d in _hooks], f)
    os.replace(HOOK_FILE + ".tmp", HOOK_FILE)


def start_hooks() -> None:
    global _hook_thread
    if _hook_thread is None:
        _hook_thread = threading.Thread(target=hook_loop, daemon=True)
        _hook_thread.start()


def queue_hook(s: dict, event: str, item: str, payload: dict, state: str = "pending") -> dict:
    """Adds a delivery: "pending" for the sender to send, or "sending" when the caller sends it."""
    d = {"id": "d_" + new_id()[2:], "event": event, "item": item, "set": s["slug"], "url": s.get("webhook", ""),
         "at": int(time.time() * 1000), "tries": 0, "state": state}
    d["body"] = {**payload, "event": event, "delivery": d["id"]}
    with HOOK_LOCK:
        hook_list().append(d)
        save_hooks()
    start_hooks()
    HOOK_WAKE.set()
    return d


def hook_loop() -> None:
    while True:
        now, wait, due = time.time() * 1000, 30.0, []
        with HOOK_LOCK:
            for d in hook_list():
                if d.get("state") != "pending" or d.get("busy"):
                    continue
                if d.get("next", 0) <= now:
                    due.append(d)
                else:
                    wait = min(wait, (d["next"] - now) / 1000)
        for d in due:
            send_hook(d, True)
        if due:
            continue
        HOOK_WAKE.wait(wait)
        HOOK_WAKE.clear()


def sign(secret: str, t: int, body: bytes) -> str:
    """The X-Zex-Signature header: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">."""
    mac = hmac.new(secret.encode(), f"{t}.".encode() + body, hashlib.sha256).hexdigest()
    return f"t={t},v1={mac}"


def send_hook(d: dict, retry: bool) -> None:
    """One try. With retry, a failure that may pass is tried again later."""
    with HOOK_LOCK:
        if d.get("busy"):
            return
        d["busy"] = True
    with API_LOCK:
        secret = api_config().get("hook_secret", "")
    body = json.dumps(d["body"]).encode()
    code, err = 0, ""
    req = urllib.request.Request(d["url"], data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("User-Agent", "Zex/" + VERSION)
    req.add_header("X-Zex-Event", d["event"])
    req.add_header("X-Zex-Delivery", d["id"])
    req.add_header("X-Zex-Signature", sign(secret, int(time.time()), body))
    opener = DIRECT if urllib.parse.urlparse(d["url"]).hostname in LOOPBACK else DEFAULT
    try:
        with opener.open(req, timeout=15) as r:
            code = r.status
    except urllib.error.HTTPError as e:
        code = e.code
    except Exception as e:  # unreachable host, timeout, TLS, DNS
        err = str(e)
    with HOOK_LOCK:
        d["busy"] = False
        d["tries"] += 1
        d.update(code=code, error=err, last=int(time.time() * 1000), next=0)
        if 200 <= code < 300:
            d.update(state="sent", error="")
        elif 400 <= code < 500 and code not in (408, 429):
            d.update(state="failed", error=f"the receiver answered HTTP {code}")
        elif retry and d["tries"] <= len(HOOK_DELAYS):
            d.update(state="pending", next=int((time.time() + HOOK_DELAYS[d["tries"] - 1]) * 1000))
            if code:
                d["error"] = f"the receiver answered HTTP {code}"
        else:
            d["state"] = "failed"
            if code:
                d["error"] = f"the receiver answered HTTP {code}"
        save_hooks()


def model_label(a: dict) -> str:
    """The model's top option as a label: "true" or "false", the option, or the level."""
    t = a.get("type")
    if t == "noul":
        return "true" if float(a.get("noul") or 0) >= 0.5 else "false"
    if t == "choice":
        return str(a.get("choice"))
    probs = a.get("probabilities") or {}
    if t == "score" and probs:
        return max(sorted(probs), key=lambda k: probs[k])
    return ""


def typed(kind, label: str):
    """A label as its value: true or false for noul, the level number for score."""
    if kind == "noul":
        return str(label).lower() == "true"
    if kind == "score":
        try:
            return int(label)
        except (TypeError, ValueError):
            pass
    return label


def iso(ms) -> str:
    return datetime.datetime.fromtimestamp(ms / 1000, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def public_status(it: dict) -> str:
    st = item_status(it)
    return {"waiting": "review", "spot": "auto"}.get(st, st)


def public_item(it: dict) -> dict:
    """A logged request as the API shows it, with its final answers once it has them."""
    st = public_status(it)
    out = {"id": it["id"], "set": it.get("set"), "status": st, "state": it.get("state"), "model": it.get("model", ""),
           "created_at": iso(it.get("at", 0)), "updated_at": iso(it.get("at", 0))}
    if it.get("ref"):
        out["ref"] = it["ref"]
    if it.get("error"):
        out["error"] = it["error"]
        return out
    out["decisions"] = it.get("decisions") or {}
    rv = it.get("review") or {}
    final, changed = {}, []
    for k in sorted(it.get("answers") or {}):
        a = it["answers"][k] or {}
        label = model_label(a)
        if rv and k in (rv.get("labels") or {}):
            if str(rv["labels"][k]).lower() != label.lower():
                changed.append(k)
            label = rv["labels"][k]
        final[k] = typed(a.get("type"), label)
    if st == "review":
        out["final"] = None
    elif st == "auto":
        out.update(final=final, by="model")
        if it.get("audit"):
            out["spot_check"] = True
    elif st == "reviewed":
        out.update(final=final, by="person", changed=changed, reviewed_at=iso(rv["at"]), updated_at=iso(rv["at"]))
    return out


def hook_for(slug: str) -> dict | None:
    with API_LOCK:
        cfg = api_config()
        if not cfg.get("enabled"):
            return None
        return next((dict(x) for x in cfg.get("sets") or [] if x.get("slug") == slug and valid_hook_url(x.get("webhook", ""))), None)


def after_review(it: dict, before: dict | None) -> None:
    """Sends what a review changed: the final answer for a request that was waiting, or a
    correction for one whose answer was already out."""
    s = hook_for(it.get("set"))
    if not s:
        return
    p = public_item(it)
    if not before and not it.get("all_auto"):
        queue_hook(s, "answer.reviewed", it["id"], p)
    elif not before:
        if p.get("changed"):
            queue_hook(s, "answer.corrected", it["id"], p)
    elif {k: str(v).lower() for k, v in (before.get("labels") or {}).items()} != {k: str(v).lower() for k, v in it["review"]["labels"].items()}:
        queue_hook(s, "answer.corrected", it["id"], p)


def call_model(conn: dict, model: str, state, questions) -> dict:
    """One request to the connection: Ollaya first tries /api/decide, then /v1/systemone."""
    body = json.dumps({"model": model, "state": state, "questions": questions}).encode()
    base = (conn.get("url") or "").rstrip("/")
    name = conn.get("name") or base
    paths = ["/api/decide", "/v1/systemone"] if conn.get("kind") in ("ollaya", "cloud") else ["/v1/systemone"]
    for i, path in enumerate(paths):
        req = urllib.request.Request(base + path, data=body, method="POST")
        req.add_header("Content-Type", "application/json")
        req.add_header("Accept", "application/json")
        req.add_header("User-Agent", "Zex/" + VERSION)
        if conn.get("key"):
            req.add_header("Authorization", "Bearer " + conn["key"])
        opener = DIRECT if urllib.parse.urlparse(base).hostname in LOOPBACK else DEFAULT
        try:
            with opener.open(req, timeout=180) as r:
                status, data = r.status, r.read()
        except urllib.error.HTTPError as e:
            status, data = e.code, e.read()
        except Exception as e:  # unreachable host, timeout, TLS, DNS
            raise UpstreamError(f"Zex could not reach {name}: {e}")
        if status in (404, 405) and i < len(paths) - 1:
            continue
        try:
            out = json.loads(data)
        except ValueError:
            raise UpstreamError(f"{name} sent something that isn't JSON (HTTP {status})")
        if status >= 300 or out.get("error") or out.get("error_type"):
            msg = out.get("error") or out.get("message") or out.get("detail")
            raise UpstreamError(f"{name} answered with an error (HTTP {status}): {msg}")
        return out
    raise UpstreamError(f"{name} has no decision endpoint")


class Handler(http.server.SimpleHTTPRequestHandler):
    server_version = "Zex/" + VERSION
    allowed_origins: set[str] = set()

    def log_message(self, fmt, *args):
        if self.path.startswith("/zex/proxy"):
            sys.stderr.write("relay %s\n" % (fmt % args))

    def end_headers(self):
        self.send_header("Cache-Control", "no-store")
        super().end_headers()

    def translate_path(self, path):
        # /tests/* is served from the repo's tests folder, everything else from web/
        clean = urllib.parse.urlparse(path).path
        if clean.startswith("/tests/"):
            return os.path.join(TESTS, os.path.normpath(clean[len("/tests/"):]).lstrip(os.sep + "."))
        return super().translate_path(path)

    def _json(self, status: int, obj) -> None:
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _relay(self) -> None:
        if self.headers.get("X-Zex") != "1":
            return self._json(403, {"error": "Zex relay: missing X-Zex header"})
        origin = self.headers.get("Origin")
        if origin and origin not in self.allowed_origins:
            return self._json(403, {"error": f"Zex relay: origin {origin} not allowed"})
        target = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query).get("u", [""])[0]
        t = urllib.parse.urlparse(target)
        if t.scheme not in ("http", "https") or not t.netloc or not ALLOWED_PATH.search(t.path):
            return self._json(400, {"error": "Zex relay: only decision-model API paths can be relayed", "target": target})
        body = self._raw if self.command in ("POST", "DELETE") else None
        req = urllib.request.Request(target, data=body, method=self.command)
        req.add_header("Content-Type", self.headers.get("Content-Type", "application/json"))
        req.add_header("Accept", "application/json")
        req.add_header("User-Agent", "Zex/" + VERSION)
        key = self.headers.get("X-Zex-Key")
        if key:
            req.add_header("Authorization", "Bearer " + key)
        opener = DIRECT if t.hostname in LOOPBACK else DEFAULT
        try:
            with opener.open(req, timeout=180) as r:
                ctype = r.headers.get("Content-Type", "application/json")
                if t.path.rstrip("/").endswith("/api/pull"):
                    # a download reports its progress as it goes: pass each line on at once
                    self.send_response(r.status)
                    self.send_header("Content-Type", ctype)
                    self.end_headers()
                    for line in r:
                        self.wfile.write(line)
                        self.wfile.flush()
                    self.close_connection = True
                    return
                status, data = r.status, r.read()
        except urllib.error.HTTPError as e:
            status, data, ctype = e.code, e.read(), e.headers.get("Content-Type", "application/json")
        except Exception as e:  # unreachable host, timeout, TLS, DNS
            return self._json(502, {"error": f"Zex relay could not reach {t.netloc}: {e}", "code": "UNREACHABLE"})
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    # A stand-in Ollaya for tests/flows.html: a model list, downloads that report their progress
    # line by line, and removal. Only this dev server has it; the app doesn't.
    _fake_models = {"fake:small": 52_000_000}

    def _fake_ollaya(self) -> None:
        path = urllib.parse.urlparse(self.path).path[len("/tests/fake-ollaya"):]
        have = Handler._fake_models
        body = json.loads(self._raw or b"{}") if self.command in ("POST", "DELETE") else {}
        if path == "/v1/models":
            return self._json(200, {"models": [{"name": n} for n in sorted(have)]})
        if path == "/api/tags":
            return self._json(200, {"models": [{"name": n, "model": n, "size": z, "details": {"format": "onnx", "parameter_size": "22M"}} for n, z in sorted(have.items())]})
        if path == "/api/ps":
            return self._json(200, {"models": []})
        if path == "/api/pull" and self.command == "POST":
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.end_headers()
            total = 40_000_000
            for line in [{"status": "pulling manifest"}] + [{"status": "pulling 1a2b", "digest": "sha256:1a2b", "total": total, "completed": total * k // 5} for k in range(6)] + [{"status": "success"}]:
                self.wfile.write((json.dumps(line) + "\n").encode())
                self.wfile.flush()
            have[body.get("model", "")] = total
            self.close_connection = True
            return
        if path == "/api/delete" and self.command == "DELETE":
            if have.pop(body.get("model", ""), None) is None:
                return self._json(404, {"error": "model not found", "code": "MODEL_NOT_FOUND"})
            return self._json(200, {})
        return self._json(404, {"error": "not found"})

    # A stand-in webhook receiver for tests/flows.html: it keeps what it gets, and answers 503
    # on paths with "/down" in them. GET /tests/fake-hook lists what came in.
    _fake_hook_calls: list = []

    def _fake_hook(self) -> None:
        if self.command == "GET":
            return self._json(200, {"calls": Handler._fake_hook_calls[-200:]})
        path = urllib.parse.urlparse(self.path).path
        try:
            body = json.loads(self._raw or b"{}")
        except ValueError:
            body = {}
        Handler._fake_hook_calls.append({"path": path, "event": self.headers.get("X-Zex-Event"), "signature": self.headers.get("X-Zex-Signature"), "body": body})
        return self._json(503 if "/down" in path else 200, {})

    def _catalog(self) -> None:
        if not self._from_page():
            return
        if not _catalog["data"] or time.time() - _catalog["at"] > 3600:
            try:
                with DEFAULT.open(urllib.request.Request(CATALOG_URL, headers={"User-Agent": "Zex/" + VERSION}), timeout=20) as r:
                    _catalog.update(at=time.time(), data=r.read())
            except Exception as e:
                if not _catalog["data"]:
                    return self._json(502, {"error": f"Zex couldn't load Ollaya's model catalog: {e}", "code": "UNREACHABLE"})
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(_catalog["data"])))
        self.end_headers()
        self.wfile.write(_catalog["data"])

    def _from_page(self) -> bool:
        if self.headers.get("X-Zex") != "1":
            self._json(403, {"error": "Zex: missing X-Zex header"})
            return False
        origin = self.headers.get("Origin")
        if origin and origin not in self.allowed_origins:
            self._json(403, {"error": f"Zex: origin {origin} not allowed"})
            return False
        return True

    def _api_config(self) -> None:
        if not self._from_page():
            return
        with API_LOCK:
            cfg = api_config()
            if self.command == "POST":
                try:
                    body = json.loads(self._raw or b"{}")
                except ValueError:
                    return self._json(400, {"error": "Zex: bad JSON"})
                cfg["enabled"] = bool(body.get("enabled"))
                if not cfg.get("token") or body.get("regenerate"):
                    cfg["token"] = "zex_" + secrets.token_hex(16)
                if not cfg.get("hook_secret") or body.get("regenerate_secret"):
                    cfg["hook_secret"] = "zexwh_" + secrets.token_hex(24)
                for k in ("model", "thr", "connection", "sets", "log", "audit"):
                    if cfg["enabled"] and body.get(k) is not None:
                        cfg[k] = body.get(k)
                    else:
                        cfg.pop(k, None)
                try:
                    api_save(cfg)
                except OSError as e:
                    return self._json(500, {"error": f"Zex could not save its API settings: {e}"})
            return self._json(200, {"enabled": bool(cfg.get("enabled")), "token": cfg.get("token", ""), "hook_secret": cfg.get("hook_secret", ""),
                                    "log": log_on(cfg), "audit": audit_rate(cfg)})

    def _api(self) -> None:
        with API_LOCK:
            cfg = json.loads(json.dumps(api_config()))
        if not cfg.get("enabled"):
            return self._json(403, {"error": "The Zex local API is off. Turn it on in Zex, under Local API."})
        auth = self.headers.get("Authorization", "")
        got = auth[7:] if auth.startswith("Bearer ") else self.headers.get("X-Zex-Token", "")
        if not cfg.get("token") or not hmac.compare_digest(got.encode(), cfg["token"].encode()):
            return self._json(401, {"error": "Missing or wrong token. Send Authorization: Bearer <token>, from Zex under Local API."})
        path = urllib.parse.urlparse(self.path).path.rstrip("/")
        sets = cfg.get("sets") or []
        if path == "/api" and self.command == "GET":
            return self._json(200, {"ok": True, "app": "zex", "version": VERSION, "sets": len(sets)})
        if path == "/api/sets" and self.command == "GET":
            return self._json(200, {"sets": [{"slug": s["slug"], "name": s["name"], "endpoint": "/api/sets/" + s["slug"],
                                              "questions": {k: (v or {}).get("type") for k, v in (s.get("questions") or {}).items()}} for s in sets]})
        if path.startswith("/api/sets/") and self.command == "GET":
            slug = urllib.parse.unquote(path[len("/api/sets/"):])
            s = next((x for x in sets if x.get("slug") == slug), None)
            if not s:
                return self._json(404, {"error": f'No set called "{slug}". GET /api/sets lists them.'})
            return self._json(200, {"slug": s["slug"], "name": s["name"], "questions": s.get("questions") or {}, "thresholds": s.get("thresholds") or {},
                                    "threshold": cfg.get("thr") or 0.8, "endpoint": "/api/sets/" + s["slug"], "webhook": bool(s.get("webhook"))})
        if path == "/api/requests" and self.command == "GET":
            return self._requests()
        if path.startswith("/api/requests/") and self.command == "GET":
            rid = urllib.parse.unquote(path[len("/api/requests/"):])
            with LOG_LOCK:
                log_items()
                it = _log_by_id.get(rid)
                if it:
                    return self._json(200, public_item(it))
            return self._json(404, {"error": f'No request with the id "{rid}". Zex keeps requests only while "keep requests for review" is on.'})
        if not (path.startswith("/api/sets/") and self.command == "POST"):
            return self._json(404, {"error": 'Not found. POST /api/sets/<set> with {"text": "..."}, GET /api/sets, or GET /api/requests/<id>.'})
        slug = urllib.parse.unquote(path[len("/api/sets/"):])
        s = next((x for x in sets if x.get("slug") == slug), None)
        if not s:
            return self._json(404, {"error": f'No set called "{slug}". GET /api/sets lists them.'})
        conn = cfg.get("connection") or {}
        if not s.get("route") and not conn.get("url"):
            return self._json(503, {"error": "Zex has no connection to answer with. Open Zex and pick one in Connections."})
        raw = self._raw
        body: dict = {}
        if self.headers.get("Content-Type", "").startswith("application/json") or raw.strip().startswith(b"{"):
            try:
                body = json.loads(raw or b"{}")
            except ValueError:
                return self._json(400, {"error": 'The body isn\'t valid JSON. Send {"text": "..."}.'})
        elif raw.strip():
            body = {"text": raw.decode("utf-8", "replace")}
        model = body.get("model") or cfg.get("model") or ""

        def one(state, ref=""):
            item = {"id": new_id(), "at": int(time.time() * 1000), "set": slug, "model": model, "state": state,
                    "all_auto": False, "audit_rate": audit_rate(cfg)}
            if ref:
                item["ref"] = str(ref)[:200]
            rr = run_route(cfg, s, model, state)
            item.update(stages=rr["stages"], by=rr["by"], cost=rr["cost"], baseline=rr["baseline"], ms=rr["ms"], route_len=len(s.get("route") or []))
            if "error" in rr:
                item["error"] = rr["error"]
                res = {"id": item["id"], "error": rr["error"]}
            else:
                all_auto = rr["all_auto"]
                item.update(model=rr["model"], decisions=rr["decisions"], answers=rr["answers"], all_auto=all_auto)
                if all_auto and log_on(cfg) and secrets.randbelow(1_000_000) < item["audit_rate"] * 1_000_000:
                    item["audit"] = True
                res = {"id": item["id"], "set": slug, "model": item["model"], "status": "auto" if all_auto else "review",
                       "decisions": rr["decisions"], "all_auto": all_auto, "answers": rr["answers"], "final": public_item(item).get("final")}
                if item.get("ref"):
                    res["ref"] = item["ref"]
                if s.get("webhook"):
                    res["webhook"] = True
                    if all_auto and s.get("events") == "all" and valid_hook_url(s["webhook"]):
                        queue_hook(s, "answer.auto", item["id"], public_item(item))
                if len(s.get("route") or []) > 1:
                    by = rr["stages"][rr["by"]] if rr["by"] >= 0 else None
                    res.update(route=rr["stages"], answered_by=f"{by['name']} · {by['model']}" if by else "person", cost=rr["cost"])
            if log_on(cfg):
                record(item)
            return res

        if body.get("items"):
            if len(body["items"]) > 100:
                return self._json(400, {"error": "Send at most 100 items at once."})
            refs = body.get("refs") or []
            return self._json(200, {"set": slug, "results": [{**one(it, refs[i] if i < len(refs) else ""), "state": it} for i, it in enumerate(body["items"])]})
        if "text" in body or "state" in body:
            res = one(body["text"] if "text" in body else body["state"], body.get("ref") or "")
            return self._json(502 if "error" in res else 200, {"id": res["id"], "error": res["error"]} if "error" in res else res)
        return self._json(400, {"error": 'Send {"text": "..."}, {"state": ...} or {"items": [...]}.'})

    _raw = b""

    def _requests(self) -> None:
        """GET /api/requests?set=&status=&since=&limit=, newest first. since is a time (unix
        milliseconds or RFC 3339); only requests that changed after it are listed."""
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        get = lambda k: (q.get(k) or [""])[0]
        try:
            limit = max(1, min(500, int(get("limit") or 50)))
        except ValueError:
            limit = 50
        since = 0
        if get("since"):
            v = get("since")
            try:
                since = int(v)
            except ValueError:
                try:
                    since = int(datetime.datetime.fromisoformat(v.replace("Z", "+00:00")).timestamp() * 1000)
                except ValueError:
                    return self._json(400, {"error": "since is a time: unix milliseconds or RFC 3339, like 2026-01-31T09:00:00Z."})
        out = []
        with LOG_LOCK:
            for it in reversed(log_items()):
                if len(out) >= limit:
                    break
                if get("set") and it.get("set") != get("set"):
                    continue
                if get("status") and public_status(it) != get("status"):
                    continue
                changed = (it.get("review") or {}).get("at") or it.get("at", 0)
                if since and changed <= since:
                    continue
                out.append(public_item(it))
        return self._json(200, {"requests": out})

    def _hooks(self) -> None:
        if not self._from_page():
            return
        with HOOK_LOCK:
            items = [{k: v for k, v in d.items() if k != "busy"} for d in reversed(hook_list()[-50:])]
        return self._json(200, {"deliveries": items})

    def _hook_send(self, test: bool) -> None:
        """Sends now and reports how it went: a ping to a set's webhook, or a delivery again."""
        if not self._from_page():
            return
        try:
            body = json.loads(self._raw or b"{}")
        except ValueError:
            body = {}
        if test:
            s = hook_for(body.get("set") or "")
            if not s:
                return self._json(400, {"error": "Zex: that set has no webhook URL"})
            d = queue_hook(s, "ping", "", {"set": s["slug"], "name": s.get("name", "")}, "sending")
        else:
            with HOOK_LOCK:
                d = next((x for x in hook_list() if x["id"] == body.get("id")), None)
                busy = bool(d) and (d.get("busy") or d.get("state") == "sending")
                if d and not busy:
                    d["state"] = "sending"
            if not d:
                return self._json(404, {"error": "Zex: no delivery with that id"})
            if busy:
                return self._json(409, {"error": "Zex: that delivery is being sent now"})
        send_hook(d, False)
        with HOOK_LOCK:
            return self._json(200, {"delivery": {k: v for k, v in d.items() if k != "busy"}})

    def _log(self) -> None:
        if not self._from_page():
            return
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        get = lambda k, d: (q.get(k) or [d])[0]
        want_set, status = get("set", ""), get("status", "")
        offset = int(get("offset", "0") or 0)
        limit = int(get("limit", "50") or 0)
        counts = {"waiting": 0, "spot": 0, "reviewed": 0, "error": 0, "auto": 0, "all": 0}
        match = []
        with LOG_LOCK:
            for it in reversed(log_items()):
                if want_set and it.get("set") != want_set:
                    continue
                st = item_status(it)
                counts[st] += 1
                counts["all"] += 1
                if status in ("", "all", st):
                    match.append(it)
            page = match[offset:] if limit < 0 else match[offset:offset + limit]
            return self._json(200, {"items": json.loads(json.dumps(page)), "total": len(match), "counts": counts})

    def _review(self) -> None:
        if not self._from_page():
            return
        try:
            body = json.loads(self._raw or b"{}")
        except ValueError:
            body = {}
        if not body.get("id") or not body.get("labels"):
            return self._json(400, {"error": "Zex: send an id and labels"})
        with LOG_LOCK:
            log_items()
            it = _log_by_id.get(body["id"])
            if not it:
                return self._json(404, {"error": "Zex: no request with that id"})
            before = it.get("review")
            it["review"] = {"at": int(time.time() * 1000), "labels": body["labels"],
                            "checked": int(body.get("checked") or 0), "agreed": int(body.get("agreed") or 0)}
            _append(REVIEW_FILE, {"id": it["id"], "review": it["review"]})
            after_review(it, before)
            return self._json(200, {"ok": True, "item": it})

    def _stats(self) -> None:
        if not self._from_page():
            return
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        days = int((q.get("days") or ["14"])[0] or 14)
        days = days if 0 < days <= 90 else 14
        start = datetime.datetime.combine(datetime.date.today(), datetime.time()) - datetime.timedelta(days=days - 1)
        labels = [(start + datetime.timedelta(days=i)).strftime("%Y-%m-%d") for i in range(days)]
        keys = ("requests", "auto", "errors", "audit_checked", "audit_agreed", "review_checked", "review_agreed", "spent", "baseline")
        sets: dict = {}
        waiting: dict = {}
        spot: dict = {}
        routes: dict = {}
        with LOG_LOCK:
            for it in log_items():
                st, name = item_status(it), it.get("set")
                if st == "waiting":
                    waiting[name] = waiting.get(name, 0) + 1
                elif st == "spot":
                    spot[name] = spot.get(name, 0) + 1
                d = (datetime.datetime.fromtimestamp(it.get("at", 0) / 1000) - start).days
                if not 0 <= d < days:
                    continue
                s = sets.setdefault(name, {k: [0] * days for k in keys})
                s["requests"][d] += 1
                s["spent"][d] += float(it.get("cost") or 0)
                s["baseline"][d] += float(it.get("baseline") or 0)
                stages = it.get("stages") or []
                if int(it.get("route_len") or 0) > 1 and d >= days - 7 and not it.get("error"):
                    by = it.get("by", -1)
                    label = f"{stages[by]['name']} · {stages[by]['model']}" if it.get("all_auto") and 0 <= by < len(stages) else "person"
                    routes.setdefault(name, {})
                    routes[name][label] = routes[name].get(label, 0) + 1
                if it.get("error"):
                    s["errors"][d] += 1
                    continue
                if it.get("all_auto"):
                    s["auto"][d] += 1
                rv = it.get("review")
                if rv:
                    side = "audit" if it.get("all_auto") else "review"
                    s[side + "_checked"][d] += int(rv.get("checked") or 0)
                    s[side + "_agreed"][d] += int(rv.get("agreed") or 0)
        return self._json(200, {"days": labels, "sets": sets, "waiting": waiting, "spot": spot, "routes": routes})

    def _points(self) -> None:
        """One small row per recent request for the Home atlas:
        [time, set, weakest confidence, status, stage that finished it, milliseconds].
        Status: 0 auto, 1 waiting, 2 spot check, 3 reviewed and right, 4 reviewed and corrected, 5 error."""
        if not self._from_page():
            return
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        days = int((q.get("days") or ["7"])[0] or 7)
        since = (time.time() - (days if 0 < days <= 90 else 7) * 86400) * 1000
        codes = {"auto": 0, "waiting": 1, "spot": 2, "reviewed": 3, "error": 5}
        sets: list = []
        points: list = []
        with LOG_LOCK:
            for it in reversed(log_items()):
                if it.get("at", 0) < since or len(points) >= 4000:
                    break
                if it.get("set") not in sets:
                    sets.append(it.get("set"))
                confs = [d.get("confidence", 1) for d in (it.get("decisions") or {}).values() if isinstance(d, dict)]
                weakest = 0 if it.get("error") else min(confs + [1.0])
                code = codes[item_status(it)]
                rv = it.get("review") or {}
                if rv and int(rv.get("agreed") or 0) < int(rv.get("checked") or 0):
                    code = 4
                by = it.get("by", -1) if it.get("all_auto") else -1
                points.append([it.get("at"), sets.index(it.get("set")), weakest, code, by, it.get("ms", 0)])
        return self._json(200, {"sets": sets, "points": points})

    def _log_clear(self) -> None:
        global _log
        if not self._from_page():
            return
        try:
            want = (json.loads(self._raw or b"{}") or {}).get("set") or ""
        except ValueError:
            want = ""
        with LOG_LOCK:
            items = log_items()
            keep = [it for it in items if want and it.get("set") != want]
            for it in items:
                if not (want and it.get("set") != want):
                    _log_by_id.pop(it["id"], None)
            _log = keep
            _compact()
            return self._json(200, {"ok": True, "left": len(_log)})

    def do_GET(self):
        if self.path.startswith("/zex/health"):
            return self._json(200, {"ok": True, "app": "zex", "relay": True})
        if self.path.startswith("/zex/proxy"):
            return self._relay()
        if self.path.startswith("/zex/api-config"):
            return self._api_config()
        if self.path.startswith("/zex/log"):
            return self._log()
        if self.path.startswith("/zex/stats"):
            return self._stats()
        if self.path.startswith("/zex/points"):
            return self._points()
        if self.path.startswith("/zex/catalog"):
            return self._catalog()
        if self.path.startswith("/zex/hooks"):
            return self._hooks()
        if self.path.startswith("/tests/fake-ollaya/"):
            return self._fake_ollaya()
        if self.path.startswith("/tests/fake-hook"):
            return self._fake_hook()
        if self.path == "/api" or self.path.startswith("/api/") or self.path.startswith("/api?"):
            return self._api()
        return super().do_GET()

    def do_DELETE(self):
        self._raw = self.rfile.read(min(int(self.headers.get("Content-Length") or 0), 16 << 20))
        if self.path.startswith("/zex/proxy"):
            return self._relay()
        if self.path.startswith("/tests/fake-ollaya/"):
            return self._fake_ollaya()
        return self._json(405, {"error": "method not allowed"})

    def do_POST(self):
        # Read the body before anything can answer early: replying with unread request data
        # left on the socket can make the connection reset and lose the reply.
        self._raw = self.rfile.read(min(int(self.headers.get("Content-Length") or 0), 16 << 20))
        if self.path.startswith("/zex/proxy"):
            return self._relay()
        if self.path.startswith("/tests/fake-ollaya/"):
            return self._fake_ollaya()
        if self.path.startswith("/tests/fake-hook"):
            return self._fake_hook()
        if self.path.startswith("/zex/api-config"):
            return self._api_config()
        if self.path.startswith("/zex/log/clear"):
            return self._log_clear()
        if self.path.startswith("/zex/review"):
            return self._review()
        if self.path.startswith("/zex/hooks/test"):
            return self._hook_send(True)
        if self.path.startswith("/zex/hooks/retry"):
            return self._hook_send(False)
        if self.path == "/api" or self.path.startswith("/api/"):
            return self._api()
        return self._json(405, {"error": "method not allowed"})


def main() -> None:
    ap = argparse.ArgumentParser(description="Serve the Zex web UI with its local relay.")
    ap.add_argument("--port", type=int, default=int(os.environ.get("PORT", "8765")))
    ap.add_argument("--open", action="store_true", help="open the app in your browser")
    args = ap.parse_args()
    Handler.allowed_origins = {f"http://127.0.0.1:{args.port}", f"http://localhost:{args.port}"}
    server = http.server.ThreadingHTTPServer(("127.0.0.1", args.port), partial(Handler, directory=WEB))
    start_hooks()
    url = f"http://127.0.0.1:{args.port}/#/home"
    print(f"Zex on {url}  (Ctrl+C to stop)", flush=True)
    if args.open:
        threading.Timer(0.6, lambda: webbrowser.open(url)).start()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
