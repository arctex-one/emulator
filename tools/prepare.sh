#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
export GOWORK=off
sdk=github.com/arctex-one/client-go
version=$(go mod edit -json | jq -er --arg sdk "$sdk" '.Require[] | select(.Path == $sdk) | .Version')
sdk_output=.generated/arctex-client-go
go run "$sdk/tools/prepare@$version" "$sdk_output"

lock="$sdk_output/schema.lock.json"
url=$(jq -er '.url | select(startswith("https://docs.arctex.one/contracts/"))' "$lock")
sha=$(jq -er '.sha256 | select(test("^[a-f0-9]{64}$"))' "$lock")
revision=$(jq -er '.version' "$lock")
schema=$(mktemp internal/contract/.openapi.XXXXXX)
trap 'rm -f "$schema"' EXIT
trap 'exit 1' HUP INT TERM
curl --proto '=https' --fail --silent --show-error --max-time 30 --max-filesize 4194304 -o "$schema" "$url"
printf '%s  %s\n' "$sha" "$schema" | sha256sum -c -
jq -e --arg revision "$revision" '.info.version == $revision' "$schema" >/dev/null
mv "$schema" internal/contract/openapi.json
