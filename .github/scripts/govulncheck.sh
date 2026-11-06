#!/usr/bin/env bash
# Runs govulncheck and fails on every vulnerability that the code calls, except those in
# .github/govulncheck-ignore.txt. govulncheck itself has no way to ignore one.
set -euo pipefail

govulncheck="go run golang.org/x/vuln/cmd/govulncheck@v1.9.0"
ignore_file="$(dirname "$0")/../govulncheck-ignore.txt"

json="$(mktemp)"
trap 'rm -f "$json"' EXIT
$govulncheck -format json ./... > "$json"

# a finding whose trace starts at a function is called by the code
called="$(jq -r 'select(.finding) | select(.finding.trace[0].function) | .finding.osv' "$json" | sort -u)"
ignored="$(grep -v '^#' "$ignore_file" | awk 'NF {print $1}' | sort -u)"

for id in $(comm -12 <(echo "$called") <(echo "$ignored")); do
  echo "::warning::Ignoring $id, see .github/govulncheck-ignore.txt"
done
for id in $(comm -13 <(echo "$called") <(echo "$ignored")); do
  echo "::warning::$id in .github/govulncheck-ignore.txt is no longer reported, remove it"
done

remaining="$(comm -23 <(echo "$called") <(echo "$ignored"))"
if [ -n "$remaining" ]; then
  echo "Vulnerabilities that the code calls:"
  echo "$remaining"
  # the readable report with the call stacks
  $govulncheck ./... || true
  exit 1
fi
echo "No vulnerabilities that the code calls, apart from the ignored ones"
