#!/usr/bin/env bash
# Copyright 2026 3eAI Labs Ltd
# SPDX-License-Identifier: AGPL-3.0-only
# verify-chart-package.sh — packaging regression test for deploy/helm/ss7-3eai-gateway.
#
# Asserts that what `helm package` produces is what Arinna will actually pull:
#   1. helm lint passes
#   2. every file under the chart dir is git-tracked — a package a clean clone could
#      not reproduce is not the package CI published
#   3. the SHACL bundle is inside the package, byte-identical to the tree
#      (Arinna Packaging Contract §6: the shape bundle ships INSIDE the chart, so
#      `find -iname '*.ttl'` on the pulled package returns >= 2)
#   4. no nested chart archive
#   5. no x-arinna.secretValue field ships a non-empty default in values.yaml
#      (ADR-036, ARIN-ONB-0024; arinna request 2026-09-12)
#   6. a literal published by chart <= 0.2.0 is rejected at render time
#   7. secrets.existingSecret renders no chart-created Secret and every secretKeyRef
#      points at the supplied name
#
# Usage: scripts/verify-chart-package.sh        exit 0 = PASS, 1 = FAIL
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAME=ss7-3eai-gateway
CHART_DIR="$ROOT/deploy/helm/$NAME"
OUT="$(mktemp -d)"; trap 'rm -rf "$OUT"' EXIT
fail=0
ok() { echo "PASS: $*"; }
ko() { echo "FAIL: $*"; fail=1; }

if helm lint "$CHART_DIR" >/dev/null 2>&1; then ok "helm lint"; else ko "helm lint"; fi

stray="$(cd "$ROOT" && { git ls-files --others --exclude-standard -- "deploy/helm/$NAME"; \
                         git ls-files --others --ignored --exclude-standard -- "deploy/helm/$NAME"; })"
if [ -z "$stray" ]; then ok "every chart file is git-tracked"
else ko "untracked/ignored files under the chart dir (packaged here, absent on a clean clone):"; printf '  %s\n' $stray; fi

helm package "$CHART_DIR" --destination "$OUT" >/dev/null
pkg="$(ls "$OUT"/*.tgz)"
listing="$(tar -tzf "$pkg")"

ttl="$(grep -c '\.ttl$' <<<"$listing" || true)"
if [ "$ttl" -ge 2 ]; then ok "SHACL bundle in package ($ttl .ttl files)"; else ko "expected >= 2 .ttl files in package, found $ttl"; fi
for f in "$NAME.shacl.ttl" "$NAME-ontology.ttl"; do
  if grep -q "^$NAME/shapes/$f\$" <<<"$listing"; then
    tar -m -xzf "$pkg" -C "$OUT" "$NAME/shapes/$f"
    if cmp -s "$OUT/$NAME/shapes/$f" "$CHART_DIR/shapes/$f"; then ok "shapes/$f byte-identical to tree"; else ko "shapes/$f differs from tree"; fi
  else ko "shapes/$f missing from package"; fi
done

nested="$(grep -c '\.tgz$' <<<"$listing" || true)"
if [ "$nested" -eq 0 ]; then ok "no nested chart archive"; else ko "$nested nested .tgz inside the package"; fi

# 5. Same resolution order as Arinna's secretRoleFindings: values.yaml first, the
#    schema `default` keyword only as fallback.
if literal="$(python3 -I - "$CHART_DIR" <<'PY'
import json, sys, yaml
chart = sys.argv[1]
schema = json.load(open(f"{chart}/values.schema.json"))
values = yaml.safe_load(open(f"{chart}/values.yaml")) or {}
bad = []
def walk(node, path):
    if not isinstance(node, dict):
        return
    if (node.get("x-arinna") or {}).get("secretValue"):
        cur = values
        for seg in path:
            cur = cur.get(seg) if isinstance(cur, dict) else None
        if cur is None:
            cur = node.get("default")
        if str(cur if cur is not None else "").strip():
            bad.append("/" + "/".join(path))
    for k, child in (node.get("properties") or {}).items():
        walk(child, path + [k])
walk(schema, [])
print(" ".join(bad))
sys.exit(1 if bad else 0)
PY
)"; then ok "every x-arinna.secretValue default is empty (ADR-036)"
else ko "x-arinna.secretValue field(s) ship a non-empty default (ARIN-ONB-0024): $literal"; fi

if helm template v "$CHART_DIR" --set-string secrets.adminPassword=smoke-admin-override-me >/dev/null 2>&1; then
  ko "a literal published by chart <= 0.2.0 was accepted"
else ok "literals published by chart <= 0.2.0 are rejected"; fi

ext="$(helm template v "$CHART_DIR" --set secrets.create=false --set-string secrets.existingSecret=vfy-secrets)"
if grep -qE '^kind: Secret$' <<<"$ext"; then ko "existingSecret profile renders a chart-created Secret"
else ok "existingSecret profile: no chart-created Secret"; fi
refs="$(awk '/secretKeyRef:/{getline; print $2}' <<<"$ext" | sort -u)"
if [ "$refs" = "vfy-secrets" ]; then ok "every secretKeyRef resolves to secrets.existingSecret"
else ko "secretKeyRef names in existingSecret profile: $(tr '\n' ' ' <<<"$refs")"; fi

if [ "$fail" -eq 0 ]; then echo "verify-chart-package: PASS ($(basename "$pkg"))"; else echo "verify-chart-package: FAIL"; exit 1; fi
