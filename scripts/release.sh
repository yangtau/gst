#!/bin/sh
# Release gst VERSION: set the version in internal/cli/root.go, run the unit
# tests and vet, commit, and tag. It pushes nothing.
set -eu

fail() { echo "error: $*" >&2; exit 1; }

v=${1:-}
echo "$v" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || {
	echo "error: the version must be X.Y.Z, not '$v'" >&2
	echo "usage: make release V=X.Y.Z" >&2
	exit 2
}
cd "$(git rev-parse --show-toplevel)"
[ "$(git branch --show-current)" = main ] || fail "release from main"
[ -z "$(git status --porcelain)" ] || fail "the working tree has changes; commit them first"
! git rev-parse -q --verify "refs/tags/v$v" >/dev/null || fail "tag v$v exists"

file=internal/cli/root.go
old=$(sed -n 's/^const version = "\(.*\)"$/\1/p' "$file")
[ -n "$old" ] || fail "no 'const version' in $file"
[ "$(printf '%s\n%s\n' "$old" "$v" | sort -V | tail -n 1)" = "$v" ] && [ "$old" != "$v" ] ||
	fail "$v is not newer than $old"

sed "s/^const version = \".*\"$/const version = \"$v\"/" "$file" >"$file.tmp"
mv "$file.tmp" "$file"
trap 'git checkout -- "$file"' EXIT
go test ./...
go vet ./...
go vet -tags integration ./integration
trap - EXIT

git commit -q -m "Release gst $v" -- "$file"
git tag -a "v$v" -m "gst $v"
echo "released gst $v ($old before); push with: git push origin main v$v"
