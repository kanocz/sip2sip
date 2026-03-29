#!/bin/bash
# Post-call notification to Telegram.
# Sends call summary as text + recording as voice message.
# Usage: telegram_notify.sh /path/to/call.json /path/to/recording.wav
# Requires: jq, curl, ffmpeg (for voice message conversion)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONF="${TELEGRAM_NOTIFY_CONF:-$SCRIPT_DIR/telegram_notify.conf}"

if [ ! -f "$CONF" ]; then
    echo "Config not found: $CONF" >&2
    exit 1
fi
source "$CONF"

if [ -z "${TELEGRAM_BOT_TOKEN:-}" ] || [ -z "${TELEGRAM_CHAT_ID:-}" ]; then
    echo "TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID required in $CONF" >&2
    exit 1
fi

CDR_JSON="${1:-}"
WAV_FILE="${2:-none}"

if [ -z "$CDR_JSON" ] || [ ! -f "$CDR_JSON" ]; then
    echo "CDR JSON not found: $CDR_JSON" >&2
    exit 1
fi

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

# Format human-readable message
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

API="https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}"

# Send text summary
curl -s -X POST "$API/sendMessage" \
    -d chat_id="$TELEGRAM_CHAT_ID" \
    -d text="$MSG" \
    > /dev/null

# Send recording as voice message (if available)
if [ "$WAV_FILE" != "none" ] && [ -f "$WAV_FILE" ]; then
    OGG_FILE=$(mktemp /tmp/sip2sip-XXXXXX.ogg)
    trap "rm -f '$OGG_FILE'" EXIT

    # Convert WAV to OGG Opus for Telegram voice message bubble
    if ffmpeg -y -i "$WAV_FILE" -c:a libopus -b:a 32k "$OGG_FILE" 2>/dev/null; then
        curl -s -X POST "$API/sendVoice" \
            -F chat_id="$TELEGRAM_CHAT_ID" \
            -F voice=@"$OGG_FILE" \
            -F caption="${CALLER}" \
            > /dev/null
    else
        # Fallback: send WAV as document
        curl -s -X POST "$API/sendDocument" \
            -F chat_id="$TELEGRAM_CHAT_ID" \
            -F document=@"$WAV_FILE" \
            -F caption="${CALLER}" \
            > /dev/null
    fi
fi
