"""Native scoped-readback regression; management reads only, owned lab only."""
import json
import os
import pathlib
import time
import urllib.request


def validate_state(state):
    measurement = state['measurement']
    source = measurement['source_checkpoint']
    assert measurement['delivery'] == 'native-acknowledged-scoped'
    assert source['domain'] == 'request_start_only'
    assert source['account_membership_complete'] is False
    assert source['physical_carry_in_proven'] is False
    assert source['calls'] >= 2
    assert state['routing']['can_activate'] is False
    assert len(state['accounts']) == 2
    for account in state['accounts']:
        report = account['measurement']
        assert report['coverage']['observed'] is False
        assert report['five_hour']['known'] is False
        assert report['seven_day']['known'] is False
        assert account['pace_unit'] == '%/h · equal sizes'
    return {'reports': 2, 'source_calls': source['calls'], 'source_errors': source['errors'],
            'account_membership_complete': False, 'physical_carry_in_proven': False,
            'capacity_known': False, 'active_routing': False}


def main():
    if os.environ.get('PACER_COVERAGE_READY') != '1':
        raise SystemExit('Parent must grant the native coverage window and load the readback fix first')
    out = pathlib.Path(os.environ['PACER_EVIDENCE'])
    out.mkdir(parents=True, exist_ok=True)
    http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    receipt = {'pass': False, 'scope': 'Real native diagnostic/readback regression; not complete coverage'}

    def read():
        request = urllib.request.Request('http://pacer-lab-cpa.orb.local:8317/v0/management/plugins/pacer/state',
                                         headers={'Authorization': 'Bearer pacer-test-admin'})
        with http.open(request, timeout=3) as response:
            return json.load(response)

    try:
        deadline = time.monotonic() + 45  # Background refresh is every 30 seconds.
        while time.monotonic() < deadline:
            state = read()
            if len(state['accounts']) == 2 and all('measurement' in a for a in state['accounts']):
                receipt['before'] = validate_state(state)
                break
            time.sleep(.2)
        else:
            raise AssertionError('Two native scoped reports did not become available')
        # Exceed the five-second discovery cache using the real native clock.
        # This is not a coverage watermark or a unit-test clock substitution.
        time.sleep(6)
        receipt['after_unchanged_discovery'] = validate_state(read())
        receipt['pass'] = True
    finally:
        (out / 'native-coverage-readback.json').write_text(json.dumps(receipt, indent=2) + '\n')
    print(json.dumps(receipt, indent=2))


if __name__ == '__main__':
    main()
