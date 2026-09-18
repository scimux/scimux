#!/bin/bash
set -euo pipefail

EXPECTED_REPOSITORY="scimux/scimux"
REPOSITORY="${GITHUB_REPOSITORY:?Set GITHUB_REPOSITORY}"
API="${GITHUB_API_URL:-https://api.github.com}"
UPLOAD_API="${GITHUB_UPLOAD_URL:-https://uploads.github.com}"
TOKEN="${GITHUB_RELEASE_TOKEN:?Set GITHUB_RELEASE_TOKEN}"
unset GITHUB_RELEASE_TOKEN
TAG="${1:?usage: github-release.sh TAG DIRECTORY}"
DIR="${2:?usage: github-release.sh TAG DIRECTORY}"

[[ "$REPOSITORY" == "$EXPECTED_REPOSITORY" ]] || { echo "refusing repository $REPOSITORY" >&2; exit 1; }
[[ "$TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
  echo "not an exact vMAJOR.MINOR.PATCH tag: $TAG" >&2
  exit 1
}
[[ -d "$DIR" ]] || { echo "release directory does not exist" >&2; exit 1; }

auth_curl() {
  {
    printf 'header = "Authorization: Bearer %s"\n' "$TOKEN"
    printf 'header = "Accept: application/vnd.github+json"\n'
    printf 'header = "X-GitHub-Api-Version: 2022-11-28"\n'
  } | curl -q --config - "$@"
}

encoded_tag="$(jq -rn --arg value "$TAG" '$value|@uri')"
RELEASE_ID="$(auth_curl -sS --fail-with-body "$API/repos/$REPOSITORY/releases/tags/$encoded_tag" | jq -er '.id')"
[[ "$RELEASE_ID" =~ ^[1-9][0-9]*$ ]] || { echo "invalid release id" >&2; exit 1; }

for file in "$DIR"/*; do
  [[ -f "$file" && ! -L "$file" ]] || { echo "refusing non-file release entry" >&2; exit 1; }
  name="$(basename "$file")"
  encoded_name="$(jq -rn --arg value "$name" '$value|@uri')"
  if ! auth_curl -sS --fail-with-body -X POST -H "Content-Type: application/octet-stream" \
    --data-binary "@$file" \
    "$UPLOAD_API/repos/$REPOSITORY/releases/$RELEASE_ID/assets?name=$encoded_name" >/dev/null; then
    echo "failed to upload $name" >&2
    exit 1
  fi
done

PUBLISHED="$(auth_curl -sS --fail-with-body "$API/repos/$REPOSITORY/releases/$RELEASE_ID/assets" | jq -r '.[].name')"
for file in "$DIR"/*; do
  name="$(basename "$file")"
  printf '%s\n' "$PUBLISHED" | grep -Fxq "$name" || {
    echo "$name missing from published release" >&2
    exit 1
  }
done
