#!/usr/bin/env bash
# Full local rehearsal of phase1.py and dry-run-daily.py against an isolated replica of the CT104
# CPA + Caddy layout (OrbStack on the Mac). Never touches CT104, production, pacer-lab-* or
# pacer-upstream*. Usage:
#
#   scripts/production/rehearse.sh [runs]     default 2 full runs; KEEP=1 keeps the replica after a failure
#
# Per run: recon (+ event-poll probe) -> stage -> refusals -> idle gate rejects an in-flight hold ->
# injected verification failure (auto rollback) -> injected mid-cutover failure (auto rollback) ->
# cutover -> verify -> first daily report -> rollback -> verify -> rollback again (no-op) ->
# cutover -> verify -> rollback -> verify. After every rollback the tracked files are compared
# byte for byte (sha256, inode, mode) with the staged backups by this runner, independently of phase1.py.
#
# Docker objects (all removed at exit): network pacer-rehearsal-net (internal), containers
# pacer-rehearsal-{upstream,caddy,cpa}, transient pacer-rehearsal-tmp-*, image
# pacer-rehearsal-cpa:pacer-bb27d121, and only the BuildKit cache records created by this session.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
WT=$(cd "$HERE/../.." && pwd)
PACER=$(cd "$WT/../.." && pwd)
RH=$PACER/.cache/rehearsal
SESSION=$(date -u +%Y%m%dT%H%M%SZ)
EVID=$PACER/evidence/implementation/phase1-rehearsal/session-$SESSION
NATIVE=$PACER/.cache/native
CPA_BIN=$NATIVE/cpa-bb27d121-cgo-linux-amd64
PACER_DIST=$NATIVE/src-pacer-ab311f1-1/dist/linux_amd64
QH_LIB=$(cd "$PACER/../quota-switch-compaction/artifacts/release-0.1.5-local.1" && pwd)/quota-handoff-v0.1.5-local.1.so
SNIPPET_SRC=$(cd "$PACER/../credential-handoff/config" && pwd)/cliproxyapi-quota-handoff.Caddyfile
UPSTREAM_PY=$WT/tests/lab/upstream.py
BASE_IMAGE=cliproxyapi-opus55:20261007-13
NEW_IMAGE=pacer-rehearsal-cpa:pacer-bb27d121
TOOL_IMAGE=python:3.12-slim
CADDY_IMAGE=caddy:2.10.2-alpine
NET=pacer-rehearsal-net
TAG=pacer-phase1-ab311f1-bb27d121
RUNS=${1:-2}
LEGACY=/v0/resource/plugins/quota-handoff/events
FIXTURE=http://pacer-rehearsal-upstream.orb.local:8080
PUBLIC=http://pacer-rehearsal-caddy.orb.local:8080
CPA=http://pacer-rehearsal-cpa.orb.local:8317
PY=$RH/venv/bin/python
FAILED=0
CACHE_BEFORE=$RH/buildcache-before.txt

mkdir -p "$RH" "$EVID"
LOG=$EVID/rehearsal.log
RESULTS=$EVID/results.tsv
printf 'run\tstep\texit\tverdict\tseconds\n' >"$RESULTS"

log() { printf '%s %s\n' "$(date -u +%H:%M:%SZ)" "$*" | tee -a "$LOG"; }
die() { log "ABORT: $*"; FAILED=1; collect; exit 1; }

docker_names() { docker "$@" 2>/dev/null | grep -E '^pacer-rehearsal(-|:)' || true; }

teardown() {  # remove every pacer-rehearsal-* Docker object
  docker_names ps -a --format '{{.Names}}' | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker image rm "$NEW_IMAGE" >/dev/null 2>&1 || true
}

prune_build_cache() {  # only cache records that did not exist before this session
  [[ -f $CACHE_BEFORE ]] || return 0
  docker buildx du --verbose 2>/dev/null | awk '/^ID:/{print $2}' | sort >"$RH/buildcache-after.txt"
  local id n=0
  for id in $(comm -13 "$CACHE_BEFORE" "$RH/buildcache-after.txt"); do
    docker buildx prune -f --filter "id=$id" >/dev/null 2>&1 && n=$((n + 1))
  done
  log "build cache: pruned $n record(s) created by this session"
}

