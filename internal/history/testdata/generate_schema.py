"""Regenerate the CPAMP schema contract from a checked-out source (no DB access).

Usage: python3 internal/history/testdata/generate_schema.py /path/to/cpamp
Copied CPAMP SQL is MIT licensed; see ../CPAMP-LICENSE.
"""
import hashlib
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
here = pathlib.Path(__file__).resolve().parent
history = here.parent
required = set(re.findall(r'"([a-z_]+)":\s*"', (history / 'source.go').read_text().split('var required =')[1].split('func inspect')[0]))
files = ['apps/manager-server/internal/repository/sqlite/migrate.go']
files += ['apps/manager-server/internal/service/usagehistory/' + name + '.go'
          for name in ['service', 'profiles', 'forecasts', 'valuation_policy']]
tables = {}
digests = {}
for name in files:
    raw = (root / name).read_bytes()
    digests[name] = hashlib.sha256(raw).hexdigest()
    for literal in re.findall(r'`([^`]+)`', raw.decode()):
        for match in re.finditer(r'create table if not exists (\w+)\s*\(.*?\)\s*(?=;|$)', literal, re.I | re.S):
            table = match.group(1)
            if table in required | {'analytics_historical_valuations', 'analytics_token_evidence'}:
                if table in tables and tables[table] != match.group(0):
                    raise ValueError('conflicting schema: ' + table)
                tables[table] = match.group(0)
assert required <= tables.keys(), required - tables.keys()
(history / 'cpamp-schema.sql').write_text(
    '-- Copied from CPAMP (Copyright (c) 2026 Seakee), MIT; see CPAMP-LICENSE.\n'
    + '\n\n'.join(tables[n] + ';' for n in sorted(tables)) + '\n')
(here / 'schema-provenance.json').write_text(json.dumps(digests, indent=2) + '\n')
