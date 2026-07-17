# Nest Cameras in Apple HomeKit: Real Snapshots Instead of the Google "G" Logo

**The problem:** Google Nest cameras show a static Google logo ("G") as their tile image in Apple HomeKit. You never see a real preview of what the camera sees unless you tap in and wait for the live stream to load. Commercial solutions like the Starling Home Hub ($99) solve this, but there's been no open-source answer.

**This guide gets you real, continuously-refreshed camera snapshots on your HomeKit tiles using free, open-source tools.** It also cuts live-stream startup time from ~8 seconds to ~2 seconds.

Works with all Google Nest cameras and doorbells, including the newer WebRTC-only models that have no RTSP support.

## Why the "G" Exists

When Google migrated Nest cameras to the Google Home app, they converted them from RTSP to WebRTC and removed the `CameraEventImage` trait. The Homebridge plugin (`homebridge-google-nest-sdm`) can only produce snapshots via that trait, so it falls back to a static logo.

There is no Google API to request a still image from these cameras. The only way to get a picture is to grab a frame from a live video stream.

## Architecture

```
Google Nest Cloud
    |
    | (WebRTC, kept warm by preload)
    v
patched go2rtc -----> /api/frame.jpeg (cached, ~26ms)
    |                       |
    | (RTSP)                | (every 20s)
    v                       v
Homebridge          snapshot-warmer.sh
(live streams)      writes to /run/nest-snaps/ (tmpfs)
    |                       |
    | (HAP/SRTP)            | (file read, ~1ms)
    v                       v
Apple HomeKit       patched Camera.js getSnapshot()
(real tile images)
```

One warm stream per camera serves double duty: HomeKit live streams are near-instant (the connection is already open), and a warmer script grabs a JPEG every 20 seconds for the tile preview.

## What You Need

- A Raspberry Pi 4 (or any Linux box) running Homebridge with `homebridge-google-nest-sdm`
- A Google Device Access project with working credentials (client ID, client secret, refresh token, project ID)
- ~30 minutes

## The Problem with Stock go2rtc

Stock go2rtc v1.9.14 **cannot stream Nest cameras on many home networks**. The `nest:` source gathers ICE candidates on all network types including IPv6. On hosts where IPv6 addresses exist but have no working route (extremely common), pion's ICE agent fails silently and no media ever flows.

