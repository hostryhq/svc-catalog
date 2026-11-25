#!/usr/bin/env bash
# Regenerates internal/gen from the api-specs checkout (default ../api-specs-p2 while the Phase 2 contracts are
# on feat/phase2-specs; override API_SPECS_DIR). Uses the oapi-codegen version pinned by api-specs (its Makefile)
# and records the spec commit in internal/gen/SPEC_VERSION.
set -euo pipefail
cd "$(dirname "$0")/.."
SPECS="${API_SPECS_DIR:-../api-specs-p2}"
OAPI="${OAPI_CODEGEN:-$SPECS/.tools/bin/oapi-codegen}"
[ -x "$OAPI" ] || OAPI="../api-specs/.tools/bin/oapi-codegen"  # tools are installed once, in the main api-specs checkout (make tools)
REDOCLY_VERSION=2.83.0
mkdir -p .tmp internal/gen
npx --yes "@redocly/cli@${REDOCLY_VERSION}" bundle "$SPECS/openapi/catalog/v1/openapi.yaml" -o .tmp/catalog.yaml >/dev/null
"$OAPI" -generate types,std-http-server,client -package gen -o internal/gen/catalog.gen.go .tmp/catalog.yaml
go run ./tools/gen -spec "$SPECS/openapi/catalog/v1/openapi.yaml" -permissions "$SPECS/common/permissions.yaml" -out internal/gen
{
  echo "api-specs commit: $(cd "$SPECS" && git rev-parse HEAD)"
  echo "oapi-codegen: $("$OAPI" --version | tail -1)"
} > internal/gen/SPEC_VERSION
echo "generated from $(head -1 internal/gen/SPEC_VERSION)"
