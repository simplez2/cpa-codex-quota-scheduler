"""Deterministic, offline scheduling research; no services, credentials or dependencies.

Run: python research/quota_policy_simulation.py
This is a simplified workload model, NOT a replacement for CPA integration tests.
Results are written alongside this script. See the output assumptions before use.
"""

from __future__ import annotations

import dataclasses
import hashlib
import json
import math
from pathlib import Path
import random
from typing import Optional


WEEK = 7 * 24 * 60.0
FIVE = 5 * 60.0
BASE_WEEK = 2000.0
BASE_FIVE = 100.0


@dataclasses.dataclass(frozen=True)
class AccountSpec:
    weekly_capacity: float = BASE_WEEK
    five_capacity: float = BASE_FIVE
    weekly_remaining_fraction: float = 1.0
    five_remaining_fraction: float = 1.0
    weekly_reset: float = WEEK
    five_reset: float = FIVE
    capacity_estimate_scale: float = 1.0
    capacity_prior_weight: float = 1.0


@dataclasses.dataclass(frozen=True)
class Scenario:
    name: str
    accounts: tuple[AccountSpec, ...]
    duration: float
    load: float  # Arrival workload / nominal aggregate weekly sustainable rate.
    seed: int
    shape: str = "uniform"
    poll_minutes: float = 2.0
    poll_loss_fraction: float = 0.0
    outage: tuple[float, float] = (0.0, 0.0)
    unknown_until: float = 0.0
    split: str = "validation"
    telemetry_available: bool = True


@dataclasses.dataclass
class Account:
    spec: AccountSpec
    week: float
    five: float
    week_reset: float
    five_reset: float
    observed_at: float = -math.inf
    observed_week: float = 0.0
    observed_five: float = 0.0
    observed_week_reset: float = 0.0
    observed_five_reset: float = 0.0
    spent_since_observation: float = 0.0
    cooldown_until: float = 0.0
    last_selected: float = -math.inf
    week_waste: float = 0.0
    five_waste: float = 0.0
    week_reset_capacity: float = 0.0
    five_reset_capacity: float = 0.0
    served_units: float = 0.0

    def advance(self, now: float) -> None:
        while now >= self.week_reset:
            self.week_waste += self.week
            self.week_reset_capacity += self.spec.weekly_capacity
            self.week = self.spec.weekly_capacity
            self.week_reset += WEEK
        while now >= self.five_reset:
            self.five_waste += self.five
            self.five_reset_capacity += self.spec.five_capacity
            self.five = self.spec.five_capacity
            self.five_reset += FIVE

    def observe(self, now: float) -> None:
        self.observed_at = now
        self.observed_week = self.week
        self.observed_five = self.five
        self.observed_week_reset = self.week_reset
        self.observed_five_reset = self.five_reset
        self.spent_since_observation = 0.0

    def estimate(self, now: float, local_debit: bool) -> Optional[tuple[float, float]]:
        if now - self.observed_at > 15.0:
            return None
        # Expired observations cannot prove a fresh window; allow fallback probes.
        if now >= self.observed_week_reset or now >= self.observed_five_reset:
            return None
        debit = self.spent_since_observation if local_debit else 0.0
        return (
            max(0.0, self.observed_week - debit) / self.spec.weekly_capacity,
            max(0.0, self.observed_five - debit) / self.spec.five_capacity,
        )


@dataclasses.dataclass(frozen=True)
class Policy:
    name: str
    floor_hours: float = 6.0
    hold_minutes: float = 5.0
    relative_advantage: float = 0.2
    local_debit: bool = False


