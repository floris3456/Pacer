"""Pack successful ORIGINAL CPAMP oracle outputs into small reproducible goldens."""
import gzip
import hashlib
import json
import pathlib
import sys

source = pathlib.Path(sys.argv[1])
target = pathlib.Path(__file__).resolve().parents[1] / 'testdata/golden'
provenance = json.loads((source / 'provenance.json').read_text())
assert provenance['commit'] == 'e8492c245bf69144afdcf20b8b4ea8935f0476e5'
target.mkdir(exist_ok=True)
for name, digest in provenance['outputs'].items():
    raw = (source / name).read_bytes()
    assert hashlib.sha256(raw).hexdigest() == digest
    (target / (name + '.gz')).write_bytes(gzip.compress(raw, compresslevel=9, mtime=0))
(target / 'provenance.json').write_text(json.dumps(provenance, indent=2) + '\n')
