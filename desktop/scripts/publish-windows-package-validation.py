#!/usr/bin/env python3
"""Prepare and publish component files for the isolated Windows A/B round."""

import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import runpy
import tempfile
import time
from urllib.error import HTTPError
from urllib.request import build_opener


REPO = 'sunjh2222/lazymind-updates'
DATASET = f'https://modelscope.cn/datasets/{REPO}/resolve/master/'
BASE = DATASET + 'updates/windows-policy-validation/v1/'
VERSIONS = ('0.5.401', '0.5.402')
READBACK_DELAYS = (1, 2, 4, 8, 15, 30, 30)
PLANNER = runpy.run_path(str(Path(__file__).with_name('prepare-update-ci-plan.py')))
PUBLISHER = PLANNER['publisher']
VERIFY = runpy.run_path(str(Path(__file__).with_name('verify-python-components.py')))


class ValidationError(ValueError):
    """An error safe to print in the publication job."""


def require(condition, message):
    if not condition:
        raise ValidationError(message)


class ValidationPublishClient:
    def __init__(self, download):
        self.download = download
        self.api = None
        self.uploaded = set()

    def upload_file(self, *, repo_id, repo_type, path_or_fileobj, path_in_repo, revision, commit_message):
        require(repo_id == REPO and repo_type == 'dataset' and revision == 'master', 'Unexpected validation upload repository')
        url = DATASET + path_in_repo
        PUBLISHER.destination(url, BASE)
        if self.api is None:
            token = os.environ.get('MODELSCOPE_API_TOKEN')
            require(token, 'Missing the validation dataset write token')
            from modelscope_hub import HubApi
            self.api = HubApi(token=token, endpoint='https://modelscope.cn')
        self.api.upload_file(repo_id=repo_id, repo_type=repo_type, path_or_fileobj=path_or_fileobj,
                             path_in_repo=path_in_repo, revision=revision, commit_message=commit_message)
        self.uploaded.add(url)

    def fetch(self, url):
        PUBLISHER.destination(url, BASE)
        delays = iter(READBACK_DELAYS if url in self.uploaded else ())
        self.uploaded.discard(url)
        while True:
            try:
                return self.download(url)
            except HTTPError as error:
                wait = next(delays, None)
                if error.code != 404 or wait is None:
                    raise
                error.close()
                time.sleep(wait)


