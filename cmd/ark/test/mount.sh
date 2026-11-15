#!/usr/bin/env sh
# Mounts a local repository with two snapshots and checks the mount against
# ark restore: every snapshot's tree, mode, size and mtime, a read of a large
# file at a random offset, and that SIGINT unmounts and leaves no lock behind.
# Linux only; needs go, fuse3 (fusermount3) and /dev/fuse. Run from anywhere.
set -eu

root=$(cd "$(dirname "$0")/../../.." && pwd)
tmp=$(mktemp -d)
mnt="$tmp/mnt"
pid=
cleanup() {
	if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; fi
	fusermount3 -u "$mnt" 2>/dev/null || true
	rm -rf "$tmp"
}
trap cleanup EXIT

ark="$tmp/ark"
(cd "$root" && CGO_ENABLED=0 go build -o "$ark" ./cmd/ark)

unset ARK_REPOSITORY ARK_INSECURE_NO_PASSWORD ARK_PASSWORD_FILE ARK_PASSWORD_COMMAND
export ARK_PASSWORD=mount-password
repo="$tmp/repo"
src="$tmp/src"

mkdir -p "$src/dir/sub" "$mnt"
printf 'hello\n' > "$src/a.txt"
printf 'secret\n' > "$src/dir/private"
chmod 600 "$src/dir/private"
head -c 33554432 /dev/urandom > "$src/dir/big.bin"
ln -s ../a.txt "$src/dir/link"
ln "$src/a.txt" "$src/dir/hardlink"
: > "$src/dir/sub/empty"
chmod 750 "$src/dir/sub"
touch -d '2026-01-02 03:04:05.123456789' "$src/a.txt" "$src/dir/sub"

"$ark" -r "$repo" init > /dev/null
"$ark" -q -r "$repo" backup "$src" > /dev/null

printf 'changed\n' >> "$src/dir/private"
rm "$src/dir/sub/empty"
printf 'new\n' > "$src/new.txt"
"$ark" -q -r "$repo" backup "$src" > /dev/null

"$ark" -r "$repo" mount "$mnt" &
pid=$!
i=0
until mountpoint -q "$mnt" && [ -d "$mnt/ids" ]; do
	i=$((i + 1))
	if [ "$i" -gt 300 ] || ! kill -0 "$pid" 2>/dev/null; then
		echo "FAIL: mount did not come up" >&2
		exit 1
	fi
	sleep 0.1
done

# meta prints path, type, mode, size (regular files only) and mtime of every
# entry below $1, sorted by path. The mtime of the root and of symlinks is left
# out: ark restore sets neither.
meta() {
	(cd "$1" && find . \( -path . -o -type l \) -printf '%p %y %m\n' \
		-o -type f -printf '%p %y %m %s %T@\n' -o -printf '%p %y %m %T@\n' | sort)
}

fail=0
bad() {
	echo "FAIL: $*" >&2
	fail=$((fail + 1))
}

n=0
for id in $(cd "$mnt/ids" && ls); do
	n=$((n + 1))
	before=$fail
	out="$tmp/restore-$id"
	"$ark" -q -r "$repo" restore "$id" "$out" > /dev/null
	snap="$mnt/ids/$id"
	diff -r "$snap" "$out" || bad "snapshot $id: mounted tree differs from the restore"
	meta "$snap" > "$tmp/meta-mount"
	meta "$out" > "$tmp/meta-restore"
	diff "$tmp/meta-mount" "$tmp/meta-restore" || bad "snapshot $id: stat differs between mount and restore"

	size=$(stat -c %s "$out/dir/big.bin")
	off=$(($(od -An -N4 -tu4 /dev/urandom) % (size - 1048576)))
	dd if="$snap/dir/big.bin" of="$tmp/part-mount" bs=65536 skip="$off" count=1048576 \
		iflag=skip_bytes,count_bytes status=none
	dd if="$out/dir/big.bin" of="$tmp/part-restore" bs=65536 skip="$off" count=1048576 \
		iflag=skip_bytes,count_bytes status=none
	cmp "$tmp/part-mount" "$tmp/part-restore" || bad "snapshot $id: 1 MiB read at offset $off differs"
	if [ "$fail" = "$before" ]; then echo "ok: snapshot $id matches its restore, read at offset $off"; fi
done
[ "$n" = 2 ] || bad "ids/ lists $n snapshots, want 2"

# A background job of a non-interactive shell starts with SIGINT ignored; the
# signal.NotifyContext in main re-enables it.
before=$fail
kill -INT "$pid"
set +e
wait "$pid"
code=$?
set -e
pid=
[ "$code" = 0 ] || bad "ark mount exited $code after SIGINT, want 0"
if mountpoint -q "$mnt"; then bad "$mnt is still mounted after SIGINT"; fi
locks=$(ls -A "$repo/locks")
[ -z "$locks" ] || bad "locks left after unmount: $locks"
if [ "$fail" = "$before" ]; then echo "ok: SIGINT unmounts and leaves no lock"; fi
[ "$fail" = 0 ]
