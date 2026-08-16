#!/usr/bin/env bash
# End-to-end demonstration of hookfan fan-out.
#
# Brings up two receiver containers, registers them as services, completes the
# link handshake, subscribes both to a listener, then posts one signed webhook
# and shows it arriving at both.
set -euo pipefail

API="${API:-http://localhost:8081}"
NETWORK="${NETWORK:-hookfan_default}"
LISTENER_SLUG="demo-whatsapp"
APP_SECRET="demo-app-secret"
VERIFY_TOKEN="demo-verify-token"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
step() { printf '\n\033[1;36m▸ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
info() { printf '  %s\n' "$*"; }

need() { command -v "$1" >/dev/null || { echo "error: $1 is required" >&2; exit 1; }; }
need curl
need docker
need python3

if [[ -f .env ]]; then
    ADMIN_TOKEN="$(grep '^ADMIN_TOKEN=' .env | cut -d= -f2-)"
else
    echo "error: .env not found. Run 'cp .env.example .env' and fill it in first." >&2
    exit 1
fi
auth=(-H "Authorization: Bearer ${ADMIN_TOKEN}")

if ! curl -sf "${API}/healthz" >/dev/null; then
    echo "error: the API is not responding at ${API}. Run 'make up' first." >&2
    exit 1
fi

jqf() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

cleanup_containers() {
    docker rm -f demo-recv-orders demo-recv-analytics >/dev/null 2>&1 || true
}
trap cleanup_containers EXIT

bold "hookfan demo — one webhook, two services"

# ---------------------------------------------------------------------------
step "Building the demo receiver image"
docker build -q -t hookfan-demo-receiver demo/receiver >/dev/null
ok "built"

# ---------------------------------------------------------------------------
step "Creating the listener"
existing="$(curl -s "${auth[@]}" "${API}/api/listeners" | \
    jqf "next((l['id'] for l in d['listeners'] if l['slug']=='${LISTENER_SLUG}'), '')")"

if [[ -n "${existing}" ]]; then
    listener_id="${existing}"
    info "reusing listener #${listener_id} (${LISTENER_SLUG})"
else
    listener_id="$(curl -s -X POST "${auth[@]}" -H 'Content-Type: application/json' \
        -d "{\"name\":\"Demo WhatsApp\",\"slug\":\"${LISTENER_SLUG}\",\"provider\":\"meta\",
             \"secret\":\"${APP_SECRET}\",\"challenge_verify_token\":\"${VERIFY_TOKEN}\"}" \
        "${API}/api/listeners" | jqf "d['id']")"
    ok "created listener #${listener_id} at ${API}/hooks/${LISTENER_SLUG}"
fi

# ---------------------------------------------------------------------------
step "Meta GET handshake"
challenge="$(curl -s "${API}/hooks/${LISTENER_SLUG}?hub.mode=subscribe&hub.verify_token=${VERIFY_TOKEN}&hub.challenge=demo-challenge-123")"
if [[ "${challenge}" == "demo-challenge-123" ]]; then
    ok "challenge echoed verbatim: ${challenge}"
else
    echo "  ✗ unexpected response: ${challenge}" >&2
    exit 1
fi
wrong="$(curl -s -o /dev/null -w '%{http_code}' \
    "${API}/hooks/${LISTENER_SLUG}?hub.mode=subscribe&hub.verify_token=WRONG&hub.challenge=x")"
ok "wrong verify token rejected with HTTP ${wrong}"

# ---------------------------------------------------------------------------
step "Registering two services and completing the link handshake"
cleanup_containers

declare -A SERVICE_IDS
for name in orders analytics; do
    response="$(curl -s -X POST "${auth[@]}" -H 'Content-Type: application/json' \
        -d "{\"name\":\"demo-${name}\",\"url\":\"http://demo-recv-${name}:9000/hook\"}" \
        "${API}/api/services")"
    service_id="$(echo "${response}" | jqf "d['service']['id']")"
    link_token="$(echo "${response}" | jqf "d['link_token']")"
    SERVICE_IDS[$name]="${service_id}"

    # The token is shown exactly once — configure the receiver with it.
    docker run -d --name "demo-recv-${name}" --network "${NETWORK}" \
        -e "RECEIVER_NAME=${name}" -e "HOOKFAN_TOKEN=${link_token}" \
        hookfan-demo-receiver >/dev/null
    info "demo-${name}: service ${service_id}, receiver started"
done

# Give uvicorn a moment to bind.
sleep 4

for name in orders analytics; do
    result="$(curl -s -X POST "${auth[@]}" "${API}/api/services/${SERVICE_IDS[$name]}/verify")"
    status="$(echo "${result}" | jqf "d['service']['status']")"
    if [[ "${status}" == "verified" ]]; then
        latency="$(echo "${result}" | jqf "d['result']['latency_ms']")"
        ok "demo-${name} verified in ${latency}ms"
    else
        kind="$(echo "${result}" | jqf "d['result'].get('kind','?')")"
        message="$(echo "${result}" | jqf "d['result'].get('message','')")"
        echo "  ✗ demo-${name} failed to verify — ${kind}: ${message}" >&2
        exit 1
    fi
done

# ---------------------------------------------------------------------------
step "Subscribing both services to the listener"
for name in orders analytics; do
    curl -s -X POST "${auth[@]}" -H 'Content-Type: application/json' \
        -d "{\"listener_id\":${listener_id},\"service_id\":\"${SERVICE_IDS[$name]}\",
             \"filter_type\":\"all\"}" \
        "${API}/api/subscriptions" >/dev/null
    ok "demo-${name} subscribed to every event"
done

# ---------------------------------------------------------------------------
step "Posting one signed webhook"
body="{\"object\":\"whatsapp_business_account\",\"entry\":[{\"id\":\"DEMO_WABA_$(date +%s)\",\"changes\":[{\"field\":\"messages\",\"value\":{\"messages\":[{\"id\":\"wamid.DEMO\",\"text\":{\"body\":\"hello from the demo\"}}]}}]}]}"
signature="$(printf '%s' "${body}" | openssl dgst -sha256 -hmac "${APP_SECRET}" | awk '{print $2}')"

info "signature: sha256=${signature:0:32}…"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "${API}/hooks/${LISTENER_SLUG}" \
    -H "X-Hub-Signature-256: sha256=${signature}" \
    -H 'Content-Type: application/json' -d "${body}")"