leftovers() {
  { docker_names ps -a --format '{{.Names}}'
    docker_names network ls --format '{{.Name}}'
    docker_names images --format '{{.Repository}}:{{.Tag}}'
    docker_names volume ls --format '{{.Name}}'; } | sort -u
}

cleanup() {
  local rc=$?
  set +e
  if [[ $FAILED != 0 && ${KEEP:-0} == 1 ]]; then
    log "KEEP=1: leaving the failed replica and containers in place under $RH"
    return
  fi
  teardown
  prune_build_cache
  local left
  left=$(leftovers)
  if [[ -n $left ]]; then
    log "LEFTOVER Docker objects: $left"
  else
    log "cleanup: no pacer-rehearsal-* containers, networks, images or volumes remain"
  fi
  rm -rf "$RH"/run-* "$CACHE_BEFORE" "$RH/buildcache-after.txt"
  printf '{"session": "%s", "failed": %s, "leftovers": %s}\n' "$SESSION" "$FAILED" \
    "$([[ -z $left ]] && echo '[]' || echo '"see log"')" >"$EVID/cleanup.json"
  exit $rc
}
trap cleanup EXIT

# ------------------------------------------------------------------ preflight
for f in "$CPA_BIN" "$PACER_DIST/pacer-v0.2.0-dev.so" "$PACER_DIST/pacer-state" "$QH_LIB" "$SNIPPET_SRC" "$UPSTREAM_PY"; do
  [[ -f $f ]] || die "missing artifact $f"
done
for i in "$BASE_IMAGE" "$TOOL_IMAGE" "$CADDY_IMAGE"; do
  docker image inspect "$i" >/dev/null 2>&1 || die "image $i is not local (the rehearsal never pulls)"
done
if [[ ! -x $PY ]] || ! "$PY" -c 'import yaml' 2>/dev/null; then
  log "creating task-local venv with PyYAML under $RH/venv"
  /usr/bin/python3 -m venv "$RH/venv"
  "$RH/venv/bin/pip" install --no-cache-dir --disable-pip-version-check -q pyyaml
fi
[[ -z $(leftovers) ]] || { log "removing leftovers from an earlier session: $(leftovers | tr '\n' ' ')"; teardown; }
# A kept (KEEP=1) failed session leaves its snapshot so its build cache is still pruned later.
[[ -f $CACHE_BEFORE ]] || docker buildx du --verbose 2>/dev/null | awk '/^ID:/{print $2}' | sort >"$CACHE_BEFORE"
log "session $SESSION: $RUNS run(s); evidence $EVID"

# ------------------------------------------------------------------ helpers

record() {  # record NAME RC WANT SECONDS
  local verdict=pass
  [[ $2 == "$3" ]] || verdict=FAIL
  printf '%s\t%s\t%s\t%s\t%s\n' "$RUN" "$1" "$2" "$verdict" "$4" >>"$RESULTS"
  log "run $RUN: $1 exit=$2 want=$3 $verdict (${4}s)"
  if [[ $verdict != pass ]]; then
    FAILED=1
    collect
    exit 1
  fi
}

