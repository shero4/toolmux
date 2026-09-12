#!/usr/bin/env bash
# Fresh native Linux install. Run as root from the reviewed checkout.
# Usage: sudo bash deploy/install-host.sh http://localhost:8081
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
base_url=${1:?provide the client-facing URL}
if [[ -e /etc/toolmux/toolmux.env ]]; then
  echo 'Existing installation detected; use the documented upgrade procedure.' >&2
  exit 1
fi
id toolmux >/dev/null 2>&1 || useradd --create-home --home-dir /var/lib/toolmux --shell /bin/bash toolmux
usermod -aG docker toolmux
install -d -m 750 -o toolmux -g toolmux /etc/toolmux
docker build --target build -t toolmux-host-build .
build_container=$(docker create toolmux-host-build true)
trap 'docker rm "$build_container" >/dev/null' EXIT
docker cp "$build_container:/out/toolmux" /usr/local/bin/toolmux
chmod 755 /usr/local/bin/toolmux
umask 077
db_password=$(openssl rand -hex 32)
bridge_key=$(openssl rand -hex 32)
{
  /usr/local/bin/toolmux keygen
  printf 'TOOLMUX_DB_PASSWORD=%s\n' "$db_password"
  printf 'TOOLMUX_DATABASE_URL=postgres://toolmux:%s@127.0.0.1:5432/toolmux?sslmode=disable\n' "$db_password"
  printf 'TOOLMUX_BASE_URL=%s\n' "$base_url"
  printf 'TOOLMUX_ADDR=127.0.0.1:8080\nTOOLMUX_CODEX_CONTAINER=toolmux-host-models-1\n'
  printf 'LITELLM_MASTER_KEY=%s\n' "$bridge_key"
  printf 'LITELLM_IMAGE=ghcr.io/berriai/litellm@sha256:a3715fa7ad8387941ab697259bd2881d68931657247a41984f90fae6d11c62bf\n'
} > /etc/toolmux/toolmux.env
cat > /etc/toolmux/litellm.yaml <<'YAML'
model_list:
  - model_name: codex
    litellm_params:
      model: chatgpt/gpt-5.6-terra
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
YAML
chown toolmux:toolmux /etc/toolmux/toolmux.env /etc/toolmux/litellm.yaml
chmod 600 /etc/toolmux/toolmux.env
chmod 644 /etc/toolmux/litellm.yaml
docker compose --env-file /etc/toolmux/toolmux.env -p toolmux-host -f deploy/compose.host.yaml up -d
install -m 644 deploy/toolmux.service /etc/systemd/system/toolmux.service
systemctl daemon-reload
systemctl enable --now toolmux
for attempt in $(seq 1 60); do
  if curl -fsS http://127.0.0.1:8080/healthz >/dev/null; then
    echo 'Toolmux is healthy. Complete administrator setup over your private tunnel.'
    exit 0
  fi
  sleep 2
done
echo 'Startup did not become healthy; inspect journalctl -u toolmux.' >&2
exit 1