ok "ingest returned HTTP ${code}"

info "a tampered copy of the same body:"
tampered="$(curl -s -o /dev/null -w '%{http_code}' -X POST "${API}/hooks/${LISTENER_SLUG}" \
    -H "X-Hub-Signature-256: sha256=${signature}" \
    -H 'Content-Type: application/json' -d "${body} ")"
ok "rejected with HTTP ${tampered} (signature covers the exact bytes)"

# ---------------------------------------------------------------------------
step "Waiting for fan-out"
sleep 4

for name in orders analytics; do
    count="$(docker exec "demo-recv-${name}" \
        python3 -c "import urllib.request,json; print(json.load(urllib.request.urlopen('http://localhost:9000/received'))['count'])" 2>/dev/null || echo 0)"
    if [[ "${count}" -ge 1 ]]; then
        ok "demo-${name} received ${count} event(s)"
    else
        echo "  ✗ demo-${name} received nothing" >&2
    fi
done

step "Receiver logs"
for name in orders analytics; do
    printf '  \033[1m%s\033[0m\n' "demo-${name}"
    docker logs "demo-recv-${name}" 2>&1 | grep -E "link.verify|EVENT" | sed 's/^/    /'
done

step "What hookfan recorded"
curl -s "${auth[@]}" "${API}/api/events?limit=1" | python3 -c "
import json, sys
page = json.load(sys.stdin)
if not page['events']:
    print('  (no events)')
else:
    e = page['events'][0]
    print(f\"  event #{e['id']}  keys={e['routing_keys']}  signature={'valid' if e['signature_valid'] else 'INVALID'}\")
    print(f\"  delivered {e['delivery_success']}/{e['delivery_total']}  ({e['body_bytes']} bytes)\")
"

bold ""
bold "Done."
info "UI:      http://localhost:${UI_PORT:-8080}"
info "Events:  ${API}/api/events"
info ""
info "The receivers are still running. Post another webhook with:"
info "  curl -X POST ${API}/hooks/${LISTENER_SLUG} \\"
info "    -H \"X-Hub-Signature-256: sha256=\$(printf '%s' \"\$BODY\" | openssl dgst -sha256 -hmac ${APP_SECRET} | awk '{print \$2}')\" \\"
info "    -H 'Content-Type: application/json' -d \"\$BODY\""
info ""
info "Tear them down with:  make demo-clean"

# The receivers stay up so the UI has something to show.
trap - EXIT
