import contextlib
import io
from pathlib import Path
import runpy
import unittest
from unittest.mock import Mock


FILES = runpy.run_path(str(Path(__file__).with_name('files.py')))
REPORT = runpy.run_path(str(Path(__file__).with_name('report.py')))


class ReportDeliveryTest(unittest.TestCase):
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
