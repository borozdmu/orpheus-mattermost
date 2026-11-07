"""Install explicitly selected local tools without messenger credentials or sources."""
import base64
import hashlib
import json
import os
import re
import sys
import uuid


def directory(parent, name):
    try:
        os.mkdir(name, 0o700, dir_fd=parent)
    except FileExistsError:
        pass
    return os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)


def write(parent, name, data):
    temp = '.install-' + uuid.uuid4().hex
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=parent)
    try:
        with os.fdopen(fd, 'wb') as out:
            out.write(data)
            out.flush()
            os.fsync(out.fileno())
        # Replace the entry, never follow an existing link.
        os.rename(temp, name, src_dir_fd=parent, dst_dir_fd=parent)
    finally:
        try:
            os.unlink(temp, dir_fd=parent)
        except FileNotFoundError:
            pass


def prepare(export=False):
    context = {}
    for env, name in [('ORPHEUS_SESSION_ID', 'session_id'), ('ORPHEUS_RUN_ID', 'run_id')]:
        value = os.environ[env]
        if str(uuid.UUID(value)) != value:
            raise ValueError('invalid_run')
        context[name] = value
    root = os.open(os.environ['ORPHEUS_WORKSPACE_PATH'], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        seen = set()
        exporters = []
        for name, value in sorted(os.environ.items()):
            if not re.fullmatch('ORPHEUS_LOCAL_TOOL_[A-Z0-9_]{1,64}', name):
                continue
            if len(value) > 128 * 1024 or len(seen) >= 16:
                raise ValueError('tool_limit')
            item = json.loads(base64.b64decode(value, validate=True))
            if not isinstance(item, dict) or set(item) - {'directory', 'source_b64', 'sha256', 'after_run'} or not {'directory', 'source_b64', 'sha256'} <= set(item):
                raise ValueError('invalid_tool')
            folder, code, expected = item['directory'], item['source_b64'], item['sha256']
            if not isinstance(folder, str) or not re.fullmatch(r'\.[a-z][a-z0-9-]{1,63}', folder) or folder in seen:
                raise ValueError('invalid_tool_directory')
            if not isinstance(expected, str) or not re.fullmatch('[0-9a-f]{64}', expected):
                raise ValueError('invalid_tool_digest')
            source = base64.b64decode(code, validate=True)
            if hashlib.sha256(source).hexdigest() != expected:
                raise ValueError('tool_integrity')
            seen.add(folder)
            if type(item.get('after_run', False)) is not bool:raise ValueError('invalid_export')
            if item.get('after_run'):
                exporters.append(source)
            if export:
                continue
            child = directory(root, folder)
            try:
                write(child, 'client.py', source)
                write(child, 'current-run.json', json.dumps(context).encode())
            finally:
                os.close(child)
        if export:
            if len(exporters) != 1:raise ValueError('exporter_required')
            os.chdir(os.environ['ORPHEUS_WORKSPACE_PATH'])
            sys.argv = ['local-output', 'export']
            exec(compile(exporters[0], 'local-output-client', 'exec'), {'__name__': '__main__'})
    finally:
        os.close(root)


if __name__ == '__main__':
    try:
        prepare(export=len(sys.argv) == 2 and sys.argv[1] == 'export')
    except Exception:
        # Do not include environment values, filenames or source text in logs.
        sys.exit('Local tool initialization failed')
