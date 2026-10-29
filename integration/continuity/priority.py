"""Verify expiry-first routing through native CPA using loopback-only quotas."""

import argparse
import json
from pathlib import Path
import tempfile
import time

from acceptance import (CPA, Fixture, MANAGEMENT_KEY, credential_id,
                        credential_index, digest, open_stream, read_to_end,
                        remove_fixture_directory, wait_until)


class PriorityFixture(Fixture):
    def __init__(self, profile="expiry"):
        self.profile = profile
        self.anchor = int(time.time())
        super().__init__("priority")
        self.accounts = [
            {"id": credential_id(key, self.base_url),
             "auth_index": credential_index(key, self.base_url),
             "key": key, "provider": "codex", "status": "active"}
            for key in ("local-priority-sooner", "local-priority-later", "local-priority-weekly")
        ]

    def handler_type(self):
        fixture = self
        base = super().handler_type()

        class Handler(base):
            def do_POST(self):
                if self.path != "/v0/management/api-call":
                    return super().do_POST()
                raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                if self.headers.get("Authorization") != f"Bearer {MANAGEMENT_KEY}":
                    self.reply(401, b"{}")
                    return
                request = json.loads(raw)
                index = request.get("authIndex", request.get("auth_index", ""))
                position = next((i for i, account in enumerate(fixture.accounts)
                                 if account["auth_index"] == index), None)
                if position is None:
                    self.reply(400, b"{}")
                    return
                weekly_only = position == 2
                used = 100 if fixture.profile == "fallback" else (18, 8, 0)[position]
                primary = None if weekly_only else {
                    "used_percent": used, "limit_window_seconds": 18000,
                    "reset_at": fixture.anchor + (40 if position == 0 else 180) * 60}
                weekly = {"used_percent": (51, 7, 1)[position],
                          "limit_window_seconds": 604800,
                          "reset_at": fixture.anchor + (3600 if weekly_only else 6 * 86400)}
                quota = {"plan_type": "team", "rate_limit": {"allowed": True,
                         "limit_reached": False, "primary_window": primary,
                         "secondary_window": weekly}}
                if not weekly_only:
                    quota["rate_limit_reset_credits_available_count"] = (2, 0)[position]
                with fixture.lock:
                    fixture.quota_calls.append({"auth_index": index, "at": time.time(),
                                                "refilled": False, "weekly_refreshed": False})
                response = {"status_code": 200, "header": {}, "body": json.dumps(quota)}
                self.reply(200, json.dumps(response).encode())

        return Handler


def select_mode(cpa, mode):
    if mode != "serial":
        path = cpa.dir / "config.yaml"
        source = path.read_text(encoding="utf-8")
        assert "      scheduler_mode: serial\n" in source
        path.write_text(source.replace("      scheduler_mode: serial\n",
                                       f"      scheduler_mode: {mode}\n"), encoding="utf-8")
        wait_until(lambda: cpa.status().get("scheduler_mode") == mode, 15,
                   "native configuration reload")
    cpa.ready()


def run_case(cpa, fixture, mode):
    select_mode(cpa, mode)
    before = cpa.status()
    snapshots = {item["auth_id"]: item for item in before["snapshots"]}
    sooner, later, weekly = [snapshots[account["id"]] for account in fixture.accounts]
    assert sooner["reset_credits_known"] and sooner["reset_credits"] == 2
    assert later["reset_credits_known"] and later["reset_credits"] == 0
    assert not weekly["reset_credits_known"]
    assert weekly["five_hour_limit_status"] == "not_reported"
    if fixture.profile == "expiry":
        assert sooner["selection_priority"]["rank"] < later["selection_priority"]["rank"]
        assert later["selection_priority"]["rank"] < weekly["selection_priority"]["rank"]
    connection, response = open_stream(cpa.port)
    try:
        payload = read_to_end(response)
    finally:
        response.close()
        connection.close()
    assert b"response.completed" in payload, payload[:1000]
    generations = fixture.snapshot()["generations"]
    expected = fixture.accounts[0 if fixture.profile == "expiry" else 2]
    assert len(generations) == 1 and generations[0]["auth_index"] == expected["auth_index"], generations
    return {"status": "PASS", "profile": fixture.profile, "mode": mode,
            "selected": "five-hour-sooner" if fixture.profile == "expiry" else "weekly-only-backup",
            "generations": len(generations), "official_counts_distinguish_2_0_unknown": True,
            "generation_active": before.get("generation_active")}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=Path, required=True)
    parser.add_argument("--plugin", type=Path, required=True)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    executable, plugin = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    result = {"scope": "loopback synthetic quotas and generations; no production calls",
              "cpa_sha256": digest(executable), "plugin_sha256": digest(plugin),
              "harness_sha256": digest(Path(__file__).resolve()), "cases": {}}
    for mode in ("serial", "balanced"):
        for profile in ("expiry", "fallback"):
            name = mode + "/" + profile
            temp = Path(tempfile.mkdtemp(prefix="cpa-continuity-priority-"))
            fixture, cpa = PriorityFixture(profile), None
            try:
                cpa = CPA(executable, plugin, fixture, temp)
                cpa.ready()
                result["cases"][name] = run_case(cpa, fixture, mode)
            except Exception as error:
                result["cases"][name] = {"status": "FAIL", "error": str(error)[:1000]}
            finally:
                if cpa is not None:
                    cpa.close()
                result["cases"][name]["blocked_background_proxy_requests"] = fixture.snapshot()["blocked_proxy_requests"]
                fixture.close()
                remove_fixture_directory(temp, "priority")
            print(json.dumps({name: result["cases"][name]}, ensure_ascii=False), flush=True)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2)+"\n", encoding="utf-8")
    return int(any(case["status"] != "PASS" for case in result["cases"].values()))


if __name__ == "__main__":
    raise SystemExit(main())
