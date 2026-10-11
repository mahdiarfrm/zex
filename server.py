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
import hmac
import http.server
import json
import os
import re
import secrets
import sys
import threading
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from functools import partial

HERE = os.path.dirname(os.path.abspath(__file__))
WEB = os.path.join(HERE, "web")
TESTS = os.path.join(HERE, "tests")
ALLOWED_PATH = re.compile(r"/(v1/(systemone|decisions|models)|api/(decide|tags|ps))/?$")
LOOPBACK = {"localhost", "127.0.0.1", "::1"}

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


def call_model(conn: dict, model: str, state, questions) -> dict:
    """One request to the connection: Ollaya first tries /api/decide, then /v1/systemone."""
    body = json.dumps({"model": model, "state": state, "questions": questions}).encode()
    base = (conn.get("url") or "").rstrip("/")
    name = conn.get("name") or base
    paths = ["/api/decide", "/v1/systemone"] if conn.get("kind") == "ollaya" else ["/v1/systemone"]
    for i, path in enumerate(paths):
        req = urllib.request.Request(base + path, data=body, method="POST")
        req.add_header("Content-Type", "application/json")
        req.add_header("Accept", "application/json")
        req.add_header("User-Agent", "Zex/0.1")
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
    server_version = "Zex/0.1"
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
        body = self._raw if self.command == "POST" else None
        req = urllib.request.Request(target, data=body, method=self.command)
        req.add_header("Content-Type", self.headers.get("Content-Type", "application/json"))
        req.add_header("Accept", "application/json")
        req.add_header("User-Agent", "Zex/0.1")
        key = self.headers.get("X-Zex-Key")
        if key:
            req.add_header("Authorization", "Bearer " + key)
        opener = DIRECT if t.hostname in LOOPBACK else DEFAULT
        try:
            with opener.open(req, timeout=180) as r:
                status, data, ctype = r.status, r.read(), r.headers.get("Content-Type", "application/json")
        except urllib.error.HTTPError as e:
            status, data, ctype = e.code, e.read(), e.headers.get("Content-Type", "application/json")
        except Exception as e:  # unreachable host, timeout, TLS, DNS
            return self._json(502, {"error": f"Zex relay could not reach {t.netloc}: {e}", "code": "UNREACHABLE"})
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

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
                for k in ("model", "thr", "connection", "sets"):
                    if cfg["enabled"]:
                        cfg[k] = body.get(k)
                    else:
                        cfg.pop(k, None)
                try:
                    api_save(cfg)
                except OSError as e:
                    return self._json(500, {"error": f"Zex could not save its API settings: {e}"})
            return self._json(200, {"enabled": bool(cfg.get("enabled")), "token": cfg.get("token", "")})

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
            return self._json(200, {"ok": True, "app": "zex", "version": "0.1.0", "sets": len(sets)})
        if path == "/api/sets" and self.command == "GET":
            return self._json(200, {"sets": [{"slug": s["slug"], "name": s["name"], "endpoint": "/api/sets/" + s["slug"],
                                              "questions": {k: (v or {}).get("type") for k, v in (s.get("questions") or {}).items()}} for s in sets]})
        if not (path.startswith("/api/sets/") and self.command == "POST"):
            return self._json(404, {"error": 'Not found. POST /api/sets/<set> with {"text": "..."}, or GET /api/sets.'})
        slug = urllib.parse.unquote(path[len("/api/sets/"):])
        s = next((x for x in sets if x.get("slug") == slug), None)
        if not s:
            return self._json(404, {"error": f'No set called "{slug}". GET /api/sets lists them.'})
        conn = cfg.get("connection") or {}
        if not conn.get("url"):
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

        def one(state):
            try:
                data = call_model(conn, model, state, s.get("questions") or {})
            except UpstreamError as e:
                return {"error": str(e)}
            answers = data.get("answers") or {}
            dec, all_auto = decide(answers, s.get("thresholds") or {}, cfg.get("thr") or 0.8)
            return {"set": slug, "model": data.get("model") or model, "decisions": dec, "all_auto": all_auto, "answers": answers}

        if body.get("items"):
            if len(body["items"]) > 100:
                return self._json(400, {"error": "Send at most 100 items at once."})
            return self._json(200, {"set": slug, "results": [{**one(it), "state": it} for it in body["items"]]})
        if "text" in body or "state" in body:
            res = one(body["text"] if "text" in body else body["state"])
            return self._json(502 if "error" in res else 200, {"error": res["error"]} if "error" in res else res)
        return self._json(400, {"error": 'Send {"text": "..."}, {"state": ...} or {"items": [...]}.'})

    _raw = b""

    def do_GET(self):
        if self.path.startswith("/zex/health"):
            return self._json(200, {"ok": True, "app": "zex", "relay": True})
        if self.path.startswith("/zex/proxy"):
            return self._relay()
        if self.path.startswith("/zex/api-config"):
            return self._api_config()
        if self.path == "/api" or self.path.startswith("/api/") or self.path.startswith("/api?"):
            return self._api()
        return super().do_GET()

    def do_POST(self):
        # Read the body before anything can answer early: replying with unread request data
        # left on the socket can make the connection reset and lose the reply.
        self._raw = self.rfile.read(min(int(self.headers.get("Content-Length") or 0), 16 << 20))
        if self.path.startswith("/zex/proxy"):
            return self._relay()
        if self.path.startswith("/zex/api-config"):
            return self._api_config()
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
