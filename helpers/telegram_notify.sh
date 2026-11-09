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

# Parse CDR fields. Numbers come from the network: keep only digits, + - ( )
# so they can never be read by curl as a file reference.
CALLER=$(jq -r '.caller_number // ""' "$CDR_JSON" | tr -cd '0-9+()-')
CALLED=$(jq -r '.called_number // ""' "$CDR_JSON" | tr -cd '0-9+()-')
CALLER="${CALLER:-neznamy}"
DIRECTION=$(jq -r '.direction' "$CDR_JSON")
ANSWERED=$(jq -r '.answered' "$CDR_JSON")
ANSWERED_BY=$(jq -r '.answered_by // empty' "$CDR_JSON")
DURATION=$(jq -r '.duration_seconds | floor' "$CDR_JSON")
TALK_TIME=$(jq -r '.talk_time_seconds | floor' "$CDR_JSON")
HANGUP_BY=$(jq -r '.hangup_by' "$CDR_JSON")
VOICEMAIL=$(jq -r '.voicemail' "$CDR_JSON")
MSG_DURATION=$(jq -r '.message_duration_seconds // 0 | floor' "$CDR_JSON")
# Czech label for voicemail_reason; unknown values are shown as is
REASON=$(jq -r '{vacation: "dovolena", outside_hours: "mimo pracovni dobu", unavailable: "nikdo neni k dispozici"}[.voicemail_reason // ""] // .voicemail_reason // empty' "$CDR_JSON")
START_TIME=$(jq -r '.start_time | split(".")[0] | gsub("T";" ")' "$CDR_JSON")
ERROR=$(jq -r '.error // empty' "$CDR_JSON")

# Format human-readable message
if [ "$VOICEMAIL" = "true" ]; then
    MSG="Hlasova schranka${REASON:+ (${REASON})}
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

# A failed recording or voicemail is worth a line of its own: the audio is missing, not just late
if [ -n "$ERROR" ]; then
    MSG="CHYBA: ${ERROR}
${MSG}"
fi

# The token is part of the URL. curl reads it from stdin (-K -), not from argv,
# so it does not show up in `ps` for other local users.
tg() {
    local method="$1"; shift
    curl -s -X POST -K - "$@" <<EOF
url = "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/${method}"
EOF
}

# Send text summary. --data-urlencode keeps "+" in numbers from turning into a space.
tg sendMessage \
    --data-urlencode "chat_id=$TELEGRAM_CHAT_ID" \
    --data-urlencode "text=$MSG" \
    > /dev/null

# Send recording as voice message (if available)
if [ "$WAV_FILE" != "none" ] && [ -f "$WAV_FILE" ]; then
    OGG_FILE=$(mktemp /tmp/sip2sip-XXXXXX.ogg)
    trap "rm -f '$OGG_FILE'" EXIT

    # Convert WAV to OGG Opus for Telegram voice message bubble
    if ffmpeg -y -i "$WAV_FILE" -c:a libopus -b:a 32k "$OGG_FILE" 2>/dev/null; then
        # --form-string: a value starting with @ or < would otherwise be read as a file
        tg sendVoice \
            --form-string "chat_id=$TELEGRAM_CHAT_ID" \
            -F voice=@"$OGG_FILE" \
            --form-string "caption=$CALLER" \
            > /dev/null
    else
        # Fallback: send WAV as document
        tg sendDocument \
            --form-string "chat_id=$TELEGRAM_CHAT_ID" \
            -F document=@"$WAV_FILE" \
            --form-string "caption=$CALLER" \
            > /dev/null
    fi
fi
