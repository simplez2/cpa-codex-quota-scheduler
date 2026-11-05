"""Prepare the CPA and Keeper images used by the 1Panel latest channel."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import urllib.request


ROOT = Path(__file__).resolve().parents[2]


def run(*args, cwd=None):
    return subprocess.check_output(args, cwd=cwd, text=True).strip()


def latest_cpa():
    request = urllib.request.Request(
        "https://api.github.com/repos/router-for-me/CLIProxyAPI/releases/latest",
        headers={"Accept": "application/vnd.github+json", "User-Agent": "cpa-1panel-images"},
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        release = json.load(response)
    if release.get("draft") or release.get("prerelease"):
        raise ValueError("Expected a stable CPA release")
    return release["tag_name"]


def prepare(kind, destination, requested_cpa):
    if destination.exists():
        raise ValueError("Build destination already exists")
    if kind == "cpa":
        manifest = json.loads((ROOT / "integration/continuity-host-patch-v8.0.57.json").read_text())
        tag = latest_cpa() if requested_cpa == "latest" else requested_cpa
        repository = "https://github.com/router-for-me/CLIProxyAPI.git"
        patch = ROOT / "integration" / manifest["patch_file"]
        if hashlib.sha256(patch.read_bytes()).hexdigest() != manifest["patch_sha256"]:
            raise ValueError("CPA compatibility patch checksum mismatch")
        version = tag + "-continuity"
    else:
        tag = "v2.0.3"
        repository = "https://github.com/Willxup/cpa-usage-keeper.git"
        patch = ROOT / "integration/reset-credit-picker/keeper-v2.0.3.patch"
        version = tag + "-reset-selection"
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", tag):
        raise ValueError("Expected a stable version tag")
    run("git", "-c", "core.autocrlf=false", "clone", "--depth=1", "--branch", tag, repository, str(destination))
    commit = run("git", "rev-parse", "HEAD", cwd=destination)
    if kind == "cpa" and tag == manifest["upstream_tag"] and commit != manifest["upstream_commit"]:
        raise ValueError("CPA tag no longer matches its verified base")
    if kind == "keeper" and commit != "9db74cfc380d11064af80cf1626efd6e3d068217":
        raise ValueError("Keeper tag no longer matches its verified base")
    run("git", "apply", "--check", str(patch), cwd=destination)
    run("git", "apply", str(patch), cwd=destination)
    dockerfile = destination / "Dockerfile"
    text = dockerfile.read_text(encoding="utf-8")
    if kind == "cpa":
        text = text.replace("FROM golang:1.26-bookworm", "FROM golang:1.26.10-bookworm")
        packages = "./internal/pluginhost ./sdk/cliproxy/auth ./sdk/api/handlers ./sdk/api/handlers/openai"
        validation = (
            "ENV GOMAXPROCS=2 GOFLAGS=-p=2\n"
            f"RUN go test {packages} && go test -race {packages} && go vet {packages}\n"
        )
        if "ARG VERSION=dev" not in text:
            raise ValueError("CPA Dockerfile changed; adapt the build before publishing latest")
        text = text.replace("ARG VERSION=dev", validation + "ARG VERSION=dev", 1)
    else:
        text = text.replace("apk add --no-cache build-base", "apk add --no-cache build-base bash git", 1)
        text = text.replace("COPY internal/ ./internal/", "COPY internal/ ./internal/\nCOPY . ./", 1)
        text = text.replace("RUN npm run build", "RUN npm run typecheck && npm run lint && npm test && npm run build", 1)
        text = text.replace("ARG VERSION=dev", "ENV GOMAXPROCS=2 GOFLAGS=-p=2\nRUN go test ./... && go vet ./...\nARG VERSION=dev", 1)
    runtime_license = f"COPY LICENSE /usr/share/doc/{'cli-proxy-api' if kind == 'cpa' else 'cpa-usage-keeper'}/LICENSE\n"
    marker = 'CMD ["./CLIProxyAPI"]' if kind == "cpa" else 'ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]'
    if marker not in text:
        raise ValueError("Runtime Dockerfile layout changed")
    text = text.replace(marker, runtime_license + marker, 1)
    dockerfile.write_text(text, encoding="utf-8", newline="\n")
    ignore = destination / ".dockerignore"
    existing = ignore.read_text(encoding="utf-8") if ignore.exists() else ""
    ignore.write_text(existing.rstrip() + "\n!LICENSE\n", encoding="utf-8", newline="\n")
    output = {"version": version, "commit": commit, "source": str(destination)}
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as handle:
            handle.writelines(f"{key}={value}\n" for key, value in output.items())
    print(json.dumps(output))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("kind", choices=["cpa", "keeper"])
    parser.add_argument("destination", type=Path)
    parser.add_argument("--cpa-version", default="latest")
    args = parser.parse_args()
    prepare(args.kind, args.destination.resolve(), args.cpa_version)
