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
- **Outgoing calls**: short numbers routed internally, longer numbers go through the uplink
- **CDR**: JSON file with call details after each call
- **Post-call script**: invokes an external script after each call
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
sudo cp sip2sip announcement.wav /opt/sip2sip/
sudo cp config.example.json /opt/sip2sip/config.json
sudo nano /opt/sip2sip/config.json  # edit config

# Install service
sudo cp sip2sip.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sip2sip

# Check status / logs
sudo systemctl status sip2sip
sudo journalctl -u sip2sip -f
```

## How It Works

### Incoming call filtering

Incoming INVITEs from non-local users are checked against two optional filters:

- **`filter_called_no`** (recommended): rejects calls where the To number doesn't match `uplink.username`. Blocks SIP spam/scanning attempts targeting random numbers.
- **`filter_source_ip`**: rejects calls not originating from the uplink server's resolved IP.

Both filters are independent and can be combined.

### Incoming calls

1. sip2sip registers on the uplink SIP server
2. Incoming INVITE is checked against configured filters
3. All registered local phones ring simultaneously
4. The first phone to answer gets the call
5. Symmetric RTP (NAT traversal) is enabled — mobile clients behind carrier NAT work correctly

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
  "hangup_by": "caller",
  "recording_file": "/opt/sip2sip/recordings/20260328_120000_a1b2c3d4.wav"
}
```

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
