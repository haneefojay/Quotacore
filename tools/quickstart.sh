#!/usr/bin/env bash
# Runs the README's quickstart, verbatim, on this machine (IP-15 DoD 1, D16, deployment.md
# section 2), then J-1 step 6: ten consumes against a limit of five answer five 200s and five
# 429 quota_exceeded.
#
# The block is read from the README, not copied here, so a README that rots fails this script. Two
# substitutions are made and no others:
#   - the clone URL becomes this checkout, so the clone runs and clones what is under test;
#   - `quotacore init` becomes `quotacore init --yes --limit 5`, because a script has no terminal
#     to answer init's questions (the IP-14 note in section 2) and step 6 needs a limit of five.
# The block must also be deployment.md section 2's, character for character.
#
# It needs Docker with Compose v2, curl and a free 127.0.0.1:8080. It leaves nothing behind: the
# stack is taken down with its volumes and the clone is removed, pass or fail.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"

# The fenced bash block under a heading, from the line after "```bash" to the closing fence.
block() {
  awk -v heading="$2" '
    $0 ~ "^#+ " heading { inside = 1; next }
    inside && /^#+ / { exit }
    inside && /^```bash$/ { fenced = 1; next }
    fenced && /^```$/ { exit }
    fenced { print }
  ' "$1"
}

readme="$(block "$repo/README.md" "Quickstart")"
guide="$(block "$repo/docs/architecture/deployment.md" "2. The quickstart")"
if [ -z "$readme" ]; then
  echo "quickstart: the README has no bash block under its Quickstart heading" >&2
  exit 1
fi
if [ "$readme" != "$guide" ]; then
  echo "quickstart: the README's block differs from deployment.md section 2's" >&2
  diff <(echo "$readme") <(echo "$guide") >&2 || true
  exit 1
fi
for line in 'git clone https://github.com/haneefojay/Quotacore.git quotacore' 'docker compose exec quotacore quotacore init'; do
  if ! grep -qxF "$line" <<<"$readme"; then
    echo "quickstart: the block has no line '$line', so this script cannot run it as written" >&2
    exit 1
  fi
done

work="$(mktemp -d)"
log="$work/quickstart.log"
cleanup() {
  status=$?
  if [ -d "$work/quotacore" ]; then
    if [ "$status" -ne 0 ]; then
      (cd "$work/quotacore" && docker compose logs --no-color >&2) || true
    fi
    (cd "$work/quotacore" && docker compose down -v --remove-orphans >/dev/null 2>&1) || true
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT

script="$(sed \
  -e "s#^git clone https://github.com/haneefojay/Quotacore.git quotacore\$#git clone --quiet file://$repo quotacore#" \
  -e 's#^docker compose exec quotacore quotacore init$#docker compose exec quotacore quotacore init --yes --limit 5#' \
  <<<"$readme")"

echo "quickstart: running the README's block in $work"
(cd "$work" && bash -euo pipefail -c "$script") | tee "$log"

key="$(sed -n 's/^  key      \([^ ]*\) .*/\1/p' "$log")"
if [ -z "$key" ]; then
  echo "quickstart: init printed no runtime key" >&2
  exit 1
fi

ok=0 refused=0
for i in $(seq 1 10); do
  code="$(curl -sS -o "$work/answer" -w '%{http_code}' -X POST http://127.0.0.1:8080/v1/consume \
    -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: quickstart-$i" \
    -d '{"tenant_external_id":"local-dev","feature_key":"llm.tokens.output","amount":1}')"
  case "$code" in
    200) ok=$((ok + 1)) ;;
    429) grep -q '"quota_exceeded"' "$work/answer" && refused=$((refused + 1)) ;;
  esac
  echo "consume $i: $code $(cat "$work/answer")"
done
if [ "$ok" -ne 5 ] || [ "$refused" -ne 5 ]; then
  echo "quickstart: J-1 step 6 gave $ok 200s and $refused 429 quota_exceeded; want five of each" >&2
  exit 1
fi
echo "quickstart: PASS - the README's block reached a working service, and five of ten consumes were refused at the limit"
