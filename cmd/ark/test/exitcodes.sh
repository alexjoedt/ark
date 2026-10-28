#!/usr/bin/env sh
# Drives the built binary against a temp local repository and checks the
# exit codes ark promises: 2 usage, 3 locked, 4 not found, 5 verify failed,
# 6 wrong password.
# Each case runs twice, plain and with --json, where stdout must carry the
# error shape with the matching code; verify and check report through their
# result object instead. Run from anywhere; needs go and python3.
set -eu

root=$(cd "$(dirname "$0")/../../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

ark="$tmp/ark"
(cd "$root" && CGO_ENABLED=0 go build -o "$ark" ./cmd/ark)

export ARK_PASSWORD=exitcodes-password
printf 'wrong\n' > "$tmp/wrong"

repo="$tmp/repo"
src="$tmp/src"
mkdir -p "$src"
printf 'hello' > "$src/a.txt"
"$ark" -r "$repo" init > /dev/null
id=$("$ark" --json -r "$repo" backup "$src" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

fail=0

# expect <code> <python check on the --json stdout as j> <args...>
expect() {
	want=$1; check=$2; shift 2
	set +e
	"$ark" "$@" > /dev/null 2>&1
	got=$?
	out=$("$ark" --json "$@" 2>/dev/null)
	gotj=$?
	set -e
	if [ "$got" != "$want" ] || [ "$gotj" != "$want" ]; then
		echo "FAIL: ark $*: exit $got, json exit $gotj, want $want" >&2
		fail=1
		return
	fi
	if ! printf '%s' "$out" | python3 -c "
import json, sys
j = json.load(sys.stdin)
assert $check, j
"; then
		echo "FAIL: ark --json $*: stdout does not satisfy $check: $out" >&2
		fail=1
		return
	fi
	echo "ok: ark $* -> $want"
}
usage="j['error']['code'] == 'usage' and j['error']['message']"
locked="j['error']['code'] == 'locked' and j['error']['message']"
not_found="j['error']['code'] == 'not_found' and j['error']['message']"
wrong_password="j['error']['code'] == 'wrong_password' and j['error']['message']"

expect 2 "$usage" snapshots
expect 2 "$usage" -r "$repo" inspect
expect 2 "$usage" -r "$repo" restore --bogus "$id" "$tmp/out"
expect 4 "$not_found" -r "$repo" inspect nope
expect 4 "$not_found" -r "$repo" verify nope
expect 6 "$wrong_password" --password-file "$tmp/wrong" -r "$repo" snapshots
expect 2 "$usage" --insecure-no-password -r "$repo" snapshots

# an import stalled on an empty fifo holds a shared lock, prune needs the
# exclusive one; locks are sealed, so a live process has to hold it
mkfifo "$tmp/fifo"
"$ark" -q -r "$repo" import - < "$tmp/fifo" > /dev/null 2>&1 &
held=$!
exec 3> "$tmp/fifo"
for _ in $(seq 50); do
	if [ -n "$(ls "$repo/locks" 2>/dev/null)" ]; then break; fi
	sleep 0.1
done
sleep 0.5 # past the lock settle delay of the import
expect 3 "$locked" -r "$repo" prune --yes
exec 3>&-
wait "$held" || true

# a corrupted chunk fails verification
for c in "$repo"/chunks/*/*/*; do printf 'garbage' > "$c"; done
expect 5 "j['ok'] is False and j['corrupt_chunks']" -r "$repo" verify "$id"
expect 5 "j['errors'] and j['snapshots_checked'] == 1" -r "$repo" check

exit $fail
