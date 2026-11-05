"""Exercise the built CPA image against real plugin ABI and fake upstreams."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import urllib.request
import zipfile
import io

ROOT = Path(__file__).resolve().parents[2]


def download(url):
    with urllib.request.urlopen(url, timeout=45) as response:
        return response.read()


def main(image, output):
    output.mkdir(parents=True, exist_ok=True)
    release = "https://github.com/simplez2/cpa-codex-quota-scheduler/releases/download/v0.3.39/"
    archive = "codex-quota-scheduler_0.3.39_linux_amd64.zip"
    manifest = json.loads(download(release + "release-verification.json"))
    asset = next(item for item in manifest["assets"] if item["name"] == archive)
    blob = download(release + archive)
    if hashlib.sha256(blob).hexdigest() != asset["archive_sha256"]:
        raise ValueError("Plugin package checksum mismatch")
    library = zipfile.ZipFile(io.BytesIO(blob)).read("codex-quota-scheduler.so")
    if hashlib.sha256(library).hexdigest() != asset["library_sha256"]:
        raise ValueError("Plugin library checksum mismatch")
    plugin = output / "codex-quota-scheduler.so"
    plugin.write_bytes(library)
    container = subprocess.check_output(["docker", "create", "--network=none", image], text=True).strip()
    binary = output / "CLIProxyAPI"
    try:
        subprocess.run(["docker", "cp", container + ":/CLIProxyAPI/CLIProxyAPI", str(binary)], check=True)
    finally:
        subprocess.run(["docker", "rm", container], check=True, stdout=subprocess.DEVNULL)
    binary.chmod(0o755)
    for fixture in ["acceptance", "priority", "anti_storm", "reset_credits"]:
        subprocess.run([
            "python3", str(ROOT / "integration/continuity" / (fixture + ".py")),
            "--cpa", str(binary), "--plugin", str(plugin),
            "--output", str(output / (fixture + ".json")),
        ], check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    main(args.image, args.output.resolve())
