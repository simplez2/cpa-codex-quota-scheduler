"""Check card-weighted ordinary placement using native CPA and loopback-only providers."""
import argparse
import json
from pathlib import Path
import tempfile
import time

from acceptance import (CPA, MANAGEMENT_KEY, digest, open_stream, read_to_end,
                        remove_fixture_directory, wait_until)
from priority import PriorityFixture
from reset_credits import CREDITS, USAGE


class Cards(PriorityFixture):
    def __init__(self):
        self.unexpected = []
        super().__init__()
        self.accounts = sorted(self.accounts[:2], key=lambda a: a["id"])
        self.rich = self.accounts[-1]["auth_index"]

    def handler_type(self):
        fixture = self
        base = super().handler_type()

        class Handler(base):
            def do_POST(self):
                if self.path != "/v0/management/api-call":
                    return super().do_POST()
                call = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                idx = call.get("auth_index", call.get("authIndex"))
                if (self.headers.get("Authorization") != f"Bearer {MANAGEMENT_KEY}" or
                        idx not in {a["auth_index"] for a in fixture.accounts} or call.get("method") != "GET"):
                    fixture.unexpected.append("unexpected write, credential or caller")
                    return self.reply(403, b"{}")
                n = 3 if idx == fixture.rich else 1
                url = call.get("url")
                if url == CREDITS:
                    expiry = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(fixture.anchor + 10 * 86400))
                    body = {"available_count": n, "credits": [
                        {"id": f"local-{idx}-{i}", "reset_type": "codex_rate_limits", "status": "available",
                         "is_supported_by_plan": True, "expires_at": expiry} for i in range(n)]}
                elif url in (USAGE, fixture.base_url + "/quota"):
                    def win(used, length, reset):
                        return {"used_percent": used, "limit_window_seconds": length, "reset_at": fixture.anchor + reset}
                    body = {"plan_type": "team", "rate_limit": {"allowed": True, "limit_reached": False,
                            "primary_window": win(20, 18000, 10800), "secondary_window": win(64, 604800, 518400)},
                            "rate_limit_reset_credits": {"available_count": n, "applicable_available_count": 0}}
                else:
                    fixture.unexpected.append("unexpected upstream URL")
                    return self.reply(400, b"{}")
                self.reply(200, json.dumps({"status_code": 200, "header": {}, "body": json.dumps(body)}).encode())
        return Handler


def run(host, plugin):
    fixture = Cards()
    temp = Path(tempfile.mkdtemp(prefix="cpa-continuity-manual-cards-"))
    cpa = None
    try:
        cpa = CPA(host, plugin, fixture, temp)
        cpa.ready()
        path = temp / "config.yaml"
        source = path.read_text()
        source = source.replace("scheduler_mode: serial", "scheduler_mode: balanced")
        source = source.replace(json.dumps(fixture.base_url + "/quota"), json.dumps(USAGE))
        source += "      balanced_priority_policy: capacity_deadline\n      reset_credits_auto_use: false\n      prefer_reset_credits: true\n"
        path.write_text(source)
        def plans():
            rows = cpa.status().get("snapshots", [])
            return len(rows) == 2 and {r.get("manual_card_cycle", {}).get("feasible_cards") for r in rows} == {1, 3}
        wait_until(plans, 35, "both verified native card inventories")
        for turn in range(2):
            conn, response = open_stream(cpa.port, {"Session-Id": "local-manual-cards"})
            try:
                assert response.status == 200 and b"response.completed" in read_to_end(response)
            finally:
                response.close()
                conn.close()
            if turn == 0:
                assert "手动重置卡" in cpa.status()["adq"]["last_decision_reason"]
        actual = fixture.snapshot()["generations"]
        assert [a["auth_index"] for a in actual] == [fixture.rich] * 2
        path.write_text(source.replace("prefer_reset_credits: true", "prefer_reset_credits: false"))
        wait_until(lambda: not any(q.get("manual_card_cycle") for q in cpa.status()["snapshots"]), 20, "operator opt out")
        assert not fixture.unexpected, fixture.unexpected
        return {"status": "PASS", "cards": [1, 3], "first_and_continuation_on_three_card_account": True,
                "completed_streams": 2, "redemption_attempts": 0, "opt_out_verified": True}
    finally:
        if cpa:
            cpa.close()
        fixture.close()
        remove_fixture_directory(temp, "manual-cards")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for arg in ("cpa", "plugin", "output"):
        parser.add_argument("--" + arg, type=Path, required=True)
    args = parser.parse_args()
    result = run(args.cpa.resolve(strict=True), args.plugin.resolve(strict=True))
    result.update(plugin_sha256=digest(args.plugin), cpa_sha256=digest(args.cpa))
    args.output.write_text(json.dumps(result, indent=2))
    print(json.dumps(result), flush=True)
