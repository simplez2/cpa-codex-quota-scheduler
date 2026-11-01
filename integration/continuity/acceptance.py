#!/usr/bin/env python3
"""Exercise a patched CPA executable and the real native scheduler locally.

All credentials and quota observations are synthetic. Both the fake quota
management endpoint and the Codex upstream listen only on 127.0.0.1. This is
not a production test and it makes no external model calls.

Example:
    python integration/continuity/acceptance.py --cpa output/continuity/cpa.exe \
        --plugin output/continuity/codex-quota-scheduler.dll \
        --output output/continuity/acceptance.json
"""

from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


MODEL = "gpt-5.4"
CLIENT_KEY = "local-acceptance-client-key"
MANAGEMENT_KEY = "local-acceptance-management-key"
UPSTREAM_KEYS = ("local-fake-account-a", "local-fake-account-b")


def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def remove_fixture_directory(path: Path, case: str):
    # Do not recursively delete a redirected or unexpected Windows path.
    temp_root = Path(tempfile.gettempdir()).resolve(strict=True)
    if path.is_symlink() or (hasattr(path, "is_junction") and path.is_junction()):
        raise RuntimeError(f"refusing to remove redirected fixture directory: {path}")
    resolved = path.resolve(strict=True)
    if (resolved.parent != temp_root or resolved.name != path.name or
            not resolved.name.startswith(f"cpa-continuity-{case}-")):
        raise RuntimeError(f"refusing to remove unexpected fixture directory: {resolved}")
    shutil.rmtree(resolved)


def credential_id(key: str, base_url: str) -> str:
    # CPA v8.0.4 synthesizer.StableIDGenerator, with empty proxy/prefix/headers.
    pieces = ("codex:apikey", key, base_url, "", "", "")
    return "codex:apikey:" + hashlib.sha256("\0".join(pieces).encode()).hexdigest()[:12]


def credential_index(key: str, base_url: str) -> str:
    # CPA Auth.EnsureIndex for a Codex API key.
    seed = f"codex-api-key:{base_url}+{key}"
    return hashlib.sha256(seed.encode()).hexdigest()[:16]


