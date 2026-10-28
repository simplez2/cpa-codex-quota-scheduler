#!/usr/bin/env python3
"""Count retries against loopback fakes using a real CPA and scheduler plugin.

The tests never use production credentials or contact a model provider. See
README.md for the timing contracts and examples.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import http.client
import json
import math
from pathlib import Path
import socket
import tempfile
import threading
import time
import traceback

from acceptance import (CLIENT_KEY, CPA, Fixture, MANAGEMENT_KEY, MODEL, UPSTREAM_KEYS,
                        digest, remove_fixture_directory, wait_until)


CLIENTS = 8
RESET_SECONDS = 7
RETRY_AFTER_SECONDS = 18
QUOTA_RETRY_AFTER_SECONDS = 40


class StormFixture(Fixture):
    def __init__(self, case: str):
        self.mode = case
        # Keep the 8 waiters alive beyond the default 30-second standby
        # refresh interval while measuring the longer quota Retry-After.
        self.max_wait = "60s" if case == "quota_backoff" else ("45s" if case in ("half_open", "shared_account") else "30s")
        self.short_fallback_ban = True
        self.release_probe = threading.Event()
        self.retry_after_until = 0.0
        self.second_retry_after_until = 0.0
        self.second_429_sent_at = 0.0
        self.second_429_sent_count = 0
        self.long_reset_at = 0
        super().__init__(case)
        if case in ("shared_account", "healthy_weekly"):
            # Distinct credentials, same underlying account in both the
            # inventory and the authoritative native quota response.
            for account in self.accounts:
                account["id_token"] = {"chatgpt_account_id": "local-shared-account"}
        else:
            self.accounts = self.accounts[:1]

    def handler_type(self):
        fixture = self
        BaseHandler = super().handler_type()

        class Handler(BaseHandler):
            def do_POST(self):
                if self.path not in ("/v0/management/api-call", "/responses"):
                    return super().do_POST()
                raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                request = json.loads(raw)
                if self.path == "/v0/management/api-call":
                    if self.headers.get("Authorization") != f"Bearer {MANAGEMENT_KEY}":
                        return self.reply(401, b"{}")
                    index = request.get("authIndex", request.get("auth_index", ""))
                    account = next((a for a in fixture.accounts if index == a["auth_index"]), None)
                    if account is None:
                        return self.reply(400, b'{"error":"unknown fake identity"}')
                    now = time.time()
                    with fixture.lock:
                        if not fixture.reset_at:
                            # The half-open case intentionally creates an
                            # earlier plugin deadline than CPA's 10-second
                            # native 429 cooldown, to exercise that boundary.
                            horizon = 3 if fixture.mode == "half_open" else RESET_SECONDS
                            fixture.reset_at = math.ceil(now + horizon)
                        post_reset = now >= fixture.reset_at + 2
                        status = 429 if fixture.mode == "quota_backoff" and post_reset else 200
                        used = 100 if fixture.mode in ("quota_backoff", "shared_account") and not post_reset else 10
                        weekly_only = (fixture.mode == "healthy_weekly" and account["key"] == UPSTREAM_KEYS[1]
                                       and post_reset)
                        if fixture.mode == "healthy_weekly":
                            if account["key"] == UPSTREAM_KEYS[0]:
                                used = 100 if fixture.rejected_once else 10
                            else:
                                used = 20 if weekly_only else 100
                        fixture.quota_calls.append({"at": now, "monotonic": time.monotonic(),
                                                    "status": status, "used_percent": used,
                                                    "post_reset": post_reset, "auth_index": index,
                                                    "weekly_only": weekly_only})
                        reset_at = fixture.reset_at
                        if fixture.mode == "healthy_weekly" and account["key"] == UPSTREAM_KEYS[0] and fixture.rejected_once:
                            reset_at = fixture.long_reset_at
                    if status == 429:
                        response = {"status_code": 429,
                                    "header": {"Retry-After": [str(QUOTA_RETRY_AFTER_SECONDS)]},
                                    "body": '{"error":"synthetic quota probe limit"}'}
                    else:
                        limited_a = (fixture.mode == "healthy_weekly" and account["key"] == UPSTREAM_KEYS[0]
                                     and fixture.rejected_once)
                        primary = {"used_percent": used, "limit_window_seconds": 18000,
                                   "reset_at": reset_at if limited_a or not post_reset else int(now + 18000)}
                        weekly = {"used_percent": 20, "limit_window_seconds": 604800,
                                  "reset_at": int(now + 604800)}
                        if weekly_only:
                            primary, weekly = weekly, None
                        quota = {"plan_type": "team", "rate_limit": {"allowed": True,
                                 "limit_reached": False, "primary_window": primary,
                                 "secondary_window": weekly}}
                        if fixture.mode in ("shared_account", "healthy_weekly"):
                            quota["account_id"] = "local-shared-account"
                        response = {"status_code": 200, "header": {}, "body": json.dumps(quota)}
                    return self.reply(200, json.dumps(response).encode())

                account = next((a for a in fixture.accounts
                                if self.headers.get("Authorization") == f"Bearer {a['key']}"), None)
                if account is None:
                    return self.reply(401, b'{"error":"unknown fake key"}')
                identity = json.dumps({"model": request.get("model"), "input": request.get("input")},
                                      sort_keys=True, separators=(",", ":")).encode()
                now = time.time()
                with fixture.lock:
                    attempt = len(fixture.generations) + 1
                    if attempt == 1 and fixture.mode not in ("quota_backoff", "shared_account"):
                        # Reuse the timestamp returned by the initial native
                        # quota observation so CPA schedules the same reset.
                        # The independent half-open case lets Retry-After
                        # expire by the time the new quota observation arrives.
                        retry_after = 1 if fixture.mode == "half_open" else (30 if fixture.mode == "healthy_weekly" else RETRY_AFTER_SECONDS)
                        fixture.retry_after_until = now + retry_after
                        outcome = "initial_429"
                        reset_at = fixture.reset_at
                        if fixture.mode == "healthy_weekly":
                            fixture.rejected_once = True
                            fixture.long_reset_at = math.ceil(now + retry_after)
                            reset_at = fixture.long_reset_at
                    else:
                        outcome = ("held_probe" if fixture.mode in ("half_open", "shared_account") else
                                   "completed" if fixture.mode == "healthy_weekly" and account["key"] == UPSTREAM_KEYS[1]
                                   else "unexpected_generation")
                        reset_at = math.ceil(now + RESET_SECONDS)
                        retry_after = RETRY_AFTER_SECONDS
                    fixture.generations.append({"at": now, "monotonic": time.monotonic(),
                                                "outcome": outcome, "auth_index": account["auth_index"],
                                                "input_sha256": hashlib.sha256(identity).hexdigest()})
                if outcome == "completed":
                    output = [{"id": "msg_local", "type": "message", "role": "assistant", "status": "completed",
                               "content": [{"type": "output_text", "text": "local-ok", "annotations": []}]}]
                    events = [
                        {"type": "response.created", "response": {"id": "resp_local", "object": "response",
                         "status": "in_progress", "model": MODEL, "output": []}},
                        {"type": "response.output_item.done", "output_index": 0, "item": output[0]},
                        {"type": "response.completed", "response": {"id": "resp_local", "object": "response",
                         "status": "completed", "model": MODEL, "output": output,
                         "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
                    ]
                    body = b"".join(("data: " + json.dumps(event) + "\n\n").encode() for event in events)
                    return self.reply(200, body, "text/event-stream")
                if outcome == "held_probe":
                    # A generation remains open until all concurrent contenders
                    # have had an opportunity to race through CPA selection.
                    fixture.release_probe.wait(timeout=12)
                    now = time.time()
                    with fixture.lock:
                        reset_at = math.ceil(now + RESET_SECONDS)
                        fixture.second_retry_after_until = now + RETRY_AFTER_SECONDS
                body = json.dumps({"error": {"type": "usage_limit_reached", "code": "usage_limit_reached",
                                             "message": "synthetic rate limit", "resets_at": reset_at}}).encode()
                self.reply(429, body, headers={
                    "X-Codex-Primary-Window-Minutes": "300",
                    "X-Codex-Primary-Used-Percent": "100",
                    "X-Codex-Primary-Reset-At": str(reset_at),
                    "Retry-After": str(retry_after),
                })
                if outcome == "held_probe":
                    with fixture.lock:
                        fixture.second_429_sent_at = time.time()
                        fixture.second_429_sent_count += 1

        return Handler

    def snapshot(self):
        with self.lock:
            return {"reset_at": self.reset_at, "retry_after_until": self.retry_after_until,
                    "second_retry_after_until": self.second_retry_after_until,
                    "second_429_sent_at": self.second_429_sent_at,
                    "second_429_sent_count": self.second_429_sent_count,
                    "quota_calls": list(self.quota_calls), "generations": list(self.generations),
                    "blocked_proxy_requests": self.blocked_proxy_requests}


class Clients:
    def __init__(self, port: int, count: int, label: str):
        self.lock = threading.Lock()
        self.sent = 0
        self.finished = 0
        self.finished_details = []
        self.first = []
        self.errors = []
        self.connections = []
        self.threads = []
        self.closing = threading.Event()
        barrier = threading.Barrier(count + 1)
        for index in range(count):
            thread = threading.Thread(target=self.run, args=(port, index, label, barrier), daemon=True)
            self.threads.append(thread)
            thread.start()
        barrier.wait(timeout=5)

    def run(self, port: int, index: int, label: str, barrier: threading.Barrier):
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=35)
        terminal = []
        with self.lock:
            self.connections.append(conn)
        try:
            barrier.wait(timeout=5)
            body = {"model": MODEL, "stream": True,
                    "input": [{"role": "user", "content": f"local {label} {index}"}]}
            conn.request("POST", "/v1/responses", json.dumps(body), headers={
                "Authorization": f"Bearer {CLIENT_KEY}", "Content-Type": "application/json"})
            with self.lock:
                self.sent += 1
            response = conn.getresponse()
            first_line = response.readline()
            with self.lock:
                self.first.append({"http_status": response.status, "heartbeat": first_line.startswith(b":")})
            while True:
                line = response.readline()
                if not line:
                    break
                if line.startswith(b"data:"):
                    text = line[5:].strip().decode("utf-8", errors="replace")
                    if text:
                        terminal.append(text[:1000])
                        del terminal[:-3]
        except (OSError, http.client.HTTPException, threading.BrokenBarrierError, AttributeError) as err:
            # CPython can race HTTPResponse's EOF cleanup with an intentional
            # socket close. Suppress only that known teardown exception.
            known_close_race = (self.closing.is_set() and isinstance(err, AttributeError) and
                                str(err) == "'NoneType' object has no attribute 'close'")
            if not known_close_race:
                with self.lock:
                    self.errors.append(f"{type(err).__name__}: {err}")
        finally:
            conn.close()
            with self.lock:
                self.finished += 1
                self.finished_details.append({"index": index, "last_data": terminal})

    def snapshot(self):
        with self.lock:
            return {"sent": self.sent, "finished": self.finished, "finished_details": list(self.finished_details),
                    "first": list(self.first), "errors": list(self.errors)}

    def close(self):
        self.closing.set()
        with self.lock:
            connections = list(self.connections)
        for conn in connections:
            try:
                if conn.sock is not None:
                    conn.sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            conn.close()
        for thread in self.threads:
            thread.join(timeout=1)


def sent_and_waiting(clients: Clients, count: int):
    wait_until(lambda: clients.snapshot()["sent"] == count, 5, "all client requests sent")
    wait_until(lambda: len(clients.snapshot()["first"]) == count, 6, "all client SSE headers")
    observed = clients.snapshot()
    assert all(item["http_status"] == 200 and item["heartbeat"] for item in observed["first"]), observed


def assert_open(cpa: CPA, seed: Clients, clients: Clients):
    contender_status, seed_status = clients.snapshot(), seed.snapshot()
    if contender_status["finished"] or seed_status["finished"]:
        status = cpa.status()
        raise AssertionError({"clients": contender_status, "seed": seed_status,
                              "continuity": status.get("continuity"),
                              "quarantine": status.get("quarantine"),
                              "recent_decisions": status.get("recent_decisions")})


def run_case(case: str, cpa: CPA, fixture: StormFixture):
    started = time.monotonic()
    cpa.ready()
    groups = []
    try:
        if case not in ("quota_backoff", "shared_account"):
            seed = Clients(cpa.port, 1, "initial")
            groups.append(seed)
            if case == "healthy_weekly":
                # The original request must remain open after A's 429 while
                # both 5-hour accounts are exhausted.
                sent_and_waiting(seed, 1)
                seed_first = seed.snapshot()["first"][0]
                wait_until(lambda: cpa.status().get("quarantine", {}).get("total", 0) >= 1,
                           3, "limited credential quarantine")
            else:
                sent_and_waiting(seed, 1)
            assert len(fixture.snapshot()["generations"]) == 1, fixture.snapshot()
        clients = Clients(cpa.port, CLIENTS, "contender")
        groups.append(clients)
        sent_and_waiting(clients, CLIENTS)

        if case == "healthy_weekly":
            a, b = fixture.accounts
            wait_until(lambda: any(q["auth_index"] == b["auth_index"] and q["weekly_only"] and q["status"] == 200
                                   for q in fixture.snapshot()["quota_calls"]), 20, "fresh weekly-only native quota")
            wait_until(lambda: any(g["auth_index"] == b["auth_index"] and g["outcome"] == "completed"
                                   for g in fixture.snapshot()["generations"]), 10, "healthy weekly credential selected")
            wait_until(lambda: any('"response.completed"' in data
                                   for detail in seed.snapshot()["finished_details"]
                                   for data in detail["last_data"]), 5, "healthy weekly SSE completion")
            first_success = next(g for g in fixture.snapshot()["generations"] if g["outcome"] == "completed")
            wait_until(lambda: time.time() >= first_success["at"] + 2, 4, "continued isolation of limited credential")
            observed = fixture.snapshot()
            a_attempts = [g for g in observed["generations"] if g["auth_index"] == a["auth_index"]]
            b_attempts = [g for g in observed["generations"] if g["auth_index"] == b["auth_index"]]
            assert len(a_attempts) == 1 and a_attempts[0]["outcome"] == "initial_429", observed
            assert b_attempts and all(g["outcome"] == "completed" for g in b_attempts), observed
            assert any(g["input_sha256"] == a_attempts[0]["input_sha256"] for g in b_attempts), observed
            assert first_success["at"] < observed["retry_after_until"], observed
            return {"status": "PASS", "same_workspace_id": True, "distinct_credentials": True,
                    "limited_credential_attempts": len(a_attempts), "healthy_weekly_completions": len(b_attempts),
                    "fresh_weekly_only_quota_observed": True, "completed_before_limited_retry_after": True,
                    "same_seed_request_completed": True, "seed_client_http_status": seed_first["http_status"],
                    "elapsed_seconds": round(time.monotonic()-started, 3)}

        if case == "quota_backoff":
            wait_until(lambda: any(q["status"] == 429 for q in fixture.snapshot()["quota_calls"]),
                       20, "post-reset native quota probe 429")
            first_429 = next(q for q in fixture.snapshot()["quota_calls"] if q["status"] == 429)
            # The fake Retry-After exceeds the 30-second standby refresh
            # cooldown. Keep all clients alive beyond the shorter interval.
            wait_until(lambda: time.monotonic() >= first_429["monotonic"] + 33, 35,
                       "shared native quota backoff observation")
            observed = fixture.snapshot()
            after = [q for q in observed["quota_calls"] if q["at"] >= first_429["at"]]
            assert len(after) == 1, observed
            assert not observed["generations"], observed
            assert clients.snapshot()["finished"] == 0, clients.snapshot()
            return {"status": "PASS", "clients_waiting": CLIENTS, "generation_count": 0,
                    "quota_429_count_during_backoff": len(after), "quota_retry_after_seconds": QUOTA_RETRY_AFTER_SECONDS,
                    "observation_seconds": 33, "elapsed_seconds": round(time.monotonic()-started, 3)}

        if case == "shared_account":
            expected_indexes = {a["auth_index"] for a in fixture.accounts}
            wait_until(lambda: {q["auth_index"] for q in fixture.snapshot()["quota_calls"]
                               if q["status"] == 200 and q["post_reset"]} == expected_indexes,
                       20, "positive native quota on both credentials")
            wait_until(lambda: len(fixture.snapshot()["generations"]) >= len(expected_indexes) or
                       clients.snapshot()["finished"], 10,
                       "one half-open trial for each distinct credential or prematurely finished client")
            assert not clients.snapshot()["finished"], clients.snapshot()
            observed = fixture.snapshot()
            assert len(observed["generations"]) >= len(expected_indexes), observed
            trial = observed["generations"][0]
            for generation in observed["generations"]:
                assert any(q["auth_index"] == generation["auth_index"] and q["status"] == 200 and
                           q["post_reset"] and q["at"] <= generation["at"]
                           for q in observed["quota_calls"]), observed
            wait_until(lambda: time.monotonic() >= trial["monotonic"] + 3, 5,
                       "concurrent shared-account contenders during held trial")
            held = fixture.snapshot()
            assert len(held["generations"]) == len(expected_indexes), held
            assert {g["auth_index"] for g in held["generations"]} == expected_indexes, held
            assert not clients.snapshot()["finished"], clients.snapshot()
            fixture.release_probe.set()
            wait_until(lambda: fixture.snapshot()["second_429_sent_count"] == len(expected_indexes),
                       5, "both independent trial 429 responses")
            wait_until(lambda: time.time() >= fixture.snapshot()["second_retry_after_until"] - 1.5,
                       20, "shared-account second Retry-After observation")
            observed = fixture.snapshot()
            assert len(observed["generations"]) == len(expected_indexes), observed
            assert time.time() < observed["second_retry_after_until"], observed
            return {"status": "PASS", "credential_count": len(expected_indexes), "shared_account_id": True,
                    "clients_waiting": CLIENTS, "positive_quota_credentials": len(expected_indexes),
                    "half_open_trials_while_held": len(expected_indexes),
                    "generation_count_after_429": len(expected_indexes),
                    "second_retry_after_seconds": RETRY_AFTER_SECONDS,
                    "seconds_remaining": round(observed["second_retry_after_until"]-time.time(), 3),
                    "elapsed_seconds": round(time.monotonic()-started, 3)}

        wait_until(lambda: any(q["status"] == 200 and q["post_reset"] for q in fixture.snapshot()["quota_calls"]),
                   20, "new positive native quota observation")
        observed = fixture.snapshot()
        if case == "retry_after":
            assert observed["retry_after_until"] > observed["reset_at"] + 2, observed
            # Stay strictly before the upstream Retry-After deadline while
            # proving that a positive post-reset quota poll already arrived.
            wait_until(lambda: time.time() >= fixture.snapshot()["retry_after_until"] - 1.5,
                       20, "observation immediately before Retry-After expires")
            observed = fixture.snapshot()
            assert len(observed["generations"]) == 1, observed
            assert time.time() < observed["retry_after_until"], observed
            assert clients.snapshot()["finished"] == 0 and seed.snapshot()["finished"] == 0, {
                "seed": seed.snapshot(), "clients": clients.snapshot()}
            return {"status": "PASS", "clients_waiting": CLIENTS,
                    "generation_count_before_retry_after": 1, "post_reset_positive_quota_observed": True,
                    "retry_after_seconds": RETRY_AFTER_SECONDS,
                    "seconds_remaining": round(observed["retry_after_until"]-time.time(), 3),
                    "elapsed_seconds": round(time.monotonic()-started, 3)}

        wait_until(lambda: len(fixture.snapshot()["generations"]) >= 2 or
                   clients.snapshot()["finished"] or seed.snapshot()["finished"],
                   20, "half-open upstream trial or prematurely finished client")
        assert_open(cpa, seed, clients)
        first_trial = fixture.snapshot()["generations"][1]
        assert first_trial["at"] >= fixture.snapshot()["retry_after_until"] - 0.25, fixture.snapshot()
        assert first_trial["at"] >= fixture.snapshot()["generations"][0]["at"] + 9.75, fixture.snapshot()
        wait_until(lambda: time.monotonic() >= first_trial["monotonic"] + 3, 5,
                   "concurrent contenders during held trial")
        held = fixture.snapshot()
        assert len(held["generations"]) == 2, held
        assert_open(cpa, seed, clients)
        fixture.release_probe.set()
        wait_until(lambda: fixture.snapshot()["second_429_sent_at"] > 0, 5, "trial 429 response")
        wait_until(lambda: time.time() >= fixture.snapshot()["second_retry_after_until"] - 1.5,
                   20, "observation immediately before the second Retry-After expires")
        observed = fixture.snapshot()
        assert len(observed["generations"]) == 2, observed
        assert time.time() < observed["second_retry_after_until"], observed
        assert observed["generations"][1]["outcome"] == "held_probe", observed
        return {"status": "PASS", "clients_waiting": CLIENTS, "initial_429": 1,
                "half_open_trials_while_held": 1, "generation_count_after_second_429": 2,
                "post_reset_positive_quota_observed": True, "trial_hold_seconds": 3,
                "second_retry_after_seconds": RETRY_AFTER_SECONDS,
                "seconds_remaining": round(observed["second_retry_after_until"]-time.time(), 3),
                "elapsed_seconds": round(time.monotonic()-started, 3)}
    finally:
        fixture.release_probe.set()
        for group in groups:
            group.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=Path, required=True)
    parser.add_argument("--plugin", type=Path, required=True)
    parser.add_argument("--case", choices=("all", "core", "retry_after", "quota_backoff", "half_open", "shared_account", "healthy_weekly"), default="all")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--keep", action="store_true")
    args = parser.parse_args()
    cpa_path, plugin_path = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    core_cases = ("retry_after", "quota_backoff", "half_open", "shared_account")
    cases = core_cases + ("healthy_weekly",) if args.case == "all" else (core_cases if args.case == "core" else (args.case,))
    result = {"cpa_sha256": digest(cpa_path), "plugin_sha256": digest(plugin_path),
              "harness_sha256": digest(Path(__file__).resolve()),
              "base_harness_sha256": digest(Path(__file__).with_name("acceptance.py")),
              "scope": "loopback synthetic quota and upstream; no production or paid model calls", "cases": {}}
    def execute_case(case):
        temp = Path(tempfile.mkdtemp(prefix=f"cpa-continuity-{case}-"))
        fixture = StormFixture(case)
        cpa = None
        try:
            cpa = CPA(cpa_path, plugin_path, fixture, temp)
            outcome = run_case(case, cpa, fixture)
        except Exception as err:
            outcome = {"status": "FAIL", "error": str(err)[:500],
                       "traceback": traceback.format_exc(limit=5), "observed": fixture.snapshot()}
        finally:
            if cpa is not None:
                cpa.close()
            outcome["observed_at_teardown"] = fixture.snapshot()
            fixture.close()
            if outcome["status"] == "FAIL" and (temp / "cpa.log").exists():
                outcome["cpa_log_tail"] = (temp / "cpa.log").read_text(encoding="utf-8", errors="replace")[-6000:]
            if args.keep:
                outcome["temporary_directory"] = str(temp)
            else:
                remove_fixture_directory(temp, case)
        return case, outcome
    outcomes = {}
    # Separate CPA processes, temp directories and fake servers permit two
    # independent cases to run concurrently without changing their timings.
    with concurrent.futures.ThreadPoolExecutor(max_workers=min(2, len(cases))) as pool:
        for future in concurrent.futures.as_completed(pool.submit(execute_case, case) for case in cases):
            case, outcome = future.result()
            outcomes[case] = outcome
            printed = {k: v for k, v in outcome.items() if k not in
                       ("traceback", "cpa_log_tail", "observed", "observed_at_teardown")}
            print(json.dumps({case: printed}, ensure_ascii=False), flush=True)
    result["cases"] = {case: outcomes[case] for case in cases}
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return 1 if any(value["status"] != "PASS" for value in result["cases"].values()) else 0


if __name__ == "__main__":
    raise SystemExit(main())
