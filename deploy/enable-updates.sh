#!/usr/bin/env bash
# Enable one-click updates for the supplied native toolmux-host layout.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
[[ $EUID -eq 0 ]] || { echo 'Run as root.' >&2; exit 1; }
[[ -f /etc/toolmux/toolmux.env ]] || { echo 'Native installation required.' >&2; exit 1; }
install -d -m 755 /usr/local/lib/toolmux /var/lib/toolmux-updater
install -m 644 deploy/update-host.py /usr/local/lib/toolmux/update-host.py
install -m 644 deploy/toolmux-update.service /etc/systemd/system/toolmux-update.service
printf '%s\n' 'toolmux ALL=(root) NOPASSWD: /usr/bin/systemctl start --no-block toolmux-update.service' > /etc/sudoers.d/toolmux-update
chmod 440 /etc/sudoers.d/toolmux-update
visudo -cf /etc/sudoers.d/toolmux-update
install -d /etc/systemd/system/toolmux.service.d
printf '[Service]\nEnvironment=TOOLMUX_UPDATES_ENABLED=true\n' > /etc/systemd/system/toolmux.service.d/updates.conf
systemctl daemon-reload
echo 'Updater installed. Ensure ExecStart uses /usr/local/bin/toolmux and the binary includes its Git revision, then restart toolmux.'
