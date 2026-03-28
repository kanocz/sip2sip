# sip2sip

A minimal SIP B2BUA server written in Go, built on [diago](https://github.com/emiago/diago) and [sipgo](https://github.com/emiago/sipgo). Registers on an external SIP server (uplink), accepts incoming calls and distributes them to locally registered phones. Supports outgoing calls via uplink and internal calls between extensions.

## Features

- **SIP client**: registers on an external SIP server to receive incoming calls
- **SIP server (registrar)**: registers 2-5 SIP phones with digest authentication (MD5, with qop=auth support)
- **Incoming call forking**: all registered phones ring simultaneously, the first to answer gets the call
- **Outgoing calls**: numbers with ≤ N digits are routed internally, longer numbers go through the uplink
- **Call recording**: stereo WAV (left channel — one party, right channel — the other)
- **CDR**: JSON file with call details after each call
- **Post-call script**: invokes an external script after each call
- **Debug mode**: `--debug` flag enables full SIP message tracing

## Building

```bash
go build -o sip2sip .
```

## Configuration

Copy the example config:

```bash
cp config.example.json config.json
```

Edit `config.json`:

| Parameter | Description |
|---|---|
| `sip.listen_addr` | Bind address (usually `0.0.0.0`) |
| `sip.listen_port` | SIP port (default `5060`) |
| `sip.external_ip` | Your public (external) IP for NAT traversal |
| `sip.external_port` | External SIP port as seen from internet (default: same as `listen_port`) |
| `sip.rtp_port_min/max` | RTP media port range (default `10000`-`10200`) |
| `uplink.host` | External SIP server hostname |
| `uplink.port` | External SIP server port (usually `5060`) |
| `uplink.username` | SIP account username |
| `uplink.password` | SIP account password |
| `uplink.expiry` | Registration expiry in seconds (default `300`) |
| `users` | List of local extensions `[{"username": "100", "password": "..."}]` |
| `dialplan.internal_max_digits` | Max digits for an internal number (default `3`) |
| `recording.enabled` | Enable call recording (`true`/`false`) |
| `recording.dir` | Directory for WAV and JSON files |
| `recording.announcement` | WAV file to play before recording starts (EU compliance, optional) |
| `post_call.script` | Path to the post-call script (optional) |

## Port Forwarding (NAT)

If the server is behind NAT, the following ports must be forwarded:

| Port | Protocol | Purpose |
|---|---|---|
| `5060` | **UDP** | SIP signaling |
| `10000-10200` | **UDP** | RTP media (audio) |

Make sure `sip.external_ip` in the config points to your public IP.

## Running

```bash
# Normal mode
./sip2sip -config config.json

# Debug mode — full SIP message tracing
./sip2sip -config config.json --debug
```

## How It Works

### Incoming calls

1. sip2sip registers on the uplink SIP server
2. When a call arrives, all registered local phones ring simultaneously
3. The first phone to answer gets the call
4. Audio is bridged between the caller and the answering phone
5. If `recording.announcement` is set, the WAV file is played to both parties (EU recording notification)
6. If recording is enabled, a stereo WAV file is created
6. After hangup, a CDR (JSON) is saved and the post-call script is executed

### Outgoing calls

1. A local phone dials a number
2. If the number has ≤ `internal_max_digits` digits — it's routed to another local extension
3. If longer — the call goes through the uplink to the external network

### Dial plan

The dial plan is simple: the `internal_max_digits` setting (default `3`) determines whether a dialed number is internal or external. For example, dialing `101` rings extension 101 directly, while dialing `+420123456789` goes through the uplink.

## Post-call Script

After each call, the configured script is invoked with two arguments:

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
  "recording_file": "/var/spool/sip2sip/recordings/20260328_120000_a1b2c3d4.wav"
}
```

## SIP Phone Setup

Configure your SIP phone/app (Otwrt, Linphone, MicroSIP, Otwrt, etc.) to register on this server:

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

- **Uplink registration**: custom implementation with manual digest auth for full control
- **Local registrar**: digest authentication (MD5, qop=auth)
- **Call handling**: diago library for media bridging and SDP negotiation
- **Announcement**: optional WAV playback to both parties before recording (EU compliance)
- **Recording**: stereo WAV via diago's `AudioStereoRecordingCreate`
