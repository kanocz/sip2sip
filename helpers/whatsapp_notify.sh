#!/bin/bash
# Post-call notification to WhatsApp via Meta Cloud API.
# Sends call summary as text + recording as audio message.
# Usage: whatsapp_notify.sh /path/to/call.json /path/to/recording.wav
# Requires: jq, curl, ffmpeg (for audio conversion)
#
# Setup:
#   1. Create a Meta Business account: https://business.facebook.com
#   2. Create a WhatsApp Business app: https://developers.facebook.com/apps
#   3. Get Phone Number ID and permanent access token from the app dashboard
#   4. The recipient must first message your business number (24h session window)
#      or you must use pre-approved message templates
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONF="${WHATSAPP_NOTIFY_CONF:-$SCRIPT_DIR/whatsapp_notify.conf}"

if [ ! -f "$CONF" ]; then
    echo "Config not found: $CONF" >&2
    exit 1
fi
source "$CONF"

if [ -z "${WHATSAPP_TOKEN:-}" ] || [ -z "${WHATSAPP_PHONE_ID:-}" ] || [ -z "${WHATSAPP_TO:-}" ]; then
    echo "WHATSAPP_TOKEN, WHATSAPP_PHONE_ID and WHATSAPP_TO required in $CONF" >&2
    exit 1
fi

CDR_JSON="${1:-}"
WAV_FILE="${2:-none}"

if [ -z "$CDR_JSON" ] || [ ! -f "$CDR_JSON" ]; then
    echo "CDR JSON not found: $CDR_JSON" >&2
    exit 1
fi

API="https://graph.facebook.com/v21.0/${WHATSAPP_PHONE_ID}"

# Parse CDR fields
CALLER=$(jq -r '.caller_number' "$CDR_JSON")
CALLED=$(jq -r '.called_number' "$CDR_JSON")
DIRECTION=$(jq -r '.direction' "$CDR_JSON")
ANSWERED=$(jq -r '.answered' "$CDR_JSON")
ANSWERED_BY=$(jq -r '.answered_by // empty' "$CDR_JSON")
DURATION=$(jq -r '.duration_seconds | floor' "$CDR_JSON")
TALK_TIME=$(jq -r '.talk_time_seconds | floor' "$CDR_JSON")
HANGUP_BY=$(jq -r '.hangup_by' "$CDR_JSON")
VOICEMAIL=$(jq -r '.voicemail' "$CDR_JSON")
MSG_DURATION=$(jq -r '.message_duration_seconds // 0 | floor' "$CDR_JSON")
START_TIME=$(jq -r '.start_time | split(".")[0] | gsub("T";" ")' "$CDR_JSON")

# Format message
if [ "$VOICEMAIL" = "true" ]; then
    MSG="Hlasova schranka
Od: ${CALLER}
Cas: ${START_TIME}
Delka vzkazu: ${MSG_DURATION}s"
elif [ "$ANSWERED" = "true" ]; then
    if [ "$DIRECTION" = "incoming" ]; then
        MSG="Prichozi hovor
Od: ${CALLER}
Prijal: ${ANSWERED_BY}
Delka: ${TALK_TIME}s
Zavesil: ${HANGUP_BY}"
    else
        MSG="Odchozi hovor
${CALLER} -> ${CALLED}
Delka: ${TALK_TIME}s
Zavesil: ${HANGUP_BY}"
    fi
else
    MSG="Zmeskany hovor
Od: ${CALLER}
Cas: ${START_TIME}"
fi

# Send text message
curl -s -X POST "$API/messages" \
    -H "Authorization: Bearer $WHATSAPP_TOKEN" \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg to "$WHATSAPP_TO" --arg body "$MSG" '{
        messaging_product: "whatsapp",
        to: $to,
        type: "text",
        text: { body: $body }
    }')" \
    > /dev/null

# Send recording as audio (if available)
if [ "$WAV_FILE" != "none" ] && [ -f "$WAV_FILE" ]; then
    OGG_FILE=$(mktemp /tmp/sip2sip-XXXXXX.ogg)
    trap "rm -f '$OGG_FILE'" EXIT

    # Convert WAV to OGG Opus
    ffmpeg -y -i "$WAV_FILE" -c:a libopus -b:a 32k "$OGG_FILE" 2>/dev/null || {
        echo "ffmpeg conversion failed" >&2
        exit 0
    }

    # Upload media first
    MEDIA_ID=$(curl -s -X POST "$API/media" \
        -H "Authorization: Bearer $WHATSAPP_TOKEN" \
        -F messaging_product=whatsapp \
        -F type=audio/ogg \
        -F file=@"$OGG_FILE" \
        | jq -r '.id // empty')

    if [ -n "$MEDIA_ID" ]; then
        # Send audio message
        curl -s -X POST "$API/messages" \
            -H "Authorization: Bearer $WHATSAPP_TOKEN" \
            -H "Content-Type: application/json" \
            -d "$(jq -n --arg to "$WHATSAPP_TO" --arg id "$MEDIA_ID" '{
                messaging_product: "whatsapp",
                to: $to,
                type: "audio",
                audio: { id: $id }
            }')" \
            > /dev/null
    fi
fi
