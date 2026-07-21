#!/usr/bin/env bash
# End-to-end engine test against aft-sim (no camera): discovery, head-sweep
# thumbnails, image fetch, video streaming, breaker trip + recovery.
set -uo pipefail

SP=$(mktemp -d)
trap 'rm -rf "$SP"' EXIT
cd "$(dirname "$0")/.."
go build -o "$SP/aft-sim" ./cmd/aft-sim
go build -o "$SP/fuji-cull-sim" ./cmd/fuji-cull

export FUJI_FAKE_ROOT="${SIM_CORPUS:?set SIM_CORPUS to a dir with SLOT 1/DCIM/NNN_FUJI media}"
export FUJI_FAKE_SETUP_MS=300
export FUJI_FAKE_MBPS=60
export FUJI_AFT="$SP/aft-sim"
export FUJI_AFT_PART="$SP/aft-sim"
rm -f "$FUJI_FAKE_ROOT/.sick" "$FUJI_FAKE_ROOT/.lock"
rm -rf "$SP/simcache"

"$SP/fuji-cull-sim" --listen 127.0.0.1:8899 --session sim --cache-dir "$SP/simcache" \
  --skip-immich --log "$SP/sim-engine.log" &
ENGINE=$!
trap 'kill $ENGINE 2>/dev/null; rm -rf "$SP"' EXIT

api() { curl -sf "http://127.0.0.1:8899$1"; }
fail() { echo "FAIL: $1"; tail -20 "$SP/sim-engine.log"; exit 1; }

# 1. discovery
for i in $(seq 1 60); do
  shots=$(api /api/state | python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d.get(\"shots\",[])))" 2>/dev/null || echo 0)
  [ "${shots:-0}" -gt 0 ] && break
  sleep 1
done
[ "${shots:-0}" -gt 0 ] || fail "no shots discovered"
echo "PASS discovery: $shots shots"

# 2. thumbnails via head sweep (alternating with window fill)
for i in $(seq 1 90); do
  have=$(api /api/thumbs | python3 -c "import json,sys; print(json.load(sys.stdin)[\"have\"])" 2>/dev/null || echo 0)
  [ "${have:-0}" -ge $((shots * 8 / 10)) ] && break
  sleep 1
done
[ "${have:-0}" -ge $((shots * 8 / 10)) ] || fail "thumbs stalled at ${have:-0}/$shots"
echo "PASS thumbs: $have/$shots"

# 3. full image fetch
id=$(api /api/state | python3 -c "import json,sys; print(json.load(sys.stdin)[\"shots\"][0][\"id\"])")
enc=$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))" "$id")
size=$(curl -sf "http://127.0.0.1:8899/api/image?id=$enc" | wc -c)
[ "$size" -gt 1000000 ] || fail "image fetch returned $size bytes"
echo "PASS image: $size bytes"

# 4. video streaming (range request off the persistent session)
vid=$(api /api/state | python3 -c "import json,sys; print(next(s[\"id\"] for s in json.load(sys.stdin)[\"shots\"] if s[\"kind\"]==\"video\"))")
venc=$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))" "$vid")
vhead=$(curl -sf -r 0-65535 "http://127.0.0.1:8899/api/video?id=$venc" | head -c 12 | xxd -p)
echo "$vhead" | grep -q "667479" || fail "video head lacks ftyp: $vhead"
echo "PASS stream: ftyp present"

# 5. breaker: make partial reads sick; chunk 0 of an untouched video gets
# ftyp-validated, so its stale bytes trip the breaker (mid-file chunks are
# unvalidatable by design — nothing to match against)
vid2=$(api /api/state | python3 -c "import json,sys; vs=[s[\"id\"] for s in json.load(sys.stdin)[\"shots\"] if s[\"kind\"]==\"video\"]; print(vs[1])")
venc2=$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))" "$vid2")
echo part > "$FUJI_FAKE_ROOT/.sick"
sleep 22  # let the idle janitor close the healthy stream session first
curl -s -r 0-65535 "http://127.0.0.1:8899/api/video?id=$venc2" >/dev/null
for i in $(seq 1 60); do
  sick=$(api /api/status | python3 -c "import json,sys; print(json.load(sys.stdin)[\"partSick\"])" 2>/dev/null)
  [ "$sick" = "True" ] && break
  sleep 1
done
[ "$sick" = "True" ] || fail "partSick never tripped"
echo "PASS breaker tripped"

# 6. recovery: power-cycle (remove .sick), expect probe to clear it
rm "$FUJI_FAKE_ROOT/.sick"
for i in $(seq 1 45); do
  sick=$(api /api/status | python3 -c "import json,sys; print(json.load(sys.stdin)[\"partSick\"])" 2>/dev/null)
  [ "$sick" = "False" ] && break
  sleep 1
done
[ "$sick" = "False" ] || fail "partSick never recovered"
echo "PASS breaker recovered"

echo "ALL PASS"
