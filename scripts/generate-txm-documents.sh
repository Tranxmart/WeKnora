#!/usr/bin/env bash
# Regenerate locally vendored Document Service bindings; no private Go module is used.
# Canonical source: Tranxmart/tranxmart @ dbdda22991d48744f991d42906ba71d217cbe7f7
# Sources are retained unmodified under internal/txmdocuments/proto and verified
# against SHA256SUMS. Updating them requires fetching that exact revision via gh.
# Requires protoc 33.5 (https://github.com/protocolbuffers/protobuf/releases/tag/v33.5).
set -euo pipefail
cd "$(dirname "$0")/.."
revision=dbdda22991d48744f991d42906ba71d217cbe7f7
source_root=internal/txmdocuments/proto
output_root=internal/txmdocuments/gen
# Optional authenticated source refresh; ordinary regeneration is offline with
# respect to the private repository and uses the verified checked-in sources.
if [[ "${1:-}" == --fetch-sources ]]; then
  while read -r digest source; do
    gh api -H 'Accept: application/vnd.github.raw+json' \
      "repos/Tranxmart/tranxmart/contents/$source?ref=$revision" > "$source_root/$source"
  done < "$source_root/SHA256SUMS"
elif [[ $# -ne 0 ]]; then
  echo 'Usage: scripts/generate-txm-documents.sh [--fetch-sources]' >&2
  exit 1
fi
protoc_bin=${PROTOC:-protoc}
if [[ "$("$protoc_bin" --version)" != "libprotoc 33.5" ]]; then
  echo 'protoc 33.5 is required (set PROTOC to its binary).' >&2
  exit 1
fi
(cd "$source_root" && sha256sum --check SHA256SUMS)
tool_dir=$(mktemp -d)
trap 'rm -rf "$tool_dir"' EXIT
if [[ -n "${PROTOC_GEN_GO:-}" ]]; then
  go_plugin=$PROTOC_GEN_GO
else
  GOBIN="$tool_dir" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
  go_plugin=$tool_dir/protoc-gen-go
fi
if [[ -n "${PROTOC_GEN_CONNECT_GO:-}" ]]; then
  connect_plugin=$PROTOC_GEN_CONNECT_GO
else
  GOBIN="$tool_dir" go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.18.1
  connect_plugin=$tool_dir/protoc-gen-connect-go
fi
[[ "$("$go_plugin" --version)" == 'protoc-gen-go v1.36.11' ]]
[[ "$("$connect_plugin" --version)" == '1.18.1' ]]
mapfile -t sources < <(cd "$source_root" && find txm -name '*.proto' -type f | LC_ALL=C sort)
options=(--go_opt=module=github.com/Tencent/WeKnora/internal/txmdocuments/gen --connect-go_opt=module=github.com/Tencent/WeKnora/internal/txmdocuments/gen)
for source in "${sources[@]}"; do
  package=$(sed -n 's/^option go_package = ".*\/\([^/]*\)";/\1/p' "$source_root/$source")
  mapped="github.com/Tencent/WeKnora/internal/txmdocuments/gen/$package"
  options+=("--go_opt=M$source=$mapped" "--connect-go_opt=M$source=$mapped")
done
mkdir -p "$output_root"
includes=(-I "$source_root")
if [[ -n "${PROTOC_INCLUDE:-}" ]]; then includes+=(-I "$PROTOC_INCLUDE"); fi
"$protoc_bin" "${includes[@]}" \
  "--plugin=protoc-gen-go=$go_plugin" "--plugin=protoc-gen-connect-go=$connect_plugin" \
  "--go_out=$output_root" "--connect-go_out=$output_root" "${options[@]}" "${sources[@]}"
