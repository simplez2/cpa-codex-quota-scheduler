from pathlib import Path
import re


def replace_once(path, old, new):
    p = Path(path)
    text = p.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected exactly one match, got {count}: {old[:120]!r}")
    p.write_text(text.replace(old, new, 1))


# Keep the existing placeholder tolerance semantics. This PR is for confirmed
# 5h/week classification, time anchoring, and stale partial-cache recovery.
replace_once(
    "warmup.go",
    '''\tif window.ResetAfterSecondsKnown {\n\t\tdelta := window.ResetAfterSeconds - window.WindowSeconds\n\t\tif delta < 0 {\n\t\t\tdelta = -delta\n\t\t}\n\t\treturn delta <= toleranceSeconds\n\t}\n''',
    '''\tif window.ResetAfterSecondsKnown {\n\t\treturn window.ResetAfterSeconds >= window.WindowSeconds-toleranceSeconds\n\t}\n''',
)
replace_once(
    "keeper_refresh.go",
    '''\t\t\t// Keep this exactly aligned with mergePartialQuotaSnapshot: only a\n\t\t\t// missing window whose old reset is still in the future would be\n\t\t\t// carried into the new runtime snapshot.\n''',
    '''\t\t\t// Keep this exactly aligned with mergePartialQuotaSnapshot: a missing\n\t\t\t// window is carried while its old reset is unknown or still in the\n\t\t\t// future; an explicitly expired reset is dropped.\n''',
)

p = Path("warmup_window_cycles_regression_test.go")
text = p.read_text()
text, count = re.subn(
    r'''\nfunc TestPlaceholderResetRequiresApproximatelyFullCycle\(t \*testing\.T\) \{.*?\n\}\n''',
    "\n",
    text,
    count=1,
    flags=re.S,
)
if count != 1:
    raise SystemExit(f"placeholder hardening test removal matched {count} functions")
p.write_text(text)
