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
