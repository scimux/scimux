#!/bin/sh
#
# Bootstrap installer for scimux: fetch one release binary, verify it,
# put it somewhere you can run it. Nothing else.
#
#   curl -fsSL https://scimux.com/install | sh
#   curl -fsSL https://scimux.com/install -o install.sh   # read before running, if you prefer
#   sh install.sh --dry-run                               # say what it would do, do nothing
#   sh install.sh --version v1.2.3                        # pin a release instead of the latest
#
# This script only has to solve the *first* install. scimux updates
# itself from the burger menu afterwards, from this same release host and
# through the same checksum check, so nothing here ever runs again -- and
# it deliberately installs no service, no cron entry and no shell hook
# that would make it run again.
#
# Three things it will not do, because scimux is a single-user, no-root,
# no-daemon program and an installer behaving otherwise would be lying
# about what it installs:
#
#   - no sudo: the binary lands in ~/.local/bin, a directory you own
#   - no PATH edits: it prints the line to add; your shell rc is yours
#   - no agent CLIs: scimux supervises claude/codex/pi/opencode/grok,
#     it does not install them and never touches their credentials
#
# On integrity, plainly: SHA256SUMS is published by the same release as
# the binary, so comparing against it catches a truncated or corrupted
# download -- not a compromised release, and not a compromised
# scimux.com. Release signing is the actual fix, it is on the v1.0.0
# checklist, and it is not live yet: verify_signature() below is where
# the key goes. Until it holds one, this script is exactly as
# trustworthy as the domain you piped it from. If that is not good
# enough -- and it is a reasonable thing to decide -- build from source
# instead. It is one `go build`.

set -eu

REPO_URL="https://codeberg.org/chrberger/scimux"
API_URL="https://codeberg.org/api/v1/repos/chrberger/scimux"
INSTALL_DIR="${SCIMUX_INSTALL_DIR:-$HOME/.local/bin}"

# Empty until release signing goes live. See verify_signature().
MINISIGN_PUBKEY=""

die() { echo "scimux install: $*" >&2; exit 1; }

usage() {
	echo "usage: install.sh [--dry-run] [--version <tag>]"
	echo "       SCIMUX_INSTALL_DIR=<dir> overrides $HOME/.local/bin"
}

DRY_RUN=0
VERSION="${SCIMUX_VERSION:-}"
while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) DRY_RUN=1 ;;
	--version) [ $# -ge 2 ] || die "--version needs a tag"; VERSION="$2"; shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option: $1" ;;
	esac
	shift
done

# The four names below are exactly the artifacts .forgejo/workflows/release.yml
# builds. freebsd/amd64 is built by CI on purpose but never released -- it
# exists to keep the static-build invariant honest -- so it is named here
# rather than falling into the generic "unsupported" arm, which would read
# like an oversight.
os=$(uname -s)
arch=$(uname -m)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
FreeBSD) die "FreeBSD is built from source on purpose, not released: go build -o scimux ./cmd/scimux" ;;
*) die "no release binary for $os; build from source: $REPO_URL" ;;
esac
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "no release binary for $arch; build from source: $REPO_URL" ;;
esac
asset="scimux-$os-$arch"

# sha256sum is GNU coreutils, shasum ships with macOS. One of the two is
# present on every machine this script supports, and needing neither would
# mean trusting the download.
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "need sha256sum or shasum to verify the download"
fi
command -v curl >/dev/null 2>&1 || die "need curl"

# Release signing is not live yet. When it is, put the public key in
# MINISIGN_PUBKEY above and this becomes the real gate. Until then it says
# so once, rather than printing a reassuring line it has not earned.
verify_signature() {
	if [ -z "$MINISIGN_PUBKEY" ]; then
		echo "  note: release signatures are not live yet; integrity here is HTTPS + SHA256SUMS"
		return 0
	fi
	command -v minisign >/dev/null 2>&1 || die "minisign is needed to verify this release"
	curl -fsSL "$1/SHA256SUMS.minisig" -o "$2/SHA256SUMS.minisig"
	minisign -V -P "$MINISIGN_PUBKEY" -x "$2/SHA256SUMS.minisig" -m "$2/SHA256SUMS" >/dev/null ||
		die "signature verification failed -- do not run the downloaded file"
}

# One read-only API call, parsed with sed because requiring jq to install a
# zero-dependency binary would be a poor joke. Splitting on commas first
# keeps the match anchored to its own field.
if [ -z "$VERSION" ]; then
	VERSION=$(curl -fsSL "$API_URL/releases/latest" | tr ',' '\n' |
		sed -n 's/.*"tag_name":"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$VERSION" ] || die "could not resolve the latest release; try --version <tag>"
fi
base="$REPO_URL/releases/download/$VERSION"

echo "scimux $VERSION ($os/$arch)"
echo "  from $base/$asset"
echo "  to   $INSTALL_DIR/scimux"
if [ "$DRY_RUN" -eq 1 ]; then
	echo "  --dry-run: nothing downloaded, nothing written"
	exit 0
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/scimux-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM

curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
verify_signature "$base" "$tmp"

want=$(sed -n "s/^\([0-9a-f]\{64\}\) [ *]$asset\$/\1/p" "$tmp/SHA256SUMS" | head -n 1)
[ -n "$want" ] || die "SHA256SUMS carries no line for $asset"
got=$(sha256 "$tmp/$asset")
[ "$want" = "$got" ] || die "checksum mismatch for $asset (want $want, got $got)"
echo "  sha256 ok"

# Install by rename inside the target directory: a rename replaces the
# directory entry while a running scimux keeps its own inode, so upgrading
# under a live process neither fails with ETXTBSY nor rewrites a binary
# somebody is executing.
mkdir -p "$INSTALL_DIR"
cp "$tmp/$asset" "$INSTALL_DIR/.scimux.new"
chmod 755 "$INSTALL_DIR/.scimux.new"
mv -f "$INSTALL_DIR/.scimux.new" "$INSTALL_DIR/scimux"

# scimux supervises agents; with none installed it starts fine and has
# nothing to show. That is the real first-run disappointment, so say it
# here -- as a warning, never a failure, because installing in the wrong
# order is allowed.
found=""
for cli in claude codex pi opencode grok; do
	command -v "$cli" >/dev/null 2>&1 && found="$found $cli"
done
echo
echo "installed: $INSTALL_DIR/scimux"
if [ -n "$found" ]; then
	echo "agents found:$found"
else
	echo "no agent CLI found (claude, codex, pi, opencode, grok) -- scimux has"
	echo "nothing to supervise until one is installed and logged in: $REPO_URL#faq"
fi
command -v tmux >/dev/null 2>&1 || echo "tmux not found -- needed for Claude Code sessions only"
case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*) echo "$INSTALL_DIR is not on your PATH; add: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
echo
echo "next: scimux   then open http://127.0.0.1:8787/   then tap +"
