#!/usr/bin/env python3
"""Small stdio MCP adapters for Bugbase's existing business API accounts."""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


PROTOCOL_VERSION = "2025-06-18"
MAX_RESPONSE_BYTES = 8 * 1024 * 1024
# Files cross between Toolmux and the Hermes agents only through this directory.
EXCHANGE_DIR = Path(os.environ.get("TOOLMUX_EXCHANGE_DIR", "/var/lib/hermes/shared/exchange"))
RETRY_STATUSES = {429, 503}
RETRY_ATTEMPTS = 4
RETRY_MAX_WAIT = 20.0

XPAYROLL_OPERATIONS = {
    "people.create": ("/api/people", "POST", "people", "create"),
    "people.edit": ("/api/people", "POST", "people", "edit"),
    "people.view": ("/api/people", "POST", "people", "view"),
    "people.set_salary": ("/api/people", "POST", "people", "set-salary"),
    "people.dismiss": ("/api/people", "POST", "people", "dismiss"),
    "payroll.view": ("/api/payroll", "POST", "payroll", "view-payroll"),
    "payroll.add_additions": ("/api/payroll", "POST", "payroll", "add-additions"),
    "payroll.add_deduction": ("/api/payroll", "POST", "payroll", "add-deduction"),
    "payroll.reset_modifications": ("/api/payroll", "POST", "payroll", "reset-modifications"),
    "payroll.pause_resume": ("/api/payroll", "POST", "payroll", "do-not-pay"),
    "contractor.create": ("/api/contractorPayment", "POST", "contractor-payment", "create"),
    "contractor.delete": ("/api/contractorPayment", "POST", "contractor-payment", "delete"),
    "contractor.list_pending": ("/api/contractorPayment", "POST", "contractor-payment", "list-pending"),
    "contractor.status": ("/api/contractorPayment", "POST", "contractor-payment", "get-status"),
    "attendance.modify": ("/api/att", "POST", "attendance", "modify"),
    "attendance.fetch": ("/api/att", "POST", "attendance", "fetch"),
    "attendance.edit": ("/api/att", "PATCH", "attendance", "modify"),
    "advance_salary.create": ("/api/advanceSalary", "POST", "advance-salary", "create"),
}


def require_env(*names: str) -> list[str]:
    values = [os.environ.get(name, "").strip() for name in names]
    if any(not value for value in values):
        raise ValueError("The connection credential is incomplete")
    return values


def relative_endpoint(value: Any) -> str:
    raw_endpoint = str(value or "").strip()
    if raw_endpoint.lower().startswith(("http://", "https://")):
        raise ValueError("endpoint must be a relative API path such as 'Deals?fields=Deal_Name,Stage&per_page=200' (the base URL is configured on the connection)")
    if raw_endpoint.startswith(("//", "\\")):
        raise ValueError("endpoint must be a relative API path")
    endpoint = raw_endpoint.lstrip("/")
    parsed = urllib.parse.urlsplit(endpoint)
    if (
        not endpoint
        or ".." in endpoint
        or parsed.scheme
        or parsed.netloc
        or any(character.isspace() for character in endpoint)
    ):
        raise ValueError("endpoint must be a relative API path")
    return endpoint


def exchange_path(name: Any) -> Path:
    cleaned = str(name or "").strip().replace("\\", "/")
    if not cleaned or cleaned.startswith("/") or ".." in cleaned.split("/"):
        raise ValueError("download_to/upload_file must be a file name relative to the exchange directory")
    full = EXCHANGE_DIR / cleaned
    full.parent.mkdir(parents=True, exist_ok=True)
    # The service umask is 0077; folders must stay enterable for the agents.
    folder = full.parent
    while folder != EXCHANGE_DIR and EXCHANGE_DIR in folder.parents:
        try:
            folder.chmod(0o2775)
        except OSError:
            pass
        folder = folder.parent
    return full


def multipart_body(fields: dict[str, Any], file_field: str, path: Path) -> tuple[bytes, str]:
    import mimetypes
    import uuid

    boundary = "toolmux-" + uuid.uuid4().hex
    parts: list[bytes] = []
    for key, value in (fields or {}).items():
        text = value if isinstance(value, str) else json.dumps(value)
        parts.append(f"--{boundary}\r\nContent-Disposition: form-data; name=\"{key}\"\r\n\r\n{text}\r\n".encode("utf-8"))
    mime = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
    parts.append(
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"{file_field}\"; filename=\"{path.name}\"\r\nContent-Type: {mime}\r\n\r\n".encode("utf-8")
        + path.read_bytes()
        + b"\r\n"
    )
    parts.append(f"--{boundary}--\r\n".encode("utf-8"))
    return b"".join(parts), f"multipart/form-data; boundary={boundary}"


