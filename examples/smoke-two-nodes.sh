#!/usr/bin/env bash
# Hermetic local smoke test. Requires bash, Python 3, and Go (unless both
# MEDIATRIX_BIN and MEDIATRIXD_BIN point to prebuilt executables).
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP=$(mktemp -d)
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "$ROOT"
CLI=${MEDIATRIX_BIN:-$TMP/mediatrix}
DAEMON=${MEDIATRIXD_BIN:-$TMP/mediatrixd}
if [[ -z ${MEDIATRIX_BIN:-} ]]; then go build -o "$CLI" ./cmd/mediatrix; fi
if [[ -z ${MEDIATRIXD_BIN:-} ]]; then go build -o "$DAEMON" ./cmd/mediatrixd; fi
export MEDIATRIX_API_TOKEN
MEDIATRIX_API_TOKEN=$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')
python3 - "$TMP" <<'PY'
import json, os, sys
root = sys.argv[1]
for name in ("a", "b"):
    config = {"data_dir": os.path.join(root, name), "api_addr": "127.0.0.1:0",
              "network": "local-smoke", "listen": ["/ip4/127.0.0.1/tcp/0"],
              "allowed_peers": [], "bootstrap": [], "dht_server": True}
    with open(os.path.join(root, name + ".json"), "w") as f:
        json.dump(config, f)
PY
start_node() {
  local name=$1
  : > "$TMP/$name.log"
  "$DAEMON" --config "$TMP/$name.json" > "$TMP/$name.log" 2>&1 &
  NODE_PID=$!
  pids+=("$NODE_PID")
  for ((i=0; i<600; i++)); do
    if ! kill -0 "$NODE_PID" 2>/dev/null; then cat "$TMP/$name.log" >&2; return 1; fi
    API=$(python3 - "$TMP/$name.log" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    try:
        entry = json.loads(line)
    except ValueError:
        continue
    if entry.get("msg") == "daemon started":
        print("http://" + entry["api_addr"])
        break
PY
)
    if [[ -n $API ]] && "$CLI" --api "$API" node > "$TMP/$name-info.json" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  cat "$TMP/$name.log" >&2
  echo "node startup timed out" >&2
  return 1
}
start_node a; A_PID=$NODE_PID
start_node b; B_PID=$NODE_PID
kill "$A_PID" "$B_PID"
wait "$A_PID"; wait "$B_PID"
pids=()
python3 - "$TMP" <<'PY'
import json, os, sys
root = sys.argv[1]
infos = {name: json.load(open(os.path.join(root, name + "-info.json"))) for name in ("a", "b")}
addresses = {name: next(a for a in info["addrs"] if a.startswith("/ip4/127.0.0.1/tcp/"))
             for name, info in infos.items()}
for name, other in (("a", "b"), ("b", "a")):
    path = os.path.join(root, name + ".json")
    config = json.load(open(path))
    config["listen"] = [addresses[name].split("/p2p/")[0]]
    config["allowed_peers"] = [infos[other]["peer_id"]]
    config["bootstrap"] = [addresses[other]]
    with open(path, "w") as f:
        json.dump(config, f)
PY
start_node a; A_API=$API
start_node b; B_API=$API
B_ID=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["peer_id"])' "$TMP/b-info.json")
python3 "$ROOT/examples/python-handler.py" > "$TMP/handler-url" 2> "$TMP/handler.log" &
pids+=("$!")
for ((i=0; i<100; i++)); do [[ -s "$TMP/handler-url" ]] && break; sleep 0.1; done
HANDLER_URL=$(cat "$TMP/handler-url")
[[ -n $HANDLER_URL ]] || { cat "$TMP/handler.log" >&2; exit 1; }
"$CLI" --api "$A_API" register-service --name service:demo --url "$HANDLER_URL" --allow-peer "$B_ID"
"$CLI" --api "$B_API" call --service service:demo --method add --params '[20,22]' --request-id smoke-rpc > "$TMP/call.json"
python3 - "$TMP/call.json" <<'PY'
import json, sys
assert json.load(open(sys.argv[1]))["result"] == 42, "remote RPC result mismatch"
PY
printf 'Mediatrix verified remote file\n' > "$TMP/source.txt"
"$CLI" --api "$A_API" upload --file "$TMP/source.txt" > "$TMP/upload.json"
KEY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["key"])' "$TMP/upload.json")
"$CLI" --api "$A_API" grant-file --key "$KEY" --allow-peer "$B_ID"
"$CLI" --api "$B_API" fetch --key "$KEY" --out "$TMP/download.txt" > /dev/null
cmp "$TMP/source.txt" "$TMP/download.txt"
"$CLI" --api "$A_API" remove-service --name service:demo
if "$CLI" --api "$B_API" call --service service:demo --method add --params '[1,2]' > /dev/null 2>&1; then
  echo 'removed service unexpectedly remained callable' >&2; exit 1
fi
printf 'PASS: two private nodes, remote RPC, authorized file transfer, removal\n'
