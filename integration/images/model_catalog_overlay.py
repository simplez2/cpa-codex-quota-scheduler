#!/usr/bin/env python3
"""Merge verified model additions into fresh CPA-maintained catalogs.

Upstream entries take precedence over additions. Explicit compatibility headers
also apply after upstream adds a model. A failed fetch leaves published catalogs
intact. Run with --refresh after configuring CPA to read the files.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import tempfile
import urllib.request


GENERAL_SOURCES = (
    'https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json',
    'https://models.router-for.me/models.json',
)
CLIENT_SOURCES = (
    'https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/codex_client_models.json',
    'https://models.router-for.me/codex_client_models.json',
)


def validate_models(models, id_field):
    if not isinstance(models, list) or not models:
        raise ValueError('Expected a nonempty model list')
    identifiers = [model.get(id_field) for model in models]
    if any(not isinstance(value, str) or not value for value in identifiers):
        raise ValueError('A model identifier is missing')
    if len(set(identifiers)) != len(identifiers):
        raise ValueError('Duplicate model identifiers')


def validate_general(data):
    for tier in ('codex-free', 'codex-team', 'codex-plus', 'codex-pro'):
        validate_models(data[tier], 'id')


def validate_client(data):
    validate_models(data['models'], 'slug')


def fetch_catalog(sources, validate):
    failures = []
    for url in sources:
        try:
            request = urllib.request.Request(url, headers={
                'User-Agent': 'CPA-model-catalog-overlay/1',
                'Cache-Control': 'no-cache',
            })
            with urllib.request.urlopen(request, timeout=30) as response:
                raw = response.read()
            data = json.loads(raw)
            validate(data)
            return data, {'url': url, 'sha256': hashlib.sha256(raw).hexdigest()}
        except Exception as exc:
            failures.append(type(exc).__name__)
    raise RuntimeError('All catalog sources failed: ' + ', '.join(failures))


def merge_models(upstream, additions, id_field):
    validate_models(additions, id_field)
    existing = {model[id_field] for model in upstream}
    missing = [model for model in additions if model[id_field] not in existing]
    return upstream + missing, [model[id_field] for model in missing]


def merge_catalogs(general, client, overlay):
    additions = {}
    for tier, models in overlay['general'].items():
        if tier not in ('codex-free', 'codex-team', 'codex-plus', 'codex-pro'):
            raise ValueError('Overlay may only modify Codex plan catalogs')
        general[tier], additions[tier] = merge_models(general[tier], models, 'id')
    client['models'], additions['codex-client'] = merge_models(client['models'], overlay['codex'], 'slug')
    for tier, models in general.items():
        if not tier.startswith('codex-'):
            continue
        for model in models:
            headers = overlay.get('runtime_headers', {}).get(model['id'])
            if headers:
                model.setdefault('config', {}).setdefault('override_header', {}).update(headers)
    validate_general(general)
    validate_client(client)
    return general, client, additions


def publish(path, data):
    raw = (json.dumps(data, ensure_ascii=False, indent=2) + '\n').encode()
    if path.exists() and path.read_bytes() == raw:
        return False
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix='.' + path.name, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        except BaseException:
            temporary.unlink(missing_ok=True)
            raise
    os.chmod(temporary, 0o600)
    os.replace(temporary, path)
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output-dir', required=True, type=Path)
    parser.add_argument('--overlay', required=True, type=Path)
    parser.add_argument('--refresh', action='store_true')
    parser.add_argument('--management-url', default='http://127.0.0.1:8317')
    parser.add_argument('--management-key-file', type=Path, default=Path('/opt/cli-proxy-api/secrets/management_key'))
    args = parser.parse_args()
    overlay = json.loads(args.overlay.read_text())
    general, general_source = fetch_catalog(GENERAL_SOURCES, validate_general)
    client, client_source = fetch_catalog(CLIENT_SOURCES, validate_client)
    general, client, additions = merge_catalogs(general, client, overlay)
    args.output_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    result = {
        'sources': {'general': general_source, 'codex-client': client_source},
        'additions': additions,
        'changed': {
            'general': publish(args.output_dir / 'models.json', general),
            'codex-client': publish(args.output_dir / 'codex_client_models.json', client),
        },
    }
    if args.refresh:
        key = args.management_key_file.read_text().strip()
        request = urllib.request.Request(args.management_url.rstrip('/') + '/v8/management/models/refresh',
            data=b'{}', headers={'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=90) as response:
            refreshed = json.load(response)
        if not refreshed.get('ok'):
            raise RuntimeError('CPA did not confirm catalog refresh')
        result['refresh'] = refreshed
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    main()
