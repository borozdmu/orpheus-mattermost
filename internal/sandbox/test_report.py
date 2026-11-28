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
