#!/usr/bin/env sh
# Smoke test for ark serve: builds the binary with the embedded UI, starts the
# server against a temp repository and database, logs in, backs up a Docker
# volume through a job and checks the snapshot in the API and in the CLI.
# The web UI carries no stability promise, this only proves the binary still
# serves it. Run from anywhere; needs go, docker, curl and python3. Skips
# with exit 0 where no Docker daemon answers.
set -eu

root=$(cd "$(dirname "$0")/../../.." && pwd)

if ! docker info > /dev/null 2>&1; then
	echo "skip: no Docker daemon"
	exit 0
fi
if [ ! -f "$root/cmd/ark/internal/ui/server/static/index.html" ]; then
	echo "static/index.html missing, run task build-ui first" >&2
	exit 1
fi

tmp=$(mktemp -d)
volume="ark-smoke-$$"
pid=""
cleanup() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	docker volume rm -f "$volume" > /dev/null 2>&1 || true
	rm -rf "$tmp"
}
trap cleanup EXIT

ark="$tmp/ark"
(cd "$root" && CGO_ENABLED=0 go build -o "$ark" ./cmd/ark)

# serve reads the repository password from the same sources as every command
export ARK_PASSWORD=smoke-repo-password
repo="$tmp/repo"
"$ark" -r "$repo" init > /dev/null

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
base="http://127.0.0.1:$port"
password="smoke-test-password"

ARK_ADMIN_PASSWORD="$password" "$ark" -r "$repo" serve \
	--addr "127.0.0.1:$port" --db "$tmp/ark.db" > "$tmp/serve.log" 2>&1 &
pid=$!

for _ in $(seq 50); do
	if curl -sf "$base/health" > /dev/null 2>&1; then break; fi
	if ! kill -0 "$pid" 2>/dev/null; then break; fi
	sleep 0.2
done
if ! curl -sf "$base/health" | grep -q '"status":"ok"'; then
	echo "ark serve did not come up" >&2
	cat "$tmp/serve.log" >&2
	exit 1
fi
echo "ok: health"

# json <expr over the response as j> reads stdin, prints the expression
json() {
	python3 -c "import json, sys; j = json.load(sys.stdin); print($1)"
}
cookies="$tmp/cookies"
api() {
	curl -sf -b "$cookies" -c "$cookies" -H 'Content-Type: application/json' "$@"
}

api -X POST "$base/api/auth/login" -d "{\"username\":\"admin\",\"password\":\"$password\"}" > /dev/null
csrf=$(api "$base/api/auth/csrf" | json "j['csrf_token']")
echo "ok: login"

docker volume create "$volume" > /dev/null
docker run --rm -v "$volume:/data" busybox:stable sh -c 'echo hello > /data/hello.txt'

job=$(api -H "X-CSRF-Token: $csrf" -X POST "$base/api/jobs" \
	-d "{\"name\":\"smoke\",\"volume_name\":\"$volume\",\"schedule\":\"0 0 * * *\",\"enabled\":false}" \
	| json "j['job']['id']")
api -H "X-CSRF-Token: $csrf" -X POST "$base/api/jobs/$job/run" > /dev/null
echo "ok: job $job started"

status=running
for _ in $(seq 150); do
	status=$(api "$base/api/jobs/$job/history" | json "j[0]['status'] if j else 'running'")
	if [ "$status" != "running" ]; then break; fi
	sleep 0.2
done
if [ "$status" != "success" ]; then
	echo "job ended with status $status" >&2
	api "$base/api/jobs/$job/history" >&2 || true
	cat "$tmp/serve.log" >&2
	exit 1
fi
id=$(api "$base/api/jobs/$job/history" | json "j[0]['archive_id']")
echo "ok: snapshot $id"

api "$base/api/snapshots" | json "[s['id'] for s in j]" | grep -q "$id" \
	|| { echo "snapshot $id missing from GET /api/snapshots" >&2; exit 1; }
"$ark" --json -r "$repo" snapshots | json "[s['id'] for s in j]" | grep -q "$id" \
	|| { echo "snapshot $id missing from ark snapshots --json" >&2; exit 1; }
echo "ok: snapshot listed by the API and the CLI"