class Fixture:
    def __init__(self, case: str):
        self.case = case
        self.lock = threading.Lock()
        self.reset_at = 0
        self.weekly_reset_at = 0
        self.refill_enabled = False
        self.rejected_once = False
        self.quota_calls = []
        self.generations = []
        self.blocked_proxy_requests = 0

        class LocalServer(ThreadingHTTPServer):
            def handle_error(self, request, client_address):
                # A local client may close its keep-alive socket during teardown.
                import sys
                if isinstance(sys.exc_info()[1], (ConnectionError, BrokenPipeError)):
                    return
                super().handle_error(request, client_address)

        self.server = LocalServer(("127.0.0.1", 0), self.handler_type())
        self.base_url = f"http://127.0.0.1:{self.server.server_port}"
        keys = UPSTREAM_KEYS[:1] if case == "retry429" else UPSTREAM_KEYS
        self.accounts = [
            {"id": credential_id(key, self.base_url),
             "auth_index": credential_index(key, self.base_url),
             "key": key, "provider": "codex", "status": "active"}
            for key in keys
        ]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def handler_type(self):
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def reply(self, code: int, body: bytes, content_type="application/json", headers=None):
                self.send_response(code)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                for name, value in (headers or {}).items():
                    self.send_header(name, value)
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                if self.path == "/v0/management/auth-files":
                    if self.headers.get("Authorization") != f"Bearer {MANAGEMENT_KEY}":
                        self.reply(401, b"{}")
                        return
                    files = [{key: value for key, value in account.items() if key != "key"}
                             for account in fixture.accounts]
                    self.reply(200, json.dumps({"files": files}).encode())
                    return
                self.reply(404, b"{}")

            def do_CONNECT(self):
                # CPA has an unrelated background metadata updater. Keep its
                # default-proxy traffic local and reject external CONNECTs.
                with fixture.lock:
                    fixture.blocked_proxy_requests += 1
                self.reply(403, b"{}")

            def do_POST(self):
                length = int(self.headers.get("Content-Length", "0"))
                raw = self.rfile.read(length)
                if self.path == "/v0/management/api-call":
                    if self.headers.get("Authorization") != f"Bearer {MANAGEMENT_KEY}":
                        self.reply(401, b"{}")
                        return
                    request = json.loads(raw)
                    index = request.get("authIndex", request.get("auth_index", ""))
                    account = next((a for a in fixture.accounts if a["auth_index"] == index), None)
                    if account is None:
                        self.reply(400, b'{"error":"unknown fake identity"}')
                        return
                    now = time.time()
                    with fixture.lock:
                        if not fixture.reset_at:
                            horizon = 60 if fixture.case in ("timeout", "weekly") else 10
                            fixture.reset_at = math.ceil(now + horizon)
                        reset_at = fixture.reset_at
                        weekly_only = fixture.case == "weekly" and account["key"] == UPSTREAM_KEYS[1]
                        if weekly_only and not fixture.weekly_reset_at:
                            fixture.weekly_reset_at = math.ceil(now + 5)
                        weekly_reset_at = fixture.weekly_reset_at
                        refilled = fixture.refill_enabled and now >= reset_at + 2
                        pre_429 = fixture.case == "retry429" and not fixture.rejected_once
                        weekly_refreshed = weekly_only and now >= weekly_reset_at + 2
                        fixture.quota_calls.append({"auth_index": index, "at": now, "refilled": refilled,
                                                    "weekly_refreshed": weekly_refreshed})
                    weekly = {"used_percent": 20, "limit_window_seconds": 604800,
                              "reset_at": (weekly_reset_at if weekly_only and not weekly_refreshed
                                           else int(now + 604800))}
                    if weekly_only:
                        primary, secondary = weekly, None
                    else:
                        primary = {"used_percent": 10 if refilled or pre_429 else 100,
                                   "limit_window_seconds": 18000,
                                   "reset_at": int(now + 18000) if refilled else reset_at}
                        secondary = weekly
                    quota = {"plan_type": "team", "rate_limit": {
                        "allowed": True, "limit_reached": False,
                        "primary_window": primary, "secondary_window": secondary}}
                    response = {"status_code": 200, "header": {}, "body": json.dumps(quota)}
                    self.reply(200, json.dumps(response).encode())
                    return
                if self.path == "/responses":
                    authorization = self.headers.get("Authorization", "")
                    account = next((a for a in fixture.accounts
                                    if authorization == f"Bearer {a['key']}"), None)
                    if account is None:
                        self.reply(401, b'{"error":"unknown fake key"}')
                        return
                    request = json.loads(raw)
                    input_identity = json.dumps({"model": request.get("model"), "input": request.get("input")},
                                                sort_keys=True, separators=(",", ":")).encode()
                    input_digest = hashlib.sha256(input_identity).hexdigest()
                    with fixture.lock:
                        is_rejection = fixture.case == "retry429" and not fixture.rejected_once
                        if is_rejection:
                            fixture.rejected_once = True
                        fixture.generations.append({"auth_index": account["auth_index"], "at": time.time(),
                                                    "outcome": "429" if is_rejection else "completed",
                                                    "input_sha256": input_digest})
                        reset_at = fixture.reset_at
                    if is_rejection:
                        body = json.dumps({"error": {"type": "usage_limit_reached", "code": "usage_limit_reached",
                                                     "message": "local synthetic 5h exhaustion", "resets_at": reset_at}}).encode()
                        self.reply(429, body, headers={
                            "X-Codex-Primary-Window-Minutes": "300",
                            "X-Codex-Primary-Used-Percent": "100",
                            "X-Codex-Primary-Reset-At": str(reset_at),
                            "Retry-After": str(max(1, reset_at - int(time.time()))),
                        })
                        return
                    output = [{"id": "msg_local", "type": "message", "role": "assistant",
                               "status": "completed", "content": [
                                   {"type": "output_text", "text": "local-ok", "annotations": []}]}]
                    events = [
                        {"type": "response.created", "response": {"id": "resp_local", "object": "response",
                         "status": "in_progress", "model": MODEL, "output": []}},
                        {"type": "response.output_item.done", "output_index": 0, "item": output[0]},
                        {"type": "response.completed", "response": {"id": "resp_local", "object": "response",
                         "status": "completed", "model": MODEL, "output": output,
                         "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
                    ]
                    body = b"".join(("data: " + json.dumps(event) + "\n\n").encode() for event in events)
                    self.reply(200, body, "text/event-stream")
                    return
                self.reply(404, b"{}")

        return Handler

    def snapshot(self):
        with self.lock:
            return {"reset_at": self.reset_at, "quota_calls": list(self.quota_calls),
                    "generations": list(self.generations),
                    "blocked_proxy_requests": self.blocked_proxy_requests}

    def enable_refill(self):
        with self.lock:
            self.refill_enabled = True

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


def request_json(port: int, path: str):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=2)
    try:
        conn.request("GET", path, headers={"Authorization": f"Bearer {MANAGEMENT_KEY}"})
        response = conn.getresponse()
        body = response.read()
        if response.status != 200:
            raise RuntimeError(f"management {path}: HTTP {response.status}")
        return json.loads(body)
    finally:
        conn.close()


def wait_until(predicate, seconds: float, label: str):
    deadline = time.monotonic() + seconds
    last = None
    while time.monotonic() < deadline:
        try:
            value = predicate()
            if value:
                return value
        except (OSError, ValueError, RuntimeError, http.client.HTTPException) as err:
            last = str(err)
        time.sleep(0.15)
    raise AssertionError(f"timeout waiting for {label}; last error={last}")


class CPA:
    def __init__(self, executable: Path, plugin: Path, fixture: Fixture, temp: Path):
        self.fixture = fixture
        self.port = free_port()
        self.dir = temp
        self.log_path = temp / "cpa.log"
        self.log_file = self.log_path.open("w", encoding="utf-8")
        self.process = None
        plugin_dir = temp / "plugins"
        plugin_dir.mkdir()
        if plugin.suffix.lower() not in (".dll", ".so", ".dylib"):
            raise ValueError("unsupported native scheduler library suffix")
        shutil.copy2(plugin, plugin_dir / ("codex-quota-scheduler" + plugin.suffix.lower()))
        (temp / "auths").mkdir()
        (temp / "management.key").write_text(MANAGEMENT_KEY, encoding="utf-8")
        max_wait = getattr(fixture, "max_wait", "5s" if fixture.case == "timeout" else "30s")
        # Isolated fixtures may shorten these two fallback timers; production
        # configuration and ordinary acceptance cases keep their defaults.
        short_ban = bool(getattr(fixture, "short_fallback_ban", False))
        ban_test_config = "      fallback_ban: 1s\n      half_open_retry_after: 1s\n" if short_ban else ""
        keys_yaml = "\n".join(f"        - api-key: {json.dumps(a['key'])}" for a in fixture.accounts)
        q = json.dumps  # JSON string literals are valid YAML scalars.
        config = f"""config-version: 8
server:
  host: 127.0.0.1
  port: {self.port}
management:
  secret-key: {q(MANAGEMENT_KEY)}
  disable-control-panel: true
access:
  api-keys: [{q(CLIENT_KEY)}]
oauth:
  auth-dir: {q(str(temp / 'auths'))}
requests:
  proxy-url: direct
  streaming:
    keepalive-seconds: 1
routing:
  retry:
    request-retry: 0
api-keys:
  codex:
    - name: local-fixture
      base-url: {q(fixture.base_url)}
      models:
        - name: {q(MODEL)}
      keys:
{keys_yaml}
plugins:
  enabled: true
  dir: {q(str(plugin_dir))}
  configs:
    codex-quota-scheduler:
      enabled: true
      priority: 2000
      scheduler_mode: serial
      account_concurrency_enabled: false
      continuity_enabled: true
      continuity_pacing: false
      continuity_max_wait: {max_wait}
      continuity_poll_interval: 1s
{ban_test_config}      cpa_management_url: {q(fixture.base_url + '/v0/management/api-call')}
      cpa_management_key_file: {q(str(temp / 'management.key'))}
      quota_url: {q(fixture.base_url + '/quota')}
      refresh_interval: 1s
      quota_refresh_cooldown: 30s
      stale_after: 2m
      state_path: {q(str(temp / 'state.json'))}
      reserve_5h_percent: 0
      reserve_weekly_percent: 0
      auth_expiry_auto_repair: false
      warmup_enabled: false
"""
        config_path = temp / "config.yaml"
        config_path.write_text(config, encoding="utf-8")
        env = dict(os.environ)
        env["NO_PROXY"] = "127.0.0.1,localhost"
        env["no_proxy"] = env["NO_PROXY"]
        for key in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
            env[key] = fixture.base_url
        self.process = subprocess.Popen(
            [str(executable), "--config", str(config_path), "--local-model"], cwd=temp,
            env=env, stdout=self.log_file, stderr=subprocess.STDOUT,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))

    def status(self):
        return request_json(self.port, "/v0/management/plugins/codex-quota-scheduler/scheduler-status")

    def ready(self):
        stable_generation, stable_since = None, 0.0
        def check():
            nonlocal stable_generation, stable_since
            if self.process.poll() is not None:
                raise AssertionError(f"CPA exited with code {self.process.returncode}")
            status = self.status()
            if not status.get("generation_active"):
                stable_generation, stable_since = None, 0.0
                return False
            generation = status.get("runtime_generation")
            if generation != stable_generation:
                stable_generation, stable_since = generation, time.monotonic()
                return False
            # CPA can reconfigure the initial plugin after loading native
            # credentials. Never seed a continuity test on that retired owner.
            if time.monotonic() - stable_since < .5:
                return False
            actual = request_json(self.port, "/v0/management/codex-api-key").get("codex-api-key", [])
            actual_indexes = {item.get("auth-index") for item in actual}
            expected_indexes = {item["auth_index"] for item in self.fixture.accounts}
            if actual_indexes != expected_indexes:
                return False
            polls = status.get("quota_polls", {})
            if not all(a["id"] in polls for a in self.fixture.accounts):
                return False
            if status.get("fresh_snapshots", 0) < len(self.fixture.accounts):
                return False
            # Cached snapshots may have been restored by CPA's startup
            # reconfiguration while the new plugin instance has no full native
            # observation yet. Weekly-only classification requires a fresh
            # native probe in this runtime, not merely a fresh persisted cache.
            if (self.fixture.case == "weekly" and
                    not any(q.get("weekly_refreshed") for q in self.fixture.snapshot()["quota_calls"])):
                return False
            snapshots = {item.get("auth_id"): item for item in status.get("snapshots", [])}
            for a in self.fixture.accounts:
                snapshot = snapshots.get(a["id"], {})
                if (not snapshot.get("fresh") or snapshot.get("auth_index") != a["auth_index"]
                        or snapshot.get("five_hour_limit_status") not in ("limited", "not_reported")):
                    return False
                if self.fixture.case == "weekly":
                    expected = "not_reported" if a["key"] == UPSTREAM_KEYS[1] else "limited"
                    if snapshot.get("five_hour_limit_status") != expected:
                        return False
                if self.fixture.case == "retry429" and not snapshot.get("eligible"):
                    return False
            return status
        return wait_until(check, 45, "CPA plugin and native quota observations")

    def close(self):
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        self.log_file.close()


