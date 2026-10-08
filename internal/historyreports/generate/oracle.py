"""Run original CPAMP using Go overlays; never writes the analytics worktree.

Usage: python3 internal/historyreports/generate/oracle.py ANALYTICS OUTPUT
OUTPUT must be a fresh task-local directory. No live DB/HTTP/provider is used.
"""
import hashlib
import json
import os
import pathlib
import subprocess
import sys

source = pathlib.Path(sys.argv[1]).resolve()
output = pathlib.Path(sys.argv[2]).resolve()
output.mkdir(parents=True, exist_ok=False)
here = pathlib.Path(__file__).resolve().parents[1]
env = dict(os.environ, PACER_ORACLE_OUT=str(output), PACER_SCHEMA=str(here.parent / 'history/cpamp-schema.sql'))
module = source / 'apps/manager-server'
overlay = {'Replace': {}}
for domain, template in [('service', 'oracle_service'), ('http/controller', 'oracle_exports')]:
    overlay['Replace'][str(module / f'internal/{domain}/usagehistory/pacer_oracle_test.go')] = str(here / f'testdata/{template}_test.go.txt')
(output / 'overlay.json').write_text(json.dumps(overlay))
for domain, test in [('service', 'TestPacerReportOracle'), ('http/controller', 'TestPacerExportOracle')]:
    with (output / (test + '.log')).open('w') as log:
        subprocess.run(['go', 'test', '-count=1', '-timeout=180s', '-overlay', str(output / 'overlay.json'), '-run', '^' + test + '$', '-v', './internal/' + domain + '/usagehistory'], cwd=module, env=env, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=240)
provenance = {'commit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=source, text=True).strip(), 'outputs': {}}
for name in ['fixture.sql', 'oracle.json', 'exports.json']:
    provenance['outputs'][name] = hashlib.sha256((output / name).read_bytes()).hexdigest()
(output / 'provenance.json').write_text(json.dumps(provenance, indent=2) + '\n')
