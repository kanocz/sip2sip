#!/bin/bash
# Generate default Czech TTS audio files for sip2sip voicemail.
# Uses tts.sh helper. Requires: pip install gtts, ffmpeg
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

echo "Generating Czech TTS audio files..."

./tts.sh cs unavailable.wav \
    "Dobrý den, momentálně nemůžeme váš hovor přijmout. Zavolejte nám prosím později, nebo nám po zaznění tónu zanechte vzkaz. Kontaktovat nás můžete také prostřednictvím formuláře na našem webu. Děkujeme za pochopení."

./tts.sh cs outside_hours.wav \
    "Dobrý den, dovolali jste se mimo naši pracovní dobu. Jsme vám k dispozici od pondělí do pátku mezi desátou a sedmnáctou hodinou. Zavolejte nám prosím v této době, nebo nám po zaznění tónu zanechte vzkaz. Kontaktovat nás můžete také prostřednictvím formuláře na našem webu. Děkujeme za pochopení."

./tts.sh cs vacation.wav \
    "Dobrý den, momentálně jsme na dovolené a váš hovor nemůžeme přijmout. Po zaznění tónu nám prosím zanechte vzkaz, ozveme se vám, jakmile se vrátíme. Kontaktovat nás můžete také prostřednictvím formuláře na našem webu. Děkujeme za pochopení."

echo "Done!"