def retry_after_seconds(response: Any, attempt: int) -> float:
    value = ""
    try:
        value = response.headers.get("Retry-After", "") or ""
    except AttributeError:
        pass
    try:
        return float(value)
    except ValueError:
        return float(2 ** attempt)


def request_json(
    url: str,
    method: str = "GET",
    *,
    headers: dict[str, str] | None = None,
    body: Any = None,
    download_to: Any = None,
    upload: tuple[dict[str, Any], str, Path] | None = None,
) -> tuple[int, Any]:
    request_headers = {
        "Accept": "application/json",
        "User-Agent": "Mozilla/5.0 (compatible; Toolmux/1.0; +https://github.com/shero4/toolmux)",
        **(headers or {}),
    }
    if upload is not None:
        data, content_type = multipart_body(*upload)
        request_headers["Content-Type"] = content_type
    else:
        data = None if body is None else json.dumps(body).encode("utf-8")
        if data is not None:
            request_headers["Content-Type"] = "application/json"
    for attempt in range(RETRY_ATTEMPTS):
        request = urllib.request.Request(url, data=data, headers=request_headers, method=method)
        try:
            response = urllib.request.urlopen(request, timeout=45)
        except urllib.error.HTTPError as error:
            response = error
        # 429 and 503 mean the request was not processed, so replaying is safe
        # even for writes; anything else is returned as-is.
        if response.status in RETRY_STATUSES and attempt < RETRY_ATTEMPTS - 1:
            wait = retry_after_seconds(response, attempt)
            with response:
                response.read(4096)
            if wait > RETRY_MAX_WAIT:
                break
            time.sleep(wait)
            continue
        break
    with response:
        raw = response.read(MAX_RESPONSE_BYTES + 1)
        if len(raw) > MAX_RESPONSE_BYTES:
            raise ValueError("upstream response exceeded 8 MiB")
        content_type = ""
        try:
            content_type = response.headers.get("Content-Type", "") or ""
        except AttributeError:
            pass
        if download_to:
            target = exchange_path(download_to)
            target.write_bytes(raw)
            target.chmod(0o644)
            return response.status, {"saved_file": str(target), "bytes": len(raw), "contentType": content_type}
        try:
            value = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            if "json" not in content_type and "text" not in content_type and raw:
                value = {
                    "binary": True,
                    "contentType": content_type,
                    "bytes": len(raw),
                    "hint": "non-text response; repeat the call with download_to=<file name> to save it in the exchange directory",
                }
            else:
                value = raw.decode("utf-8", errors="replace")
        return response.status, value


def zammad_call(arguments: dict[str, Any]) -> tuple[int, Any]:
    base_url, token = require_env("ZAMMAD_BASE_URL", "ZAMMAD_TOKEN")
    method = str(arguments.get("method", "GET")).upper()
    if method not in {"GET", "POST", "PUT", "PATCH", "DELETE"}:
        raise ValueError("unsupported method")
    endpoint = relative_endpoint(arguments.get("endpoint"))
    # The base URL already ends in /api/v1; agents often repeat it.
    if endpoint.startswith("api/v1/"):
        endpoint = endpoint[len("api/v1/"):]
    upload = None
    if arguments.get("upload_file"):
        upload = (arguments.get("fields") or {}, str(arguments.get("file_field") or "File"), exchange_path(arguments["upload_file"]))
    return request_json(
        base_url.rstrip("/") + "/" + endpoint,
        method,
        headers={"Authorization": "Token token=" + token},
        body=arguments.get("body") if method in {"POST", "PUT", "PATCH"} else None,
        download_to=arguments.get("download_to"),
        upload=upload,
    )


@contextlib.contextmanager
def exclusive_lock(path: Path):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a+b") as handle:
        try:
            import fcntl
        except ImportError:  # pragma: no cover - production runs on Linux
            fcntl = None
        if fcntl is not None:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            if fcntl is not None:
                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


