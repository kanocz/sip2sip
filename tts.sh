#!/bin/bash
# Generate a WAV file from text using Google Translate TTS.
# Usage: ./tts.sh <language> <output.wav> "<text>"
# Requires: pip install gtts, ffmpeg
#
# Examples:
#   ./tts.sh cs unavailable.wav "Dobrý den, momentálně nemůžeme váš hovor přijmout."
#   ./tts.sh en greeting.wav "Hello, please leave a message after the beep."
#   ./tts.sh de closed.wav "Guten Tag, wir sind derzeit nicht erreichbar."
set -e

if [ $# -lt 3 ]; then
    echo "Usage: $0 <language> <output.wav> \"<text>\""
    echo "  language: cs, en, de, sk, pl, etc. (Google TTS language code)"
    echo "  output.wav: output WAV file path"
    echo "  text: text to synthesize"
    exit 1
fi

LANG_CODE="$1"
OUTPUT="$2"
TEXT="$3"

# Check dependencies
command -v python3 >/dev/null 2>&1 || { echo "Error: python3 is required"; exit 1; }
command -v ffmpeg >/dev/null 2>&1 || { echo "Error: ffmpeg is required"; exit 1; }
python3 -c "import gtts" 2>/dev/null || { echo "Error: gtts is required (pip install gtts)"; exit 1; }

TMPFILE=$(mktemp /tmp/tts-XXXXXX.mp3)
trap "rm -f '$TMPFILE'" EXIT

python3 -c "
from gtts import gTTS
import sys
tts = gTTS(sys.argv[1], lang=sys.argv[2])
tts.save(sys.argv[3])
" "$TEXT" "$LANG_CODE" "$TMPFILE"

ffmpeg -y -i "$TMPFILE" -ar 8000 -ac 1 -sample_fmt s16 -acodec pcm_s16le "$OUTPUT" 2>/dev/null

echo "Generated: $OUTPUT ($(du -h "$OUTPUT" | cut -f1))"
