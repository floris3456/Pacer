#!/bin/sh
# Pace-routing lab: a real CPA host and the native Pacer library on an internal
# Docker network, with capacity_upstream.py posing as api.anthropic.com for three
# dummy Claude logins. No outbound network, no production files or credentials.
#
#   tests/lab/pace-lab.sh up <CLIProxyAPI binary> <pacer .so>   # fresh state each time
#   tests/lab/pace-lab.sh check                                 # runs check-pace.py
#   tests/lab/pace-lab.sh down
#
# Build both for the Docker host's architecture with CGO (README, "Pace routing
# lab"). Everything lives in Docker volumes, so it also works where the Docker
# daemon cannot bind-mount the caller's paths.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
network=pace-lab
volume=pace-lab
cpa=pace-lab-cpa
fixture=pace-lab-upstream
runtime=${PACE_LAB_RUNTIME:-golang:1.26-bookworm} # glibc, CA bundle, curl, openssl
python=${PACE_LAB_PYTHON:-python:3.12-slim}
arch=$(docker run --rm "$runtime" go env GOARCH)

case ${1:-} in
up)
	[ -x "${2:-}" ] && [ -f "${3:-}" ] || { echo "usage: $0 up <CLIProxyAPI> <pacer .so>" >&2; exit 2; }
	"$0" down >/dev/null 2>&1 || true
	stage=$(mktemp -d)
	trap 'rm -rf "$stage"' EXIT INT TERM
	mkdir -p "$stage/auths" "$stage/bin" "$stage/fixture" "$stage/state" "$stage/tls" "$stage/plugins/linux/$arch"
	cp "$2" "$stage/bin/CLIProxyAPI"
	cp "$3" "$stage/plugins/linux/$arch/pacer-v$(cat "$here/../../VERSION").so"
	cp "$here/capacity_upstream.py" "$stage/fixture/"
	# One model name for every login: drop the per-login model prefix.
	for name in cap-a cap-b cap-c; do
		python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); d.pop("prefix",None); json.dump(d,open(sys.argv[2],"w"),indent=2)' "$here/capacity-auths/$name.json" "$stage/auths/$name.json"
	done
	cat >"$stage/config.yaml" <<-'EOF'
	# Pace-routing lab only. Fixed dummy keys; no production directories or credentials.
	config-version: 8
	server: {host: "0.0.0.0", port: 8317}
	management: {allow-remote: true, secret-key: "pacer-test-admin", disable-control-panel: true}
	access: {api-keys: ["pacer-test-inference"]}
	oauth: {auth-dir: "/lab/auths"}
	routing: {strategy: fill-first, session-affinity: true, session-affinity-ttl: 24h}
	transient-error-cooldown-seconds: -1 # as on CT104: one 5xx does not cool an account down
	plugins:
	  enabled: true
	  dir: "/lab/plugins"
	  configs:
	    pacer:
	      enabled: true
	      priority: 1
	      state_path: "/lab/state/pacer.sqlite"
	      compact_on_switch: true
	      routing: {mode: dry-run, providers: [claude]} # the former mode: must load as off
	EOF
	docker run --rm "$runtime" sh -c 'cd /tmp && openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=Pace routing lab only" -addext "subjectAltName=DNS:api.anthropic.com,DNS:platform.claude.com" -keyout key.pem -out cert.pem 2>/dev/null && tar -cf - cert.pem key.pem' | tar -xf - -C "$stage/tls"
	docker volume create "$volume" >/dev/null
	tar -C "$stage" -cf - . | docker run --rm -i -v "$volume:/lab" "$runtime" sh -c 'tar -xf - -C /lab && chown -R 1000:1000 /lab && chmod 600 /lab/auths/*.json'
	docker network create --internal "$network" >/dev/null
	docker run -d --name "$fixture" --network "$network" --network-alias api.anthropic.com --network-alias platform.claude.com \
		-v "$volume:/lab:ro" -e FIXTURE_CERT=/lab/tls/cert.pem -e FIXTURE_KEY=/lab/tls/key.pem "$python" python3 /lab/fixture/capacity_upstream.py >/dev/null
	docker run -d --name "$cpa" --network "$network" --user 1000:1000 -v "$volume:/lab" -e SSL_CERT_FILE=/lab/tls/cert.pem -e HOME=/lab/home -w /lab \
		"$runtime" /lab/bin/CLIProxyAPI --config /lab/config.yaml --local-model >/dev/null
	sleep 8
	docker logs "$cpa" 2>&1 | grep -q "plugin registered plugin_id=pacer" || { docker logs "$cpa" | tail -20; echo "Pacer did not register" >&2; exit 1; }
	echo "pace-lab: up ($arch); run: $0 check"
	;;
check)
	docker run --rm -i --network "$network" "$python" python3 - <"$here/check-pace.py"
	;;
down)
	docker rm -f "$cpa" "$fixture" >/dev/null 2>&1 || true
	docker network rm "$network" >/dev/null 2>&1 || true
	docker volume rm "$volume" >/dev/null 2>&1 || true
	echo "pace-lab: removed"
	;;
*)
	echo "usage: $0 up <CLIProxyAPI> <pacer .so> | check | down" >&2
	exit 2
	;;
esac
