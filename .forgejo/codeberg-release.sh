#!/bin/bash
# Adopted from https://github.com/shtorm-7/sing-box-extended/blob/extended/codeberg-release.sh
set -euo pipefail

# Usage: ./codeberg-release.sh [--replace] [--draft] [--prerelease] [-p N] TAG PATH

OWNER="chrberger"
REPO="scimux"
# Overridable so the upload path can be exercised against a stub in tests
# (internal/app/release_upload_test.go). Releases never set it.
API="${CODEBERG_API:-https://codeberg.org/api/v1}"
TOKEN="${CODEBERG_TOKEN:?Set CODEBERG_TOKEN}"

REPLACE=false
DRAFT=false
PRERELEASE=false
PARALLEL=1

while [[ $# -gt 2 ]]; do
  case "$1" in
    --replace) REPLACE=true; shift ;;
    --draft) DRAFT=true; shift ;;
    --prerelease) PRERELEASE=true; shift ;;
    -p) PARALLEL="$2"; shift 2 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

echo "1: $1"
echo "2: $2"
TAG="$1"
DIR="$2"

if [[ ! -d "$DIR" ]]; then
  echo "Error: $DIR is not a directory"
  exit 1
fi

RELEASE_URL="$API/repos/$OWNER/$REPO/releases"
# Plain -s on purpose: a tag with no release yet is answered 404, and that is
# the ordinary first-publish case, not an error. Every *state-changing* call
# below uses --fail-with-body instead.
RELEASE_ID=$(curl -s -H "Authorization: token $TOKEN" "$RELEASE_URL/tags/$TAG" | jq -r '.id // empty')

if [[ -n "$RELEASE_ID" && "$REPLACE" == "true" ]]; then
  curl -sS --fail-with-body -H "Authorization: token $TOKEN" "$RELEASE_URL/$RELEASE_ID/assets" | \
    jq -r '.[].id' | while read -r aid; do
      curl -sS --fail-with-body -X DELETE -H "Authorization: token $TOKEN" "$RELEASE_URL/$RELEASE_ID/assets/$aid" >/dev/null
    done
  curl -sS --fail-with-body -X PATCH -H "Authorization: token $TOKEN" -H "Content-Type: application/json" \
    "$RELEASE_URL/$RELEASE_ID" \
    -d "{\"draft\":$DRAFT,\"prerelease\":$PRERELEASE}" >/dev/null
elif [[ -z "$RELEASE_ID" ]]; then
  RELEASE_ID=$(curl -sS --fail-with-body -X POST -H "Authorization: token $TOKEN" -H "Content-Type: application/json" \
    "$RELEASE_URL" \
    -d "{\"tag_name\":\"$TAG\",\"name\":\"$TAG\",\"draft\":$DRAFT,\"prerelease\":$PRERELEASE}" | jq -r '.id')
fi

if [[ -z "$RELEASE_ID" || "$RELEASE_ID" == "null" ]]; then
  echo "Error: failed to get or create release"
  exit 1
fi

echo "Release ID: $RELEASE_ID"
echo "Uploading files from $DIR (parallelism: $PARALLEL)..."

export TOKEN RELEASE_URL RELEASE_ID

# --fail-with-body is what makes this a check at all: plain `curl -s` exits 0
# on 403 and 500, so a token that had lost a scope produced a ✓ and a release
# missing that asset. -with-body keeps the server's explanation, which is the
# only thing that says *why*.
#
# The `return 1` matters just as much: the old else-branch ended in `echo`, so
# the function returned 0 even when it had just printed ✗, and xargs saw a
# clean run.
upload() {
  local file="$1"
  local name body
  name=$(basename "$file")
  if body=$(curl -sS --fail-with-body -X POST \
    -H "Authorization: token $TOKEN" \
    "$RELEASE_URL/$RELEASE_ID/assets?name=$name" \
    -F "attachment=@$file" 2>&1); then
    echo "✓ $name"
  else
    echo "✗ $name: $body" >&2
    return 1
  fi
}
export -f upload

if ! find "$DIR" -maxdepth 1 -type f -print0 | xargs -0 -P "$PARALLEL" -I {} bash -c 'upload "$@"' _ {}; then
  echo "Error: at least one asset failed to upload" >&2
  exit 1
fi

# Ask the release what it actually holds. Every ✓ above is the server's claim
# about one request; this is the artifact users will download. They can differ
# — a concurrent --replace sweep, a partial write, an upload the API accepted
# and dropped — and only this one is worth publishing on.
PUBLISHED=$(curl -sS --fail-with-body -H "Authorization: token $TOKEN" \
  "$RELEASE_URL/$RELEASE_ID/assets" | jq -r '.[].name')
MISSING=0
while IFS= read -r -d '' file; do
  name=$(basename "$file")
  if ! printf '%s\n' "$PUBLISHED" | grep -Fxq "$name"; then
    echo "✗ $name uploaded but is not in the published release" >&2
    MISSING=1
  fi
done < <(find "$DIR" -maxdepth 1 -type f -print0)
if [[ $MISSING -ne 0 ]]; then
  echo "Error: the published release is incomplete" >&2
  exit 1
fi

echo "Done."