def bigin_access_token() -> tuple[str, str]:
    base_url, accounts_url, client_id, client_secret, refresh_token = require_env(
        "BIGIN_BASE_URL",
        "BIGIN_ACCOUNTS_URL",
        "BIGIN_CLIENT_ID",
        "BIGIN_CLIENT_SECRET",
        "BIGIN_REFRESH_TOKEN",
    )
    cache_key = hashlib.sha256(f"{accounts_url}\0{client_id}\0{refresh_token}".encode()).hexdigest()[:24]
    cache_root = Path(os.environ.get("TOOLMUX_TOKEN_CACHE_DIR", tempfile.gettempdir()))
    cache_path = cache_root / f"toolmux-bigin-token-{cache_key}.json"
    lock_path = cache_path.with_suffix(".lock")
    with exclusive_lock(lock_path):
        try:
            cached = json.loads(cache_path.read_text(encoding="utf-8"))
        except (FileNotFoundError, OSError, json.JSONDecodeError):
            cached = {}
        if cached.get("access_token") and float(cached.get("expires_at", 0)) > time.time() + 60:
            return base_url, str(cached["access_token"])

        body = urllib.parse.urlencode(
            {
                "grant_type": "refresh_token",
                "client_id": client_id,
                "client_secret": client_secret,
                "refresh_token": refresh_token,
            }
        ).encode("utf-8")
        request = urllib.request.Request(accounts_url.rstrip("/") + "/oauth/v2/token", data=body, method="POST")
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                result = json.loads(response.read(65537).decode("utf-8"))
        except urllib.error.HTTPError as error:
            detail = error.read(2048).decode("utf-8", errors="replace")
            raise ValueError(f"Bigin authorization refresh failed ({error.code}): {detail}") from error
        token = str(result.get("access_token", ""))
        if not token:
            raise ValueError("Bigin authorization could not be refreshed")
        cache_root.mkdir(parents=True, exist_ok=True)
        staged = cache_path.with_suffix(f".{os.getpid()}.tmp")
        staged.write_text(json.dumps({"access_token": token, "expires_at": time.time() + int(result.get("expires_in", 3600))}), encoding="utf-8")
        staged.chmod(0o600)
        staged.replace(cache_path)
        return base_url, token


def bigin_call(arguments: dict[str, Any]) -> tuple[int, Any]:
    base_url, token = bigin_access_token()
    method = str(arguments.get("method", "GET")).upper()
    if method not in {"GET", "POST", "PUT", "DELETE"}:
        raise ValueError("unsupported method")
    endpoint = relative_endpoint(arguments.get("endpoint"))
    upload = None
    if arguments.get("upload_file"):
        upload = (arguments.get("fields") or {}, str(arguments.get("file_field") or "file"), exchange_path(arguments["upload_file"]))
    return request_json(
        base_url.rstrip("/") + "/" + endpoint,
        method,
        headers={"Authorization": "Zoho-oauthtoken " + token},
        body=arguments.get("body") if method in {"POST", "PUT"} else None,
        download_to=arguments.get("download_to"),
        upload=upload,
    )


def xpayroll_call(arguments: dict[str, Any]) -> tuple[int, Any]:
    api_id, api_key = require_env("RAZORPAY_PAYROLL_API_ID", "RAZORPAY_PAYROLL_API_KEY")
    operation_name = str(arguments.get("operation", ""))
    if operation_name not in XPAYROLL_OPERATIONS:
        raise ValueError("unknown payroll operation")
    path, method, request_type, subtype = XPAYROLL_OPERATIONS[operation_name]
    payload: dict[str, Any] = {
        "auth": {"id": int(api_id) if api_id.isdigit() else api_id, "key": api_key},
        "request": {"type": request_type, "sub-type": subtype},
    }
    data = arguments.get("data")
    if data is not None:
        if not isinstance(data, dict):
            raise ValueError("data must be an object")
        payload["data"] = data
    return request_json("https://payroll.razorpay.com" + path, method, body=payload)


