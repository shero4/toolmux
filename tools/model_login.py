"""Sign the optional local LiteLLM bridge into ChatGPT/Codex.

Run from any directory: python tools/model_login.py --env-file PATH
The Compose bridge must already be running. Account login stays interactive;
tokens remain in its Docker volume and are never printed by this helper.
"""
import argparse
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--env-file", type=Path, required=True, help="Local Compose environment file")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    # Use the bridge's installed authentication implementation rather than
    # duplicating OAuth endpoints, client IDs, polling or token refresh logic.
    script = """
import litellm
litellm.request_timeout = 20
from litellm.llms.chatgpt.authenticator import Authenticator
try:
    Authenticator().get_access_token()
except Exception as exc:
    print('Sign-in failed:', type(exc).__name__, 'status:', getattr(exc, 'status_code', 'unavailable'), flush=True)
    raise SystemExit(1)
print('Codex sign-in saved in the bridge volume.', flush=True)
"""
    compose = ["docker", "compose", "--env-file", str(args.env_file.resolve()),
               "-f", str(root / "compose.models.yaml")]
    command = [*compose, "exec", "-T", "models", "python", "-u", "-c", script]
    result = subprocess.call(command, cwd=root)
    if result == 0:
        # A first-start bridge may still be polling its original device code.
        # Restart only this service so it loads the newly saved account session.
        print("Restarting the model bridge to load the saved session.", flush=True)
        result = subprocess.call([*compose, "restart", "models"], cwd=root)
    raise SystemExit(result)


if __name__ == "__main__":
    main()
