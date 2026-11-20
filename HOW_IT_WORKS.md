# How nvrfetch Works

Downloads NVR footage for a camera channel + time range as MP4 using ONVIF Profile G. One binary, two modes: CLI and HTTP API.

## Usage

### API (Windows cmd)
```
nvrfetch-windows-amd64.exe serve -addr :8080
curl -X POST http://localhost:8080/api/v1/footage -H "Content-Type: application/json" -d "{\"ip\":\"10.254.23.241\",\"username\":\"admin\",\"password\":\"Admin@123\",\"channel\":2,\"start\":\"2026-11-20 10:00:00\",\"end\":\"2026-11-20 10:01:00\",\"fast\":true}" -o channel2.mp4
```

| Field | Notes |
|---|---|
| `ip`, `port`, `username`, `password` | NVR connection |
| `channel` | N-th recording as returned by the NVR (vendor-dependent) |
| `recording_token` | Overrides `channel` |
| `start`, `end` | `YYYY-MM-DD HH:MM:SS`, NVR local time unless `tz` set |
| `tz` | `""` = NVR timezone, `Local`, or IANA name |
| `fast` | Download faster than real time |

Other endpoints: `POST /api/v1/recordings` (list channels/tokens), `GET /healthz`.
Optional auth: `-api-key` / `NVR_API_KEY` → send `X-API-Key` header.

### CLI
```
nvrfetch -ip 10.254.23.241 -user admin -pass ... -channel 2 -start "..." -end "..."
nvrfetch -list -ip ...        # show channels
```

## Flow

1. **ONVIF discovery** (`internal/onvif`): hand-rolled SOAP client.
   - `GetSystemDateAndTime` → clock skew + NVR UTC offset.
   - `GetServices` (fallback `GetCapabilities`) → Recording/Search/Replay endpoints.
   - `GetRecordings` (fallback Search `FindRecordings`) → channel list.
   - `GetReplayUri` → RTSP URL for playback.
   - Auth: WS-UsernameToken digest, plus HTTP Digest/Basic after a 401.
2. **Channel mapping**: channel N = N-th recording the NVR returns.
3. **Timezone**: `start`/`end` interpreted in NVR offset, converted to UTC.
4. **Local RTSP proxy** (`internal/rtspproxy`): ffmpeg can't send ONVIF replay headers, so a proxy on `127.0.0.1:<random>` sits between ffmpeg and the NVR:
   - Rewrites request URIs to the upstream NVR.
   - Handles NVR Digest/Basic auth itself; adjusts CSeq after 401 retries.
   - On `PLAY` injects `Range: clock=<start>-<end>`, `Require: onvif-replay`, and `Rate-Control: no` (fast mode).
   - Keeps rewriting keep-alives/TEARDOWN during streaming; upstream idle timeout ends the stream.
5. **ffmpeg**: RTSP over TCP, `-t <duration>`, video copied, audio transcoded to AAC (G.711 can't go in MP4), `+faststart`. Cancel = proxy closes, ffmpeg finalizes the file.

## Notes

- NVRs often advertise internal IPs; HTTP endpoints and the replay URI host are rewritten to the IP you supplied.
- API `/footage` is synchronous: takes ~real-time of the range unless `fast`. Output staged in a temp dir, deleted after send.
- Errors: invalid input → 400; NVR/ffmpeg failures → 502; too many concurrent requests → 429.
- Requires `ffmpeg` ≥ 5 on PATH.
- Without `-api-key`, anyone reaching the port can make the server connect to arbitrary hosts. Bind to `127.0.0.1:8080` or set a key.

## Build

```
go build -o nvrfetch .
make windows      # dist/nvrfetch-windows-{amd64,arm64}.exe
make test         # vet (host + windows) + race tests
```
