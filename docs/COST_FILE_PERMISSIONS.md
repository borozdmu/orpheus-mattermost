# Cost-file permissions

This guide applies to connector builds with `BOT_COST_FILE` support. These
builds read the file each time they format a new cost footer.
A readable file inside an inaccessible directory still fails with `EACCES`.
The final message then reports that cost is unavailable, even when run usage,
model pricing and dated exchange rates exist.

## Installation

Keep public model prices and exchange rates in a dedicated directory, without
credentials. The directory and file remain owned by the administrator; the
connector needs only traversal and read access. Mount the directory read-only.

For example, use a root-owned `/srv/example-costs` containing only public
configuration (replace this illustrative path with your chosen mount source):

```sh
install -d -o root -g root -m 0755 /srv/example-costs
install -o root -g root -m 0644 costs.json /srv/example-costs/costs.json
```

The second command is for initial installation only. Preserve historical rates
when updating an existing file. Write updates to a temporary file in the same
directory, explicitly set its mode to `0644`, then atomically replace the file.

Python installers must apply the directory mode after creation:

```python
directory.mkdir(exist_ok=True, mode=0o755)
directory.chmod(0o755)
```

`mkdir(mode=0o755)` alone is insufficient: `umask(0o077)` reduces a new directory
to `0700`, and `exist_ok=True` does not repair an existing directory's mode.
Do not loosen the installer's global umask: private credentials and receipts
still require their existing protections.

## Repair and verification

1. Confirm that the directory contains only public cost configuration, inspect
   its ownership and the connector's actual UID/GID, and record current modes.
2. Repair only the directory: `chmod 0755 /srv/example-costs`. Do not recursively
   change permissions or replace pricing data.
3. Read and parse `costs.json` under the connector's UID/GID. Confirm that this
   user cannot write either the file or its directory. A root-only read test
   does not verify connector access.
4. Check that file hashes and running container identities have not changed.
   The permission repair takes effect immediately; no restart is needed.
5. Send a new short request in each affected bot's test channel. Require an
   actual numeric cost footer and verify it against recorded usage and the
   configured tariff. Existing final delivery receipts retain their original
   footer; they are not a valid test of this repair.

## Reproduce the installer regression

Run this self-contained check as an unprivileged user. It creates only temporary
directories and restores the process umask. It covers both a new directory and
an existing directory left by an earlier installation:

```python
import os
from pathlib import Path
from tempfile import TemporaryDirectory

with TemporaryDirectory() as temporary:
    directory = Path(temporary) / "public-costs"
    previous_umask = os.umask(0o077)
    try:
        directory.mkdir(mode=0o755)
        assert directory.stat().st_mode & 0o777 == 0o700
        for _ in range(2):
            directory.mkdir(exist_ok=True, mode=0o755)
            directory.chmod(0o755)
            assert directory.stat().st_mode & 0o777 == 0o755
    finally:
        os.umask(previous_umask)
```

Also perform the UID/GID read and write checks above on the deployment host.
Mode assertions alone cannot detect an inaccessible parent directory, ACL or
container mount mismatch.
