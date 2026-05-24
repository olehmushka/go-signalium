#!/bin/sh
set -e

CONF="/var/lib/signal-cli"

echo "🔍 Checking if linked..."
if signal-cli --config "$CONF" listDevices > /dev/null 2>&1; then
    echo "✅ Already linked"
else
    echo "Not linked. Generating QR code..."

    # generate new link session ONLY ONCE
    signal-cli --config "$CONF" link -n "docker-signal" \
      | xargs -L 1 qrencode -t utf8

    echo "Waiting for user to scan the QR..."
    while ! signal-cli --config "$CONF" listDevices >/dev/null 2>&1; do
        sleep 2
    done

    echo "Successfully linked!"
fi

echo "🚀 Starting daemon..."
exec signal-cli --config "$CONF" \
  --trust-new-identities always \
  daemon --tcp 0.0.0.0:7610 --http 0.0.0.0:7611 \
  --send-read-receipts --no-receive-stdout
