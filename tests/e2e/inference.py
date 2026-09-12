"""Live inference smoke checks using only the isolated Hermes profile's proxy token.

Requires PyYAML (available in Hermes' Python environment). Makes one inference
request. Never prints credentials, request payloads or provider error bodies.
"""
import argparse
import json
from pathlib import Path
import urllib.error
import urllib.request
import yaml


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--profile", type=Path, required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--api", choices=["chat-stream", "responses"], required=True)
    args = parser.parse_args()
    if args.profile.name != "toolmux-e2e":
        parser.error("use the isolated toolmux-e2e profile")
    config = yaml.safe_load((args.profile / "config.yaml").read_text(encoding="utf-8"))
    base = config["model"]["base_url"].rstrip("/")
    headers = {**config["mcp_servers"]["toolmux"]["headers"], "Content-Type": "application/json"}
    prompt = "Reply with exactly PROXY_OK."
    if args.api == "responses":
        path = "/responses"
        body = {"model": args.model, "input": [{"role": "user", "content": prompt}], "store": False, "stream": True}
    else:
        path = "/chat/completions"
        body = {"model": args.model, "messages": [{"role": "user", "content": prompt}],
                "stream": True, "stream_options": {"include_usage": True},
                "thinking": {"type": "disabled"}, "max_tokens": 128}
    request = urllib.request.Request(base + path, data=json.dumps(body).encode(), headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=150) as response:
            if args.api == "responses":
                value = None
                deltas = []
                for raw in response:
                    line = raw.decode().strip()
                    if line.startswith("data: ") and line != "data: [DONE]":
                        event = json.loads(line[6:])
                        if event.get("type") == "response.output_text.delta":
                            deltas.append(event.get("delta", ""))
                        if event.get("type") == "response.completed":
                            value = event["response"]
                assert value is not None, "Missing response.completed event"
                text = "".join(deltas) or "".join(part.get("text", "") for item in value.get("output", []) for part in item.get("content", []))
                usage = value.get("usage")
                assert value.get("status") == "completed", "Incomplete response"
            else:
                assert "text/event-stream" in response.headers["Content-Type"]
                text, done, usage = "", False, None
                for raw in response:
                    line = raw.decode().strip()
                    if line == "data: [DONE]":
                        done = True
                    elif line.startswith("data: "):
                        value = json.loads(line[6:])
                        usage = value.get("usage") or usage
                        for choice in value.get("choices", []):
                            text += choice.get("delta", {}).get("content") or ""
                assert done, "Stream did not complete"
            assert "PROXY_OK" in text, "Expected response missing"
            counts = {key: usage[key] for key in ("input_tokens", "output_tokens", "prompt_tokens", "completion_tokens", "total_tokens") if usage and key in usage}
            print("PASS:", args.model, args.api, "response verified; usage:", json.dumps(counts))
    except urllib.error.HTTPError as error:
        print("FAIL:", args.model, "gateway HTTP", error.code)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
