#!/bin/sh
set -eu
cd "$(dirname "$0")"

fail() { echo "FAIL: $*"; exit 1; }
https() {
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    curl -sf --max-time 10 --cacert ca/ca.crt --resolve "$1:18443:127.0.0.1" "https://$1:18443$2" && return 0
    sleep 1
  done
  return 1
}
cli() { docker compose exec -T router internal-router "$@"; }

mkdir -p ca
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 30 \
  -subj "/CN=internal-router test CA" -keyout ca/ca.key -out ca/ca.crt \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
chmod 644 ca/ca.key

docker compose down -v >/dev/null 2>&1 || true
docker compose up -d --build --wait-timeout 120 >/dev/null 2>&1 || docker compose up -d --build
trap 'docker compose down -v >/dev/null 2>&1' EXIT

for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 1; done
cli health || fail "router never became healthy"

cli add whoami http://whoami:80
cli list | grep -q "whoami.internal" || fail "list does not show the new route"

body=$(https whoami.internal /) || fail "https request to whoami.internal failed"
echo "$body" | grep -q "Host: whoami.internal" || fail "upstream did not get the original Host header: $body"
echo "ok: whoami.internal proxied over HTTPS signed by the test CA"

https router.internal / | grep -q "<title>Internal Router</title>" || fail "router.internal does not serve the UI"
https router.internal /icon.svg | grep -q "<svg" || fail "icon missing"
echo "ok: UI and icon on router.internal"

code=$(curl -s -o /dev/null -w "%{http_code}" --cacert ca/ca.crt --resolve router.internal:18443:127.0.0.1 \
  -X DELETE -H "Origin: https://evil.example" https://router.internal:18443/api/routes/whoami)
[ "$code" = 403 ] || fail "cross-origin delete returned $code"
code=$(curl -s -o /dev/null -w "%{http_code}" --cacert ca/ca.crt --resolve router.internal:18443:127.0.0.1 \
  -X PUT -H "Origin: https://router.internal" -H "Content-Type: application/json" \
  -d '{"target":"http://whoami:80"}' https://router.internal:18443/api/routes/echo)
[ "$code" = 200 ] || fail "UI-origin put returned $code"
https echo.internal / | grep -q "Host: echo.internal" || fail "route added through the API does not work"
echo "ok: API add from the UI origin, cross-origin writes refused"

machines=$(https router.internal /api/machines)
echo "$machines" | grep -q '"dns":"whoami"' || fail "tailnet machines missing: $machines"
echo "$machines" | grep -q '"name":"nas","dns":"nas.example.ts.net","ip":"100.64.0.21","os":"linux","online":false' \
  || fail "offline machine not listed as expected: $machines"
echo "ok: tailnet machines listed from the Tailscale socket"

cli add router http://whoami:80 2>/dev/null && fail "reserved name was accepted"
cli add bad http://whoami/path 2>/dev/null && fail "target with a path was accepted"
echo "ok: bad input refused"

ans=$(dig +short -p 18053 @127.0.0.1 anything.internal A)
[ "$ans" = 10.9.9.9 ] || fail "DNS A answer: '$ans'"
ans=$(dig +short +tcp -p 18053 @127.0.0.1 whoami.internal A)
[ "$ans" = 10.9.9.9 ] || fail "DNS over TCP answer: '$ans'"
status=$(dig -p 18053 @127.0.0.1 example.com A | grep -o "status: [A-Z]*")
[ "$status" = "status: REFUSED" ] || fail "outside the zone: $status"
echo "ok: DNS answers *.internal and refuses other names"

docker compose restart caddy >/dev/null 2>&1
sleep 3
https whoami.internal / | grep -q "Host: whoami.internal" || fail "route lost after Caddy restart"
docker compose restart router >/dev/null 2>&1
for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 1; done
cli list | grep -q "echo.internal" || fail "routes lost after router restart"
echo "ok: routes survive Caddy and router restarts"

docker compose restart net >/dev/null 2>&1
recovered=
for i in $(seq 1 60); do
  if curl -sf --max-time 5 --cacert ca/ca.crt --resolve router.internal:18443:127.0.0.1 https://router.internal:18443/ >/dev/null 2>&1 \
    && cli health >/dev/null 2>&1 \
    && [ "$(dig +short +time=2 +tries=1 -p 18053 @127.0.0.1 anything.internal A)" = 10.9.9.9 ]; then
    recovered=1
    break
  fi
  sleep 2
done
[ -n "$recovered" ] || fail "stack did not recover after the net container restarted"
echo "ok: caddy and router recover after the network container restarts"

cli rm whoami
sleep 1
curl -sf --max-time 5 --cacert ca/ca.crt --resolve whoami.internal:18443:127.0.0.1 https://whoami.internal:18443/ >/dev/null 2>&1 \
  && fail "removed route still answers"
echo "ok: removed route stops answering"

echo "all tests passed"
