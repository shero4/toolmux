#!/usr/bin/env python3
"""Small stdio MCP adapters for Bugbase's existing business API accounts."""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


PROTOCOL_VERSION = "2025-06-18"
MAX_RESPONSE_BYTES = 8 * 1024 * 1024

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


def request_json(
    url: str,
    method: str = "GET",
    *,
    headers: dict[str, str] | None = None,
    body: Any = None,
) -> tuple[int, Any]:
    data = None if body is None else json.dumps(body).encode("utf-8")
    request_headers = {"Accept": "application/json", **(headers or {})}
    if data is not None:
        request_headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=data, headers=request_headers, method=method)
    try:
        response = urllib.request.urlopen(request, timeout=45)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        raw = response.read(MAX_RESPONSE_BYTES + 1)
        if len(raw) > MAX_RESPONSE_BYTES:
            raise ValueError("upstream response exceeded 8 MiB")
        try:
            value = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            value = raw.decode("utf-8", errors="replace")
        return response.status, value


def zammad_call(arguments: dict[str, Any]) -> tuple[int, Any]:
    base_url, token = require_env("ZAMMAD_BASE_URL", "ZAMMAD_TOKEN")
    method = str(arguments.get("method", "GET")).upper()
    if method not in {"GET", "POST", "PUT", "PATCH", "DELETE"}:
        raise ValueError("unsupported method")
    endpoint = relative_endpoint(arguments.get("endpoint"))
    return request_json(
        base_url.rstrip("/") + "/" + endpoint,
        method,
        headers={"Authorization": "Token token=" + token},
        body=arguments.get("body") if method in {"POST", "PUT", "PATCH"} else None,
    )


def bigin_access_token() -> tuple[str, str]:
    base_url, accounts_url, client_id, client_secret, refresh_token = require_env(
        "BIGIN_BASE_URL",
        "BIGIN_ACCOUNTS_URL",
        "BIGIN_CLIENT_ID",
        "BIGIN_CLIENT_SECRET",
        "BIGIN_REFRESH_TOKEN",
    )
    body = urllib.parse.urlencode(
        {
            "grant_type": "refresh_token",
            "client_id": client_id,
            "client_secret": client_secret,
            "refresh_token": refresh_token,
        }
    ).encode("utf-8")
    request = urllib.request.Request(accounts_url.rstrip("/") + "/oauth/v2/token", data=body, method="POST")
    with urllib.request.urlopen(request, timeout=30) as response:
        result = json.loads(response.read(65537).decode("utf-8"))
    token = str(result.get("access_token", ""))
    if not token:
        raise ValueError("Bigin authorization could not be refreshed")
    return base_url, token


def bigin_call(arguments: dict[str, Any]) -> tuple[int, Any]:
    base_url, token = bigin_access_token()
    method = str(arguments.get("method", "GET")).upper()
    if method not in {"GET", "POST", "PUT", "DELETE"}:
        raise ValueError("unsupported method")
    endpoint = relative_endpoint(arguments.get("endpoint"))
    return request_json(
        base_url.rstrip("/") + "/" + endpoint,
        method,
        headers={"Authorization": "Zoho-oauthtoken " + token},
        body=arguments.get("body") if method in {"POST", "PUT"} else None,
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
            "description": "Read or update the configured Bugbase Zammad account using a relative API endpoint.",
            "inputSchema": {
                "type": "object",
                "additionalProperties": False,
                "required": ["endpoint"],
                "properties": {
                    "method": {"type": "string", "enum": ["GET", "POST", "PUT", "PATCH", "DELETE"], "default": "GET"},
                    "endpoint": {"type": "string", "description": "Relative Zammad API path."},
                    "body": common_body,
                },
            },
        }]
    if provider == "bigin":
        return [{
            "name": "request",
            "title": "Zoho Bigin API request",
            "description": "Read or update the configured Bugbase Bigin CRM account using a relative API endpoint.",
            "inputSchema": {
                "type": "object",
                "additionalProperties": False,
                "required": ["endpoint"],
                "properties": {
                    "method": {"type": "string", "enum": ["GET", "POST", "PUT", "DELETE"], "default": "GET"},
                    "endpoint": {"type": "string", "description": "Relative Bigin API v2 path."},
                    "body": common_body,
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