p1() {  # p1 [INJECT] PHASE ARGS... ; runs the bundled script on stdin like `pct exec 104 -- python3 -B -`
  local inject=
  if [[ $1 == inject=* ]]; then inject=${1#inject=}; shift; fi
  env PHASE1_INJECT_FAIL="$inject" "$PY" -B - "$@" <"$RELEASE/phase1.py"
}

step() {  # step NAME WANT_EXIT COMMAND...
  local name=$1 want=$2 rc=0 t0
  shift 2
  t0=$(date +%s)
  "$@" >"$OUT/$name.out" 2>"$OUT/$name.err" || rc=$?
  record "$name" "$rc" "$want" $(($(date +%s) - t0))
}

expect() {  # expect NAME JSON_FILE PYTHON_EXPR  (r = parsed JSON, rc = receipt JSON if the file names one)
  local rc=0
  "$PY" -B - "$2" "$3" >"$OUT/$1.out" 2>&1 <<'EOF' || rc=$?
import json, sys
r = json.load(open(sys.argv[1]))
rc = json.load(open(r['receipt'])) if isinstance(r, dict) and r.get('receipt') else None
ok = eval(sys.argv[2], {'r': r, 'rc': rc, 'any': any, 'all': all, 'len': len, 'set': set})
print(json.dumps({'expr': sys.argv[2], 'ok': bool(ok)}))
sys.exit(0 if ok else 1)
EOF
  record "$1" "$rc" 0 0
}

files_restored() {  # independent byte-for-byte check against the staged backups
  local rc=0
  "$PY" -B - "$ROOT" "$CADDY" "$BACKUP" >"$OUT/$1.out" 2>&1 <<'EOF' || rc=$?
import hashlib, json, os, sys
root, caddy, backup = sys.argv[1:4]
st = json.load(open(os.path.join(backup, 'stage.json')))
paths = {'config': root + '/config.yaml', 'compose': root + '/docker-compose.yml',
         'snippet': caddy + '/cliproxyapi-quota-handoff.Caddyfile',
         'qh_lib': root + '/plugins/linux/amd64/quota-handoff-v0.1.5-local.1.so'}
sha = lambda p: hashlib.sha256(open(p, 'rb').read()).hexdigest()
out = {}
for name, path in paths.items():
    s, b = os.stat(path), st['files'][name]
    out[name] = {'sha256': sha(path) == b['sha256'] == sha(os.path.join(backup, 'files', name)),
                 'inode': s.st_ino == b['inode'], 'mode': oct(s.st_mode & 0o7777) == b['mode']}
out['pacer_library_absent'] = not os.path.exists(root + '/plugins/linux/amd64/pacer-v0.2.0-dev.so')
out['pacer_state_kept'] = os.path.exists(root + '/plugins/pacer/state.sqlite')
ok = all(all(v.values()) if isinstance(v, dict) else v for v in out.values())
print(json.dumps({'ok': ok, 'files': out}, sort_keys=True))
sys.exit(0 if ok else 1)
EOF
  record "$1" "$rc" 0 0
}

decisions() {  # current pacer_decisions row count, read inside the Docker VM
  docker run --rm --name "pacer-rehearsal-tmp-count-$$-$RANDOM" --network none -v "$ROOT/plugins/pacer:$ROOT/plugins/pacer" "$TOOL_IMAGE" \
    python3 -c "import sqlite3,sys; db=sqlite3.connect('file:'+sys.argv[1]+'?mode=ro',uri=True); print(db.execute('SELECT count(*) FROM pacer_decisions').fetchone()[0])" \
    "$ROOT/plugins/pacer/state.sqlite"
}

save_diffs() {  # -U0 diffs of the edited files (no secrets in the touched lines)
  local name
  for name in config compose snippet; do
    case $name in
      config) live=$ROOT/config.yaml ;;
      compose) live=$ROOT/docker-compose.yml ;;
      snippet) live=$CADDY/cliproxyapi-quota-handoff.Caddyfile ;;
    esac
    diff -U0 "$BACKUP/files/$name" "$live" | sed "s#$RUNDIR#<run>#g" >"$OUT/$1-diff-$name.txt" || true
  done
}

