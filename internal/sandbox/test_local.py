import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('local', Path(__file__).with_name('local.py'))
local = importlib.util.module_from_spec(spec)
spec.loader.exec_module(local)


class LocalTools(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.env = dict(ORPHEUS_WORKSPACE_PATH=self.tmp.name,
                       ORPHEUS_SESSION_ID='11111111-1111-4111-8111-111111111111',
                       ORPHEUS_RUN_ID='22222222-2222-4222-8222-222222222222')
        source = b'print("synthetic client")'
        self.tool = dict(directory='.agent-memory', source_b64=base64.b64encode(source).decode(), sha256=hashlib.sha256(source).hexdigest())
        self.set_tool()

    def set_tool(self):
        self.env['ORPHEUS_LOCAL_TOOL_MEMORY'] = base64.b64encode(json.dumps(self.tool).encode()).decode()

    def test_private_context_and_verified_code_without_transport(self):
        with patch.dict(os.environ, self.env, clear=True):
            local.prepare()
        context = self.root / '.agent-memory/current-run.json'
        self.assertEqual(context.stat().st_mode & 0o777, 0o600)
        self.assertEqual(json.loads(context.read_text())['run_id'], self.env['ORPHEUS_RUN_ID'])
        self.assertEqual((self.root / '.agent-memory/client.py').read_bytes(), b'print("synthetic client")')
        self.assertEqual(list(self.root.iterdir()), [self.root / '.agent-memory'])

    def test_rejects_modified_code(self):
        self.tool['sha256'] = '0' * 64
        self.set_tool()
        with patch.dict(os.environ, self.env, clear=True), self.assertRaises(ValueError):
            local.prepare()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_rejects_directory_symlink(self):
        (self.root / 'elsewhere').mkdir()
        (self.root / '.agent-memory').symlink_to(self.root / 'elsewhere')
        with patch.dict(os.environ, self.env, clear=True), self.assertRaises(OSError):
            local.prepare()
        self.assertEqual(list((self.root / 'elsewhere').iterdir()), [])

    def test_does_not_execute_client_or_read_transport(self):
        self.env['MM_TOKEN_ENV'] = 'PRIVATE_TOKEN'
        self.env['PRIVATE_TOKEN'] = 'synthetic-secret'
        with patch.dict(os.environ, self.env, clear=True):
            local.prepare()
        self.assertNotIn('synthetic-secret', (self.root / '.agent-memory/current-run.json').read_text())

    def test_rejects_escape_and_duplicate_destination(self):
        for directory in ('../private', '/tmp/private', '.memory/child'):
            self.tool['directory'] = directory
            self.set_tool()
            with patch.dict(os.environ, self.env, clear=True), self.assertRaises(ValueError):
                local.prepare()
        self.tool['directory'] = '.agent-memory'
        self.set_tool()
        self.env['ORPHEUS_LOCAL_TOOL_DUPLICATE'] = self.env['ORPHEUS_LOCAL_TOOL_MEMORY']
        with patch.dict(os.environ, self.env, clear=True), self.assertRaises(ValueError):
            local.prepare()

    def test_export_executes_service_pinned_bytes_not_workspace_code(self):
        marker = self.root / 'trusted-export'
        source = ('from pathlib import Path; Path(' + repr(str(marker)) + ').write_text("ok")').encode()
        self.tool.update(source_b64=base64.b64encode(source).decode(), sha256=hashlib.sha256(source).hexdigest(), after_run=True)
        self.set_tool()
        (self.root / '.agent-memory').mkdir()
        (self.root / '.agent-memory/client.py').write_text('raise RuntimeError("untrusted replacement")')
        old = os.getcwd()
        try:
            with patch.dict(os.environ, self.env, clear=True):local.prepare(export=True)
        finally:os.chdir(old)
        self.assertEqual(marker.read_text(),'ok')

    def test_export_requires_exactly_one_selected_exporter(self):
        with patch.dict(os.environ, self.env, clear=True), self.assertRaises(ValueError):local.prepare(export=True)


    def exporter(self):
        source = b'print("synthetic exporter")'
        self.tool.update(directory='.orpheus-output', source_b64=base64.b64encode(source).decode(), sha256=hashlib.sha256(source).hexdigest(), after_run=True)
        self.set_tool()
        return self.root / '.orpheus-output'

    def test_continued_session_exports_only_current_run_files(self):
        output = self.exporter()
        with patch.dict(os.environ, self.env, clear=True):
            local.prepare()
        (output / 'files').mkdir()
        (output / 'files/first.xlsx').write_bytes(b'first run')
        self.env['ORPHEUS_RUN_ID'] = '33333333-3333-4333-8333-333333333333'
        with patch.dict(os.environ, self.env, clear=True):
            local.prepare()
        self.assertFalse((output / 'files').exists())
        retired = list((output / 'previous-runs').iterdir())
        self.assertEqual(len(retired), 1)
        self.assertEqual((retired[0] / 'first.xlsx').read_bytes(), b'first run')
        self.assertEqual(json.loads((output / 'current-run.json').read_text())['run_id'], self.env['ORPHEUS_RUN_ID'])
        (output / 'files').mkdir()
        (output / 'files/second.xlsx').write_bytes(b'second run')
        self.env['ORPHEUS_RUN_ID'] = '44444444-4444-4444-8444-444444444444'
        with patch.dict(os.environ, self.env, clear=True):
            local.prepare()
        self.assertEqual(sorted(p.name for r in (output / 'previous-runs').iterdir() for p in r.iterdir()), ['first.xlsx', 'second.xlsx'])

    def test_retiring_output_moves_planted_link_without_following_it(self):
        output = self.exporter()
        (self.root / 'elsewhere').mkdir()
        (self.root / 'elsewhere/keep.txt').write_text('outside')
        output.mkdir()
        (output / 'files').symlink_to(self.root / 'elsewhere')
        with patch.dict(os.environ, self.env, clear=True):
            local.prepare()
        self.assertFalse((output / 'files').exists() or (output / 'files').is_symlink())
        self.assertEqual((self.root / 'elsewhere/keep.txt').read_text(), 'outside')
        self.assertTrue(next((output / 'previous-runs').iterdir()).is_symlink())

    def test_occupied_archive_name_cannot_block_later_runs(self):
        output = self.exporter()
        (self.root / 'elsewhere').mkdir()
        for occupant in ('link', 'file'):
            output.mkdir(exist_ok=True)
            (output / 'files').mkdir()
            (output / 'files/new.csv').write_text('a')
            if occupant == 'link':
                (output / 'previous-runs').symlink_to(self.root / 'elsewhere')
            else:
                (output / 'previous-runs').write_text('not a folder')
            with patch.dict(os.environ, self.env, clear=True):
                local.prepare()
            self.assertTrue((output / 'previous-runs').is_dir() and not (output / 'previous-runs').is_symlink())
            self.assertFalse((output / 'files').exists())
            self.assertEqual(list((self.root / 'elsewhere').iterdir()), [])
            for p in list(output.iterdir()):
                if p.name.startswith('previous-runs'):
                    if p.is_symlink() or p.is_file():p.unlink()
                    else:
                        import shutil;shutil.rmtree(p)

    def test_unwritable_archive_cannot_block_later_runs(self):
        output = self.exporter()
        (output / 'previous-runs').mkdir(parents=True)
        (output / 'files').mkdir()
        (output / 'files/new.csv').write_text('a')
        real = os.rename
        def rename(src, dst, *, src_dir_fd=None, dst_dir_fd=None):
            if src_dir_fd != dst_dir_fd:
                raise PermissionError(13, 'synthetic unwritable archive')
            return real(src, dst, src_dir_fd=src_dir_fd, dst_dir_fd=dst_dir_fd)
        with patch.dict(os.environ, self.env, clear=True), patch.object(local.os, 'rename', rename):
            local.prepare()
        self.assertFalse((output / 'files').exists())
        retired = [p for p in output.iterdir() if p.name.startswith('files.retired-')]
        self.assertEqual(len(retired), 1)
        self.assertEqual((retired[0] / 'new.csv').read_text(), 'a')

    def test_non_exporting_tool_keeps_its_files(self):
        (self.root / '.agent-memory/files').mkdir(parents=True)
        with patch.dict(os.environ, self.env, clear=True):
            local.prepare()
        self.assertTrue((self.root / '.agent-memory/files').is_dir())
        self.assertFalse((self.root / '.agent-memory/previous-runs').exists())

    def test_export_does_not_retire_current_files(self):
        output = self.exporter()
        output.mkdir()
        (output / 'files').mkdir()
        old = os.getcwd()
        try:
            with patch.dict(os.environ, self.env, clear=True):local.prepare(export=True)
        finally:os.chdir(old)
        self.assertTrue((output / 'files').is_dir())