class Scheduler:
    def __init__(self, accounts: list[Account], policy: Policy, telemetry_available: bool):
        self.accounts = accounts
        self.policy = policy
        self.telemetry_available = telemetry_available
        self.current: Optional[int] = None
        self.selected_at = -math.inf
        self.challenger: Optional[int] = None
        self.evidence = (-math.inf, -math.inf)
        self.confirmations = 0
        self.switches = 0

    def values(self, i: int, now: float) -> Optional[tuple[float, float]]:
        return self.accounts[i].estimate(now, self.policy.local_debit and self.telemetry_available)

    def score(self, i: int, now: float) -> float:
        a = self.accounts[i]
        values = self.values(i, now)
        if values is None:
            return -1.0
        weekly, five = values
        if self.policy.name == "weekly_percent":
            return weekly
        week_rate = max(0.0, weekly - 0.08) / max(
            a.observed_week_reset - now, self.policy.floor_hours * 60.0
        )
        if self.policy.name in ("weekly_deadline_rate", "layered_safe_deadline_rate", "layered_deadline_prior_tiebreak"):
            return week_rate
        if self.policy.name == "layered_deadline_with_capacity_prior":
            # A configured relative prior, never a measured weekly capacity.
            return week_rate * a.spec.capacity_prior_weight
        # Convert BOTH windows to a common workload unit before comparing them.
        # Absolute capacity estimates are an additional assumption of this policy.
        scale = a.spec.capacity_estimate_scale if self.telemetry_available else 1.0
        week_capacity = a.spec.weekly_capacity if self.telemetry_available else BASE_WEEK
        five_capacity = a.spec.five_capacity if self.telemetry_available else BASE_FIVE
        week_rate *= week_capacity * scale
        five_rate = max(0.0, five - 0.02) * five_capacity * scale / max(
            a.observed_five_reset - now, 5.0
        )
        return min(week_rate, five_rate)

    def choose(self, now: float, excluded: set[int]) -> Optional[int]:
        candidates = [
            i for i, a in enumerate(self.accounts)
            if i not in excluded and a.cooldown_until <= now
            and (self.values(i, now) is None or min(self.values(i, now)) > 1e-10)
        ]
        if not candidates:
            return None
        # Common safety partitions: reserve and soft handoff are preferences;
        # they do not refuse traffic when all remaining accounts are below them.
        groups = {}
        for i in candidates:
            v = self.values(i, now)
            groups[i] = (v is None,
                         self.policy.name in ("layered_safe_deadline_rate", "layered_deadline_with_capacity_prior", "layered_deadline_prior_tiebreak") and v is not None and v[1] <= 0.10,
                         v is not None and v[0] <= 0.08,
                         v is not None and min(v) <= 0.02)
        top_group = min(groups.values())
        preferred = [i for i in candidates if groups[i] == top_group]
        if self.policy.name == "weekly_percent":
            maximum = max(self.score(i, now) for i in preferred)

            def ordering(i: int) -> tuple:
                v = self.values(i, now)
                delta = maximum - self.score(i, now)
                tier = 0 if delta <= 0.02 + 1e-12 else math.ceil((delta - 0.02) / 0.02)
                return (tier, -(v[1] if v else -1), -self.score(i, now),
                        self.accounts[i].last_selected, i)
        elif self.policy.name == "layered_deadline_prior_tiebreak":
            maximum = max(self.score(i, now) for i in preferred)
            band = max(1e-12, maximum * 0.1)

            def ordering(i: int) -> tuple:
                v = self.values(i, now)
                tier = max(0, math.floor((maximum - self.score(i, now)) / band + 1e-12))
                weighted_five = (v[1] if v else -1) * self.accounts[i].spec.capacity_prior_weight
                return (tier, -weighted_five, -self.score(i, now),
                        self.accounts[i].last_selected, i)
        else:
            def ordering(i: int) -> tuple:
                v = self.values(i, now)
                return (-self.score(i, now), -(v[1] if v else -1),
                        self.accounts[i].last_selected, i)

        best = min(preferred, key=ordering)
        previous = self.current
        if previous not in preferred:
            return self.commit(best, now)
        if best == previous:
            self.reset_evidence()
            return previous
        old_score, new_score = self.score(previous, now), self.score(best, now)
        if self.policy.name == "weekly_percent":
            advantage = new_score - old_score >= 0.10 - 1e-12
        else:
            advantage = new_score > max(0.0, old_score) * (1 + self.policy.relative_advantage) + 1e-12
        if not advantage:
            self.reset_evidence()
            return previous
        pair = (self.accounts[previous].observed_at, self.accounts[best].observed_at)
        if self.challenger != best:
            self.reset_evidence()
            self.challenger = best
        if min(pair) > self.selected_at and all(x > y for x, y in zip(pair, self.evidence)):
            self.confirmations += 1
            self.evidence = pair
        if self.confirmations >= 2 and now - self.selected_at >= self.policy.hold_minutes:
            return self.commit(best, now)
        return previous

    def reset_evidence(self) -> None:
        self.challenger = None
        self.evidence = (-math.inf, -math.inf)
        self.confirmations = 0

    def commit(self, best: int, now: float) -> int:
        if self.current != best:
            self.switches += int(self.current is not None)
            self.current = best
            self.selected_at = now
            self.accounts[best].last_selected = now
            self.reset_evidence()
        return best


