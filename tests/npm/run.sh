#!/bin/sh
# NPM integration test (TST-3). Usage: IMAGE=switchyard:test tests/npm/run.sh
set -eu
cd "$(dirname "$0")"
export IMAGE="${IMAGE:?set IMAGE}"
# Coverage of the instrumented image (COVER=1) lands here.
export COVERDIR="${COVERDIR:-$(mktemp -d)}"
mkdir -p "$COVERDIR" && chmod 777 "$COVERDIR"
dc() { docker compose -f compose.yml "$@"; }
fail() { echo "FAIL: $*"; dc logs a npm; dc down -v >/dev/null 2>&1; exit 1; }

zone() { sed "s/SERIAL/$1/" coredns/db.test.in > coredns/db.test; }
zone 1

dc up -d --wait

api=http://127.0.0.1:8081/api
for i in $(seq 1 60); do
  token=$(curl -fsS "$api/tokens" -H 'Content-Type: application/json' \
    -d '{"identity":"admin@example.com","secret":"test-password-123"}' 2>/dev/null |
    sed -n 's/.*"token":"\([^"]*\)".*/\1/p') && [ -n "$token" ] && break
  sleep 2
done
[ -n "${token:-}" ] || fail "NPM API token"

host() {
  curl -fsS "$api/nginx/proxy-hosts" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d "{\"domain_names\":[\"$1\"],\"forward_scheme\":\"http\",\"forward_host\":\"$2\",\"forward_port\":$3}" \
    >/dev/null || fail "create proxy host $1"
}
get() { curl -fsS --max-time 5 -H "Host: $1" http://127.0.0.1:8080/; }
retry() {
  for i in $(seq 1 "$1"); do
    if get "$2" 2>/dev/null | grep -q Hostname; then return 0; fi
    sleep 1
  done
  return 1
}

echo "short name from NPM's files"
host short.example web.b.switchyard 80
retry 30 short.example || fail "short name"

echo "name unknown to DNS at first"
host late.example late.test 80
dc exec -T npm sh -c 'grep -rqs "set \$server *\"late.test\"" /data/nginx/proxy_host/' ||
  fail "proxy host not written with variables"
if get late.example 2>/dev/null | grep -q Hostname; then fail "late.test resolved too early"; fi

echo "make it resolvable, no NPM reload"
zone 2
echo "late 5 IN A 172.29.0.200" >> coredns/db.test
retry 60 late.example || fail "runtime resolution"

dc down -v >/dev/null
rm -f coredns/db.test
chmod -R a+rX "$COVERDIR"
echo PASS
