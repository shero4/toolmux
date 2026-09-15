#!/usr/bin/env python3
import json
import logging
import subprocess
import time
from collections import defaultdict, deque
from pathlib import Path

PROFILES = ("product-ops", "marketing", "company-ops", "customer-ops", "revenue")
CHECK_SECONDS = 15
HEARTBEAT_TIMEOUT_SECONDS = 90
STARTUP_GRACE_SECONDS = 120
RESTART_COOLDOWN_SECONDS = 300
RESTART_WINDOW_SECONDS = 600
MAX_RESTARTS_PER_WINDOW = 5

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
failures = defaultdict(int)
restart_history = defaultdict(deque)
startup_grace_until = defaultdict(float)


def run(*args):
    return subprocess.run(args, check=False, text=True, capture_output=True)


def enabled(profile):
    return run("systemctl", "is-enabled", f"hermes-gateway@{profile}.service").returncode == 0


def active(profile):
    return run("systemctl", "is-active", f"hermes-gateway@{profile}.service").returncode == 0


def health(profile):
    root = Path("/var/lib/hermes/profiles") / profile
    heartbeat = root / "state" / "gateway.heartbeat"
    state_path = root / "gateway_state.json"
    if not heartbeat.exists():
        return False, "heartbeat missing"
    age = time.time() - heartbeat.stat().st_mtime
    if age > HEARTBEAT_TIMEOUT_SECONDS:
        return False, f"heartbeat stale ({age:.0f}s)"
    try:
        state = json.loads(state_path.read_text(encoding="utf-8"))
        slack = state.get("platforms", {}).get("slack", {})
        if slack.get("state") != "connected":
            return False, f"Slack state is {slack.get('state', 'missing')}"
    except (OSError, ValueError) as exc:
        return False, f"state unreadable ({exc})"
    return True, "healthy"


def restart(profile, reason):
    now = time.time()
    history = restart_history[profile]
    while history and now - history[0] > RESTART_WINDOW_SECONDS:
        history.popleft()
    if len(history) >= MAX_RESTARTS_PER_WINDOW:
        logging.error("profile=%s restart suppressed reason=%s count=%d", profile, reason, len(history))
        return
    if history and now - history[-1] < RESTART_COOLDOWN_SECONDS:
        logging.warning("profile=%s restart cooldown reason=%s", profile, reason)
        return
    # systemd is already reviving it (a graceful SIGUSR1 exit, or a crash
    # within RestartSec); racing it with a second restart kills the new turn.
    unit = f"hermes-gateway@{profile}.service"
    shown = dict(line.split("=", 1) for line in run("systemctl", "show", "-p", "ActiveState,SubState,ExecMainExitTimestampMonotonic", unit).stdout.splitlines() if "=" in line)
    state, sub = shown.get("ActiveState", ""), shown.get("SubState", "")
    try:
        uptime_us = float(open("/proc/uptime").read().split()[0]) * 1_000_000
        exited_ago = (uptime_us - float(shown.get("ExecMainExitTimestampMonotonic", "0"))) / 1_000_000
    except (OSError, ValueError):
        exited_ago = 1e9
    if state in {"activating", "reloading"} or sub in {"auto-restart", "start", "start-pre"} or exited_ago < 15:
        logging.info("profile=%s restart skipped state=%s/%s exited_ago=%.0fs reason=%s", profile, state, sub, exited_ago, reason)
        return
    # Drain-aware first: the gateway finishes in-flight turns before exiting.
    result = run("/usr/local/sbin/hermes-gateway-restart", profile, "180")
    if result.returncode != 0:
        result = run("systemctl", "restart", f"hermes-gateway@{profile}.service")
    if result.returncode == 0:
        history.append(now)
        startup_grace_until[profile] = now + STARTUP_GRACE_SECONDS
        failures[profile] = 0
        logging.warning("profile=%s restarted reason=%s", profile, reason)
    else:
        logging.error("profile=%s restart failed reason=%s error=%s", profile, reason, result.stderr.strip())


def supervise():
    logging.info("fleet supervisor started profiles=%s", ",".join(PROFILES))
    while True:
        now = time.time()
        for profile in PROFILES:
            if not enabled(profile):
                failures[profile] = 0
                continue
            if not active(profile):
                restart(profile, "service inactive")
                continue
            if now < startup_grace_until[profile]:
                continue
            ok, reason = health(profile)
            if ok:
                if failures[profile]:
                    logging.info("profile=%s recovered", profile)
                failures[profile] = 0
                continue
            failures[profile] += 1
            logging.warning("profile=%s unhealthy reason=%s consecutive=%d", profile, reason, failures[profile])
            if failures[profile] >= 2:
                restart(profile, reason)
        time.sleep(CHECK_SECONDS)


if __name__ == "__main__":
    supervise()
