#!/bin/sh
# Integration test (TST-2). Usage: IMAGE=switchyard:test tests/integration/run.sh
set -eu
cd "$(dirname "$0")"
export IMAGE="${IMAGE:?set IMAGE}"
# Coverage of the instrumented image (COVER=1) lands here.
export COVERDIR="${COVERDIR:-$(mktemp -d)}"
mkdir -p "$COVERDIR" && chmod 777 "$COVERDIR"
dc() { docker compose -f compose.yml "$@"; }
fail() { echo "FAIL: $*"; dc logs a b; dc down -v >/dev/null 2>&1; exit 1; }
cli() { dc exec -T client "$@"; }

dc up -d --wait
cli apk add -q socat curl bind-tools >/dev/null

echo "long name over the partner"
cli curl -fsS --max-time 10 http://web.80.b.switchyard/ | grep -q Hostname || fail "long name"

echo "case-insensitive, same address"
a1=$(cli dig +short web.80.b.switchyard)
a2=$(cli dig +short WEB.80.B.switchyard)
[ -n "$a1" ] && [ "$a1" = "$a2" ] || fail "case: $a1 / $a2"

echo "container name works besides the service name"
cli curl -fsS --max-time 10 http://sy-it-web-1.80.b.switchyard/ | grep -q Hostname || fail "container name"

echo "local name"
cli curl -fsS --max-time 10 http://localweb.80.local.switchyard/ | grep -q Hostname || fail "local"

echo "AAAA for a valid name is NOERROR without data"
cli dig AAAA web.80.b.switchyard | grep -q "status: NOERROR" || fail "AAAA"

echo "unknown partner is NXDOMAIN"
cli dig nope.80.x.switchyard | grep -q "status: NXDOMAIN" || fail "NXDOMAIN"

echo "port collision gets a second address"
a3=$(cli dig +short other.80.b.switchyard)
[ -n "$a3" ] && [ "$a3" != "$a1" ] || fail "collision: $a1 / $a3"

echo "late member with a fixed address the agent uses is logged"
late=$(docker run -d --network sy-it_imp --ip "$a3" alpine:3.22 sleep 60)
sleep 2
docker rm -f "$late" >/dev/null
dc logs a | grep -q "address the agent uses" || fail "IMP-8"

echo "target sharing no network is refused"
if cli curl -fsS --max-time 10 http://localweb.80.b.switchyard/ >/dev/null 2>&1; then fail "refusal"; fi

echo "other names are forwarded"
cli dig +short example.com | grep -q . || fail "forwarding"

echo "UDP"
out=$(cli sh -c 'echo ping | socat -t2 - UDP4:udpecho.9999.b.switchyard:9999')
[ "$out" = "ping" ] || fail "udp: $out"

echo "metrics"
cli curl -fsS http://172.28.0.2:9480/metrics | grep -q 'switchyard_peer_up{peer="b"} 1' || fail "peer_up"
dc exec -T b /switchyard health || fail "health b"

echo "opt-out closes and refuses"
docker network disconnect sy-it_tgt "$(dc ps -q web)"
sleep 1
if cli curl -fsS --max-time 10 http://web.80.b.switchyard/ >/dev/null 2>&1; then fail "opt-out"; fi

dc down -v >/dev/null
chmod -R a+rX "$COVERDIR" 2>/dev/null || true  # files belong to root; Go writes them readable
echo PASS
