#!/bin/sh
# Generates /config.js from the environment, then starts nginx.
#
# The API address is resolved here rather than baked in at build time, so one
# image runs in every environment. The app reads window.__HOOKFAN_CONFIG__ at
# boot.
set -eu

: "${API_BASE_URL:=}"

if [ -z "$API_BASE_URL" ]; then
    echo "hookfan-ui: WARNING — API_BASE_URL is not set." >&2
    echo "hookfan-ui: The UI will call the API on its own origin, which is" >&2
    echo "hookfan-ui: almost never right. Set it to the address your BROWSER" >&2
    echo "hookfan-ui: can reach, e.g. http://localhost:8081" >&2
fi

# This must be the browser-reachable address. A compose service name such as
# http://api:8081 resolves only inside the compose network, and the request
# originates in the user's browser — this is the mistake everyone makes first.
case "$API_BASE_URL" in
    http://api:*|http://api|https://api:*|https://api)
        echo "hookfan-ui: WARNING — API_BASE_URL is \"$API_BASE_URL\"." >&2
        echo "hookfan-ui: That hostname only resolves inside the compose" >&2
        echo "hookfan-ui: network. The browser cannot reach it. Use the" >&2
        echo "hookfan-ui: address you would type into the address bar," >&2
        echo "hookfan-ui: e.g. http://localhost:8081" >&2
        ;;
esac

export API_BASE_URL
envsubst '${API_BASE_URL}' \
    < /usr/share/nginx/template/config.js.template \
    > /usr/share/nginx/html/config.js

echo "hookfan-ui: apiBaseUrl=${API_BASE_URL:-<same origin>}"

exec "$@"
