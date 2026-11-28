"""One deterministic step. The reviewed calculation receives only native CLI access."""
import contextlib
import hashlib
import hmac
import io
import json
import os
import re
import subprocess
import urllib.request


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
        with http.open(req, timeout=225) as response:
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
        return protocol['read_input']('report_exchange', read)
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
        result = subprocess.run(['/opt/orpheus-native/venv/bin/python', '-I', method], cwd=work, env=child,
                                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                timeout=540, check=False)
        if result.returncode == 75:
            # No upload has started. A later admission can repeat this exact position.
            report_marker(cap, run, 'retry-before-upload', 'report-retry-v1')
        check(result.returncode == 0, 'report calculation did not finish this stage')
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
