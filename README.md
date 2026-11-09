# sip2sip

A minimal SIP B2BUA server written in Go, built on [diago](https://github.com/emiago/diago) and [sipgo](https://github.com/emiago/sipgo). Registers on an external SIP server (uplink), accepts incoming calls and distributes them to locally registered phones. Supports outgoing calls via uplink and internal calls between extensions.

## Features

- **SIP client**: registers on an external SIP server to receive incoming calls
- **SIP server (registrar)**: registers 2-5 SIP phones with digest authentication (MD5); outgoing INVITEs from extensions are authenticated the same way
- **Brute-force guard**: a source IP that tries too many different logins or passwords is blocked for an hour; a phone retrying one wrong password is not. Failures are logged with the IP for fail2ban
- **Incoming call filtering**: only the uplink server's IP may deliver calls; optionally also by called number
- **Call limit**: `max_calls` concurrent calls in both directions, the rest get 486 Busy Here
- **Incoming call forking**: all registered phones ring simultaneously, first to answer wins
- **NAT traversal**: symmetric RTP for mobile clients behind carrier NAT
- **Answer-first mode**: answer caller → play announcement → ringback → ring phones
- **Call recording**: stereo WAV (left channel — one party, right channel — the other)
- **Recording announcement**: EU-compliant notification played to both parties before recording
- **Business hours**: accept calls only during configured days and hours
- **Vacation mode**: send every incoming call to voicemail during configured date ranges
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
| `sip.listen_port` | SIP port, UDP and TCP | `5060` |
| `sip.external_ip` | Public IP for NAT traversal | — |
| `sip.external_port` | External SIP port as seen from internet | `listen_port` |
| `sip.tls_port` | SIP over TLS port, listened on when `tls_cert` is set; also the external port for TLS | `5061` |
| `sip.tls_cert` | PEM certificate chain for the TLS listener; empty means no TLS listener | — |
| `sip.tls_key` | PEM private key for `tls_cert` | — |
| `sip.rtp_port_min` | RTP media port range start | `10000` |
| `sip.rtp_port_max` | RTP media port range end | `10200` |

UDP and TCP are always on. Phones pick one in their account settings; a phone registered over TCP is called back over the same connection, so a phone behind NAT keeps working as long as its periodic REGISTER keeps the connection open. RTP stays on UDP whatever the SIP transport.

### TLS

The certificate is an ordinary X.509 pair, the same kind an HTTPS server uses; Let's Encrypt works. Phones connect by the hostname the certificate is issued for, on `tls_port`, and verify it against their CA store. The files must be readable by the `sip2sip` user, which Let's Encrypt's `/etc/letsencrypt/archive` is not by default.

The files are checked once an hour and on `SIGHUP`; when either changed, the pair is loaded again without a restart. A pair that fails to load is logged and the previous one stays in use.

Talking TLS to the provider (`uplink.transport: tls`) also needs `tls_cert` and `tls_key`: outgoing calls go through diago, which only has a TLS transport when it listens on one. The provider's certificate is verified against the system CA store. Contacts on the TLS transport, ours in REGISTER and diago's in dialogs, are `sips:` URIs, which mandate TLS on their own. They name `external_ip`, not the certificate's hostname, so incoming TLS traffic relies on the peer reusing the connection we or the phone opened; a peer that opens a fresh connection to the Contact and verifies the certificate against the IP will fail. Note for setups behind NAT: Contact headers built for TCP carry no `transport=` parameter when `listen_addr` differs from `external_ip` (a diago limitation), so a BYE from the other side can arrive over UDP 5060; that listener is always on, so the call still ends correctly.

One sipgo client serves every transport, so the Via header on TLS requests names `external_port`, not `tls_port`. Responses on TCP and TLS come back on the connection the request went out on, so this only matters to a peer that opens a new connection to the Via address, which would land on TCP `external_port` without TLS.

With `uplink.transport` set to `tcp` or `tls`, the Contact we register on the provider names `external_port` (or `tls_port`), not the ephemeral port of our outgoing connection. A provider that reuses our connection for incoming INVITEs needs nothing else; one that opens its own connection reaches us on the forwarded port.

### Uplink (external SIP server)

| Parameter | Description | Default |
|---|---|---|
| `uplink.host` | SIP server hostname | — (required) |
| `uplink.port` | SIP server port. Omitted, the transport default applies: `5060`, or `5061` for `tls`. The example sets `5060`, so remove or change it when switching to `tls` | transport default |
| `uplink.username` | SIP account username | — (required) |
| `uplink.password` | SIP account password | — |
| `uplink.transport` | `udp`, `tcp` or `tls`. With `tcp` or `tls`, an INVITE from the provider's IP over UDP is not trusted: a UDP source address can be forged, a TCP connection cannot | `udp` |
| `uplink.expiry` | Registration expiry (seconds) | `300` |
| `uplink.filter_called_no` | Only accept calls addressed to our registered number | `false` |

### Local extensions

| Parameter | Description | Default |
|---|---|---|
| `users` | Array of `{"username": "100", "password": "..."}` | — (required) |
| `max_calls` | Concurrent calls, incoming and outgoing together; above it INVITEs get 486 Busy Here | `3` |
| `dialplan.internal_max_digits` | Max digits for an internal number | `3` |

A phone may ask for any registration lifetime; the server grants at most 120 seconds and says so in the 200 OK. The periodic REGISTER keeps the NAT binding of a mobile phone open, which a one-hour lifetime would let expire.

### Auth guard

Every REGISTER, and every INVITE that does not come from the uplink's IP, must pass digest authentication. The digest must sign the Request-URI itself (user and host), so an INVITE signed for one number cannot be redialled to another by rewriting the request line. A source IP that tries too many *different* wrong guesses is blocked. A guess is a login/password pair: an answer for an extension that does not exist is one guess per login, a wrong password is one guess per password. Retrying the same wrong credentials counts once, so a phone configured with a typo can retry for ten minutes and its owner is not locked out when the typo is fixed.

An unknown login gets the same 401 challenge as a real one, so the response code does not reveal which extensions exist. Only an answer to a challenge we actually issued is counted: SIP over UDP lets anyone forge the source address, and a forger never sees the nonce, so forged packets cannot get a real phone's IP blocked.

To make the password visible as "same" or "different", the challenge for a failing IP+user keeps the same nonce for up to 5 minutes and offers no `qop`; a successful answer deletes its nonce, so a captured valid response cannot be replayed.

| Parameter | Description | Default |
|---|---|---|
| `auth_guard.enabled` | Block IPs after repeated auth failures | `true` |
| `auth_guard.max_guesses` | Distinct wrong guesses per IP within the window that trigger a block | `10` |
| `auth_guard.window` | Window length, seconds | `300` |
| `auth_guard.block_time` | Block duration, seconds | `3600` |

Each failure is logged as one line, and so is the block:

```
level=WARN msg="Auth failed" method=REGISTER user=100 source=203.0.113.9:5060 reason="bad credentials"
level=WARN msg="Auth failed" method=INVITE user=999 source=203.0.113.9:5060 reason="unknown user"
level=WARN msg="Source IP blocked after repeated auth failures" source=203.0.113.9 until=...
```

A fail2ban filter can match `msg="Auth failed" .* source=<HOST>:`.

### Recording

| Parameter | Description | Default |
|---|---|---|
| `recording.enabled` | Enable call recording | `false` |
| `recording.dir` | Directory for recordings, voicemail and CDR JSON files, used even when recording is off | `./recordings` |
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

The CDR JSON includes `"voicemail": true`, `"voicemail_reason"` and `"message_duration_seconds"` for voicemail calls, so post-call scripts can handle them accordingly. `voicemail_reason` is one of:

| Value | When |
|---|---|
| `vacation` | Call came in during a `vacation.periods` range |
| `outside_hours` | Call came in outside `business_hours` |
| `unavailable` | No phones were registered |

### Business hours

| Parameter | Description | Default |
|---|---|---|
| `business_hours.enabled` | Enable business hours checking | `false` |
| `business_hours.timezone` | IANA timezone (e.g. `Europe/Prague`) | `UTC` |
| `business_hours.days` | Accepted days of week | — |
| `business_hours.start_time` | Start time (`HH:MM`) | — |
| `business_hours.end_time` | End time (`HH:MM`) | — |
| `business_hours.outside_hours_announcement` | WAV file played outside business hours | — |

Day names use 3-letter English abbreviations: `mon`, `tue`, `wed`, `thu`, `fri`, `sat`, `sun`. A schedule that cannot be parsed (unknown day, time not `HH:MM`, end not after start) is logged at startup and the schedule is disabled, so calls keep ringing instead of all going to voicemail. An unknown timezone is logged and replaced with UTC. Missing announcement files are logged too.

### Vacation

| Parameter | Description | Default |
|---|---|---|
| `vacation.announcement` | Default WAV file played during vacation | — |
| `vacation.periods` | Array of `{"from": "YYYY-MM-DD", "to": "YYYY-MM-DD", "announcement": "..."}` | `[]` |
| `vacation.periods[].announcement` | WAV file for this period, overrides `vacation.announcement` | — |

During any period every incoming call skips the phones and goes to voicemail, the same flow as outside business hours. The period's own `announcement` is played if set, `vacation.announcement` otherwise. If periods overlap, the first one listed wins. Both dates are inclusive and use `business_hours.timezone`, even when `business_hours.enabled` is `false`. A message is recorded only if `voicemail.enabled` is `true`.

```json
"vacation": {
  "announcement": "./vacation.wav",
  "periods": [
    { "from": "2026-12-23", "to": "2027-01-03", "announcement": "./vacation_2027-01-04.wav" },
    { "from": "2027-08-02", "to": "2027-08-13" }
  ]
}
```

An empty or missing `periods` list turns vacation mode off. A malformed date, or `to` earlier than `from`, fails config loading; on `SIGHUP` the old config stays active.

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

Generate a vacation announcement that tells callers when they can call again (the first day back, not the last day of vacation):

```bash
./generate_audio_vacation.sh 2027-01-04
# -> vacation_2027-01-04.wav: "... Opět vám budeme k dispozici od čtvrtého ledna. ..."
./generate_audio_vacation.sh 2027-01-04 /opt/sip2sip/xmas.wav   # custom output path
```

Use the file as `announcement` of the matching period in `vacation.periods`.

## Port Forwarding (NAT)

If the server is behind NAT, forward these ports:

| Port | Protocol | Purpose |
|---|---|---|
| `5060` | **UDP** | SIP signaling |
| `5060` | **TCP** | SIP signaling |
| `5061` | **TCP** | SIP over TLS, only if `sip.tls_cert` is set |
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
# Service user: the unit runs as sip2sip, not root
sudo useradd -r -s /usr/sbin/nologin -d /opt/sip2sip sip2sip

# Copy files
sudo mkdir -p /opt/sip2sip/recordings
sudo cp sip2sip announcement.wav unavailable.wav outside_hours.wav vacation.wav /opt/sip2sip/
sudo cp config.example.json /opt/sip2sip/config.json
sudo nano /opt/sip2sip/config.json  # edit config

# config.json holds SIP passwords, recordings hold call audio
sudo chown -R sip2sip:sip2sip /opt/sip2sip
sudo chmod 600 /opt/sip2sip/config.json
sudo chmod 700 /opt/sip2sip/recordings

# Install service
sudo cp sip2sip.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sip2sip

# Check status / logs
sudo systemctl status sip2sip
sudo journalctl -u sip2sip -f

# Reload config without restart (applies users, voicemail, business hours, vacation, recording, dialplan)
sudo systemctl reload sip2sip
```

### Config hot reload

Sending `SIGHUP` (or `systemctl reload sip2sip`) reloads the configuration file without dropping active calls. The following settings are applied immediately:

- User list (extensions, passwords)
- Recording settings
- Voicemail settings
- Business hours schedule
- Vacation periods
- Dialplan
- `max_calls`
- `uplink.filter_called_no` (the uplink host is also re-resolved on reload and once an hour)
- Auth guard

The `sip` section and the uplink host, port and credentials are fixed at startup. If they differ in the reloaded file, a warning is logged and the old values stay, so outgoing calls keep using the account the registration was made with.

## How It Works

### Incoming call filtering

An INVITE is accepted from two places only:

- **The uplink server's IP** (`uplink.host`, resolved at startup, on reload and once an hour), treated as an incoming call. With `uplink.transport` set to `tcp` or `tls`, an INVITE from that IP over UDP is challenged like a phone instead and logged with `over UDP while uplink.transport is not udp`. With **`filter_called_no`** (recommended) a call whose To number is not `uplink.username` is rejected with 404, which stops scanners dialing random numbers. If the provider sends INVITEs from a server other than `uplink.host`, they are challenged instead of connected; the log line `Incoming INVITE ... uplink=false` with the provider's address is what to look for when incoming calls stop.
- **Anywhere else**: challenged with digest auth (401) and treated as an outgoing call from the extension named in From. Without the extension's password it goes nowhere.

An uplink call whose caller number happens to equal a local extension is logged and still treated as incoming.

### Incoming calls

1. sip2sip registers on the uplink SIP server
2. Incoming INVITE is checked: it must come from the uplink's IP, and optionally be addressed to our number
3. Vacation periods are checked: during a period → voicemail with `vacation.announcement`
4. Business hours are checked (if enabled) — outside hours → voicemail with `outside_hours_announcement`
5. Registered phones are checked — if none connected → voicemail with `unavailable_announcement`
6. All registered local phones ring simultaneously
7. The first phone to answer gets the call
8. Symmetric RTP (NAT traversal) is enabled — mobile clients behind carrier NAT work correctly

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

If recording is disabled or the call was not answered, `none` is passed instead of the WAV path. The script is killed after 5 minutes.

When a recording or voicemail could not be written (disk full, for one), the CDR carries an `"error"` field with the reason and no `recording_file`; the bundled helpers put it on the first line of the message.

`caller_number` and `called_number` in the CDR contain only digits, `+`, `-` and brackets; anything else from the SIP From/To header is dropped before the file is written. CDR and WAV files are created with mode 0600 in a 0700 directory.

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

`hangup_by` is `caller`, `callee`, `timeout` (nobody answered), `voicemail` or `system` (sip2sip ended the call itself, for example after a failed answer).

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
  "voicemail_reason": "outside_hours",
  "message_duration_seconds": 12.5,
  "error": "voicemail recording failed: write /opt/sip2sip/recordings/vm_20260328_183000_b2c3d4e5.wav: no space left on device",
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
chmod 600 helpers/telegram_notify.conf
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
| **Transport** | UDP or TCP; TLS when `sip.tls_cert` is set (then use the certificate's hostname and `tls_port`) |

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
- **Local registrar**: digest authentication (MD5, single-use nonces)
- **Call handling**: diago library for media bridging and SDP negotiation
- **NAT traversal**: symmetric RTP on all media legs
- **Announcement**: WAV playback to both parties before recording (EU compliance)
- **Recording**: stereo WAV via diago's `AudioStereoRecordingCreate`
- **Voicemail**: announcement + beep + silence-detected recording
- **Business hours**: timezone-aware day/time checking
- **Hot reload**: atomic config swap on `SIGHUP`
