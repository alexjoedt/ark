#!/usr/bin/env bash
# release.sh - Tag ark, arkdocker and cmd/ark in lockstep
# Usage: ./scripts/release.sh [--dry-run] [--push] <X.Y.Z | X.Y.Z-rc.N>
#
# Order: tag vX.Y.Z, bump arkdocker's require, tidy, commit, tag arkdocker/vX.Y.Z,
# then cmd/ark with both requires, then examples (untagged). Each tidy resolves the
# previous tag from the remote, so a real run pushes main and the tag after each step.
# Existing tags are skipped: after a failure, fix the cause and rerun the same command.

set -Eeuo pipefail

readonly ROOT_MOD=github.com/alexjoedt/ark
readonly DOCKER_MOD=$ROOT_MOD/arkdocker

usage() {
    echo "usage: $0 [--dry-run] [--push] <X.Y.Z | X.Y.Z-rc.N>" >&2
    exit 2
}

die() {
    echo "release: $*" >&2
    exit 1
}

dry_run=0
push=0
version=
for arg in "$@"; do
    case $arg in
        --dry-run) dry_run=1 ;;
        --push) push=1 ;;
        -*) usage ;;
        *) [[ -z $version ]] || usage; version=$arg ;;
    esac
done
[[ -n $version ]] || usage
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] ||
    die "version must be X.Y.Z or X.Y.Z-rc.N, got '$version'"
[[ $version =~ ^[01]\. ]] || die "major version must be 0 or 1, the module paths carry no /vN suffix"

cd "$(git rev-parse --show-toplevel)"

branch=$(git symbolic-ref --short -q HEAD || true)
[[ $branch == main ]] || die "must run on main, on '${branch:-detached HEAD}'"
[[ -z $(git status --porcelain --untracked-files=no) ]] || die "working tree is dirty"
if (( !dry_run && !push )); then
    die "a real run needs --push, each tidy resolves the previous tag from the remote; preview with --dry-run"
fi

# Fetch the tags just pushed straight from git, the proxy may not know them yet.
goprivate=$ROOT_MOD
if current=$(go env GOPRIVATE) && [[ -n $current ]]; then
    goprivate+=",$current"
fi

tags=("v$version" "arkdocker/v$version" "cmd/ark/v$version")
resume="$0 $version"
if (( push )); then
    resume="$0 --push $version"
fi

on_error() {
    echo >&2
    echo "release: failed, see the last command above" >&2
    local remote= t
    if (( push )); then
        remote=$(git ls-remote --tags origin 2>/dev/null || true)
    fi
    for t in "${tags[@]}"; do
        local state=missing
        if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then
            state=local
            [[ $remote == *"refs/tags/$t"* ]] && state="local, pushed"
        fi
        echo "  $t: $state" >&2
    done
    echo "discard uncommitted go.mod/go.sum changes (git checkout -- <files>), fix the cause, then resume:" >&2
    echo "  $resume" >&2
}
trap on_error ERR

show() {
    local out= a
    for a in "$@"; do
        [[ $a =~ [[:space:]] ]] && a="'$a'"
        out+=" $a"
    done
    echo "+$out"
}

run() {
    show "$@"
    if (( !dry_run )); then
        "$@"
    fi
}

tag() {
    local name=$1 msg=$2
    if git rev-parse -q --verify "refs/tags/$name" >/dev/null; then
        echo "tag $name exists, skipping"
    else
        run git tag -a "$name" -m "$msg"
    fi
    if (( push )); then
        run git push --atomic origin main "$name"
    fi
}

# bump <dir> <commit message> <module>...: require each module at the release
# version, tidy outside the workspace and commit when anything changed.
bump() {
    local dir=$1 msg=$2 m
    shift 2
    local req=()
    for m in "$@"; do
        req+=("-require=$m@v$version")
    done
    run go -C "$dir" mod edit "${req[@]}"
    run env GOWORK=off GOPRIVATE="$goprivate" go -C "$dir" mod tidy
    run git add -- "$dir/go.mod" "$dir/go.sum"
    if (( dry_run )) || ! git diff --cached --quiet; then
        run git commit -q -m "$msg"
    fi
}

tag "v$version" "ark v$version"

bump arkdocker "chore(arkdocker): require ark v$version" "$ROOT_MOD"
tag "arkdocker/v$version" "arkdocker v$version"

bump cmd/ark "chore(cmd/ark): require ark and arkdocker v$version" "$ROOT_MOD" "$DOCKER_MOD"
tag "cmd/ark/v$version" "cmd/ark v$version"

bump examples "chore(examples): require ark and arkdocker v$version" "$ROOT_MOD" "$DOCKER_MOD"
if (( push )); then
    run git push origin main
fi

if (( dry_run )); then
    echo "dry run, nothing tagged, committed or pushed"
else
    echo "released $version: ${tags[*]}"
fi
