#!/usr/bin/env python3
"""Root-owned, fixed-repository updater for the native systemd installation."""
import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path('/var/lib/toolmux-updater')
BINARY = Path('/usr/local/bin/toolmux')
ROOT.mkdir(mode=0o755, parents=True, exist_ok=True)
lock = (ROOT / 'lock').open('w')
fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)

def status(phase, message):
    p = ROOT / 'status.next'
    p.write_text(json.dumps({'phase': phase, 'message': message}))
    p.chmod(0o644)
    p.replace(ROOT / 'status.json')

def run(args, **kwargs):
    return subprocess.run(args, check=True, timeout=1800, **kwargs)

def healthy():
    for _ in range(30):
        try:
            with urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2) as response:
                if response.status == 200:
                    # /healthz alone does not exercise PostgreSQL.
                    with urllib.request.urlopen('http://127.0.0.1:8080/login', timeout=2) as login:
                        if login.status == 200: return True
        except Exception:
            pass
        time.sleep(2)
    return False

stopped = False
previous = None
try:
    status('running', 'Checking the requested GitHub revision.')
    fd = os.open('/var/lib/toolmux/update-request', os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd) as request:
        revision = request.read(128).strip()
    if not re.fullmatch('[a-f0-9]{40}', revision): raise ValueError('Invalid revision')
    with tempfile.TemporaryDirectory(prefix='build-', dir=ROOT) as temporary:
        source = Path(temporary) / 'source'
        run(['git', 'clone', '--depth', '1', '--branch', 'main', 'https://github.com/shero4/toolmux.git', str(source)])
        actual = subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip()
        if actual != revision: raise ValueError('GitHub changed since the check; wait for the next check and retry')
        status('running', 'Building the new version. Toolmux is still serving requests.')
        tag = 'toolmux-update:' + revision
        run(['docker', 'build', '--target', 'build', '--build-arg', 'VCS_REF='+revision, '-t', tag, str(source)])
        container = subprocess.check_output(['docker', 'create', tag], text=True).strip()
        candidate = BINARY.with_name('toolmux.next')
        try:
            run(['docker', 'cp', container+':/out/toolmux', str(candidate)])
        finally:
            run(['docker', 'rm', container])
        candidate.chmod(0o755)
        # Check the binary can execute before interrupting the service.
        run([str(candidate), 'keygen'], stdout=subprocess.DEVNULL)
        status('running', 'Backing up the database and restarting Toolmux.')
        run(['systemctl', 'stop', 'toolmux.service']); stopped = True
        backups = ROOT / 'backups'; backups.mkdir(mode=0o700, exist_ok=True)
        stamp = time.strftime('%Y%m%d-%H%M%S')
        with (backups / (stamp+'.dump')).open('xb') as backup:
            run(['docker', 'exec', 'toolmux-host-db-1', 'pg_dump', '-U', 'toolmux', '-d', 'toolmux', '-Fc'], stdout=backup)
        previous = backups / (stamp+'.binary')
        shutil.copy2(BINARY, previous)
        candidate.replace(BINARY)
        run(['systemctl', 'start', 'toolmux.service']); stopped = False
        if not healthy(): raise RuntimeError('New version failed its startup checks')
        status('complete', 'Update installed successfully: '+revision[:7])
except Exception as error:
    # Retain the database snapshot for recovery; never silently roll back a schema.
    if previous is not None:
        shutil.copy2(previous, BINARY.with_name('toolmux.rollback'))
        BINARY.with_name('toolmux.rollback').replace(BINARY)
        run(['systemctl', 'restart', 'toolmux.service'])
        recovered = healthy()
        status('failed', 'Update failed. Previous binary restored. '+('Service recovered.' if recovered else 'Manual database recovery may be required; the backup is retained.'))
    else:
        if stopped: run(['systemctl', 'start', 'toolmux.service'])
        status('failed', 'Update could not be installed. Existing version retained; see the update service log.')
    raise
