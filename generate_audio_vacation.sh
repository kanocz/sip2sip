#!/bin/bash
# Generate a Czech vacation announcement that names the day we are back.
# Usage: ./generate_audio_vacation.sh <back-date YYYY-MM-DD> [output.wav]
# Default output: vacation_<back-date>.wav
# Uses tts.sh helper. Requires: python3 and ffmpeg; gtts goes into tts.sh's own venv
#
# Example:
#   ./generate_audio_vacation.sh 2027-01-04
#   -> vacation_2027-01-04.wav: "... Opět vám budeme k dispozici od čtvrtého ledna. ..."
# Then point a vacation period at it in config.json:
#   { "from": "2026-12-23", "to": "2027-01-03", "announcement": "./vacation_2027-01-04.wav" }
set -e

if [ $# -lt 1 ] || ! [[ "$1" =~ ^[0-9]{4}-([0-9]{2})-([0-9]{2})$ ]]; then
    echo "Usage: $0 <back-date YYYY-MM-DD> [output.wav]"
    echo "  back-date: first day callers can reach us again"
    exit 1
fi

MONTH=$((10#${BASH_REMATCH[1]}))
DAY=$((10#${BASH_REMATCH[2]}))
if [ "$MONTH" -lt 1 ] || [ "$MONTH" -gt 12 ] || [ "$DAY" -lt 1 ] || [ "$DAY" -gt 31 ]; then
    echo "Invalid date: $1"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT="${2:-$SCRIPT_DIR/vacation_$1.wav}"

# Spelled out so TTS reads "od čtvrtého ledna", not a cardinal number.
# Index 0 is unused: days and months start at 1.
DAYS=("" prvního druhého třetího čtvrtého pátého šestého sedmého osmého devátého desátého
    jedenáctého dvanáctého třináctého čtrnáctého patnáctého šestnáctého sedmnáctého osmnáctého devatenáctého dvacátého
    "dvacátého prvního" "dvacátého druhého" "dvacátého třetího" "dvacátého čtvrtého" "dvacátého pátého"
    "dvacátého šestého" "dvacátého sedmého" "dvacátého osmého" "dvacátého devátého" třicátého "třicátého prvního")
MONTHS=("" ledna února března dubna května června července srpna září října listopadu prosince)

BACK="${DAYS[$DAY]} ${MONTHS[$MONTH]}"

"$SCRIPT_DIR/tts.sh" cs "$OUTPUT" \
    "Dobrý den, momentálně jsme na dovolené a váš hovor nemůžeme přijmout. Opět vám budeme k dispozici od ${BACK}. Po zaznění tónu nám prosím zanechte vzkaz, ozveme se vám, jakmile se vrátíme. Kontaktovat nás můžete také prostřednictvím formuláře na našem webu. Děkujeme za pochopení."