def request_trace(scenario: Scenario) -> list[tuple[float, float]]:
    rng = random.Random(scenario.seed)
    sustainable = sum(a.weekly_capacity for a in scenario.accounts) / WEEK
    events = []
    # Piecewise-constant Poisson arrivals; expected cost is exactly one unit.
    for minute in range(math.ceil(scenario.duration)):
        multiplier = 1.0
        if scenario.shape == "burst":
            multiplier = 8.0 if minute % 1440 < 60 else (1440 - 480) / 1380
        elif scenario.shape == "daily":
            multiplier = 2.5 if 8 * 60 <= minute % 1440 < 16 * 60 else 0.25
        rate = sustainable * scenario.load * multiplier
        offset = rng.expovariate(rate) if rate else math.inf
        while offset < 1.0 and minute + offset < scenario.duration:
            cost = rng.choice((0.5, 0.75, 1.0, 1.25, 1.5))
            events.append((minute + offset, cost))
            offset += rng.expovariate(rate)
    return events


def poll_allowed(scenario: Scenario, tick: int, account_id: int) -> bool:
    now = tick * scenario.poll_minutes
    if now < scenario.unknown_until or scenario.outage[0] <= now < scenario.outage[1]:
        return False
    # A separate fixed hash means routing decisions never alter observation loss.
    key = f"{scenario.seed}:{tick}:{account_id}".encode()
    number = int.from_bytes(hashlib.sha256(key).digest()[:8], "big") / 2**64
    return number >= scenario.poll_loss_fraction


