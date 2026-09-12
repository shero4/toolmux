# Dashboard updates

Toolmux checks the public GitHub `main` branch every 15 minutes. Administrators
see an Update available link beside sign-out and can review the installed and
latest revisions under Administration > Updates. Checks are read-only; only an
explicit administrator action starts installation. GitHub failures are shown
without interrupting normal service. No account or configuration data is sent.

## Native Linux service

The supplied helper supports the native `toolmux-host` deployment: systemd service
`toolmux.service`, binary `/usr/local/bin/toolmux`, and database container
`toolmux-host-db-1`. It requires Docker, Git, Python 3, sudo, and systemd. Build
with `--build-arg VCS_REF=$(git rev-parse HEAD)` so the image records its revision.

From a reviewed checkout, run `sudo bash deploy/enable-updates.sh`. Inspect
`systemctl cat toolmux` and ensure ExecStart uses `/usr/local/bin/toolmux`; remove
or update any versioned-binary override deliberately. Restart Toolmux to enable
the feature. The helper and service are installed root-owned. The application
can start only the fixed update unit through its dedicated sudo rule.

The helper verifies the requested revision is still GitHub main, builds it in
an isolated checkout, then stops Toolmux, backs up PostgreSQL, replaces the
binary, and restarts. Active requests can be interrupted. Database credentials,
the encryption key, configuration, and provider state are preserved. The helper
does not modify the public proxy or migrate local client files.

Status survives the restart in `/var/lib/toolmux-updater/status.json`. Failure
logs are in `journalctl -u toolmux-update.service`. Backups and previous binaries
are retained under `/var/lib/toolmux-updater/backups`; include them in your
backup retention plan and monitor disk space. Startup failure restores the prior
binary. Schema rollback is never automatic: an incompatible migration may need
manual restoration of the matching database snapshot. Retain the encryption key
separately. A build failure leaves the running application untouched.

Application updates do not replace the privileged helper itself. Changes to
that helper require rerunning the reviewed enable script. Infrastructure and
Compose configuration changes likewise remain explicit deployment operations.

## Containers and custom deployments

Update checks can identify a revision embedded at build time, but the dashboard
does not replace a running container or assume access to the Docker socket.
Rebuild/recreate through the deployment process. Builds without a known Git
revision show their status on the Updates page and cannot self-install.
