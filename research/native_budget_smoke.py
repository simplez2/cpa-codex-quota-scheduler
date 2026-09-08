import base64
import ctypes as C
import contextlib
import datetime as dt
import json
import pathlib
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Buffer(C.Structure):
    _fields_ = [('ptr', C.c_void_p), ('len', C.c_size_t)]


CALL = C.CFUNCTYPE(C.c_int, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
FREE = C.CFUNCTYPE(None, C.c_void_p, C.c_size_t)
STOP = C.CFUNCTYPE(None)


class API(C.Structure):
    _fields_ = [('version', C.c_uint32), ('call', CALL), ('free', FREE), ('stop', STOP)]


api = API()
requests = []
reset_5h, reset_weekly = int(time.time()) + 14400, int(time.time()) + 345600
files = [{'id': f'account-{x}', 'auth_index': f'idx-{x}', 'provider': 'codex'} for x in 'ab']
candidate_request = {'Candidates': [{'ID': f'account-{x}', 'Provider': 'codex'} for x in 'ab'],
                     'Options': {'Headers': {'X-Session-ID': ['same-weekly-session']}}}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, data):
        assert self.headers['Authorization'] == 'Bearer local-test-key'
        raw = json.dumps(data).encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        assert self.path == '/v0/management/auth-files'
        self.reply({'files': files})

    def do_POST(self):
        assert self.path == '/v0/management/api-call'
        payload = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert payload['header']['Authorization'] == 'Bearer $TOKEN$'
        assert payload['method'] == 'GET' and payload['auth_index'] in {'idx-a', 'idx-b'}
        requests.append(payload['auth_index'])
        weekly_used = 60 if payload['auth_index'] == 'idx-a' else 20
        limit = {'allowed': True, 'limit_reached': False,
                 'primary_window': {'used_percent': 10, 'limit_window_seconds': 18000, 'reset_at': reset_5h},
                 'secondary_window': {'used_percent': weekly_used, 'limit_window_seconds': 604800, 'reset_at': reset_weekly}}
        self.reply({'status_code': 200, 'body': json.dumps({'rate_limit': limit})})


def invoke(method, payload):
    raw = json.dumps(payload).encode()
    source, out = C.create_string_buffer(raw), Buffer()
    status = api.call(method.encode(), source, len(raw), C.byref(out))
    try:
        envelope = json.loads(C.string_at(out.ptr, out.len))
    finally:
        api.free(out.ptr, out.len)
    assert status == 0 and envelope['ok'], envelope
    return envelope['result']


def quota_status():
    response = invoke('management.handle', {'Method': 'GET', 'Path': '/v0/management/plugins/codex-quota-scheduler/quota'})
    return json.loads(base64.b64decode(response['Body']))


def configure(cfg, method):
    invoke(method, {'config_yaml': base64.b64encode(json.dumps(cfg).encode()).decode()})
    deadline = time.monotonic() + 8
    while quota_status().get('fresh_snapshots', 0) < 2:
        assert time.monotonic() < deadline, quota_status()
        time.sleep(.05)


server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
dll = C.CDLL(str(pathlib.Path(__file__).with_name('codex-quota-scheduler.dll')))
dll.cliproxy_plugin_init.argtypes = [C.c_void_p, C.POINTER(API)]
assert dll.cliproxy_plugin_init(None, C.byref(api)) == 0 and api.version == 1
try:
    # Use a normal workspace directory: Windows sandbox tokens cannot always
    # reopen Python 3.13's owner-only mkdtemp directory ACL.
    with contextlib.nullcontext(pathlib.Path(__file__).parent / ('smoke-state-' + uuid.uuid4().hex)) as temp:
        root = pathlib.Path(temp)
        root.mkdir()
        (root / 'key').write_text('local-test-key', encoding='utf-8')
        cfg = {'enabled': True, 'state_path': str(root / 'cold.json'),
               'cpa_management_url': f'http://127.0.0.1:{server.server_port}/v0/management/api-call',
               'cpa_management_key_file': str(root / 'key'), 'refresh_interval': '1s',
               'quota_refresh_cooldown': '30s', 'warmup_enabled': False}
        configure(cfg, 'plugin.register')
        first = invoke('scheduler.pick', candidate_request)
        assert first['Handled'] and first['AuthID'] == 'account-b', first

        # Restore a previously committed 40% primary. No production state is used.
        cfg['state_path'] = str(root / 'ongoing.json')
        selected_at = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(minutes=10)).isoformat()
        (root / 'ongoing.json').write_text(json.dumps({'version': 6, 'serial_active_auth_id': 'account-a',
            'serial_selection_source': 'auto', 'serial_selected_at': selected_at}), encoding='utf-8')
        configure(cfg, 'plugin.reconfigure')
        current = invoke('scheduler.pick', candidate_request)
        assert current['AuthID'] == 'account-a', current
        evidence = quota_status()
        assert evidence['serial_weekly_rebalance']['confirmations'] == 1, evidence
        assert evidence['serial_weekly_rebalance']['advantage_percent'] > 100, evidence
        assert evidence['serial_weekly_rebalance']['metric'] == 'weekly_budget_relative_percent', evidence
        assert evidence['quota_default_plan'] == 'team_standard', evidence
        assert evidence['serial_weekly_rebalance_min_hold'] == '5m0s', evidence
        assert evidence['serial_weekly_rebalance_required_confirmations'] == 2, evidence
        for _ in range(100):
            assert invoke('scheduler.pick', candidate_request)['AuthID'] == 'account-a'
        assert quota_status()['serial_weekly_rebalance']['confirmations'] == 1
        deadline = time.monotonic() + 38
        while invoke('scheduler.pick', candidate_request)['AuthID'] == 'account-a':
            assert time.monotonic() < deadline, quota_status()
            time.sleep(.1)
        final_status = quota_status()
        assert final_status['serial_active_auth_id'] == 'account-b', final_status
        assert final_status['serial_last_switch_reason'] == 'weekly_budget_rebalance', final_status
        for _ in range(100):
            assert invoke('scheduler.pick', candidate_request)['AuthID'] == 'account-b'
        api.stop()
        saved = json.loads((root / 'ongoing.json').read_text(encoding='utf-8'))
        assert saved['serial_active_auth_id'] == 'account-b'
        assert all(b['auth_id'] == 'account-b' for b in saved['serial_overdraft'].values())
        print(json.dumps({'abi': 1, 'cold_start_80_over_40': True, 'cache_reads_do_not_confirm': True,
                          'two_observations_rebalance': True, 'same_session_stays_on_replacement': True,
                          'switch_persisted': True, 'model_requests': 0, 'local_quota_queries': len(requests)}))
finally:
    api.stop()
    server.shutdown()
    server.server_close()