collect() {  # copy receipts, outputs and reports into the evidence directory
  [[ -n ${RUNDIR:-} && -d ${RUNDIR:-/nonexistent} ]] || return 0
  local dest=$EVID/run-$RUN
  mkdir -p "$dest/receipts" "$dest/out" "$dest/reports"
  cp "$OUT"/* "$dest/out/" 2>/dev/null || true
  cp "$RELEASE"/receipts/*.json "$dest/receipts/" 2>/dev/null || true
  cp "$BACKUP/stage.json" "$dest/receipts/stage-state.json" 2>/dev/null || true
  cp "$ROOT"/reports/pacer-dry-run/* "$dest/reports/" 2>/dev/null || true
  cp "$RELEASE/manifest.json" "$dest/manifest.json" 2>/dev/null || true
  cp -R "$RELEASE/systemd" "$dest/systemd" 2>/dev/null || true
  docker logs pacer-rehearsal-cpa >"$RUNDIR/cpa-last.log" 2>&1 || true
  # Paths are made relative so receipts are portable; secrets must never appear.
  find "$dest" -type f \( -name '*.json' -o -name '*.out' -o -name '*.err' -o -name '*.txt' -o -name '*.md' \) -print0 |
    xargs -0 sed -i '' "s#$RUNDIR#<run>#g"
  if grep -rqF -e "$MGMT_KEY" -e "$CLIENT_KEY" -e 'sk-ant-oat01-pacer' "$dest"; then
    log "SECRET FOUND in evidence for run $RUN; removing the run's evidence"
    rm -rf "$dest"
    FAILED=1
  fi
}

# ------------------------------------------------------------------ replica

write_config() {  # style a: CPA-saved 4-space YAML, plugins mid-file; style b: 2-space, sibling plugin after quota-handoff
  if [[ $1 == a ]]; then
    cat >"$ROOT/config.yaml" <<EOF
# Pacer phase 1 rehearsal replica of /opt/cliproxyapi/config.yaml (dummy keys, offline fixture only).
config-version: 8
server:
    host: "0.0.0.0"
    port: 8317
management:
    allow-remote: true
    secret-key: "$MGMT_KEY"
    disable-control-panel: true
access:
    api-keys:
        - "$CLIENT_KEY"
routing:
    strategy: fill-first
    session-affinity: true
    session-affinity-ttl: 24h
    retry:
        request-retry: 0
plugins:
    enabled: true
    dir: "/CLIProxyAPI/plugins"
    configs:
        quota-handoff:
            enabled: true # incumbent since 2026-10-03
            priority: 1
            state_path: "/CLIProxyAPI/plugins/quota-handoff/state.sqlite"
            discover_credentials: true
            max_age_seconds: 90
            notifications_enabled: false
            compact_on_switch: true
            adaptive:
                mode: shadow

            # trailing comment inside the quota-handoff block stays where it is
oauth:
    auth-dir: "/CLIProxyAPI/auths"
debug: false
EOF
  else
    cat >"$ROOT/config.yaml" <<EOF
# Pacer phase 1 rehearsal replica, hand-edited style (dummy keys, offline fixture only).
config-version: 8
server:
  host: "0.0.0.0"
  port: 8317
management:
  allow-remote: true
  secret-key: "$MGMT_KEY"
  disable-control-panel: true
access:
  api-keys:
    - "$CLIENT_KEY"
oauth:
  auth-dir: "/CLIProxyAPI/auths"
routing:
  strategy: fill-first
  session-affinity: true
  session-affinity-ttl: 24h
debug: false
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    # Credential handoff (quota-handoff 0.1.5-local.1)
    quota-handoff:
      enabled: true
      priority: 1
      state_path: /CLIProxyAPI/plugins/quota-handoff/state.sqlite
      discover_credentials: true
      notifications_enabled: false
      compact_on_switch: true
      adaptive: {mode: shadow}

    opencode-go-cliproxyapi:
      enabled: false
EOF
  fi
}

build_replica() {
  RUNDIR=$RH/run-$RUN
  rm -rf "$RUNDIR"
  ROOT=$RUNDIR/opt/cliproxyapi
  CADDY=$RUNDIR/etc/caddy
  TLS=$RUNDIR/tls
  OUT=$RUNDIR/out
  RELEASE=$ROOT/releases/$TAG
  BACKUP=$ROOT/backups/$TAG
  mkdir -p "$ROOT/auths" "$ROOT/plugins/linux/amd64" "$ROOT/plugins/quota-handoff" "$CADDY" "$TLS" "$OUT" "$RUNDIR/secrets"
  chmod 700 "$RUNDIR/secrets" "$ROOT/auths"
  MGMT_KEY=rehearsal-mgmt-$(openssl rand -hex 16)
  CLIENT_KEY=rehearsal-client-$(openssl rand -hex 16)
  printf 'Authorization: Bearer %s\n' "$MGMT_KEY" >"$RUNDIR/secrets/mgmt.hdr"
  printf 'Authorization: Bearer %s\n' "$CLIENT_KEY" >"$RUNDIR/secrets/client.hdr"
  chmod 600 "$RUNDIR"/secrets/*

  cat >"$TLS/openssl.cnf" <<'EOF'
[req]
distinguished_name = dn
x509_extensions = v3
prompt = no
[dn]
CN = api.anthropic.com
[v3]
subjectAltName = DNS:api.anthropic.com,DNS:console.anthropic.com
basicConstraints = critical,CA:TRUE
keyUsage = critical,digitalSignature,keyEncipherment,keyCertSign
extendedKeyUsage = serverAuth
EOF
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 -keyout "$TLS/key.pem" -out "$TLS/cert.pem" -config "$TLS/openssl.cnf" 2>/dev/null

  local who
  for who in a b; do
    printf '{"access_token":"sk-ant-oat01-pacer-%s","account_uuid":"fixture-%s","claude_device_ids":["%s"],"disabled":false,"email":"fixture-%s@example.invalid","expired":"2026-12-01T00:00:00Z","type":"claude"}' \
      "$who" "$who" "$(printf 'pacer-rehearsal-device-%s' "$who" | shasum -a 256 | cut -c1-64)" "$who" >"$ROOT/auths/claude-fixture-$who.json"
    chmod 600 "$ROOT/auths/claude-fixture-$who.json"
  done

  write_config "$STYLE"
  cat >"$ROOT/docker-compose.yml" <<'EOF'
# Rehearsal replica of the CT104 compose file. The compose project is pacer-rehearsal (COMPOSE_PROJECT_NAME).
services:
  cli-proxy-api:
    # CLIProxyAPI 8.0.12-opus55.13
    image: cliproxyapi-opus55:20261007-13
    platform: linux/amd64
    container_name: pacer-rehearsal-cpa
    restart: unless-stopped
    environment:
      - SSL_CERT_FILE=/etc/pacer-rehearsal/ca.pem
    volumes:
      - ./config.yaml:/CLIProxyAPI/config.yaml
      - ./auths:/CLIProxyAPI/auths
      - ./plugins:/CLIProxyAPI/plugins
      - ../../tls/cert.pem:/etc/pacer-rehearsal/ca.pem:ro
    networks:
      - rehearsal

networks:
  rehearsal:
    name: pacer-rehearsal-net
    external: true
EOF
  cp "$QH_LIB" "$ROOT/plugins/linux/amd64/"
  chmod 755 "$ROOT/plugins/linux/amd64/quota-handoff-v0.1.5-local.1.so"

  cp "$SNIPPET_SRC" "$CADDY/cliproxyapi-quota-handoff.Caddyfile"
  printf 'request_header Authorization "Bearer %s"\n' "$MGMT_KEY" >"$CADDY/cliproxyapi-management-auth.Caddyfile"
  chmod 600 "$CADDY/cliproxyapi-management-auth.Caddyfile"
  cat >"$CADDY/Caddyfile" <<'EOF'
{
	admin 0.0.0.0:2019 {
		origins localhost:2019 127.0.0.1:2019 pacer-rehearsal-caddy.orb.local:2019
	}
	auto_https off
	persist_config off
}

(strip_authenticated_identity_headers) {
	request_header -X-Auth-Request-User
	request_header -X-Auth-Request-Email
	request_header -Remote-User
}

(cliproxyapi_upstream) {
	reverse_proxy pacer-rehearsal-cpa:8317 {
		header_up Host {host}
		flush_interval -1
	}
}

# Stand-in for cliproxy.boe.moe: same route order, plain HTTP inside the isolated network.
http://:8080 {
	@cliproxyapi_api {
		path /v1 /v1/* /v1beta /v1beta/*
	}

	route @cliproxyapi_api {
		import strip_authenticated_identity_headers
		import cliproxyapi_upstream
	}

	import /etc/caddy/cliproxyapi-quota-handoff.Caddyfile

	route * {
		respond "rehearsal identity gate" 403
	}
}
EOF

  export COMPOSE_PROJECT_NAME=pacer-rehearsal
  export PHASE1_REHEARSAL=1 PHASE1_ROOT=$ROOT PHASE1_CADDY_DIR=$CADDY
  export PHASE1_CADDY_CMD="docker exec pacer-rehearsal-caddy caddy" PHASE1_CADDY_CONFIG_ARG=/etc/caddy/Caddyfile
  export PHASE1_CADDY_ADMIN=http://pacer-rehearsal-caddy.orb.local:2019 PHASE1_CADDY_UPSTREAM=pacer-rehearsal-cpa:8317
  export PHASE1_CONTAINER=pacer-rehearsal-cpa PHASE1_CPAMP=pacer-rehearsal-cpamp-absent PHASE1_OTHERS_PREFIX=pacer-rehearsal-
  export PHASE1_CPA_URL=$CPA PHASE1_PUBLIC_URL=$PUBLIC PHASE1_NEW_IMAGE=$NEW_IMAGE PHASE1_PLATFORM=linux/amd64
  export PHASE1_DOCKER_PREFIX=pacer-rehearsal-tmp PHASE1_SQLITE_IMAGE=$TOOL_IMAGE PHASE1_SYSTEMD=0
  export PHASE1_IDLE_INTERVAL=2 PHASE1_POLL_WINDOW=20
}

wait_for() {  # wait_for SECONDS DESCRIPTION COMMAND...
  local deadline=$(($(date +%s) + $1)) what=$2
  shift 2
  until "$@" >/dev/null 2>&1; do
    (($(date +%s) < deadline)) || die "timed out waiting for $what"
    sleep 1
  done
}

qh_effective() {
  curl -sf --max-time 5 -H @"$RUNDIR/secrets/mgmt.hdr" "$CPA/v0/management/plugins" |
    "$PY" -c 'import json,sys; p={x["id"]:x for x in json.load(sys.stdin)["plugins"]}; sys.exit(0 if p.get("quota-handoff",{}).get("effective_enabled") else 1)'
}

usage_has_credentials() {
  curl -sf --max-time 5 -H @"$RUNDIR/secrets/client.hdr" "$PUBLIC$LEGACY?view=usage&auth=api-key&all=true" |
    "$PY" -c 'import json,sys; sys.exit(0 if json.load(sys.stdin).get("credentials") else 1)'
}

start_replica() {
  docker network create --internal "$NET" >/dev/null
  docker run -d --name pacer-rehearsal-upstream --network "$NET" --network-alias api.anthropic.com \
    --network-alias console.anthropic.com --pull never -e FIXTURE_CERT=/tls/cert.pem -e FIXTURE_KEY=/tls/key.pem \
    -v "$TLS:/tls:ro" -v "$UPSTREAM_PY:/app/upstream.py:ro" "$TOOL_IMAGE" python -B /app/upstream.py >/dev/null
  docker run -d --name pacer-rehearsal-caddy --network "$NET" --pull never -v "$CADDY:/etc/caddy" \
    "$CADDY_IMAGE" caddy run --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null
  (cd "$ROOT" && docker compose up -d --pull never >/dev/null 2>"$OUT/compose-up.err")
  wait_for 60 fixture curl -sf --max-time 3 "$FIXTURE/health"
  wait_for 60 'caddy admin' curl -sf --max-time 3 http://pacer-rehearsal-caddy.orb.local:2019/reverse_proxy/upstreams
  wait_for 180 'incumbent CPA health' curl -sf --max-time 3 "$CPA/healthz"
  wait_for 90 'quota-handoff registration' qh_effective
  # Production has live traffic: one request lets the incumbent learn its credential set.
  curl -sf --max-time 60 -o /dev/null -H @"$RUNDIR/secrets/client.hdr" -H 'Content-Type: application/json' \
    -d '{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly: OK"}]}' \
    "$PUBLIC/v1/chat/completions" || die 'priming request through Caddy failed'
  wait_for 90 'quota-handoff credential discovery' usage_has_credentials
  log "run $RUN: replica up (style $STYLE)"
}

daily_report() {  # runs the reviewed unit's ExecStart (timer mode), then --latest and --day today
  local exec_line args
  exec_line=$(sed -n 's/^ExecStart=//p' "$RELEASE/systemd/pacer-dry-run-daily.service")
  args=${exec_line#/usr/bin/python3 }
  mkdir -p "$ROOT/reports/pacer-dry-run"
  local mounts="-v $RELEASE:$RELEASE:ro -v $ROOT/plugins/pacer:$ROOT/plugins/pacer -v $ROOT/reports/pacer-dry-run:$ROOT/reports/pacer-dry-run"
  # shellcheck disable=SC2086
  step report-timer-mode 0 docker run --rm --name pacer-rehearsal-tmp-report-1 --network none $mounts "$TOOL_IMAGE" python3 $args
  # shellcheck disable=SC2086
  step report-latest 0 docker run --rm --name pacer-rehearsal-tmp-report-2 --network none $mounts "$TOOL_IMAGE" python3 $args --latest
  # shellcheck disable=SC2086
  step report-today 0 docker run --rm --name pacer-rehearsal-tmp-report-3 --network none $mounts "$TOOL_IMAGE" python3 $args --day "$(date -u +%F)"
  cp "$ROOT/reports/pacer-dry-run/pacer-dry-run-latest.json" "$OUT/report-latest.json"
  expect assert-report "$OUT/report-latest.json" \
    "r['routing_mode'] == 'dry-run' and r['placements']['decisions'] >= 1 and r['placements']['handled'] + r['agreement'].get('no_proposal', 0) == r['placements']['decisions'] and r['integrity']['decision_parse_errors'] == 0"
}

# ------------------------------------------------------------------ one run

one_run() {
  RUN=$1
  case $RUN in
    1) STYLE=a INJECT_CHECK=dry_run_trace INJECT_STEP=step:candidate_running ;;
    *) STYLE=b INJECT_CHECK=usage_view_public INJECT_STEP=step:caddy_reloaded ;;
  esac
  build_replica
  step bundle 0 "$PY" -B "$HERE/phase1.py" bundle --out "$RELEASE" --release-path "$RELEASE" \
    --cpa-binary "$CPA_BIN" --pacer-dist "$PACER_DIST" --qh-lib "$QH_LIB"
  start_replica

  step recon 0 p1 recon
  expect assert-recon "$OUT/recon.out" \
    "r['side'] == 'incumbent' and all(v['applies'] for v in r['edit_plan'].values()) and r['layout']['ok'] and r['release']['manifest_ok'] and r['idle_sample']['quiet']"

  step stage 0 p1 stage --routing-mode dry-run
  step stage-again-refused 2 p1 stage
  step cutover-without-approval 2 p1 cutover

  # The idle gate must reject an in-flight request held open through Caddy.
  curl -sf --max-time 5 -X POST -H 'Content-Type: application/json' -d '{}' "$FIXTURE/_test/hold" >/dev/null
  curl -sS -N --max-time 60 -o "$OUT/hold-stream.txt" -w '%{http_code}' -H @"$RUNDIR/secrets/client.hdr" \
    -H 'Content-Type: application/json' -H 'anthropic-version: 2023-06-01' \
    -d '{"model":"claude-opus-5-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"PACER_HOLD"}]}' \
    "$PUBLIC/v1/messages" >"$OUT/hold-status.txt" 2>&1 &
  local hold=$!
  wait_for 20 'held request in flight' sh -c "curl -sf http://pacer-rehearsal-caddy.orb.local:2019/reverse_proxy/upstreams | grep -q '\"num_requests\":[1-9]'"
  local before
  before=$(docker inspect --format '{{.Id}}' pacer-rehearsal-cpa)
  step cutover-while-busy 3 p1 cutover --i-have-user-approval --idle-wait 8
  curl -sf --max-time 5 -X POST -H 'Content-Type: application/json' -d '{}' "$FIXTURE/_test/release" >/dev/null
  wait "$hold" || true
  expect assert-gate-rejected "$OUT/cutover-while-busy.out" \
    "r['result'] == 'not idle; nothing changed' and not rc['idle_gate']['ok'] and all((s['caddy_inflight'] or 0) >= 1 and s['event_polls'] == 0 and not s['deploy_scripts'] for s in rc['idle_gate']['samples']) and 'steps' not in rc"
  [[ $(docker inspect --format '{{.Id}}' pacer-rehearsal-cpa) == "$before" ]] && record cpa-untouched-while-busy 0 0 0 || record cpa-untouched-while-busy 1 0 0
  files_restored files-unchanged-while-busy

  # Other two busy signals, one at a time: a stdin-run house script, then an OCP event poll.
  "$PY" -B - <<<'import time; time.sleep(60)' &
  local busy=$!
  sleep 1
  step recon-during-stdin-script 0 p1 recon
  kill "$busy" 2>/dev/null || true
  wait "$busy" 2>/dev/null || true
  expect assert-stdin-script-busy "$OUT/recon-during-stdin-script.out" \
    "not r['idle_sample']['quiet'] and any(s.startswith('stdin-python') for s in r['idle_sample']['deploy_scripts']) and r['idle_sample']['event_polls'] == 0"
  curl -s -o /dev/null --max-time 10 "$PUBLIC$LEGACY?after=0" || true
  step recon-after-poll 0 p1 recon
  expect assert-poll-busy "$OUT/recon-after-poll.out" \
    "not r['idle_sample']['quiet'] and r['idle_sample']['event_polls'] >= 1 and r['idle_sample']['caddy_inflight'] == 0 and not r['idle_sample']['deploy_scripts']"

  step cutover-inject-verify 4 p1 "inject=$INJECT_CHECK" cutover --i-have-user-approval --idle-wait 60
  expect assert-inject-verify "$OUT/cutover-inject-verify.out" \
    "r['result'] == 'rolled back' and r['failed'] == ['$INJECT_CHECK'] and r['rollback']['result'] == 'rolled back' and rc['rollback']['verify']['ok']"
  files_restored files-restored-after-inject-verify
  local n1
  n1=$(decisions)
  log "run $RUN: pacer_decisions after the injected verification failure: $n1"

  step cutover-inject-step 4 p1 "inject=$INJECT_STEP" cutover --i-have-user-approval --idle-wait 60
  expect assert-inject-step "$OUT/cutover-inject-step.out" \
    "r['result'] == 'rolled back' and '${INJECT_STEP#step:}' in r['error'] and rc['rollback']['verify']['ok']"
  files_restored files-restored-after-inject-step

  step cutover 0 p1 cutover --i-have-user-approval --idle-wait 60
  expect assert-cutover "$OUT/cutover.out" "r['result'] == 'deployed' and rc['verify']['ok'] and rc['verify']['checks']['dry_run_trace']"
  save_diffs cutover
  step verify-candidate 0 p1 verify
  expect assert-verify-candidate "$OUT/verify-candidate.out" "r['side'] == 'candidate' and r['failed'] == []"
  daily_report

  step rollback 0 p1 rollback --idle-wait 60
  files_restored files-restored-after-rollback
  step verify-incumbent 0 p1 verify
  expect assert-verify-incumbent "$OUT/verify-incumbent.out" "r['side'] == 'incumbent' and r['failed'] == []"
  before=$(docker inspect --format '{{.Id}}' pacer-rehearsal-cpa)
  step rollback-when-incumbent 0 p1 rollback --idle-wait 60
  [[ $(docker inspect --format '{{.Id}}' pacer-rehearsal-cpa) == "$before" ]] && record rollback-noop-no-restart 0 0 0 || record rollback-noop-no-restart 1 0 0

  local n2 n3
  n2=$(decisions)
  step cutover-again 0 p1 cutover --i-have-user-approval --idle-wait 60
  step verify-candidate-again 0 p1 verify
  n3=$(decisions)
  log "run $RUN: pacer_decisions before/after re-cutover: $n2 -> $n3 (state kept across rollbacks)"
  ((n3 > n2 && n2 >= n1)) && record pacer-state-continuity 0 0 0 || record pacer-state-continuity 1 0 0
  save_diffs cutover-again

  step rollback-final 0 p1 rollback --idle-wait 60
  files_restored files-restored-final
  step verify-final 0 p1 verify

  collect
  teardown
  log "run $RUN: complete"
}

for n in $(seq 1 "$RUNS"); do
  one_run "$n"
done
log "session $SESSION: all $RUNS run(s) passed"