def simulate(scenario: Scenario, policy: Policy, trace: list[tuple[float, float]]) -> dict:
    accounts = [Account(a, a.weekly_capacity * a.weekly_remaining_fraction,
                        a.five_capacity * a.five_remaining_fraction,
                        a.weekly_reset, a.five_reset) for a in scenario.accounts]
    scheduler = Scheduler(accounts, policy, scenario.telemetry_available)
    tick = 0
    rejected = 0
    unavailable = 0
    failed_dispatches = 0
    served_units = 0.0
    demand_units = 0.0
    first_interruption = None
    last_failure = 0.0
    longest_success_span = 0.0
    rejected_capacity_possible = 0
    first_selected = None
    served_before_first_weekly_reset = [0.0] * len(accounts)
    first_weekly_reset = min(a.weekly_reset for a in scenario.accounts)
    first_weekly_reserve_at = [None] * len(accounts)
    first_weekly_exhaustion_at = [None] * len(accounts)
    minimum_weekly_fraction_before_initial_reset = [a.weekly_remaining_fraction for a in scenario.accounts]
    weekly_stranded_five_capacity_time = [0.0] * len(accounts)
    previous_request_at = 0.0
    for now, cost in trace:
        # Time integral over the PRECEDING interval, capped by both resets.
        # This reports full-window 5h capacity unavailable because weekly is dry.
        for i, a in enumerate(accounts):
            if a.week <= 1e-9:
                end = min(now, a.week_reset)
                weekly_stranded_five_capacity_time[i] += max(0.0, end - previous_request_at) * a.spec.five_capacity
        previous_request_at = now
        while tick * scenario.poll_minutes <= now:
            at = tick * scenario.poll_minutes
            for i, a in enumerate(accounts):
                a.advance(at)
                if poll_allowed(scenario, tick, i):
                    a.observe(at)
            tick += 1
        for a in accounts:
            a.advance(now)
        demand_units += cost
        excluded: set[int] = set()
        successful = False
        while len(excluded) < len(accounts):
            i = scheduler.choose(now, excluded)
            if i is None:
                unavailable += 1
                break
            a = accounts[i]
            if a.week + 1e-9 < cost or a.five + 1e-9 < cost:
                if a.week + 1e-9 < cost and first_weekly_exhaustion_at[i] is None:
                    first_weekly_exhaustion_at[i] = now
                failed_dispatches += 1
                excluded.add(i)
                a.cooldown_until = max(a.week_reset if a.week < cost else 0.0,
                                       a.five_reset if a.five < cost else 0.0)
                continue
            a.week -= cost
            a.five -= cost
            a.spent_since_observation += cost
            a.served_units += cost
            served_units += cost
            if first_selected is None:
                first_selected = i
            if now < first_weekly_reset:
                served_before_first_weekly_reset[i] += cost
            if now < a.spec.weekly_reset:
                minimum_weekly_fraction_before_initial_reset[i] = min(
                    minimum_weekly_fraction_before_initial_reset[i], a.week / a.spec.weekly_capacity)
            if a.week <= a.spec.weekly_capacity * 0.08 and first_weekly_reserve_at[i] is None:
                first_weekly_reserve_at[i] = now
            if a.week <= 1e-9 and first_weekly_exhaustion_at[i] is None:
                first_weekly_exhaustion_at[i] = now
            successful = True
            break
        if not successful:
            rejected += 1
            rejected_capacity_possible += int(any(a.week + 1e-9 >= cost and a.five + 1e-9 >= cost for a in accounts))
            if first_interruption is None:
                first_interruption = now
            longest_success_span = max(longest_success_span, now - last_failure)
            last_failure = now
    longest_success_span = max(longest_success_span, scenario.duration - last_failure)
    for a in accounts:
        a.advance(scenario.duration)
        # Exact workload conservation through arbitrary resets and failures.
        initial_week = a.spec.weekly_capacity * a.spec.weekly_remaining_fraction
        initial_five = a.spec.five_capacity * a.spec.five_remaining_fraction
        assert abs(initial_week + a.week_reset_capacity - a.week_waste - a.week - a.served_units) < 1e-7
        assert abs(initial_five + a.five_reset_capacity - a.five_waste - a.five - a.served_units) < 1e-7
        assert a.week >= -1e-9 and a.five >= -1e-9
    assert abs(sum(a.served_units for a in accounts) - served_units) < 1e-7
    assert served_units <= demand_units + 1e-7
    weekly_reset_capacity = sum(a.week_reset_capacity for a in accounts)
    five_reset_capacity = sum(a.five_reset_capacity for a in accounts)
    round6 = lambda value: round(value, 6)
    return {
        "scenario": scenario.name, "split": scenario.split,
        "policy": dataclasses.asdict(policy), "requests": len(trace),
        "demand_units": round6(demand_units), "served_units": round6(served_units),
        "rejected_requests": rejected, "no_selectable_account_events": unavailable,
        "rejected_workload_units": round6(demand_units - served_units),
        "rejected_despite_actual_fitting_capacity": rejected_capacity_possible,
        "upstream_quota_failures": failed_dispatches,
        "first_interruption_hours": None if first_interruption is None else round6(first_interruption / 60),
        "longest_elapsed_span_without_rejection_hours": round6(longest_success_span / 60),
        "switches": scheduler.switches,
        "weekly_reset_waste_units": round6(sum(a.week_waste for a in accounts)),
        "weekly_reset_waste_fraction": None if not weekly_reset_capacity else round6(sum(a.week_waste for a in accounts) / weekly_reset_capacity),
        "five_reset_waste_units": round6(sum(a.five_waste for a in accounts)),
        "five_reset_waste_fraction": None if not five_reset_capacity else round6(sum(a.five_waste for a in accounts) / five_reset_capacity),
        "ending_weekly_remaining_units": [round6(a.week) for a in accounts],
        "served_units_per_account": [round6(a.served_units) for a in accounts],
        "first_selected_account_index": first_selected,
        "served_units_per_account_before_first_weekly_reset": served_before_first_weekly_reset,
        "first_weekly_reserve_hours_per_account": [None if at is None else round6(at / 60) for at in first_weekly_reserve_at],
        "first_weekly_exhaustion_hours_per_account": [None if at is None else round6(at / 60) for at in first_weekly_exhaustion_at],
        "minimum_weekly_fraction_before_initial_reset": [round6(value) for value in minimum_weekly_fraction_before_initial_reset],
        "weekly_dry_five_capacity_hours_per_account": [round6(value / 60) for value in weekly_stranded_five_capacity_time],
    }


