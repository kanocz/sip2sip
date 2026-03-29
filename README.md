# sip2sip

A minimal SIP B2BUA server written in Go, built on [diago](https://github.com/emiago/diago) and [sipgo](https://github.com/emiago/sipgo). Registers on an external SIP server (uplink), accepts incoming calls and distributes them to locally registered phones. Supports outgoing calls via uplink and internal calls between extensions.

## Features

- **SIP client**: registers on an external SIP server to receive incoming calls
- **SIP server (registrar)**: registers 2-5 SIP phones with digest authentication (MD5, qop=auth)
- **Incoming call filtering**: reject SIP spam by called number and/or source IP
- **Incoming call forking**: all registered phones ring simultaneously, first to answer wins
- **NAT traversal**: symmetric RTP for mobile clients behind carrier NAT
- **Answer-first mode**: answer caller → play announcement → ringback → ring phones
- **Call recording**: stereo WAV (left channel — one party, right channel — the other)
- **Recording announcement**: EU-compliant notification played to both parties before recording
- **Business hours**: accept calls only during configured days and hours
- **Voicemail**: announcement + beep + recording when no operators available or outside business hours
- **Hot reload**: `SIGHUP` / `systemctl reload` to apply config changes without restart
- **Outgoing calls**: short numbers routed internally, longer numbers go through the uplink
- **CDR**: JSON file with call details after each call
- **Post-call script**: invokes an external script after each call
- **TTS audio generation**: helper script to generate announcements via Google TTS
- **Debug mode**: `--debug` flag enables full SIP message tracing

## Building

```bash
go build -o sip2sip .
```

## Configuration

```bash
cp config.example.json config.json
```

### SIP transport

| Parameter | Description | Default |
|---|---|---|
| `sip.listen_addr` | Bind address | `0.0.0.0` |
| `sip.listen_port` | SIP port | `5060` |
| `sip.external_ip` | Public IP for NAT traversal | — |
| `sip.external_port` | External SIP port as seen from internet | `listen_port` |
| `sip.rtp_port_min` | RTP media port range start | `10000` |
| `sip.rtp_port_max` | RTP media port range end | `10200` |

### Uplink (external SIP server)

| Parameter | Description | Default |
|---|---|---|
| `uplink.host` | SIP server hostname | — (required) |
| `uplink.port` | SIP server port | `5060` |
| `uplink.username` | SIP account username | — (required) |
| `uplink.password` | SIP account password | — |
| `uplink.expiry` | Registration expiry (seconds) | `300` |
| `uplink.filter_called_no` | Only accept calls addressed to our registered number | `false` |
| `uplink.filter_source_ip` | Only accept calls from the uplink server's IP | `false` |

### Local extensions

| Parameter | Description | Default |
|---|---|---|
| `users` | Array of `{"username": "100", "password": "..."}` | — (required) |
| `dialplan.internal_max_digits` | Max digits for an internal number | `3` |

### Recording

| Parameter | Description | Default |
|---|---|---|
| `recording.enabled` | Enable call recording | `false` |
| `recording.dir` | Directory for WAV and JSON files | — |
| `recording.announcement` | WAV file played before recording (EU compliance) | — |
| `recording.answer_first` | Answer caller before ringing phones (announcement + ringback) | `false` |

### Post-call

| Parameter | Description | Default |
|---|---|---|
| `post_call.script` | Script called after each call | — |

### Voicemail

When no operators are connected or the call is outside business hours, sip2sip can play an announcement and record a voicemail message.

| Parameter | Description | Default |
|---|---|---|
| `voicemail.enabled` | Enable voicemail recording after announcement | `false` |
| `voicemail.unavailable_announcement` | WAV file played when no operators are connected | — |
| `voicemail.silence_timeout` | Seconds of silence before stopping recording | `10` |
| `voicemail.max_duration` | Maximum recording duration in seconds | `120` |

The voicemail flow: answer → play announcement → beep tone → record caller → stop on silence timeout or max duration. Voicemail recordings are saved to `recording.dir` as mono WAV (8kHz 16-bit PCM, G.711 decoded).

If `voicemail.enabled` is `false` but an announcement is configured, only the announcement is played (no recording).

The CDR JSON includes `"voicemail": true` and `"message_duration_seconds"` for voicemail calls, so post-call scripts can handle them accordingly.

### Business hours

| Parameter | Description | Default |
|---|---|---|
| `business_hours.enabled` | Enable business hours checking | `false` |
| `business_hours.timezone` | IANA timezone (e.g. `Europe/Prague`) | `UTC` |
| `business_hours.days` | Accepted days of week | — |
| `business_hours.start_time` | Start time (`HH:MM`) | — |
| `business_hours.end_time` | End time (`HH:MM`) | — |
| `business_hours.outside_hours_announcement` | WAV file played outside business hours | — |

Day names use 3-letter English abbreviations: `mon`, `tue`, `wed`, `thu`, `fri`, `sat`, `sun`.

## Generating Audio Announcements

Use the `tts.sh` helper to generate WAV files from text using Google Translate TTS:

```bash
# Requires: pip install gtts, ffmpeg
./tts.sh <language> <output.wav> "<text>"

# Examples:
./tts.sh cs unavailable.wav "Dobrý den, momentálně nemůžeme váš hovor přijmout."
./tts.sh en greeting.wav "Hello, please leave a message after the beep."
./tts.sh de closed.wav "Guten Tag, wir sind derzeit nicht erreichbar."
```

Generate the default Czech announcements:

```bash
./generate_audio.sh
```

## Port Forwarding (NAT)

If the server is behind NAT, forward these ports:

| Port | Protocol | Purpose |
|---|---|---|
| `5060` | **UDP** | SIP signaling |
| `10000-10200` | **UDP** | RTP media (audio) |

Set `sip.external_ip` to your public IP.

## Running

```bash
# Normal mode
./sip2sip -config config.json

# Debug mode — full SIP message tracing
./sip2sip -config config.json --debug
```

## Installation (systemd)

```bash
# Copy files
sudo mkdir -p /opt/sip2sip/recordings
sudo cp sip2sip announcement.wav unavailable.wav outside_hours.wav /opt/sip2sip/
sudo cp config.example.json /opt/sip2sip/config.json
sudo nano /opt/sip2sip/config.json  # edit config

# Install service
sudo cp sip2sip.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sip2sip

# Check status / logs
sudo systemctl status sip2sip
sudo journalctl -u sip2sip -f

# Reload config without restart (applies users, voicemail, business hours, recording, dialplan)
sudo systemctl reload sip2sip
```

### Config hot reload

Sending `SIGHUP` (or `systemctl reload sip2sip`) reloads the configuration file without dropping active calls. The following settings are applied immediately:

- User list (extensions, passwords)
- Recording settings
- Voicemail settings
- Business hours schedule
- Dialplan
- Uplink call filters

Settings that require a restart: SIP listen address/port, external IP/port, RTP port range, uplink host/credentials.

## How It Works

### Incoming call filtering

Incoming INVITEs from non-local users are checked against two optional filters:

- **`filter_called_no`** (recommended): rejects calls where the To number doesn't match `uplink.username`. Blocks SIP spam/scanning attempts targeting random numbers.
- **`filter_source_ip`**: rejects calls not originating from the uplink server's resolved IP.

Both filters are independent and can be combined.

### Incoming calls

1. sip2sip registers on the uplink SIP server
2. Incoming INVITE is checked against configured filters
3. Business hours are checked (if enabled) — outside hours → voicemail with `outside_hours_announcement`
4. Registered phones are checked — if none connected → voicemail with `unavailable_announcement`
5. All registered local phones ring simultaneously
6. The first phone to answer gets the call
7. Symmetric RTP (NAT traversal) is enabled — mobile clients behind carrier NAT work correctly

**Normal mode** (`answer_first: false`):
- Caller hears ringback from the uplink while phones ring
- After a phone answers: announcement plays to both parties → bridge + recording

**Answer-first mode** (`answer_first: true`):
- Caller is answered immediately
- Announcement plays to the caller
- Caller hears ringback tone while phones ring
- After a phone answers: announcement plays to the answering phone → bridge + recording

### Outgoing calls

1. A local phone dials a number
2. If the number has ≤ `internal_max_digits` digits — routed to another local extension
3. If longer — the call goes through the uplink to the external network

### Dial plan

The `internal_max_digits` setting (default `3`) determines routing. Dialing `101` rings extension 101 directly; dialing `+420123456789` goes through the uplink.

## Post-call Script

After each call, the configured script is invoked:

```bash
/path/to/script.sh /path/to/call.json /path/to/recording.wav
```

If recording is disabled or the call was not answered, `none` is passed instead of the WAV path.

### CDR Format (JSON)

Regular call:
```json
{
  "call_id": "a1b2c3d4",
  "direction": "incoming",
  "caller_number": "+420123456789",
  "called_number": "100",
  "answered_by": "101",
  "start_time": "2026-03-28T12:00:00Z",
  "answer_time": "2026-03-28T12:00:05Z",
  "end_time": "2026-03-28T12:05:00Z",
  "duration_seconds": 300,
  "talk_time_seconds": 295,
  "answered": true,
  "voicemail": false,
  "hangup_by": "caller",
  "recording_file": "/opt/sip2sip/recordings/20260328_120000_a1b2c3d4.wav"
}
```

Voicemail:
```json
{
  "call_id": "b2c3d4e5",
  "direction": "incoming",
  "caller_number": "+420123456789",
  "called_number": "100",
  "answered_by": "voicemail",
  "start_time": "2026-03-28T18:30:00Z",
  "answer_time": "2026-03-28T18:30:01Z",
  "end_time": "2026-03-28T18:30:35Z",
  "duration_seconds": 35,
  "talk_time_seconds": 34,
  "answered": true,
  "voicemail": true,
  "message_duration_seconds": 12.5,
  "hangup_by": "voicemail",
  "recording_file": "/opt/sip2sip/recordings/vm_20260328_183000_b2c3d4e5.wav"
}
```

## Notification Helpers

Ready-made post-call scripts in `helpers/` — send call summary + recording to a messenger.

### Telegram

Sends text summary + voice message via Telegram Bot API. Pure shell, no dependencies beyond `curl`, `jq`, `ffmpeg`.

```bash
cp helpers/telegram_notify.conf.example helpers/telegram_notify.conf
nano helpers/telegram_notify.conf  # set BOT_TOKEN and CHAT_ID
```

Set in config.json:
```json
"post_call": {
  "script": "/opt/sip2sip/helpers/telegram_notify.sh"
}
```

### WhatsApp

Same flow via Meta WhatsApp Cloud API. Requires a [Meta Business account](https://business.facebook.com) and a WhatsApp Business app.

```bash
cp helpers/whatsapp_notify.conf.example helpers/whatsapp_notify.conf
nano helpers/whatsapp_notify.conf  # set TOKEN, PHONE_ID, recipient
```

Key differences from Telegram:
- Requires business account verification by Meta
- Recipient must message your number first (24h session window) or you need pre-approved templates
- Free tier: 1000 service conversations/month

## SIP Phone Setup

Configure your SIP phone/app (Linphone, MicroSIP, Otwrt, etc.):

| Setting | Value |
|---|---|
| **SIP Server / Registrar** | Machine's IP address (or public IP for mobile clients) |
| **SIP Port** | `5060` |
| **Username** | Extension number (e.g. `100`) |
| **Password** | Password from config |
| **Transport** | UDP |

## Codecs

G.711 (PCMU/PCMA) is supported. The codec is negotiated automatically between the incoming and outgoing legs without transcoding.

## Architecture

```
                    ┌──────────────────────────────┐
  Uplink SIP ◄────►│         sip2sip              │◄────► SIP Phone 100
  Server           │                              │◄────► SIP Phone 101
  (provider)       │  registrar + B2BUA + recorder│◄────► SIP Phone 102
                    └──────────────────────────────┘
```

- **Uplink registration**: custom implementation with manual digest auth
- **Local registrar**: digest authentication (MD5, qop=auth)
- **Call handling**: diago library for media bridging and SDP negotiation
- **NAT traversal**: symmetric RTP on all media legs
- **Announcement**: WAV playback to both parties before recording (EU compliance)
- **Recording**: stereo WAV via diago's `AudioStereoRecordingCreate`
- **Voicemail**: announcement + beep + silence-detected recording
- **Business hours**: timezone-aware day/time checking
- **Hot reload**: atomic config swap on `SIGHUP`
