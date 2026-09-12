"""Read-only checks against the public gateway (no credentials or inference)."""
import argparse
import http.client
from urllib.parse import urlsplit

parser = argparse.ArgumentParser()
parser.add_argument("url", help="Gateway origin, e.g. https://mcp.example.com")
args = parser.parse_args()
url = urlsplit(args.url)
if url.scheme not in ("http", "https") or not url.hostname or url.path not in ("", "/"):
    parser.error("supply an HTTP(S) origin without a path")
connection = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
cases = [("POST", "/mcp", 401), ("GET", "/v1/models", 401)]
for path in ("/", "/login", "/setup", "/settings", "/users", "/admin/mcp", "/oauth/callback", "/healthz", "/static/app.js", "/mcp/../settings", "/v1/../settings"):
    cases.extend((method, path, 404) for method in ("GET", "POST"))
for method, path, expected in cases:
    conn = connection(url.hostname, url.port, timeout=15)
    conn.request(method, path)
    response = conn.getresponse()
    response.read()
    conn.close()
    assert response.status == expected, f"{method} {path}: {response.status}, expected {expected}"
print(f"PASS: {len(cases)} gateway authentication and administration isolation checks")
