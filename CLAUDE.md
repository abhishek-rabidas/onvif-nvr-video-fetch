# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`nvrfetch`: downloads NVR footage for a camera channel and local time range as MP4 via ONVIF Profile G. One binary, two modes: CLI (`nvrfetch -ip ... -start ... -end ...`, `-list` to show channels) and HTTP API (`nvrfetch serve`, Echo v5). Go stdlib only, except `github.com/labstack/echo/v5` (API layer). Requires `ffmpeg` (≥5, uses rtsp `-timeout` as socket I/O timeout) at runtime.

## Commands

```
go build -o nvrfetch .                                  # local build
go test -race ./...                                     # all tests
go test ./internal/rtspproxy -run TestProxyAuthAndReplayHeaders -v   # single test
go vet ./... && GOOS=windows go vet ./...               # vet (Windows too — it's a release target)
make windows                                            # dist/nvrfetch-windows-{amd64,arm64}.exe, CGO_ENABLED=0
make test                                               # vet (host + windows) + race tests
```
Version is injected via `-ldflags "-X main.version=..."` (`nvrfetch version`).

## Architecture

Flow (`internal/footage.Fetch` orchestrates everything; CLI and API are thin wrappers over `footage.Request`):

1. **`internal/onvif`** — hand-rolled SOAP 1.2 client (no WSDL codegen). `GetSystemDateAndTime` (unauthenticated) gives device clock skew (used in WS-Security `Created`) and the NVR's UTC offset. Then `GetServices` → fallback `GetCapabilities` to find Recording/Search/Replay XAddrs; `GetRecordings` → fallback Search `FindRecordings`; `GetReplayUri`. Auth: WS-UsernameToken digest always, plus HTTP Digest/Basic added after a 401. Responses are decoded by walking to the first element inside `Body` and matching by **local name only** (struct tags carry no namespaces) — keep it that way, vendors vary prefixes.
2. **Channel mapping** — channel N = N-th recording in the order the NVR returns. Vendor-dependent; `-recording`/`recording_token` overrides.
3. **Timezone** — `start`/`end` are wall-clock strings interpreted in the NVR's offset (from step 1) unless `TZ` is given; converted to UTC for the replay request.
4. **`internal/rtspproxy`** — the key piece. ffmpeg cannot send ONVIF replay headers, so a local RTSP proxy (127.0.0.1:random) sits between ffmpeg and the NVR. It: rewrites request URIs from local base to upstream base; strips ffmpeg's auth and does NVR Digest/Basic itself (shared `internal/digest`); on 401 retries with an incremented CSeq and keeps `offset` so upstream CSeq = client CSeq + offset (responses mapped back); on `PLAY` injects `Range: clock=<start>-<end>` (UTC), `Require: onvif-replay`, and `Rate-Control: no` when fast mode. Phase 1 (until PLAY 2xx) is synchronous request/response; phase 2 parses both directions as interleaved `$` frames + RTSP messages so keep-alives/TEARDOWN still get rewritten. Idle timeout on upstream ends the stream.
5. **ffmpeg** — `-rtsp_transport tcp`, `-t <end-start>`, video copied, audio transcoded to AAC (NVR G.711 can't go in MP4), `+faststart`. Cancellation closes the proxy (ffmpeg sees EOF and finalizes the MP4) rather than signalling — `os.Interrupt` is unsupported on Windows.

Host rewriting: NVRs often advertise internal IPs, so HTTP XAddrs are rewritten to the user-supplied host:port, and the replay URI host is replaced with the user host (RTSP port kept).

## API (`internal/api`)

- `POST /api/v1/footage` (JSON) → `video/mp4` attachment; synchronous, takes ~real time of the range (unless `fast`). Staged in a per-request temp dir, deleted after send. Served with `c.FileFS(name, os.DirFS(dir))` — Echo's default `c.File` filesystem rejects absolute paths outside CWD.
- `POST /api/v1/recordings` → channel/token list. `GET /healthz`.
- Errors: `footage.ErrInvalid` (wrapped with `%w`) → 400; everything else → 502. Wrap new user-input errors with `ErrInvalid`.
- Concurrency semaphore → 429 when full (no queueing). Optional `X-API-Key` (`-api-key`/`NVR_API_KEY`); without it the server can be used to reach arbitrary hosts.

## Testing notes

No real NVR in tests. `internal/onvif/client_test.go` uses an `httptest` fake NVR (prefixed namespaces, digest 401, SOAP fault); `internal/rtspproxy/proxy_test.go` uses a scripted TCP fake NVR to assert URI/CSeq/auth rewriting and PLAY headers. The end-to-end MP4 path (real NVR → proxy → ffmpeg) has not been verified yet.
