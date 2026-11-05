"""Restore explicitly configured Docker networks after a 1Panel container upgrade.

This host-only helper grants no additional permissions to CPA or Keeper. It
repairs named containers only after checking their Compose project and mounts.
"""
import argparse
import atexit
import ipaddress
import json
from pathlib import Path
import re
import signal
import subprocess
import time


def docker(*arguments):
    result = subprocess.run(["docker", *arguments], capture_output=True, text=True, timeout=20)
    if result.returncode:
        raise RuntimeError("Docker operation failed: " + arguments[0])
    return result.stdout


def inspect(name):
    return json.loads(docker("inspect", name))[0]


def validate(config):
    containers = config.get("containers")
    if not isinstance(containers, dict) or not containers:
        raise ValueError("Expected named container policies")
    for name, policy in containers.items():
        if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]+", name):
            raise ValueError("Invalid container name")
        if not policy.get("project") or not isinstance(policy.get("mounts"), dict):
            raise ValueError("Project and mount checks are required")
        if not isinstance(policy.get("networks"), dict) or not policy["networks"]:
            raise ValueError("Expected explicit network policies")
        for network, endpoint in policy["networks"].items():
            if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]+", network):
                raise ValueError("Invalid network name")
            address = ipaddress.IPv4Address(endpoint["ipv4"])
            if not address.is_private:
                raise ValueError("Expected a private bridge address")
            if not isinstance(endpoint.get("aliases", []), list):
                raise ValueError("Network aliases must be a list")
    return containers


def repair(name, policy):
    container = inspect(name)
    if container.get("Name") != "/" + name:
        raise ValueError("Container identity changed")
    labels = container["Config"].get("Labels") or {}
    if labels.get("com.docker.compose.project") != policy["project"]:
        raise ValueError("Container Compose project changed")
    mounts = {item["Destination"]: item["Source"] for item in container.get("Mounts", [])}
    if any(mounts.get(target) != source for target, source in policy["mounts"].items()):
        raise ValueError("Container mounts changed")
    if not container["State"].get("Running"):
        return []
    container_id = container["Id"]
    repairs = []
    for network, endpoint in policy["networks"].items():
        current = inspect(container_id)
        if current["Id"] != container_id or current["Name"] != "/" + name:
            raise ValueError("Container replaced while restoring its network")
        connected = current["NetworkSettings"]["Networks"].get(network)
        if connected and connected.get("IPAddress") == endpoint["ipv4"]:
            continue
        if connected:
            docker("network", "disconnect", network, container_id)
        command = ["network", "connect", "--ip", endpoint["ipv4"]]
        for alias in endpoint.get("aliases", []):
            command.extend(["--alias", alias])
        docker(*command, network, container_id)
        actual = inspect(container_id)["NetworkSettings"]["Networks"].get(network)
        if not actual or actual.get("IPAddress") != endpoint["ipv4"]:
            raise RuntimeError("Network address was not restored")
        repairs.append(network)
    return repairs


def audit(policies, name=None):
    healthy = True
    for target, policy in policies.items():
        if name is not None and name != target:
            continue
        try:
            networks = repair(target, policy)
            if networks:
                print(json.dumps({"container": target, "restored_networks": networks}), flush=True)
        except (RuntimeError, ValueError, KeyError, subprocess.SubprocessError) as error:
            healthy = False
            print(json.dumps({"container": target, "error": str(error)}), flush=True)
    return healthy


def watch(policies):
    child = None
    def close_child():
        if child is not None and child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=3)
            except subprocess.TimeoutExpired:
                child.kill()
    atexit.register(close_child)
    signal.signal(signal.SIGTERM, lambda *_: exit(0))
    while True:
        since = str(int(time.time()) - 1)
        audit(policies)
        child = subprocess.Popen(
            ["docker", "events", "--since", since, "--filter", "type=container", "--filter", "event=start", "--format", "{{json .}}"],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
        )
        try:
            for line in child.stdout:
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                name = (event.get("Actor", {}).get("Attributes") or {}).get("name")
                if name in policies:
                    for attempt in range(5):
                        if audit(policies, name):
                            break
                        time.sleep(1)
        finally:
            close_child()
        time.sleep(2)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args()
    policies = validate(json.loads(args.config.read_text(encoding="utf-8")))
    if args.once:
        raise SystemExit(0 if audit(policies) else 1)
    watch(policies)
