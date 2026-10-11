#!/usr/bin/env python3
"""Zex local server: serves the web UI and relays requests to decision-model servers.

Most model servers (TypeSafe, Vercel, OpenJev, jeff) send no CORS headers for a page on
127.0.0.1, so the browser cannot call them directly. The relay at /zex/proxy forwards a
request to a decision-model API path, only when it comes from this page, and the server
binds to 127.0.0.1 so nothing else on the network can use it.

Usage:
    python3 server.py [--port 8765] [--open]

Standard library only. Python 3.9 or newer.
"""

from __future__ import annotations

import argparse
import http.server
import json
import os
import re
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
        body = None
        if self.command == "POST":
            body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
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

    def do_GET(self):
        if self.path.startswith("/zex/health"):
            return self._json(200, {"ok": True, "app": "zex", "relay": True})
        if self.path.startswith("/zex/proxy"):
            return self._relay()
        return super().do_GET()

    def do_POST(self):
        if self.path.startswith("/zex/proxy"):
            return self._relay()
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
