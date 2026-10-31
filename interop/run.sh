#!/usr/bin/env bash
# Builds the backend, starts it on a temporary wiki and runs the JS interop test against it.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'kill "${pid:-}" 2>/dev/null || true; rm -rf "$work"' EXIT

cd "$repo"
go build -o "$work/mkdocsrest" .

mkdir -p "$work/wiki/docs"
printf 'site_name: Interop\n' > "$work/wiki/mkdocs.yml"
printf '# Interop\n\nHello world.\n' > "$work/wiki/docs/index.md"

hash() { printf '%s' "$1" | "$work/mkdocsrest" hash-password; }
# a free port
port="$(node -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')"
cat > "$work/mkdocsrest.yaml" <<YAML
server:
  host: "127.0.0.1"
  port: $port
  auth:
    users:
      - username: alice
        passwordHash: "$(hash alice-pw)"
      - username: bob
        passwordHash: "$(hash bob-pw)"
      - username: carol
        passwordHash: "$(hash carol-pw)"
        role: reader
mkdocs:
  projectPath: "$work/wiki"
YAML

"$work/mkdocsrest" --config "$work/mkdocsrest.yaml" > "$work/backend.log" 2>&1 &
pid=$!
for _ in $(seq 1 50); do
  if curl -fs "http://127.0.0.1:$port/alive/" > /dev/null; then break; fi
  if ! kill -0 "$pid" 2>/dev/null; then cat "$work/backend.log"; exit 1; fi
  sleep 0.2
done

doc_id="$(curl -fs -u alice:alice-pw "http://127.0.0.1:$port/section/" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).documents[0].id))')"

cd "$repo/interop"
status=0
BACKEND="http://127.0.0.1:$port" DOC_ID="$doc_id" node sync-interop.mjs || status=$?

kill "$pid"
wait "$pid" 2>/dev/null || true
if [ "$status" -ne 0 ]; then
  echo "--- backend log ---"
  cat "$work/backend.log"
  exit "$status"
fi

# the backend wrote the merged text to disk on shutdown
grep -q "Bob äöü" "$work/wiki/docs/index.md"
grep -q "Alice 😀" "$work/wiki/docs/index.md"
test -f "$work/wiki/.mkdocseditor/auth.db"
echo "disk content OK"
