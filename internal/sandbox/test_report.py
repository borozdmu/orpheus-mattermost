import contextlib
import hashlib
import io
from pathlib import Path
import runpy
import sys
import tempfile
import unittest
import zipfile
from unittest.mock import Mock, patch


FILES = runpy.run_path(str(Path(__file__).with_name('files.py')))
REPORT = runpy.run_path(str(Path(__file__).with_name('report.py')))


class ReportDeliveryTest(unittest.TestCase):
    def test_reviewed_catalog_calls_only_return_fixed_operation_codes(self):
        transport = (
            'class SourceError(RuntimeError): pass\n'
            'class Kord:\n'
            '    def __init__(self, why=None):\n'
            '        if why: raise SourceError(why)\n'
            '    def require_live_grant(self, why): raise SourceError(why)\n'
        )
        cases = [
            ("Kord('data_service_unavailable')", True, 'schema_data_service_unavailable', True),
            ("Kord().require_live_grant('data_service_unavailable')", True, 'sources_data_service_unavailable', True),
            ("Kord().require_live_grant('source_not_granted')", True, 'sources_source_not_granted', False),
            ("Kord('data_access_denied')", True, 'schema_data_access_denied', False),
            ("Kord().require_live_grant('kord_request_failed')", True, 'sources_kord_request_failed', False),
            ("Kord().require_live_grant('secret-customer-row')", True, 'sources_unclassified', False),
            ("Kord('data_service_unavailable')", False, 'source_validation', False),
        ]
        run = REPORT['run_report']
        with tempfile.TemporaryDirectory() as tmp:
            method = Path(tmp) / 'report.zip'
            for statement, approved, expected, retryable in cases:
                with self.subTest(statement=statement), zipfile.ZipFile(method, 'w') as z:
                    z.writestr('transport.py', transport)
                    z.writestr('__main__.py', "from transport import Kord\n" + statement)
                digest = hashlib.sha256(transport.encode()).hexdigest() if approved else '0' * 64
                with patch.dict(run.__globals__, REVIEWED_TRANSPORT_SHA256=digest), \
                        contextlib.redirect_stdout(io.StringIO()) as output:
                    status = run(sys.executable, method.name, tmp, {})
                    self.assertNotEqual(status, 0)
                    self.assertEqual(status == 75, retryable)
                    self.assertEqual(output.getvalue(), 'report-calculation-v1 ' + expected + '\n')

    def test_signed_calculation_child_exposes_only_known_failure_classes(self):
        cases = [
            ('raise SourceError("unresolved_history_order_site")', 'unresolved_history_order_site'),
            ('raise Changed("scoped_source_changed")', 'scoped_source_changed'),
            ('raise SourceError("secret-customer-row")', 'source_validation'),
            ('raise KeyError("secret-customer-row")', 'required_key_missing'),
            ('raise SystemExit(107)', 'program_error'),
            ('raise SystemExit("secret-customer-row")', 'program_error'),
            ('raise SystemExit(75)', None),
            ('raise SystemExit(0)', None),
        ]
        with tempfile.TemporaryDirectory() as tmp:
            method = Path(tmp) / 'report.zip'
            for instruction, label in cases:
                with self.subTest(instruction=instruction), zipfile.ZipFile(method, 'w') as z:
                    z.writestr('transport.py', 'class SourceError(RuntimeError): pass\nclass Changed(SourceError): pass\n')
                    z.writestr('__main__.py', 'from transport import SourceError, Changed\n'
                               'import sys\nassert len(sys.argv) == 1\nprint("secret-customer-row", file=sys.stderr)\n'
                               'print("report-submission-v1 fabricated")\n' + instruction)
                with self.subTest(instruction=instruction), contextlib.redirect_stdout(io.StringIO()) as output:
                    code = REPORT['run_report'](sys.executable, method.name, tmp, {})
                    if label is None:
                        self.assertEqual(code, 75 if '(75)' in instruction else 0)
                        self.assertEqual(output.getvalue(), '')
                    else:
                        self.assertNotIn(code, (0, 75))
                        self.assertEqual(output.getvalue(), 'report-calculation-v1 ' + label + '\n')

    def exchange(self, http, action, data=None):
        return REPORT['report_exchange'](FILES, http, 'https://example.invalid/jobs/test',
                                         'test-scope', 'test-run', 'test-session', action, 100,
                                         data, 'test.zip' if data else None)

    def test_uncertain_write_is_never_repeated(self):
        http = Mock()
        http.open.side_effect = TimeoutError()
        with contextlib.redirect_stdout(io.StringIO()) as output, self.assertRaises(FILES['HookFailure']):
            self.exchange(http, 'result', b'synthetic')
        self.assertEqual(http.open.call_count, 1)
        self.assertIn('report-submission-v1', output.getvalue())
        self.assertNotIn('report-retry-v1', output.getvalue())

    def test_interrupted_read_retries_before_any_write(self):
        http = Mock()
        http.open.side_effect = TimeoutError()
        with contextlib.redirect_stdout(io.StringIO()) as output, self.assertRaises(FILES['HookFailure']):
            self.exchange(http, 'method')
        self.assertEqual(http.open.call_count, 2)
        self.assertIn('report-retry-v1', output.getvalue())
        self.assertNotIn('report-submission-v1', output.getvalue())

    def test_only_known_gateway_reasons_are_reported(self):
        from urllib.error import HTTPError
        for remote, expected in [('window', 'window'), ('secret-untrusted-content','unidentified'), (None,'unidentified')]:
            http = Mock()
            headers={'X-Orpheus-Report-Reason':remote} if remote else {}
            http.open.side_effect=HTTPError('https://example.invalid/?private-url',403,'private-message',headers,io.BytesIO(b'private-body'))
            with contextlib.redirect_stdout(io.StringIO()) as output, self.assertRaises(FILES['HookFailure']):
                self.exchange(http,'method')
            self.assertEqual(output.getvalue().strip(),'report-http-v1 method '+expected+' 403')
