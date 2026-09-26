"""fornax's laya server.

The laya package is a library, not a server — this shim loads a checkpoint
directory and answers TypeSafe's /v1/systemone shape on loopback behind
fornax's generated key. Stdlib only, so the venv carries just laya's own
dependencies.
"""

import argparse
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MAX_BODY = 2 * 1024 * 1024


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True, help="checkpoint dir holding rl_agent_config.json")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--alias", required=True)
    parser.add_argument("--key-file", required=True)
    args = parser.parse_args()

    with open(args.key_file) as f:
        key = f.read().strip()

    import laya
    agent = laya.Agent(args.model)

    def reply(handler, code, obj):
        body = json.dumps(obj).encode()
        handler.send_response(code)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(body)))
        handler.end_headers()
        handler.wfile.write(body)

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def authed(self):
            return self.headers.get("Authorization") == "Bearer " + key

        def log_message(self, *args):
            pass

        def do_GET(self):
            if not self.authed():
                return reply(self, 401, {"error": {"message": "missing or invalid api key"}})
            if self.path == "/health":
                return reply(self, 200, {"status": "ok"})
            if self.path == "/v1/models":
                data = [{"id": args.alias, "object": "model", "aliases": [args.alias]}]
                return reply(self, 200, {"object": "list", "data": data})
            reply(self, 404, {"error": {"message": "not found"}})

        def do_POST(self):
            if not self.authed():
                return reply(self, 401, {"error": {"message": "missing or invalid api key"}})
            if self.path != "/v1/systemone":
                return reply(self, 404, {"error": {"message": "not found"}})
            try:
                length = int(self.headers.get("Content-Length") or 0)
            except ValueError:
                length = 0
            if length <= 0 or length > MAX_BODY:
                return reply(self, 400, {"error": {"message": "bad content length"}})
            try:
                body = json.loads(self.rfile.read(length))
            except Exception:
                return reply(self, 400, {"error": {"message": "the request was not json"}})
            started = time.time()
            try:
                out = agent.system_one(body.get("state", ""), body.get("questions") or {})
            except Exception as e:
                return reply(self, 500, {"error": {"message": str(e)}})
            out["model"] = args.alias
            out["latency_ms"] = round((time.time() - started) * 1000, 1)
            reply(self, 200, out)

    ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