def open_stream(port: int, headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=35)
    payload = {"model": MODEL, "stream": True,
               "input": [{"role": "user", "content": "local test"}]}
    request_headers = {"Authorization": f"Bearer {CLIENT_KEY}", "Content-Type": "application/json"}
    request_headers.update(headers or {})
    conn.request("POST", "/v1/responses", json.dumps(payload), headers=request_headers)
    return conn, conn.getresponse()


def read_to_end(response):
    result = []
    while True:
        line = response.readline()
        if not line:
            return b"".join(result)
        result.append(line)


def assert_wait(cpa: CPA, fixture: Fixture, response, expected_generations=0):
    assert response.status == 200, f"expected SSE 200, got {response.status}"
    assert response.getheader("Content-Type", "").startswith("text/event-stream")
    first = response.readline()
    assert first.startswith(b":"), f"expected SSE heartbeat before any output, got {first[:300]!r}"
    assert len(fixture.snapshot()["generations"]) == expected_generations, "unexpected upstream call during wait"
    status = cpa.status()["continuity"]
    assert status["host_calls"] > 0 and status["last_action"] == "wait", status
    return status


def assert_complete(payload: bytes, fixture: Fixture, expected_key=None):
    assert payload.count(b'"type":"response.completed"') + payload.count(b'"type": "response.completed"') == 1, payload[:3000]
    assert b"local-ok" in payload, payload[:3000]
    observed = fixture.snapshot()
    assert len(observed["generations"]) == 1, observed["generations"]
    if expected_key:
        expected = next(a["auth_index"] for a in fixture.accounts if a["key"] == expected_key)
        assert observed["generations"][0]["auth_index"] == expected, observed["generations"]
    return observed


