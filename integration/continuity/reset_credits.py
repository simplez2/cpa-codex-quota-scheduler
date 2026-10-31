"""Exercise reset-credit discovery and opt-in redemption in native CPA.

All upstream calls terminate in a loopback fixture. No real credit is consumed.
"""

import argparse
import json
from pathlib import Path
import tempfile
import threading
import time

from acceptance import CPA, digest, remove_fixture_directory, wait_until
from priority import PriorityFixture


USAGE = "https://chatgpt.com/backend-api/wham/usage"
CREDITS = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"


class ResetFixture(PriorityFixture):
    def __init__(self):
        self.posts = 0
        self.credit_counts = {}
        self.used = {}
        self.request_ids = set()
        self.assertions = []
        self.state_path = None
        self.reset_lock = threading.Lock()
        super().__init__()

    def handler_type(self):
        fixture = self
        base = super().handler_type()

        class Handler(base):
            def do_POST(self):
                if self.path != "/v0/management/api-call":
                    fixture.assertions.append("unexpected non-management POST")
                    self.reply(400, b"{}")
                    return
                call = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
                index = call.get("auth_index")
                if index not in {a["auth_index"] for a in fixture.accounts}:
                    self.reply(400, b"{}")
                    return
                with fixture.reset_lock:
                    count = fixture.credit_counts.setdefault(index, 2)
                    used = fixture.used.setdefault(index, 80)
                    method, url = call.get("method"), call.get("url")
                    if method == "GET" and url in (USAGE, fixture.base_url + "/quota"):
                        body = {"rate_limit": {"allowed": True, "limit_reached": False,
                            "primary_window": {"used_percent": used, "limit_window_seconds": 18000,
                                               "reset_at": fixture.anchor + 3600},
                            "secondary_window": {"used_percent": used, "limit_window_seconds": 604800,
                                                 "reset_at": fixture.anchor + 86400}},
                            "rate_limit_reset_credits": {"available_count": count,
                                                         "applicable_available_count": int(used > 0)}}
                    elif method == "GET" and url == CREDITS:
                        def credit(name, offset):
                            return {"id": index + "-" + name, "reset_type": "codex_rate_limits",
                                    "status": "available", "is_supported_by_plan": True,
                                    "granted_at": "2026-10-01T00:00:00Z",
                                    "expires_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(fixture.anchor + offset))}
                        rows = [credit("later", 86400)]
                        if count == 2:
                            rows.insert(0, credit("expiring", 480))
                        body = {"available_count": count, "credits": rows}
                    elif method == "POST" and url == CREDITS + "/consume":
                        request_id = json.loads(call.get("data", "{}"))["redeem_request_id"]
                        journal = json.loads(Path(str(fixture.state_path) + ".reset-credits.json").read_text())
                        if (request_id not in journal or journal[request_id]["phase"] != "pending"
                                or request_id in fixture.request_ids):
                            fixture.assertions.append("missing/duplicate durable redemption intent")
                        fixture.request_ids.add(request_id)
                        fixture.posts += 1
                        fixture.credit_counts[index], fixture.used[index] = 1, 0
                        body = {"code": "reset"}
                    else:
                        fixture.assertions.append("unexpected method or upstream URL")
                        self.reply(400, b"{}")
                        return
                self.reply(200, json.dumps({"status_code": 200, "header": {}, "body": json.dumps(body)}).encode())

        return Handler


def snapshots(cpa):
    return cpa.status().get("snapshots", [])


def run(cpa, fixture):
    cpa.ready()
    config = cpa.dir / "config.yaml"
    source = config.read_text(encoding="utf-8")
    source = source.replace(json.dumps(fixture.base_url + "/quota"), json.dumps(USAGE))
    source += "      reset_credits_auto_use: false\n      reset_credits_auto_use_before: 10m\n"
    fixture.state_path = cpa.dir / "state.json"
    config.write_text(source, encoding="utf-8")
    wait_until(lambda: len(snapshots(cpa)) == 3 and all(
        s.get("reset_credits") == 2 and len(s.get("reset_credit_details", {}).get("credits", [])) == 2
        for s in snapshots(cpa)), 20, "native reset-credit list discovery")
    assert fixture.posts == 0, "default-off discovery consumed a credit"
    before = snapshots(cpa)
    assert all(not s["reset_credit_details"]["auto_enabled"] for s in before)
    assert all(s["reset_credit_details"]["applicable"] == 1 for s in before)
    source = source.replace("reset_credits_auto_use: false", "reset_credits_auto_use: true")
    config.write_text(source, encoding="utf-8")
    wait_until(lambda: fixture.posts == 3 and all(
        s.get("reset_credit_details", {}).get("last_action") == "verified"
        for s in snapshots(cpa)), 25, "opt-in native credit redemption")
    assert len(fixture.request_ids) == 3
    assert all(s["reset_credits"] == 1 for s in snapshots(cpa))
    # Reproduce a stale upstream list after reload. A spent card must never be
    # redeemed again even when its cached upstream applicability returns to 1.
    config.write_text(source.replace("reset_credits_auto_use: true", "reset_credits_auto_use: false"), encoding="utf-8")
    wait_until(lambda: all(not s.get("reset_credit_details", {}).get("auto_enabled", True)
                          for s in snapshots(cpa)), 15, "disable auto-use")
    with fixture.reset_lock:
        for index in fixture.credit_counts:
            fixture.credit_counts[index], fixture.used[index] = 2, 80
    config.write_text(source, encoding="utf-8")
    wait_until(lambda: all(s.get("reset_credit_details", {}).get("auto_reason") == "previous_attempt_requires_review"
                          for s in snapshots(cpa)), 25, "native reload duplicate fence")
    assert fixture.posts == 3, "reload replayed a redemption"
    assert not fixture.assertions, fixture.assertions
    assert not fixture.snapshot()["generations"], "reset feature generated model text"
    return {"status": "PASS", "credentials": 3, "default_off_writes": 0,
            "simulated_redemptions": fixture.posts, "unique_durable_intents": 3,
            "duplicate_redemptions_after_reload": 0, "model_requests": 0}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=Path, required=True)
    parser.add_argument("--plugin", type=Path, required=True)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    executable, plugin = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    temp = Path(tempfile.mkdtemp(prefix="cpa-continuity-reset-"))
    fixture, cpa = ResetFixture(), None
    result = {"scope": "loopback-only native reset-credit smoke; no real credits or model calls",
              "cpa_sha256": digest(executable), "plugin_sha256": digest(plugin),
              "harness_sha256": digest(Path(__file__).resolve())}
    try:
        cpa = CPA(executable, plugin, fixture, temp)
        result["case"] = run(cpa, fixture)
    except Exception as error:
        result["case"] = {"status": "FAIL", "error": str(error)[:1500]}
    finally:
        if cpa is not None:
            cpa.close()
        fixture.close()
        remove_fixture_directory(temp, "reset")
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(result["case"]), flush=True)
    return int(result["case"]["status"] != "PASS")


if __name__ == "__main__":
    raise SystemExit(main())