def scenarios() -> list[Scenario]:
    cases = []
    for split, seed_base in (("training", 1300), ("validation", 5300)):
        for n in (1, 2, 5):
            for staggered in (False, True):
                specs = tuple(AccountSpec(weekly_reset=WEEK * (i + 1) / n if staggered else WEEK,
                                          five_reset=FIVE * (i + 1) / n if staggered else FIVE)
                              for i in range(n))
                label = f"{split}_{n}accounts_{'staggered' if staggered else 'aligned'}"
                cases.append(Scenario(label, specs, 14 * 1440.0, 0.85,
                                      seed_base + n * 10 + staggered, split=split))
        for shape in ("burst", "daily"):
            cases.append(Scenario(f"{split}_{shape}", (AccountSpec(),) * 2,
                                  14 * 1440.0, 0.85, seed_base + len(shape), shape, split=split))
    cases.extend([
        Scenario("validation_80pct_6days_vs_40pct_halfday", (
            AccountSpec(weekly_remaining_fraction=0.8, weekly_reset=6 * 1440),
            AccountSpec(weekly_remaining_fraction=0.4, weekly_reset=0.5 * 1440),
        ), 14 * 1440, 0.95, 6101),
        Scenario("validation_sustained_overcapacity", (AccountSpec(),) * 5, 21 * 1440, 1.35, 6102),
        Scenario("validation_5h_bottleneck", (AccountSpec(five_capacity=35),) * 2, 14 * 1440, 0.85, 6103),
        Scenario("validation_poll_outage", (AccountSpec(),) * 2, 14 * 1440, 0.85, 6104,
                 outage=(360, 1080), poll_loss_fraction=0.25),
        Scenario("validation_initially_unknown", (AccountSpec(),) * 2, 7 * 1440, 0.85, 6105,
                 unknown_until=60),
        Scenario("validation_heterogeneous_capacity", (
            AccountSpec(weekly_capacity=BASE_WEEK * 0.5, five_capacity=BASE_FIVE * 0.5),
            AccountSpec(), AccountSpec(weekly_capacity=BASE_WEEK * 2, five_capacity=BASE_FIVE * 2),
        ), 14 * 1440, 0.95, 6106),
        Scenario("validation_wrong_capacity_estimates", (
            AccountSpec(weekly_capacity=BASE_WEEK * 0.5, five_capacity=BASE_FIVE * 0.5, capacity_estimate_scale=2),
            AccountSpec(), AccountSpec(weekly_capacity=BASE_WEEK * 2, five_capacity=BASE_FIVE * 2, capacity_estimate_scale=0.5),
        ), 14 * 1440, 0.95, 6106),
        Scenario("validation_heterogeneous_unknown_capacities", (
            AccountSpec(weekly_capacity=BASE_WEEK * 0.5, five_capacity=BASE_FIVE * 0.5),
            AccountSpec(), AccountSpec(weekly_capacity=BASE_WEEK * 2, five_capacity=BASE_FIVE * 2),
        ), 14 * 1440, 0.95, 6106, telemetry_available=False),
    ])
    def weighted_accounts(actual: tuple[float, ...], prior: tuple[float, ...], remaining: float = 1.0,
                          stagger_five: bool = False) -> tuple[AccountSpec, ...]:
        return tuple(AccountSpec(weekly_capacity=BASE_WEEK * multiplier,
                                 five_capacity=BASE_FIVE * multiplier,
                                 weekly_remaining_fraction=remaining,
                                 weekly_reset=WEEK * remaining,
                                 five_reset=FIVE * (i + 1) / len(actual) if stagger_five else FIVE,
                                 capacity_prior_weight=weight)
                     for i, (multiplier, weight) in enumerate(zip(actual, prior)))
    cases.extend([
        Scenario("validation_prior_mixed_1_5_20_aligned",
                 weighted_accounts((1, 5, 20), (1, 5, 20)), 14 * 1440, 0.95, 7201,
                 telemetry_available=False),
        Scenario("validation_prior_equal_60pct_staggered_5h",
                 weighted_accounts((1, 5, 20), (1, 5, 20), remaining=0.6, stagger_five=True),
                 14 * 1440, 0.95, 7202, telemetry_available=False),
        Scenario("validation_prior_external_share_overestimate",
                 weighted_accounts((1, 5, 4), (1, 5, 20)), 14 * 1440, 0.95, 7203,
                 telemetry_available=False),
        Scenario("validation_prior_external_share_reduced_weight",
                 weighted_accounts((1, 5, 4), (1, 5, 5)), 14 * 1440, 0.95, 7203,
                 telemetry_available=False),
    ])
    return cases