def run_case(case: str, cpa: CPA, fixture: Fixture):
    started = time.monotonic()
    initial_status = cpa.ready()
    if case in ("websocket", "ws_cancel"):
        try:
            import websocket
        except ImportError as err:
            return {"status": "SKIP", "reason": str(err)}
        ws = websocket.create_connection(f"ws://127.0.0.1:{cpa.port}/v1/responses",
                                         header=[f"Authorization: Bearer {CLIENT_KEY}"], timeout=30)
        try:
            ws.send(json.dumps({"type": "response.create", "model": MODEL,
                                "input": [{"type": "message", "role": "user", "content": "local test"}]}))
            opcode, _ = ws.recv_data(control_frame=True)
            assert opcode == websocket.ABNF.OPCODE_PING, f"expected ping, opcode={opcode}"
            assert not fixture.snapshot()["generations"], "upstream was called during websocket wait"
            continuity = cpa.status()["continuity"]
            assert continuity["last_action"] == "wait", continuity
            if case == "ws_cancel":
                # Deliver a real close frame and close the transport while the
                # host is waiting, then let the quota reset be reobserved.
                ws.send_close()
                ws.shutdown()
                fixture.enable_refill()
                reset_at = fixture.snapshot()["reset_at"]
                wait_until(lambda: time.time() >= reset_at + 6, 25,
                           "post-reset WebSocket cancellation observation")
                observed = fixture.snapshot()
                assert any(q["refilled"] for q in observed["quota_calls"]), "refilled quota was not re-observed"
                assert not observed["generations"], "canceled WebSocket request later reached upstream"
                return {"status": "PASS", "ping_before_cancel": True, "generation_count": 0,
                        "refilled_quota_observed": True, "elapsed_seconds": round(time.monotonic()-started, 3)}
            fixture.enable_refill()
            frames = []
            while True:
                opcode, body = ws.recv_data(control_frame=True)
                if opcode != websocket.ABNF.OPCODE_TEXT:
                    continue
                frames.append(body)
                event = json.loads(body)
                if event.get("type") in ("response.completed", "response.failed", "error"):
                    assert event.get("type") == "response.completed", event
                    break
            observed = assert_complete(b"\n".join(frames), fixture)
            assert any(q["refilled"] and q["at"] <= observed["generations"][0]["at"]
                       for q in observed["quota_calls"]), observed
            return {"status": "PASS", "ping_before_upstream": True, "generation_count": 1,
                    "quota_calls": len(observed["quota_calls"]), "elapsed_seconds": round(time.monotonic()-started, 3)}
        finally:
            ws.close()

    request_started = time.monotonic()
    conn, response = open_stream(cpa.port)
    try:
        if case == "weekly":
            assert response.status == 200, response.status
            payload = read_to_end(response)
            observed = assert_complete(payload, fixture, UPSTREAM_KEYS[1])
            continuity = cpa.status()["continuity"]
            assert continuity["last_action"] == "ready" and continuity["last_reason"] == "weekly_only_available", {
                "continuity": continuity, "initial_snapshots": initial_status.get("snapshots"),
                "initial_polls": initial_status.get("quota_polls"),
                "generation": observed["generations"],
            }
            return {"status": "PASS", "reason": continuity["last_reason"], "generation_count": 1,
                    "selected_weekly_only": True, "fresh_weekly_probe_after_simulated_reset": True,
                    "elapsed_seconds": round(time.monotonic()-started, 3)}
        continuity = assert_wait(cpa, fixture, response, 1 if case == "retry429" else 0)
        if case == "retry429":
            before = fixture.snapshot()
            assert before["generations"][0]["outcome"] == "429", before
            fixture.enable_refill()
            payload = read_to_end(response)
            observed = fixture.snapshot()
            assert len(observed["generations"]) == 2, observed
            assert [item["outcome"] for item in observed["generations"]] == ["429", "completed"], observed
            assert observed["generations"][0]["input_sha256"] == observed["generations"][1]["input_sha256"], observed
            assert any(q["refilled"] and q["at"] <= observed["generations"][1]["at"]
                       for q in observed["quota_calls"]), observed
            assert observed["generations"][1]["at"] >= observed["reset_at"] + 2, observed
            assert payload.count(b'"type":"response.completed"') + payload.count(b'"type": "response.completed"') == 1, payload[:3000]
            assert b"local-ok" in payload and b"usage_limit_reached" not in payload, payload[:3000]
            return {"status": "PASS", "heartbeat_after_rejected_429": True,
                    "attempt_outcomes": ["429", "completed"], "refilled_quota_observed": True,
                    "same_model_and_input": True, "rejection_hidden": True,
                    "elapsed_seconds": round(time.monotonic()-started, 3)}
        if case == "timeout":
            payload = read_to_end(response)
            assert b"response.failed" in payload or b'"type":"error"' in payload or b'"type": "error"' in payload, payload[:3000]
            assert b"response.completed" not in payload, payload[:3000]
            assert not fixture.snapshot()["generations"], "timed-out request reached upstream"
            request_seconds = time.monotonic() - request_started
            assert 4 <= request_seconds <= 9, f"5s absolute wait took {request_seconds:.3f}s"
            return {"status": "PASS", "heartbeat_before_upstream": True, "generation_count": 0,
                    "max_wait_ms": continuity["max_wait_ms"], "error_after_sse_commit": True,
                    "request_seconds": round(request_seconds, 3),
                    "elapsed_seconds": round(time.monotonic()-started, 3)}
        if case == "cancel":
            response.close()
            conn.close()
            fixture.enable_refill()
            reset_at = fixture.snapshot()["reset_at"]
            wait_until(lambda: time.time() >= reset_at + 6, 25, "post-reset cancellation observation")
            observed = fixture.snapshot()
            assert any(q["refilled"] for q in observed["quota_calls"]), "refilled quota was not re-observed"
            assert not observed["generations"], "canceled request later reached upstream"
            return {"status": "PASS", "heartbeat_before_cancel": True, "generation_count": 0,
                    "refilled_quota_observed": True, "elapsed_seconds": round(time.monotonic()-started, 3)}
        fixture.enable_refill()
        payload = read_to_end(response)
        observed = assert_complete(payload, fixture)
        generation_at = observed["generations"][0]["at"]
        assert any(q["refilled"] and q["at"] <= generation_at for q in observed["quota_calls"]), observed
        assert generation_at >= observed["reset_at"] + 2, observed
        return {"status": "PASS", "heartbeat_before_upstream": True, "generation_count": 1,
                "refilled_quota_observed": True, "quota_calls": len(observed["quota_calls"]),
                "elapsed_seconds": round(time.monotonic()-started, 3)}
    finally:
        response.close()
        conn.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", required=True, type=Path, help="patched native CPA executable")
    parser.add_argument("--plugin", required=True, type=Path, help="built scheduler DLL, SO or dylib")
    parser.add_argument("--case", choices=("all", "recovery", "retry429", "weekly", "cancel", "timeout", "websocket", "ws_cancel"), default="all")
    parser.add_argument("--output", type=Path, help="write non-secret JSON evidence")
    parser.add_argument("--keep", action="store_true", help="retain generated temp directories and CPA logs")
    args = parser.parse_args()
    cpa_path, plugin_path = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    cases = ["recovery", "retry429", "weekly", "cancel", "timeout", "websocket", "ws_cancel"] if args.case == "all" else [args.case]
    result = {"cpa_sha256": digest(cpa_path), "plugin_sha256": digest(plugin_path),
              "harness_sha256": digest(Path(__file__).resolve()),
              "scope": "local fake quota and upstream; no production or paid model calls", "cases": {}}
    for case in cases:
        temp = Path(tempfile.mkdtemp(prefix=f"cpa-continuity-{case}-"))
        fixture = Fixture(case)
        cpa = None
        try:
            cpa = CPA(cpa_path, plugin_path, fixture, temp)
            result["cases"][case] = run_case(case, cpa, fixture)
        except Exception as err:
            result["cases"][case] = {"status": "FAIL", "error": str(err),
                                     "traceback": traceback.format_exc(limit=5)}
        finally:
            if cpa is not None:
                cpa.close()
            result["cases"][case]["blocked_background_proxy_requests"] = fixture.snapshot()["blocked_proxy_requests"]
            fixture.close()
            if result["cases"][case]["status"] == "FAIL" and (temp / "cpa.log").exists():
                result["cases"][case]["cpa_log_tail"] = (temp / "cpa.log").read_text(encoding="utf-8", errors="replace")[-6000:]
            if args.keep:
                result["cases"][case]["temporary_directory"] = str(temp)
            else:
                remove_fixture_directory(temp, case)
        printed = {key: value for key, value in result["cases"][case].items()
                   if key not in ("traceback", "cpa_log_tail")}
        if "error" in printed and len(printed["error"]) > 800:
            printed["error"] = printed["error"][:800] + "… (see JSON evidence)"
        print(json.dumps({case: printed}, ensure_ascii=False), flush=True)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return 1 if any(value["status"] != "PASS" for value in result["cases"].values()) else 0


if __name__ == "__main__":
    raise SystemExit(main())