The `webrtc: filters:` YAML config exists but **does not reach the nest source** -- `pkg/nest/client.go` calls `webrtc.NewAPI()` with nil filters, bypassing all config. There is no workaround without patching Go. ([go2rtc #2311](https://github.com/AlexxIT/go2rtc/issues/2311))

This fork fixes it with one line:

```go
// was: rtcAPI, err := webrtc.NewAPI()
rtcAPI, err := webrtc.NewServerAPI("", "", &webrtc.Filters{Networks: []string{"udp4"}})
```

It also removes a retry loop that burned ~130 SDM API calls/hour per offline camera (over Google's documented 100/hour quota).

## Step-by-Step Setup

### 1. Build the patched go2rtc

Clone this fork and build on the Pi (or cross-compile):

```bash
git clone https://github.com/ajplotkin/go2rtc.git
cd go2rtc
git checkout fix/nest-ipv6-ice-failure

# Build natively on the Pi (arm64, ~3 min)
docker run --rm -v "$PWD":/src -w /src \
  -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod \
  golang:1.24-alpine sh -c \
  "CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o go2rtc_patched ."
```

### 2. Create a Docker image

```bash
mkdir -p ~/go2rtc2
cp go2rtc_patched ~/go2rtc2/

cat > ~/go2rtc2/Dockerfile <<'EOF'
FROM alexxit/go2rtc:1.9.14
COPY go2rtc_patched /usr/local/bin/go2rtc
EOF

docker build -t go2rtc-nestfix:1.9.14 ~/go2rtc2/
```

### 3. Auto-discover your cameras and generate the config

Create `~/scripts/nest-go2rtc-sync.py`:

```python
#!/usr/bin/env python3
"""
Reads Nest credentials from Homebridge's config.json, discovers cameras
via the SDM API, and generates go2rtc.yaml with a warm stream per camera.

Stream key = SDM room name, lowercased, non-alphanum replaced with underscore.
This must match the key derivation in the patched Camera.js getSnapshot().
"""
import json, sys, urllib.parse, urllib.request, subprocess, re, argparse

def token(cid, cs, rt):
    d = urllib.parse.urlencode({"client_id": cid, "client_secret": cs,
                                "refresh_token": rt, "grant_type": "refresh_token"}).encode()
    with urllib.request.urlopen("https://oauth2.googleapis.com/token", data=d, timeout=30) as r:
        return json.load(r)["access_token"]

def devices(at, project):
    req = urllib.request.Request(
        f"https://smartdevicemanagement.googleapis.com/v1/enterprises/{project}/devices",
        headers={"Authorization": "Bearer " + at})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r).get("devices", [])

def key_for(dev):
    parents = [p.get("displayName") for p in dev.get("parentRelations", []) if p.get("displayName")]
    if not parents:
        return None
    return re.sub(r"[^a-z0-9]+", "_", parents[0].lower()).strip("_")

ap = argparse.ArgumentParser()
ap.add_argument("--hb-config", default="/path/to/homebridge/config.json")
ap.add_argument("--out", default="/path/to/go2rtc2/go2rtc.yaml")
ap.add_argument("--container", default="go2rtc")
ap.add_argument("--dry-run", action="store_true")
a = ap.parse_args()

cfg = json.load(open(a.hb_config))
nest = next((p for p in cfg["platforms"] if p.get("platform") == "homebridge-google-nest-sdm"), None)
if not nest:
    print("no nest platform in homebridge config"); sys.exit(0)

cid, cs, rt, proj = nest["clientId"], nest["clientSecret"], nest["refreshToken"], nest["projectId"]
at = token(cid, cs, rt)

streams, preload = [], []
seen_keys = set()
for d in devices(at, proj):
    if d.get("type", "").split(".")[-1] not in ("CAMERA", "DOORBELL"):
        continue
    k = key_for(d)
    if not k:
        continue
    if k in seen_keys:
        print(f"  ERROR: duplicate room key '{k}' -- two devices in the same room. Refusing.")
        sys.exit(1)
    seen_keys.add(k)
    dev_id = d["name"].split("/devices/")[1]
    q = urllib.parse.urlencode({
        "client_id": cid, "client_secret": cs, "device_id": dev_id,
        "project_id": proj, "protocols": "WEB_RTC", "refresh_token": rt})
    streams.append(f'  {k}:\n    - "nest:?{q}"\n    - "ffmpeg:{k}#video=mjpeg"')
    preload.append(f'  {k}: "video"')
    print(f"  discovered: {k}")

if not streams:
    print("  ERROR: no cameras discovered -- refusing to write empty config"); sys.exit(1)

out = ("api:\n  listen: \"127.0.0.1:1985\"\nrtsp:\n  listen: \":8554\"\n"
       "webrtc:\n  listen: \":8555\"\nlog:\n  level: info\n\nstreams:\n"
       + "\n".join(streams) + "\n\npreload:\n" + "\n".join(preload) + "\n")

try:
    cur = open(a.out).read()
except FileNotFoundError:
    cur = ""
if cur == out:
    print("config unchanged"); sys.exit(0)
if a.dry_run:
    print("--- would write ---"); sys.exit(0)
open(a.out, "w").write(out)
print(f"config written; restarting {a.container}")
subprocess.run(["docker", "restart", a.container], check=False,
               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
```

Run it:
```bash
python3 ~/scripts/nest-go2rtc-sync.py \
  --hb-config ~/volumes/homebridge/config.json \
  --out ~/go2rtc2/go2rtc.yaml
```

### 4. Start go2rtc

```bash
docker run -d --name go2rtc \
  --restart unless-stopped \
  --network host \
  -v ~/go2rtc2/go2rtc.yaml:/config/go2rtc.yaml \
  go2rtc-nestfix:1.9.14
```

Wait ~30 seconds for the streams to warm up, then verify:

```bash
# Check warm streams
curl -s http://127.0.0.1:1985/api/streams | python3 -c "
import sys, json
for name, s in json.load(sys.stdin).items():
    warm = any(any((r.get('bytes') or 0) > 0
        for r in (p.get('receivers') or []))
        for p in (s.get('producers') or []))
    print(f'  {name}: {\"WARM\" if warm else \"cold\"}')"

# Grab a test frame
curl -o /tmp/test.jpg "http://127.0.0.1:1985/api/frame.jpeg?src=front_door&cache=30s"
```

### 5. Set up the snapshot warmer

The warmer pulls a JPEG from each warm stream every 20 seconds and writes it to tmpfs (RAM). This avoids SD card wear and ensures the plugin never waits for a frame.

Create `~/scripts/go2rtc-snapshot-warmer.sh`:

```bash
#!/bin/bash
# Writes a fresh JPEG per warm go2rtc stream to /run/nest-snaps/ (tmpfs).
# Stream list is auto-discovered from go2rtc, so new cameras appear automatically.
DIR=/run/nest-snaps
API=http://127.0.0.1:1985
mkdir -p "$DIR"
while true; do
  STREAMS=$(curl -s -m 10 "$API/api/streams" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for name, s in d.items():
    for p in (s.get("producers") or []):
        if any((r.get("bytes") or 0) > 0 for r in (p.get("receivers") or [])):
            print(name); break
' 2>/dev/null || echo "")
  for s in $STREAMS; do
    [[ "$s" =~ ^[a-z0-9_]+$ ]] || continue
    if curl -sf -m 15 -o "$DIR/.$s.tmp" "$API/api/frame.jpeg?src=$s&cache=30s"; then
      if [ -s "$DIR/.$s.tmp" ] && [ "$(stat -c %s "$DIR/.$s.tmp")" -gt 1000 ]; then
        mv -f "$DIR/.$s.tmp" "$DIR/$s.jpg"
      fi
    fi
    rm -f "$DIR/.$s.tmp" 2>/dev/null || true
  done
  # Remove stale snapshots (camera turned off -> no fresh frame -> honest logo instead)
  find "$DIR" -name '*.jpg' -mmin +2 -delete 2>/dev/null || true
  sleep 20
done
```

Set up tmpfs (so snapshots live in RAM, not on the SD card):

```bash
echo 'd /run/nest-snaps 0755 1000 1000 -' | sudo tee /etc/tmpfiles.d/nest-snaps.conf
sudo systemd-tmpfiles --create /etc/tmpfiles.d/nest-snaps.conf
```

Install as a systemd service:

```ini
# /etc/systemd/system/go2rtc-snapshot-warmer.service
[Unit]
Description=Keep go2rtc snapshot cache warm for Homebridge/HomeKit
After=docker.service
[Service]
ExecStart=/path/to/go2rtc-snapshot-warmer.sh
Restart=always
RestartSec=10
[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now go2rtc-snapshot-warmer.service
```

### 6. Patch Homebridge

**Important:** If you're running your Homebridge container with Docker, you need to mount the tmpfs snapshots directory:

```bash
docker run -d --name homebridge \
  ... \
  -v /path/to/homebridge:/homebridge \
  -v /run/nest-snaps:/homebridge/nest-snaps \   # <-- REQUIRED for snapshots
  homebridge/homebridge:latest
```

Then patch `homebridge-google-nest-sdm`. Two files need changes:

**`dist/sdm/Camera.js` -- serve real snapshots from the warmer:**

Find the `getSnapshot()` method and add this before the logo fallback:

```javascript
async getSnapshot() {
    if (this.image)
        return this.image;
    // Serve a real frame from the go2rtc warm stream (written by the warmer)
    try {
        const key = (this.displayName || '').toLowerCase().replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '');
        const snapPath = '/homebridge/nest-snaps/' + key + '.jpg';
        const st = await fs_1.default.promises.stat(snapPath);
        if (Date.now() - st.mtimeMs > 90000) {
            this.log.debug('snapshot too stale, using logo', this.getDisplayName());
        } else {
            const buf = await fs_1.default.promises.readFile(snapPath);
            if (buf && buf.length > 1000)
                return buf;
        }
    }
    catch (e) {
        this.log.debug('no warm snapshot on disk, using logo: ' + e, this.getDisplayName());
    }
    // ... original logo fallback continues below
```

**`dist/sdm/Api.js` -- fix the Pub/Sub crash on relationUpdate events:**

Find `if (event.resourceUpdate.events) {` and add a guard before it:

```javascript
if (!event || !event.resourceUpdate) {
    this.log.debug('Event without resourceUpdate (e.g. relationUpdate), ignoring');
    return;
}
if (event.resourceUpdate.events) {
```

Without this guard, the plugin crashes on the first Pub/Sub message Google sends after you enable events. ([Issue #214](https://github.com/potmat/homebridge-google-nest-sdm/issues/214))

**These patches live in `node_modules` -- any `npm install` of the plugin wipes them.** Save copies outside `node_modules` and create a re-apply script.

### 7. Restart and verify

```bash
docker restart homebridge

# After ~30 seconds, check:
# - Open Apple Home -- tiles should show real camera images
# - No "snapshot handler is slow to respond" warnings in the log
# - Snapshots refresh every ~20 seconds
```

## Important Notes

### SD Card Wear
If your system runs on an SD card, the snapshots **must** be on tmpfs (RAM). Writing ~100KB JPEGs every 20 seconds per camera is ~780 MB/day of pointless flash wear. The tmpfs setup above avoids this entirely.

### Cameras That Are Off
When a camera is switched off in the Google Home app, Google returns `FAILED_PRECONDITION: "The camera is not available for streaming"`. The system handles this correctly:
- go2rtc's preload backs off (no quota burn, thanks to the removed retry loop)
- The warmer skips cold streams (only polls warm ones)
- Stale snapshot files are pruned after 2 minutes
- Camera.js rejects snapshots older than 90 seconds
- The tile honestly shows the logo for off cameras

When the camera is turned back on, preload reconnects and snapshots resume automatically.

### Home/Away Assist
Google's Home/Away Assist may automatically turn cameras off when you're home. This is the most common reason for cameras appearing to work intermittently. Check: Google Home app > Settings > Home & Away Routines.

### SDM API Quotas
- Preload costs ~12 `ExtendWebRtcStream` calls/hour/camera (well within the 100/hour device limit)
- The warmer makes zero SDM calls (it reads from go2rtc's local cache)
- With the retry loop removed, an off camera costs ~1 call per outer reconnect cycle (60s backoff)

### Stream URL Encoding
**Always build `nest:` URLs from go2rtc's own `/api/nest` discovery endpoint.** Hand-written URLs fail because the refresh token contains `//` which must be URL-encoded (`1%2F%2F...`), and `protocols=WEB_RTC` must be present. The sync script handles this automatically.

### Performance
Measured on a Raspberry Pi 4 (arm64):
- Cached snapshot: **~26 ms**
- CPU: **~0%** idle, brief spikes during the 20s transcode cycle
- Bandwidth: ~1.5 Mbps per camera (wired gigabit; irrelevant on an uncapped connection)
- RAM: ~200 KB for snapshot files on tmpfs

### Live Stream Latency (Bonus)
With [PR #212](https://github.com/potmat/homebridge-google-nest-sdm/pull/212) installed alongside `vEncoder: "copy"`, measured stream startup:
- First keyframe fully received: **+2127 ms** (down from ~8s stock)
- The remaining ~4s to tile-open is Apple's HAP/SRTP setup, not addressable from Homebridge

## Related Issues and PRs

- [go2rtc #2311](https://github.com/AlexxIT/go2rtc/issues/2311) -- nest 400 / ICE failure (our comment with diagnosis + SDP validation data)
- [homebridge-google-nest-sdm #214](https://github.com/potmat/homebridge-google-nest-sdm/issues/214) -- Api.js crash on relationUpdate
- [homebridge-google-nest-sdm #215](https://github.com/potmat/homebridge-google-nest-sdm/issues/215) -- README corrections (subscriptionId, gcpProjectId, aEncoder, Node 24.17.0, self-hosted Pub/Sub)
- [homebridge-google-nest-sdm PR #212](https://github.com/potmat/homebridge-google-nest-sdm/pull/212) -- stream startup latency fix (our hardware validation comment)

## License

go2rtc is MIT licensed. This fork adds a one-line patch to `pkg/nest/client.go`.