def tools(provider: str) -> list[dict[str, Any]]:
    common_body = {"type": "object", "additionalProperties": True}
    if provider == "zammad":
        return [{
            "name": "request",
            "title": "Zammad API request",
            "description": "Read or update the configured Bugbase Zammad account using a relative API endpoint (relative to /api/v1, e.g. 'tickets/search?query=state.name:open&limit=20').",
            "inputSchema": {
                "type": "object",
                "additionalProperties": False,
                "required": ["endpoint"],
                "properties": {
                    "method": {"type": "string", "enum": ["GET", "POST", "PUT", "PATCH", "DELETE"], "default": "GET"},
                    "endpoint": {"type": "string", "description": "Zammad API path relative to /api/v1, e.g. 'tickets/123' or 'ticket_articles/by_ticket/123'."},
                    "body": common_body,
                    "download_to": {"type": "string", "description": "File name to save the raw response into the shared exchange directory (attachments, PDFs). The result reports the saved path."},
                    "upload_file": {"type": "string", "description": "File name in the shared exchange directory to send as a multipart file upload."},
                    "file_field": {"type": "string", "description": "Multipart field name for upload_file (default: File)."},
                    "fields": {"type": "object", "additionalProperties": True, "description": "Extra multipart form fields sent with upload_file."},
                },
            },
        }]
    if provider == "bigin":
        return [{
            "name": "request",
            "title": "Zoho Bigin API request",
            "description": "Read or update the configured Bugbase Bigin CRM account using a relative API v2 endpoint. List calls need the 'fields' query parameter, e.g. 'Deals?fields=Deal_Name,Stage,Amount&per_page=200'.",
            "inputSchema": {
                "type": "object",
                "additionalProperties": False,
                "required": ["endpoint"],
                "properties": {
                    "method": {"type": "string", "enum": ["GET", "POST", "PUT", "DELETE"], "default": "GET"},
                    "endpoint": {"type": "string", "description": "Bigin API v2 path relative to the base, e.g. 'Deals?fields=Deal_Name,Stage&per_page=200' or 'Deals/123/Attachments'."},
                    "body": common_body,
                    "download_to": {"type": "string", "description": "File name to save the raw response into the shared exchange directory (attachments, PDFs). The result reports the saved path."},
                    "upload_file": {"type": "string", "description": "File name in the shared exchange directory to send as a multipart file upload."},
                    "file_field": {"type": "string", "description": "Multipart field name for upload_file (default: file)."},
                    "fields": {"type": "object", "additionalProperties": True, "description": "Extra multipart form fields sent with upload_file."},
                },
            },
        }]
    return [{
        "name": "request",
        "title": "RazorpayX Payroll request",
        "description": "Run one documented operation against the configured Bugbase RazorpayX Payroll account.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["operation"],
            "properties": {
                "operation": {"type": "string", "enum": sorted(XPAYROLL_OPERATIONS)},
                "data": common_body,
            },
        },
    }]


def call_tool(provider: str, arguments: dict[str, Any]) -> dict[str, Any]:
    handler = {"zammad": zammad_call, "bigin": bigin_call, "xpayroll": xpayroll_call}[provider]
    status, value = handler(arguments)
    failed = status < 200 or status >= 300 or (isinstance(value, dict) and bool(value.get("error")))
    text = json.dumps({"status": status, "response": value}, ensure_ascii=False)
    return {"content": [{"type": "text", "text": text}], "isError": failed}


def response(request_id: Any, *, result: Any = None, error: str = "") -> None:
    message: dict[str, Any] = {"jsonrpc": "2.0", "id": request_id}
    if error:
        message["error"] = {"code": -32603, "message": error}
    else:
        message["result"] = result
    print(json.dumps(message, separators=(",", ":")), flush=True)


def serve(provider: str) -> None:
    for line in sys.stdin:
        request: Any = {}
        try:
            request = json.loads(line)
            request_id = request.get("id")
            method = request.get("method")
            if request_id is None:
                continue
            if method in {"initialize", "server/discover"}:
                response(request_id, result={"protocolVersion": PROTOCOL_VERSION, "capabilities": {"tools": {}}, "serverInfo": {"name": provider + "-toolmux-adapter", "version": "1"}})
            elif method == "tools/list":
                response(request_id, result={"tools": tools(provider)})
            elif method == "tools/call":
                params = request.get("params") or {}
                if params.get("name") != "request":
                    response(request_id, error="unknown tool")
                else:
                    response(request_id, result=call_tool(provider, params.get("arguments") or {}))
            else:
                response(request_id, error="unsupported method")
        except Exception as error:
            response(request.get("id") if isinstance(request, dict) else None, error=str(error)[:300])


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("provider", choices=["zammad", "bigin", "xpayroll"])
    serve(parser.parse_args().provider)


if __name__ == "__main__":
    main()
