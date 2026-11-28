"""One deterministic step. The reviewed calculation receives only native CLI access."""
import contextlib
import hashlib
import hmac
import io
import json
import os
import re
import subprocess
import urllib.error
import urllib.request


# Only a child's numeric status crosses this boundary. Its normal output,
# exceptions, arguments and traceback are never copied into the hook journal.
SOURCE_DIAGNOSTICS = (
    'scoped_source_changed',
    'unresolved_history_order_site', 'site_code_mismatch', 'wrong_shop',
    'live_read_grant_unconfirmed', 'source_not_granted', 'changed_method_or_dictionary',
    'wrong_previous_assignment', 'previous_file_sha_mismatch', 'previous_stage_mismatch',
    'unproven_insertion_order_at_source_boundary', 'new_history_inside_closed_reporting_period',
    'invalid_alignment', 'incomplete_page', 'missing_history_range',
    'unconfirmed_custom_value', 'marker_size_rejected', 'incorrect_history_field',
)
REPORT_DIAGNOSTICS = SOURCE_DIAGNOSTICS + (
    'program_error', 'source_validation', 'history_alignment_changed', 'history_count_changed',
    'history_order_changed', 'history_cursor_changed', 'history_final_count_changed', 'history_boundary_changed',
    'required_key_missing', 'invalid_value', 'invalid_type', 'io_error', 'memory_error',
)

REPORT_PROGRAM = """
import hashlib, json, runpy, sys, zipfile

method = sys.argv[1]
labels, source_codes = json.loads(sys.argv[2])
sys.argv = [method]
try:
    runpy.run_path(method, run_name='__main__')
except SystemExit as e:
    code = e.code
    sys.exit(0 if code is None else code if type(code) is int and code in (0, 75) else 1)
except Exception as e:
    kind = (type(e).__module__, type(e).__name__)
    label = 'program_error'
    if kind in (('transport', 'SourceError'), ('transport', 'Changed')):
        reason = e.args[0] if len(e.args) == 1 and type(e.args[0]) is str else None
        label = reason if reason in source_codes else 'source_validation'
        if kind == ('transport', 'Changed') and label == 'scoped_source_changed':
            # These exact instructions distinguish order, pagination and count
            # changes which the released program raises as one exception type.
            tb = e.__traceback__
            while tb.tb_next:
                tb = tb.tb_next
            frame = tb.tb_frame
            if frame.f_globals.get('__name__') == 'acquire' and frame.f_code.co_name == 'acquire':
                with zipfile.ZipFile(method) as z:
                    source = z.read('acquire.py')
                if hashlib.sha256(source).hexdigest() == '86c190395d6900bc548f3ee45be0b3fcf28ebc8bddccb95e1d0df9ad93956b42':
                    label = {75:'history_alignment_changed', 95:'history_count_changed',
                             108:'history_order_changed', 114:'history_cursor_changed',
                             122:'history_final_count_changed', 143:'history_boundary_changed'}.get(tb.tb_lineno, label)
    elif kind[0] == 'builtins':
        label = {'KeyError':'required_key_missing', 'TypeError':'invalid_type',
                 'ValueError':'invalid_value', 'OSError':'io_error', 'MemoryError':'memory_error'}.get(kind[1], label)
    sys.exit(100 + labels.index(label))
"""

def run_report(python, method, work, child):
    allowed = json.dumps([REPORT_DIAGNOSTICS, SOURCE_DIAGNOSTICS])
    result = subprocess.run([python, '-I', '-c', REPORT_PROGRAM, method, allowed],
                            cwd=work, env=child, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL, timeout=540, check=False)
    if result.returncode not in (0, 75):
        index = result.returncode - 100
        label = REPORT_DIAGNOSTICS[index] if 0 <= index < len(REPORT_DIAGNOSTICS) else 'program_error'
        print('report-calculation-v1 ' + label, flush=True)
    return result.returncode


def report_marker(cap, run, purpose, label):
    marker = hmac.new(cap.encode(), (purpose + ':' + run).encode(), hashlib.sha256)
    print(label + ' ' + marker.hexdigest(), flush=True)

def report_exchange(protocol, http, endpoint, cap, run, session, action, limit, data=None, name=None):
    headers = {'Authorization': 'Bearer ' + cap, 'X-Orpheus-Run': run, 'X-Orpheus-Session': session}
    if data is not None:
        headers['Content-Type'] = 'application/octet-stream'
        headers['X-Artifact-SHA256'] = hashlib.sha256(data).hexdigest()
        headers['X-Artifact-Name'] = name
    def read():
        req = urllib.request.Request(endpoint + '/' + action, data=data, headers=headers)
        try:
            response = http.open(req, timeout=225)
        except urllib.error.HTTPError as error:
            reason = error.headers.get('X-Orpheus-Report-Reason') if error.headers else None
            if reason not in ('request', 'stage', 'window', 'origin', 'binding', 'admission',
                              'method', 'checkpoint', 'result'):
                reason = 'unidentified'
            # Never read a remote error body or print its headers verbatim.
            print('report-http-v1 ' + action + ' ' + reason + ' ' + str(int(error.code)), flush=True)
            raise
        with response:
            body = response.read(limit + 1)
            protocol['require'](len(body) <= limit, 'report response too large')
            return body
    # Once a result POST is attempted, only later native receipts may
    # reconcile it. A lost response does not prove that no post was created.
    if data is not None:
        report_marker(cap, run, 'attempted-submission', 'report-submission-v1')
        with protocol['step']('report_submission'):
            return read()
    try:
        return protocol['read_input']('report_' + action, read)
    except protocol['HookFailure'] as error:
        if protocol['transient_read'](error.cause):
            report_marker(cap, run, 'retry-before-upload', 'report-retry-v1')
        raise


