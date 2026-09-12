"""Harmless local MCP/HTTP/command fixture. No dependencies or external access."""
import argparse
import json
import secrets
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

NONCE = secrets.token_hex(8)


def rpc(request):
    method = request.get("method")
    if method == "initialize":
        result = {"protocolVersion": "2025-11-25", "capabilities": {"tools": {}},
                  "serverInfo": {"name": "toolmux-e2e", "version": "1"}}
    elif method == "tools/list":
        result = {"tools": [{"name": "multiply", "description": "Multiply two integers using the local test service.",
                            "inputSchema": {"type": "object", "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}}, "required": ["a", "b"]}},
                           {"name": "nonce", "description": "Read the random verification code from the local test service.", "inputSchema": {"type": "object", "properties": {}}}]}
    elif method == "tools/call":
        p = request["params"]
        if p["name"] == "multiply":
            value = {"product": int(p["arguments"]["a"]) * int(p["arguments"]["b"])}
        elif p["name"] == "nonce":
            value = {"verification_code": NONCE}
        else:
            return {"jsonrpc": "2.0", "id": request.get("id"), "error": {"code": -32601, "message": "Unknown tool"}}
        result = {"content": [{"type": "text", "text": json.dumps(value)}], "isError": False}
    else:
        result = {}
    return {"jsonrpc": "2.0", "id": request.get("id"), "result": result}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send(self, value, status=200):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path in ("/", "/health"):
            self.send({"status": "ok"})
        elif self.path == "/status":
            self.send({"source": "http-fixture", "verification_code": NONCE})
        else:
            self.send({"error": "not found"}, 404)

    def do_HEAD(self):
        self.send_response(200 if self.path in ("/", "/health", "/status") else 404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        size = int(self.headers.get("Content-Length", 0))
        if size > 65536:
            return self.send({"error": "too large"}, 413)
        request = json.loads(self.rfile.read(size))
        self.send(rpc(request), 200 if "id" in request else 202)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["http", "stdio", "command"])
    parser.add_argument("--port", type=int, default=8082)
    args = parser.parse_args()
    if args.mode == "http":
        print(f"Local E2E fixture listening on 127.0.0.1:{args.port}", flush=True)
        ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()
    elif args.mode == "stdio":
        for line in sys.stdin:
            request = json.loads(line)
            if "id" in request:
                print(json.dumps(rpc(request)), flush=True)
    else:
        value = json.load(sys.stdin)
        print(json.dumps({"source": "command-fixture", "sum": int(value["a"]) + int(value["b"])}))
