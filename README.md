# nvrfetch

**Author:** Abhishek Kumar Rabidas

[![LinkedIn](https://img.shields.io/badge/LinkedIn-abhishek--kumar--rabidas-0A66C2?logo=linkedin&logoColor=white)](https://www.linkedin.com/in/abhishek-kumar-rabidas-428760166)
[![GitHub](https://img.shields.io/badge/GitHub-abhishek--rabidas-181717?logo=github&logoColor=white)](https://github.com/abhishek-rabidas)

Download recorded NVR footage for a camera channel and time range as an **MP4**, using **ONVIF Profile G** (recording search + replay).

One binary, two modes:

- **CLI** — one-off downloads: `nvrfetch -ip ... -start ... -end ...`
- **HTTP API** — `nvrfetch serve`, then `POST /api/v1/footage` returns the MP4

Works with any NVR that implements ONVIF Profile G (Recording, Search and Replay services).

---

## Contents

1. [Requirements](#requirements)
2. [Install / build](#install--build)
3. [Quick start](#quick-start)
4. [CLI usage](#cli-usage)
5. [HTTP API usage](#http-api-usage)
6. [Channels and recording tokens](#channels-and-recording-tokens)
7. [Time and timezones](#time-and-timezones)
8. [Fast mode](#fast-mode)
9. [Troubleshooting](#troubleshooting)
10. [Security notes](#security-notes)
11. [Development](#development)

---

## Requirements

| Requirement | Notes |
|---|---|
| **ffmpeg ≥ 5** | Must be on `PATH`, or pass its path with `-ffmpeg`. Used to remux the stream into MP4. |
| **NVR with ONVIF Profile G** | ONVIF must be enabled on the NVR, with a user that is allowed to use ONVIF. |
| **Network access** | To the NVR's ONVIF HTTP port (default `80`) **and** its RTSP port (usually `554`). |
| Go (build only) | Version in `go.mod`. Not needed to run a prebuilt binary. |

Install ffmpeg:

```sh
brew install ffmpeg            # macOS
sudo apt install ffmpeg        # Debian/Ubuntu
winget install Gyan.FFmpeg     # Windows (or download from ffmpeg.org and add bin\ to PATH)
```

> On many NVRs ONVIF is disabled by default, or requires a separate ONVIF user. Enable it in the NVR web UI (usually under *Network → Advanced → Integration Protocol* or similar).

---

## Install / build

```sh
# Local build (current OS/arch)
go build -o nvrfetch .

# Windows binaries → dist/nvrfetch-windows-amd64.exe, dist/nvrfetch-windows-arm64.exe
make windows

# Print version
./nvrfetch version
```

Version is injected at build time: `go build -ldflags "-X main.version=1.2.3" -o nvrfetch .` (the Makefile uses `git describe`).

---

## Quick start

```sh
# 1. See which channels the NVR exposes
./nvrfetch -list -ip 192.168.1.50 -user admin -pass 'secret'

# 2. Download one minute of channel 2
./nvrfetch -ip 192.168.1.50 -user admin -pass 'secret' \
  -channel 2 -start "2026-11-20 10:00:00" -end "2026-11-20 10:01:00"
# → footage_ch2_20261120_100000.mp4
```

---

## CLI usage

```
nvrfetch -ip <host> [options] -start "<time>" -end "<time>"
nvrfetch -ip <host> [options] -list
nvrfetch serve [options]
nvrfetch version
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `-ip` | *(required)* | NVR IP address or hostname. |
| `-port` | `80` | NVR ONVIF **HTTP** port (not the RTSP port). |
| `-user` | `admin` | NVR username. |
| `-pass` | `$NVR_PASSWORD` | NVR password. Prefer the env var to keep it out of shell history. |
| `-list` | `false` | List recordings (channels) on the NVR and exit. |
| `-channel` | `1` | Camera channel number, 1-based. See [Channels](#channels-and-recording-tokens). |
| `-recording` | | ONVIF recording token; overrides `-channel`. Get it from `-list`. |
| `-start` | *(required)* | Start time, wall-clock. See [Time formats](#time-and-timezones). |
| `-end` | *(required)* | End time, wall-clock. Must be after `-start`. |
| `-tz` | NVR's timezone | Timezone of `-start`/`-end`: empty = NVR's, `Local` = this machine's, or an IANA name like `Asia/Kolkata`. |
| `-out` | `footage_ch<N>_<start>.mp4` | Output MP4 path. |
| `-fast` | `false` | Ask the NVR to stream faster than real time. See [Fast mode](#fast-mode). |
| `-ffmpeg` | `ffmpeg` | Path to the ffmpeg binary. |

### Examples

```sh
# Password via environment variable
export NVR_PASSWORD='secret'
./nvrfetch -ip 192.168.1.50 -channel 3 -start "2026-11-20 08:30:00" -end "2026-11-20 08:45:00"

# NVR with ONVIF on a non-standard port, custom output file
./nvrfetch -ip nvr.example.lan -port 8000 -channel 1 \
  -start "2026-11-20 08:30" -end "2026-11-20 08:35" -out gate.mp4

# Times given in IST regardless of what the NVR is set to
./nvrfetch -ip 192.168.1.50 -tz Asia/Kolkata -channel 1 \
  -start "2026-11-20 10:00:00" -end "2026-11-20 10:10:00"

# Select by recording token instead of channel number
./nvrfetch -ip 192.168.1.50 -recording "RecordMediaProfile_Channel2_MainStream" \
  -start "2026-11-20 10:00:00" -end "2026-11-20 10:05:00"

# Fast download, ffmpeg not on PATH
./nvrfetch -ip 192.168.1.50 -channel 2 -fast -ffmpeg /opt/ffmpeg/bin/ffmpeg \
  -start "2026-11-20 10:00:00" -end "2026-11-20 11:00:00"
```

Windows (`cmd`):

```bat
set NVR_PASSWORD=secret
nvrfetch-windows-amd64.exe -ip 192.168.1.50 -channel 2 -start "2026-11-20 10:00:00" -end "2026-11-20 10:01:00"
```

Press **Ctrl+C** to stop early — the MP4 is finalized with whatever was received so far.

### Output

- Video is copied as-is (no re-encode, so no quality loss and low CPU).
- Audio, if present, is transcoded to AAC (NVR G.711 audio is not allowed in MP4).
- The file uses `+faststart`, so it plays in browsers before fully downloading.
- Duration is capped at `end − start`.

---

## HTTP API usage

### Start the server

```sh
./nvrfetch serve -addr :8080 -api-key 'change-me'
```

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8080` | Listen address. |
| `-api-key` | `$NVR_API_KEY` | If set, every `/api/v1/*` request must send `X-API-Key: <key>`. |
| `-max-duration` | `1h` | Longest allowed `end − start` per request (Go duration: `30m`, `2h`). |
| `-max-concurrent` | `4` | Max simultaneous downloads (each runs one ffmpeg). Extra requests get `429`. |
| `-tmp-dir` | OS temp dir | Where MP4s are staged before sending. Deleted after each response. |

The server shuts down gracefully on Ctrl+C / SIGTERM (up to 30 s for in-flight requests).

### Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/healthz` | Liveness check → `204 No Content`. No auth. |
| `POST` | `/api/v1/recordings` | List channels / recording tokens on an NVR. |
| `POST` | `/api/v1/footage` | Download footage → `video/mp4` attachment. |

### `POST /api/v1/recordings`

Request body:

```json
{ "ip": "192.168.1.50", "port": 80, "username": "admin", "password": "secret" }
```

Response:

```json
[
  { "channel": 1, "token": "Rec_1", "source_name": "Front Gate", "source_id": "SRC_1", "description": "..." },
  { "channel": 2, "token": "Rec_2", "source_name": "Lobby",      "source_id": "SRC_2" }
]
```

```sh
curl -s -X POST http://localhost:8080/api/v1/recordings \
  -H 'Content-Type: application/json' -H 'X-API-Key: change-me' \
  -d '{"ip":"192.168.1.50","username":"admin","password":"secret"}'
```

### `POST /api/v1/footage`

Request body:

| Field | Type | Required | Description |
|---|---|---|---|
| `ip` | string | yes | NVR IP/hostname (bare host, no scheme/port/path). |
| `port` | int | no | ONVIF HTTP port, default `80`. |
| `username` | string | no | NVR username. |
| `password` | string | no | NVR password. |
| `channel` | int | no | 1-based channel, default `1`. |
| `recording_token` | string | no | Overrides `channel`. |
| `start` | string | yes | Start time, see [formats](#time-and-timezones). |
| `end` | string | yes | End time. |
| `tz` | string | no | `""` (NVR's timezone), `"Local"` (server's), or IANA name. |
| `fast` | bool | no | Stream faster than real time if the NVR supports it. |

The request is **synchronous**: the response starts only after the whole clip is fetched, which takes roughly the length of the clip (less with `fast`). Set client timeouts accordingly (e.g. a 30-minute clip needs a > 30-minute timeout).

```sh
curl -X POST http://localhost:8080/api/v1/footage \
  -H 'Content-Type: application/json' -H 'X-API-Key: change-me' \
  -d '{"ip":"192.168.1.50","username":"admin","password":"secret",
       "channel":2,"start":"2026-11-20 10:00:00","end":"2026-11-20 10:01:00","fast":true}' \
  -o channel2.mp4
```

Windows `cmd` (escape inner quotes):

```bat
curl -X POST http://localhost:8080/api/v1/footage -H "Content-Type: application/json" -d "{\"ip\":\"192.168.1.50\",\"username\":\"admin\",\"password\":\"secret\",\"channel\":2,\"start\":\"2026-11-20 10:00:00\",\"end\":\"2026-11-20 10:01:00\"}" -o channel2.mp4
```

The response has `Content-Disposition: attachment; filename="ch2_20261120_100000.mp4"`, so `curl -OJ` saves it under that name.

### Status codes

| Code | Meaning |
|---|---|
| `200` | MP4 body. |
| `400` | Bad input: missing/invalid `ip`, bad time format, `end` ≤ `start`, range > `-max-duration`, channel out of range, unknown `tz`. |
| `401` | Missing/wrong `X-API-Key` (only when an API key is configured). |
| `429` | All `-max-concurrent` slots busy. No queueing — retry later. |
| `502` | NVR/network/ffmpeg failure (unreachable NVR, auth rejected by NVR, no recording in range, ...). |

Error bodies are JSON: `{"message": "..."}`.

---

## Channels and recording tokens

`channel N` means **the N-th recording in the order the NVR returns them**. Most NVRs return them in camera order, but this is vendor-dependent. Always check once with:

```sh
./nvrfetch -list -ip 192.168.1.50 -user admin -pass secret
```

```
channel 1   token="Rec_1" source="Front Gate" sourceId="SRC_1"
channel 2   token="Rec_2" source="Lobby"      sourceId="SRC_2"
```

If the numbering doesn't match the NVR's UI, use the `token` with `-recording` (CLI) or `recording_token` (API) instead.

---

## Time and timezones

Accepted formats for `start` / `end`:

```
2026-11-20 10:00:00
2026-11-20T10:00:00
2026-11-20 10:00        (seconds = 0)
```

Times are **wall-clock** times, interpreted as follows:

| `tz` / `-tz` | Interpreted in |
|---|---|
| *(empty, default)* | The NVR's own timezone, as reported by ONVIF `GetSystemDateAndTime`. Matches what you see in the NVR's playback UI. |
| `Local` | The timezone of the machine running nvrfetch. |
| IANA name, e.g. `Asia/Kolkata`, `UTC` | That zone. |

If the NVR doesn't report its local time, nvrfetch logs a warning and falls back to the local machine's timezone — pass `-tz` explicitly in that case.

The resolved range (local and UTC) is logged at the start of every fetch, so you can verify it. The NVR clock skew is also measured and compensated for authentication.

---

## Fast mode

By default the NVR plays footage back at real speed, so a 10-minute clip takes ~10 minutes. With `-fast` / `"fast": true`, nvrfetch asks the NVR to send as fast as it can (ONVIF `Rate-Control: no`).

- Not all NVRs honour it; unsupported NVRs either ignore it (real-time speed) or reject playback. If a fetch fails with `fast`, retry without it.
- Output is the same either way.

---

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| `exec: "ffmpeg": executable file not found` | Install ffmpeg or pass `-ffmpeg /path/to/ffmpeg`. |
| Connection refused / timeout on ONVIF | Wrong `-port` (ONVIF port, often `80`, `8000`, `8080`, `8899`), or ONVIF disabled on the NVR. |
| `401` / auth errors from the NVR | Wrong credentials, or the user isn't allowed to use ONVIF. Some NVRs need a separate ONVIF user. |
| No Recording/Search/Replay service | NVR doesn't support Profile G over ONVIF (cameras usually don't; NVRs usually do). |
| Fetch hangs then fails during RTSP | RTSP port (usually 554) blocked between you and the NVR. nvrfetch reuses the host you pass with `-ip` and the RTSP port the NVR advertises. |
| Empty or very short MP4 | No recording in that range on that channel, or wrong timezone. Check the logged UTC range and the NVR's playback UI. |
| Wrong camera in the video | Channel order differs from the NVR UI — use `-list` and `-recording`. |
| `range ... exceeds max` (API) | Request is longer than `-max-duration`; split it or raise the limit. |
| `429` (API) | Too many concurrent downloads; retry or raise `-max-concurrent`. |

NVRs often advertise internal IPs in ONVIF responses; nvrfetch automatically rewrites them to the host you supplied, so it works through NAT/port-forwarding as long as both the ONVIF and RTSP ports are reachable.

---

## Security notes

- **Always set an API key** (`-api-key` or `NVR_API_KEY`) when running `serve` on a network. Without it, anyone who can reach the port can make the server connect to arbitrary hosts.
- NVR credentials are sent in the JSON body — put the API behind HTTPS (reverse proxy) if it crosses untrusted networks.
- On the CLI, prefer `NVR_PASSWORD` over `-pass` so the password doesn't land in shell history / process lists.

---

## Development

```sh
go test -race ./...                         # all tests
go vet ./... && GOOS=windows go vet ./...   # vet, incl. Windows target
make test                                   # both of the above
```

Tests use fake NVRs (an `httptest` ONVIF server and a scripted RTSP server); no real device is required. See [`HOW_IT_WORKS.md`](HOW_IT_WORKS.md) for the internals (ONVIF client, RTSP replay proxy, ffmpeg pipeline).