def load_inputs(artifacts):
    records, catalogs, archives = [], {}, {}
    for slot, version in zip(('a', 'b'), VERSIONS):
        release_id = 'windows-amd64-' + version
        directory = artifacts / slot / 'releases' / release_id
        bundle = artifacts / (slot + '-components')
        record, _ = PLANNER['read_json'](directory / 'release.json')
        config, release = record['config'], record['release']
        expected = {'releaseId': release_id, 'version': version, 'mode': 'windows-policy', 'updaterProtocol': 2,
                    'platform': 'windows', 'arch': 'amd64', 'audience': 'official', 'channel': 'stable',
                    'trustDomain': 'windows-policy-validation-v1', 'signingIdentity': 'WINDOWS_POLICY_VALIDATION',
                    'policyURL': BASE + 'policy.json'}
        require(all(config.get(k) == v for k, v in expected.items()), 'Unexpected validation release identity')
        require(len(config.get('publicKeys', {})) == 1, 'Expected the single validation signing key')
        PLANNER['validate_record'](record, records[0]['config'] if records else config)
        if records:
            without_version = lambda c: {k: v for k, v in c.items() if k not in ('version', 'releaseId')}
            require(without_version(config) == without_version(records[0]['config']), 'A and B do not share exact trust')
        for field, name in (('package', 'package.exe'), ('feed', 'feed.json'), ('components', 'components.json'),
                            ('installedManifest', 'installed-manifest.json')):
            descriptor = release[field]
            require(descriptor['url'] == f'{BASE}releases/{release_id}/{name}', 'Release artifact escapes validation path')
            artifact_path = directory / name
            require(artifact_path.is_file() and not artifact_path.is_symlink(), 'Missing regular release artifact')
            with artifact_path.open('rb') as stream:
                PUBLISHER.digest(stream, descriptor)
        catalog, data = PLANNER['read_json'](directory / 'components.json')
        sidecar = bundle / 'python-components.json'
        require(PLANNER['read_json'](sidecar)[1] == data, 'Component upload catalog differs from signed release inputs')
        entries = catalog['components']
        require('rag' in entries and entries.keys() <= {'rag', 'cluster'}, 'Unexpected validation component list')
        rag = {k: v for k, v in entries['rag'].items() if k not in ('schemaVersion', 'signingMode')}
        require(rag == record['componentDescriptor'], 'RAG record differs from the packaged catalog')
        for component, entry in entries.items():
            tag, filename = entry['tag'], entry['filename']
            require(entry['id'] == component and entry['platform'] == 'windows' and entry['arch'] == 'amd64'
                    and re.fullmatch(r'[a-f0-9]{64}', tag) and entry['revision'] == tag
                    and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*\.zip', filename), 'Invalid component identity')
            tag_base = f'{BASE}RAG/tags/{tag}/'
            require(entry['url'] == tag_base + filename and entry['catalogUrl'] == tag_base + 'python-components.json'
                    and not entry.get('fallbackUrl'), 'Component escapes validation path')
            path = bundle / filename
            require(path.is_file() and not path.is_symlink(), 'Missing regular component ZIP')
            descriptor = {'size': entry['sizeBytes'], 'sha256': entry['sha256']}
            with path.open('rb') as stream:
                PUBLISHER.digest(stream, descriptor)
            prior = archives.get(entry['url'])
            require(prior is None or descriptor == prior[1], 'One immutable component name has conflicting digests')
            archives[entry['url']] = (path, descriptor, entry)
            catalogs.setdefault(entry['catalogUrl'], []).append(sidecar)
        records.append(record)
    return records, catalogs, archives


def pinned_fetch(config):
    opener = build_opener(PUBLISHER.PinnedRedirects(config['allowedOrigins']))
    return lambda url: opener.open(url, timeout=60)


def read_policy(config):
    fetch = pinned_fetch(config)
    return fetch, PUBLISHER.read_remote_policy(fetch, config['policyURL'])


def digest_or_absent(envelope):
    return hashlib.sha256(envelope).hexdigest() if envelope is not None else 'absent'


def prepare(artifacts, evidence_path, validator, output):
    (source, target), _, _ = load_inputs(artifacts)
    evidence = PLANNER['read_json'](evidence_path)[0]
    a, b = source['release']['releaseId'], target['release']['releaseId']
    pair = {'sourceReleaseId': a, 'targetReleaseId': b, 'dataProfile': target['formatProfile']}
    edge = PLANNER['verified_edge'](pair, source, target, (evidence_path.resolve(), evidence), True)
    config = source['config']
    _, prior = read_policy(config)
    output.mkdir(parents=True, exist_ok=False)
    config_path = output / 'config.json'
    PLANNER['write_json'](config_path, config)
    if prior is None:
        now = datetime.now(timezone.utc).replace(microsecond=0)
        PLANNER['write_json'](output / 'plan.json', {'policySequence': 1, 'issuedAt': now.isoformat(),
            'expiresAt': (now + timedelta(days=30)).isoformat(), 'releaseIds': [a, b], 'edges': [edge], 'revoked': []})
    else:
        prior_path = output / 'published-policy.json'
        prior_path.write_bytes(prior)
        existing = PLANNER['verified_policy'](validator, prior_path, config_path)
        expected_edge = {'sourceReleaseId': a, 'targetReleaseId': b, 'priority': 0, 'migrationProfile': 'format-v2',
                         'sourceDataProfiles': [pair['dataProfile']], 'targetDataProfile': pair['dataProfile'],
                         'componentTransitions': edge['componentTransitions']}
        actual_edges = existing.get('edges') or []
        require(existing['policySequence'] == 1 and existing['releases'] == [source['release'], target['release']]
                and not existing['revoked'] and not existing.get('repairAcceptances') and len(actual_edges) == 1
                and all(actual_edges[0].get(k) == v for k, v in expected_edge.items())
                and not actual_edges[0].get('requiredComponents'), 'Unexpected signed predecessor in validation channel')
    (output / 'expected-policy-sha256').write_text(digest_or_absent(prior) + '\n', encoding='ascii')


def publish_components(artifacts, output):
    records, catalogs, archives = load_inputs(artifacts)
    config = records[0]['config']
    download, prior = read_policy(config)
    expected = (output / 'expected-policy-sha256').read_text(encoding='ascii').strip()
    require(digest_or_absent(prior) == expected, 'Validation policy changed before component publication')
    client = ValidationPublishClient(download)
    fetch = client.fetch

    def present(url, descriptor):
        try:
            with fetch(url) as stream:
                PUBLISHER.digest(stream, descriptor)
                return True
        except HTTPError as error:
            if error.code == 404:
                return False
            raise

    with tempfile.TemporaryDirectory(prefix='lm-validation-components-') as temp:
        missing = []
        for index, (url, (archive, descriptor, entry)) in enumerate(archives.items()):
            VERIFY['extract_bundle'](archive, Path(temp) / str(index), entry)
            if not present(url, descriptor):
                missing.append((url, archive, descriptor))
        pointers = []
        for url, sources in catalogs.items():
            current = PUBLISHER.read_remote_policy(fetch, url)
            choices = [path.read_bytes() for path in sources]
            require(current is None or current in choices, 'Unexpected existing catalog in validation component path')
            if current != choices[-1]:
                pointers.append((url, sources[-1], choices[-1], current))
        def upload(url, source):
            require(url.startswith(BASE + 'RAG/'), 'Unexpected validation component upload path')
            client.upload_file(repo_id=REPO, repo_type='dataset', path_or_fileobj=str(source),
                               path_in_repo=PUBLISHER.destination(url, DATASET), revision='master',
                               commit_message='Publish Windows package validation components')

        for url, archive, descriptor in missing:
            upload(url, archive)
            require(present(url, descriptor), 'Uploaded validation component failed public readback')
        for url, source, data, current in pointers:
            require(PUBLISHER.read_remote_policy(fetch, url) == current, 'Validation component catalog changed')
            upload(url, source)
            require(PUBLISHER.read_remote_policy(fetch, url) == data, 'Uploaded validation catalog failed public readback')


def publish_releases(artifacts, output, validator):
    config_path = output / 'policy-config.json'
    config, _ = PLANNER['read_json'](config_path)
    require(config['policyURL'] == BASE + 'policy.json', 'Unexpected Windows validation policy URL')
    client = ValidationPublishClient(pinned_fetch(config))
    expected = (output / 'expected-policy-sha256').read_text(encoding='ascii').strip()
    PUBLISHER.publish(artifacts, output / 'policy.json', config_path, REPO, api=client, fetch=client.fetch,
                      validator=validator.resolve(), expected_policy_sha256=expected)
    print('Windows validation release files and signed policy publicly verified.', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=('prepare', 'publish-components', 'publish-releases'))
    parser.add_argument('--artifacts', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--validator', type=Path)
    args = parser.parse_args()
    try:
        if args.command == 'prepare':
            require(args.evidence and args.validator, 'Prepare requires native format evidence and a policy validator')
            prepare(args.artifacts, args.evidence, args.validator, args.output)
        elif args.command == 'publish-components':
            publish_components(args.artifacts, args.output)
        else:
            require(args.validator, 'Publishing releases requires a policy validator')
            publish_releases(args.artifacts, args.output, args.validator)
    except (ValidationError, PLANNER['PlanError']) as error:
        raise SystemExit(str(error)) from None
    except Exception:
        # Hub failures can include temporary signed URLs or credentials.
        raise SystemExit('Windows validation publication failed; verify artifacts and remote state before retrying.') from None


if __name__ == '__main__':
    main()
