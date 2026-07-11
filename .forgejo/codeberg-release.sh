#!/bin/bash
# Adopted from https://github.com/shtorm-7/sing-box-extended/blob/extended/codeberg-release.sh
set -euo pipefail

# Usage: ./codeberg-release.sh [--replace] [--draft] [--prerelease] [-p N] TAG PATH

OWNER="chrberger"
REPO="scimux"
API="https://codeberg.org/api/v1"
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

TAG="$1"
DIR="$2"

if [[ ! -d "$DIR" ]]; then
  echo "Error: $DIR is not a directory"
  exit 1
fi

RELEASE_URL="$API/repos/$OWNER/$REPO/releases"
RELEASE_ID=$(curl -s -H "Authorization: token $TOKEN" "$RELEASE_URL/tags/$TAG" | jq -r '.id // empty')

if [[ -n "$RELEASE_ID" && "$REPLACE" == "true" ]]; then
  curl -s -H "Authorization: token $TOKEN" "$RELEASE_URL/$RELEASE_ID/assets" | \
    jq -r '.[].id' | while read -r aid; do
      curl -s -X DELETE -H "Authorization: token $TOKEN" "$RELEASE_URL/$RELEASE_ID/assets/$aid" >/dev/null
    done
  curl -s -X PATCH -H "Authorization: token $TOKEN" -H "Content-Type: application/json" \
    "$RELEASE_URL/$RELEASE_ID" \
    -d "{\"draft\":$DRAFT,\"prerelease\":$PRERELEASE}" >/dev/null
elif [[ -z "$RELEASE_ID" ]]; then
  RELEASE_ID=$(curl -s -X POST -H "Authorization: token $TOKEN" -H "Content-Type: application/json" \
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

upload() {
  local file="$1"
  local name
  name=$(basename "$file")
  if curl -s -X POST \
    -H "Authorization: token $TOKEN" \
    "$RELEASE_URL/$RELEASE_ID/assets?name=$name" \
    -F "attachment=@$file" >/dev/null; then
    echo "✓ $name"
  else
    echo "✗ $name"
  fi
}
export -f upload

find "$DIR" -maxdepth 1 -type f -print0 | xargs -0 -P "$PARALLEL" -I {} bash -c 'upload "$@"' _ {}

echo "Done."