def report_step(protocol):
    env = os.environ
    check = protocol['require']
    run = env['ORPHEUS_RUN_ID']
    session = env['ORPHEUS_SESSION_ID']
    work = env['ORPHEUS_WORKSPACE_PATH']
    request = json.loads(env['MM_INPUT_MANIFEST'])
    protocol['validate_request'](request)
    stage = json.loads(env['MM_REPORT_STAGE'])
    check(protocol['valid_run'](run) and protocol['valid_run'](session) and stage['previous_run'] != run,
          'invalid report run')
    check(stage['schema'] == 'orpheus.report-job.v1' and protocol['valid_run'](stage['job_id'])
          and type(stage['stage']) == int and stage['stage'] > 0 and type(stage['position']) == int
          and ((stage['stage'] == 1 and stage['previous_run'] == stage['checkpoint_sha256'] == ''
                and stage['position'] == 0) or (stage['stage'] > 1 and protocol['valid_run'](stage['previous_run'])
                and re.fullmatch(r'[0-9a-f]{64}', stage['checkpoint_sha256']) and stage['position'] > 0)),
          'invalid accepted report stage')
    if 'method_transition' in stage:
        change = stage['method_transition']
        check(stage['stage'] > 1 and type(change) is dict and set(change) == {'from_sha256', 'to_sha256'}
              and type(change['from_sha256']) is str and re.fullmatch(r'[0-9a-f]{64}', change['from_sha256'])
              and change['to_sha256'] == env['MM_REPORT_METHOD_SHA256'] and change['from_sha256'] != change['to_sha256'],
              'invalid approved method transition')
    check(len(request['files']) == 1 and not request.get('previous_index') and
          not request.get('delivered_batches'), 'invalid report input count')
    endpoint = env['MM_REPORT_URL']
    cap = env['MM_REPORT_CAPABILITY']
    http = urllib.request.build_opener(protocol['NoRedirect']())
    def exchange(action, limit, data=None, name=None):
        return report_exchange(protocol, http, endpoint, cap, run, session, action, limit, data, name)
    with contextlib.closing(protocol['Store'](work)) as store:
        original = request['files'][0]
        limit = min(request['limits']['max_file_bytes'], request['limits']['max_batch_bytes'])
        check(original['post_id'] == request['anchor_post_id'] and original['channel_id'] == request['channel_id']
              and original['origin'] == 'thread' and original['size_bytes'] <= limit, 'invalid report method binding')
        program = exchange('method', min(limit, 1 << 20))
        actual = hashlib.sha256(program).hexdigest()
        check(actual == env['MM_REPORT_METHOD_SHA256'] and len(program) == original['size_bytes'],
              'report method digest mismatch')
        f = dict(original, sha256=actual, size_bytes=len(program), status='ready')
        store.import_input(io.BytesIO(protocol['encode'](request) + b'\n' + protocol['encode'](f) + b'\n' + program))
        store.begin_run(run, request)
        base = protocol['BASE']
        if stage['stage'] > 1:
            previous = exchange('checkpoint', min(32 << 20, request['limits']['max_output_bytes']))
            check(hashlib.sha256(previous).hexdigest() == stage['checkpoint_sha256'], 'checkpoint digest mismatch')
            store.write(base + '/report-checkpoint.zip', previous)
        store.json(base + '/report-job.json', stage)
        method = base + '/report-method.zip'
        store.write(method, program)
        child = {k: env[k] for k in ('HOME', 'PATH', 'LANG', 'LC_ALL', 'SSL_CERT_FILE',
                                     'HTTPS_PROXY', 'NO_PROXY') if k in env}
        result = run_report('/opt/orpheus-native/venv/bin/python', method, work, child)
        if result == 75:
            # No upload has started. A later admission can repeat this exact position.
            report_marker(cap, run, 'retry-before-upload', 'report-retry-v1')
        with protocol['step']('report_calculation'):
            check(result == 0, 'report calculation did not finish this stage')
        out = base + '/output/' + run + '/outbox/'
        candidates = list(store.walk(out.rstrip('/')))
        check(len(candidates) == 1, 'report must produce one artifact')
        path = candidates[0][0]
        name = path[len(out):]
        check(name in ('report-checkpoint.zip', 'MQL_2026_RetailCRM.xlsx'), 'unexpected report artifact')
        data = store.read(path, min(request['limits']['max_output_bytes'], 32 << 20))
        ack = json.loads(exchange('result', 2048, data, name))
        check(ack == {'sha256': hashlib.sha256(data).hexdigest(), 'status': 'delivered'},
              'report publication unconfirmed')