def aggregate(rows: list[dict]) -> dict:
    requests = sum(r["requests"] for r in rows)
    rejected = sum(r["rejected_requests"] for r in rows)
    return {
        "scenarios": len(rows), "requests": requests, "rejected_requests": rejected,
        "rejection_fraction": round(rejected / requests, 8) if requests else 0,
        "served_units": round(sum(r["served_units"] for r in rows), 6),
        "upstream_quota_failures": sum(r["upstream_quota_failures"] for r in rows),
        "switches": sum(r["switches"] for r in rows),
        "uninterrupted_scenarios": sum(r["rejected_requests"] == 0 for r in rows),
        "weekly_reset_waste_units": round(sum(r["weekly_reset_waste_units"] for r in rows), 6),
        "five_reset_waste_units": round(sum(r["five_reset_waste_units"] for r in rows), 6),
    }


def main() -> None:
    cases = scenarios()
    traces = {s.name: request_trace(s) for s in cases}
    train = [s for s in cases if s.split == "training"]
    # Predeclared small grid and lexicographic objective. Validation scenarios
    # never select parameters; no demand-dependent tuning during validation.
    grid_results = []
    for floor in (1.0, 6.0, 24.0):
        for hold in (1.0, 5.0, 15.0):
            policy = Policy("bottleneck_rate_with_local_debit", floor, hold, local_debit=True)
            rows = [simulate(s, policy, traces[s.name]) for s in train]
            summary = aggregate(rows)
            grid_results.append({"parameters": dataclasses.asdict(policy), "training": summary})
    chosen = min(grid_results, key=lambda item: (
        item["training"]["rejected_requests"],
        item["training"]["upstream_quota_failures"],
        item["training"]["switches"],
        abs(item["parameters"]["floor_hours"] - 6),
        abs(item["parameters"]["hold_minutes"] - 5),
    ))
    policies = [Policy("weekly_percent"), Policy("weekly_deadline_rate"),
                Policy("layered_safe_deadline_rate"), Policy(**chosen["parameters"]),
                Policy("layered_deadline_with_capacity_prior"), Policy("layered_deadline_prior_tiebreak")]
    results = [simulate(s, p, traces[s.name]) for s in cases for p in policies]
    legacy_results = [r for r in results if not r["scenario"].startswith("validation_prior_")]
    target = Path(__file__).with_name("quota_policy_simulation_results.json")
    legacy_verified = 0
    if target.exists():
        prior_output = json.loads(target.read_text(encoding="utf-8"))
        lookup = {(r["scenario"], r["policy"]["name"]): r for r in results}
        for old in prior_output["results"]:
            if old["scenario"].startswith("validation_prior_") or old["policy"]["name"] in ("layered_deadline_with_capacity_prior", "layered_deadline_prior_tiebreak"):
                continue
            new = lookup[(old["scenario"], old["policy"]["name"])]
            assert all(new[key] == value for key, value in old.items()), (old["scenario"], old["policy"]["name"])
            legacy_verified += 1
    demand_pressure_cases = {s.name for s in cases if s.shape != "uniform" or s.name in (
        "validation_sustained_overcapacity", "validation_5h_bottleneck")}
    output = {
        "schema_version": 2,
        "assumptions": [
            "Offline simplified serial-routing model; does not execute Go/CPA and cannot validate streaming handoff, auth, warmup or real provider limits.",
            "Quota windows are fixed 5h/7d replenishments with scenario-defined initial offsets; real provider window semantics must be verified independently.",
            "Each completed request consumes identical workload units from both windows; real models and plans can debit them differently.",
            "Poisson demand and bounded variable request costs are generated once per scenario and shared by every policy; no future request knowledge is used.",
            "A failed pre-output request may immediately retry every other eligible account; an in-flight stream never fails in this model.",
            "The baseline approximates weekly-first, 2pp ranking bands, 10pp preemption, two distinct polls and 5m hold; manual pins, cycle-boundary rotation, drain, windows beyond 5h/weekly and session bindings are omitted.",
            "Fresh observations last 15m and poll every 2m; deterministic outages/losses are independent of routing. Unknown/expired observations permit fallback probing.",
            "A quota failure cools the account until the limiting reset; no bypass or request after a confirmed cooldown is modeled.",
            "Weekly 8pct reserve and 98pct soft handoff are common preference tiers, spendable as fallback; no policy deliberately delays or rejects otherwise feasible user traffic to save weekly budget.",
            "Deadline policy ranks normalized weekly remaining minus reserve over max(time-to-reset,6h), then requires 20pct advantage/two polls/5m hold.",
            "Layered policy uses the same deployable percentage-only deadline score but first prefers accounts with more than 10pct observed 5h remaining. This fixed safety threshold is not selected on validation results.",
            "Capacity-prior layered policy multiplies the normalized weekly deadline score by a configured relative 1/5/20 weight, preserving 20pct advantage/two confirmations/5m hold. It NEVER accesses true capacities or completed-workload debit.",
            "Prior-tiebreak layered policy keeps weekly deadline scores UNWEIGHTED and applies the 1/5/20 weight ONLY to 5h percentage tie-breaking within a pool-relative 10pct weekly-score band. Proactive rebalance still needs a 20pct unweighted weekly-score advantage; no automatic weight-based preemption is added.",
            "The 1/5/20 weights are plan priors supplied by the caller, not exact weekly quotas. All numeric weekly/five capacities are synthetic workload units, not official request allotments. Official estimates do not define a fixed weekly request count.",
            "New prior scenarios share the original demand model. The external-share case represents reduced effective available capacity with a stale nominal prior; it does not simulate concurrent external requests or know future sharing behavior.",
            "The external-share reduced-weight control uses the identical environment and request trace, changing only the configured largest-account prior from 20 to 5; effective capacity remains 4x.",
            "Bottleneck policy additionally estimates absolute plan capacities, ranks min(weekly workload rate,5h workload rate), and deducts actual completed workload since the last poll. This needs real host usage/capacity telemetry; percentages alone are insufficient.",
            "When scenario telemetry_available=false, bottleneck policy uses the SAME reference capacities for every account and disables local debit. Ground-truth capacities/costs remain exclusively in the environment and are never exposed to that scheduling policy.",
            "The bottleneck parameter grid is selected only on the declared training split, minimizing rejections then quota errors then switches. Validation is held out; the finite grid provides no global optimality proof.",
            "Longest uninterrupted span is elapsed time between rejection events, including idle periods; it is not a guarantee about latency or stream duration.",
            "Reset waste is unused capacity at resets, not inherently bad at low demand; more waste can coexist with perfectly served traffic.",
            "Aggregate rejection rates mix load levels; inspect the separate sustained-overcapacity and 5h-bottleneck rows before inferring capacity-inside performance.",
        ],
        "scenario_definitions": [dataclasses.asdict(s) for s in cases],
        "trace_sha256": {name: hashlib.sha256(json.dumps(trace, separators=(",", ":")).encode()).hexdigest() for name, trace in traces.items()},
        "training_grid": grid_results,
        "selected_bottleneck_parameters": chosen["parameters"],
        "summaries": {split: {p.name: aggregate([r for r in legacy_results if r["split"] == split and r["policy"]["name"] == p.name]) for p in policies} for split in ("training", "validation")},
        "prior_validation_summaries": {
            p.name: aggregate([r for r in results if r["scenario"].startswith("validation_prior_")
                              and r["policy"]["name"] == p.name]) for p in policies
        },
        "validation_excluding_declared_demand_pressure": {
            p.name: aggregate([r for r in legacy_results if r["split"] == "validation"
                              and r["scenario"] not in demand_pressure_cases
                              and r["policy"]["name"] == p.name]) for p in policies
        },
        "declared_demand_pressure_scenarios": sorted(demand_pressure_cases),
        "verification": {"per_account_both_window_workload_conservation": "asserted for every run",
                         "original_strategy_scenario_rows_verified_unchanged": legacy_verified},
        "results": results,
    }
    target.write_text(json.dumps(output, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"output": str(target), "selected": chosen["parameters"], "summaries": output["summaries"]}, indent=2))


if __name__ == "__main__":
    main()
