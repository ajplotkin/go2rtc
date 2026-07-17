# How to Get Real Snapshots from Nest Cameras in Apple HomeKit

If you use [homebridge-google-nest-sdm](https://github.com/potmat/homebridge-google-nest-sdm) to bring your Nest cameras into Apple HomeKit, you've probably noticed that your camera tiles never show a real image -- just a static logo. You have to tap in and wait several seconds for the live stream to connect before you see anything.

This is a Google limitation, not a bug in the plugin. When Google migrated Nest devices to the Google Home app, they switched cameras from RTSP to WebRTC and removed the `CameraEventImage` API trait. There is no remaining endpoint to request a still image. The plugin's `getSnapshot()` has nothing to call, so it serves a placeholder.

This guide shows how to work around it using open-source tools, most of which already exist -- they just need a patch and some glue to work together. The result: **real, continuously-refreshed camera images on your HomeKit tiles**, plus faster live stream startup (~2s instead of ~8s).

## Prerequisites

You need a working setup of:

- **[homebridge-google-nest-sdm](https://github.com/potmat/homebridge-google-nest-sdm)** by [@potmat](https://github.com/potmat) -- the Homebridge plugin that bridges Nest cameras to HomeKit via Google's SDM API
- **[go2rtc](https://github.com/AlexxIT/go2rtc)** by [@AlexxIT](https://github.com/AlexxIT) -- a versatile camera streaming tool with native Nest/SDM support, RTSP output, and JPEG snapshot serving
- A Google [Device Access](https://developers.google.com/nest/device-access) project with working credentials (client ID, client secret, refresh token, project ID)
- A Raspberry Pi 4 or any Linux box with Docker

This guide also incorporates **[PR #212](https://github.com/potmat/homebridge-google-nest-sdm/pull/212)** by [@littlepope81](https://github.com/littlepope81), which reduces stream startup latency from ~8s to ~2s via FIR keyframe requests, `-fpsprobesize 0`, and REMB bandwidth signaling. That PR is unmerged but tested and working.

## How It Works

The trick is that even though Google offers no snapshot API, the cameras *do* stream live H264 video over WebRTC. If you keep one stream warm per camera, you can grab a frame from it whenever HomeKit asks.

[go2rtc](https://github.com/AlexxIT/go2rtc) already has the pieces for this: a `nest:` source that handles SDM WebRTC negotiation (including automatic stream extension before the 5-minute expiry), a `preload:` option that keeps streams connected, and a `/api/frame.jpeg` endpoint that transcodes a frame on demand. The plugin just needs to know where to find them.

```
Google Nest Cloud
    |
    | (WebRTC, kept warm by go2rtc preload)
    v
go2rtc ──────────> /api/frame.jpeg (cached, ~26ms)
    |                       |
    | (RTSP out)            | (warmer pulls every 20s)
    v                       v
Homebridge          /run/nest-snaps/*.jpg (tmpfs)
(live streams)              |
    |                       | (file read, ~1ms)
    v                       v
Apple HomeKit       patched Camera.js getSnapshot()
(real tile images)
```

## The go2rtc IPv6 Bug

There is one blocker: **stock go2rtc cannot stream Nest cameras on many home networks.** Its `nest:` source gathers ICE candidates on all network types including IPv6. On hosts where IPv6 addresses exist but have no working route -- which is extremely common in home setups -- [pion](https://github.com/pion/webrtc)'s ICE agent fails silently and no media flows.

The `webrtc: filters:` config exists but `pkg/nest/client.go` bypasses it entirely by calling `webrtc.NewAPI()` with nil filters. There is no YAML workaround. See [go2rtc #2311](https://github.com/AlexxIT/go2rtc/issues/2311) for discussion and diagnostic data.

This fork fixes it with one line:

```go
// was: rtcAPI, err := webrtc.NewAPI()
rtcAPI, err := webrtc.NewServerAPI("", "", &webrtc.Filters{Networks: []string{"udp4"}})
```

It also removes an inner retry loop that burned ~130 SDM API calls/hour per offline camera -- over Google's documented 100/hour quota -- while holding the producer mutex for 90+ seconds.

**If your IPv6 works fine**, you may not need this patch. Try stock go2rtc first; if you see `nest: wrong status: 400 Bad Request` or streams that start but produce no media, this is likely why.

## Step by Step

### 1. Build the patched go2rtc

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

The base image from [@AlexxIT](https://github.com/AlexxIT) provides ffmpeg (needed for the MJPEG transcode leg). We just swap in the patched binary:

```bash
mkdir -p ~/go2rtc-nest
cp go2rtc_patched ~/go2rtc-nest/

cat > ~/go2rtc-nest/Dockerfile <<'EOF'
FROM alexxit/go2rtc:1.9.14
COPY go2rtc_patched /usr/local/bin/go2rtc
EOF

docker build -t go2rtc-nestfix:1.9.14 ~/go2rtc-nest/
```

### 3. Discover your cameras and generate the config

This script reads your existing Homebridge credentials (single source of truth -- no duplicated secrets), discovers cameras via the SDM API, and writes a go2rtc config with a warm stream per camera.

Save as `~/scripts/nest-go2rtc-sync.py`:

```python
#!/usr/bin/env python3
"""
Auto-discovers Nest cameras from the SDM API and generates go2rtc.yaml.
Credentials are read from Homebridge's config.json.

Stream key = SDM room name, lowercased, non-alphanum -> underscore.
This MUST match the key derivation in the patched Camera.js.
"""
import json, sys, urllib.parse, urllib.request, subprocess, re, argparse

def get_token(cid, cs, rt):
    d = urllib.parse.urlencode({"client_id": cid, "client_secret": cs,
                                "refresh_token": rt, "grant_type": "refresh_token"}).encode()
    with urllib.request.urlopen("https://oauth2.googleapis.com/token", data=d, timeout=30) as r:
        return json.load(r)["access_token"]

def list_devices(at, project):
    req = urllib.request.Request(
        f"https://smartdevicemanagement.googleapis.com/v1/enterprises/{project}/devices",
        headers={"Authorization": "Bearer " + at})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r).get("devices", [])

def stream_key(dev):
    parents = [p.get("displayName") for p in dev.get("parentRelations", []) if p.get("displayName")]
    if not parents:
        return None
    return re.sub(r"[^a-z0-9]+", "_", parents[0].lower()).strip("_")

ap = argparse.ArgumentParser()
ap.add_argument("--hb-config", required=True, help="Path to Homebridge config.json")
ap.add_argument("--out", required=True, help="Path to write go2rtc.yaml")
ap.add_argument("--container", default="go2rtc", help="Docker container name to restart")
ap.add_argument("--dry-run", action="store_true")
a = ap.parse_args()

cfg = json.load(open(a.hb_config))
nest = next((p for p in cfg["platforms"] if p.get("platform") == "homebridge-google-nest-sdm"), None)
if not nest:
    sys.exit("No homebridge-google-nest-sdm platform found in config")

cid, cs, rt, proj = nest["clientId"], nest["clientSecret"], nest["refreshToken"], nest["projectId"]
at = get_token(cid, cs, rt)

streams, preload, seen = [], [], set()
for d in list_devices(at, proj):
    if d.get("type", "").split(".")[-1] not in ("CAMERA", "DOORBELL"):
        continue
    k = stream_key(d)
    if not k:
        continue
    if k in seen:
        sys.exit(f"ERROR: duplicate room key '{k}' -- two devices in the same room")
    seen.add(k)
    dev_id = d["name"].split("/devices/")[1]
    q = urllib.parse.urlencode({
        "client_id": cid, "client_secret": cs, "device_id": dev_id,
        "project_id": proj, "protocols": "WEB_RTC", "refresh_token": rt})
    streams.append(f'  {k}:\n    - "nest:?{q}"\n    - "ffmpeg:{k}#video=mjpeg"')
    preload.append(f'  {k}: "video"')
    print(f"  discovered: {k}")

if not streams:
    sys.exit("ERROR: no cameras discovered -- refusing to write empty config")

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
    print("would write new config"); sys.exit(0)

open(a.out, "w").write(out)
print(f"config written -> restarting {a.container}")
subprocess.run(["docker", "restart", a.container], check=False,
               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
```

```bash
python3 ~/scripts/nest-go2rtc-sync.py \
  --hb-config ~/volumes/homebridge/config.json \
  --out ~/go2rtc-nest/go2rtc.yaml
```

**Important:** The script builds `nest:` URLs using proper URL encoding (the refresh token contains `//` which must be encoded as `%2F%2F`). Hand-written URLs will fail with a 400. If you skip the script, use go2rtc's own `GET /api/nest` endpoint to generate correctly-encoded URLs.

### 4. Start go2rtc

```bash
docker run -d --name go2rtc \
  --restart unless-stopped \
  --network host \
  -v ~/go2rtc-nest/go2rtc.yaml:/config/go2rtc.yaml \
  go2rtc-nestfix:1.9.14
```

Wait ~30 seconds, then verify the streams are warm:

```bash
curl -s http://127.0.0.1:1985/api/streams | python3 -c "
import sys, json
for name, s in json.load(sys.stdin).items():
    warm = any(any((r.get('bytes') or 0) > 0
        for r in (p.get('receivers') or []))
        for p in (s.get('producers') or []))
    print(f'  {name}: {\"WARM\" if warm else \"cold\"}')"
```

Test a snapshot:

```bash
curl -o /tmp/test.jpg "http://127.0.0.1:1985/api/frame.jpeg?src=front_door&cache=30s"
file /tmp/test.jpg   # should say "JPEG image data"
```

### 5. Set up the snapshot warmer

The warmer pulls a JPEG from each warm stream every 20 seconds and writes it to disk for the plugin to read. It auto-discovers streams from go2rtc, so cameras added later appear automatically.

**If your system runs on an SD card** (like a Raspberry Pi), put the snapshots in tmpfs (RAM). Writing ~100KB JPEGs every 20 seconds per camera is ~780 MB/day of pointless flash wear:

```bash
echo 'd /run/nest-snaps 0755 1000 1000 -' | sudo tee /etc/tmpfiles.d/nest-snaps.conf
sudo systemd-tmpfiles --create /etc/tmpfiles.d/nest-snaps.conf
```

Save as `~/scripts/go2rtc-snapshot-warmer.sh`:

```bash
#!/bin/bash
# Writes a fresh JPEG per warm go2rtc stream.
# Only polls streams that have active media (skips cameras that are off).
# Prunes stale files so off cameras show the honest logo, not an old frame.
DIR=/run/nest-snaps    # tmpfs -- change to a regular path if not on SD card
API=http://127.0.0.1:1985
mkdir -p "$DIR"
while true; do
  WARM=$(curl -s -m 10 "$API/api/streams" | python3 -c '
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
  for s in $WARM; do
    [[ "$s" =~ ^[a-z0-9_]+$ ]] || continue
    if curl -sf -m 15 -o "$DIR/.$s.tmp" "$API/api/frame.jpeg?src=$s&cache=30s"; then
      if [ -s "$DIR/.$s.tmp" ] && [ "$(stat -c %s "$DIR/.$s.tmp")" -gt 1000 ]; then
        mv -f "$DIR/.$s.tmp" "$DIR/$s.jpg"
      fi
    fi
    rm -f "$DIR/.$s.tmp" 2>/dev/null || true
  done
  find "$DIR" -name '*.jpg' -mmin +2 -delete 2>/dev/null || true
  sleep 20
done
```

Install as a systemd service:

```ini
# /etc/systemd/system/go2rtc-snapshot-warmer.service
[Unit]
Description=Keep go2rtc snapshot cache warm for Homebridge
After=docker.service
[Service]
ExecStart=/home/YOUR_USER/scripts/go2rtc-snapshot-warmer.sh
Restart=always
RestartSec=10
[Install]
WantedBy=multi-user.target
```

```bash
chmod +x ~/scripts/go2rtc-snapshot-warmer.sh
sudo systemctl daemon-reload
sudo systemctl enable --now go2rtc-snapshot-warmer.service
```

### 6. Patch the Homebridge plugin

Mount the snapshot directory into the Homebridge container. **Without this mount, the plugin can't see the files and tiles will show the logo:**

```bash
docker run -d --name homebridge \
  ... \
  -v /path/to/homebridge:/homebridge \
  -v /run/nest-snaps:/homebridge/nest-snaps \
  homebridge/homebridge:latest
```

Then patch two files in `node_modules/homebridge-google-nest-sdm/dist/sdm/`:

**Camera.js** -- add this at the top of `getSnapshot()`, before the logo fallback:

```javascript
// Read a real frame from the warmer (file on disk, ~1ms)
try {
    const key = (this.displayName || '').toLowerCase().replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '');
    const snapPath = '/homebridge/nest-snaps/' + key + '.jpg';
    const st = await fs_1.default.promises.stat(snapPath);
    if (Date.now() - st.mtimeMs > 90000) {
        this.log.debug('snapshot too stale (' + Math.round((Date.now() - st.mtimeMs)/1000) + 's), using logo', this.getDisplayName());
    } else {
        const buf = await fs_1.default.promises.readFile(snapPath);
        if (buf && buf.length > 1000)
            return buf;
    }
}
catch (e) {
    this.log.debug('no warm snapshot on disk, using logo: ' + e, this.getDisplayName());
}
```

The 90-second mtime check prevents a camera that was turned off from showing an indefinitely stale frame -- it falls back to the logo honestly.

**Api.js** -- add this guard before `if (event.resourceUpdate.events)`:

```javascript
if (!event || !event.resourceUpdate) {
    this.log.debug('Event without resourceUpdate (e.g. relationUpdate), ignoring');
    return;
}
```

Without this, the plugin crashes on `relationUpdate` events that Google sends when Pub/Sub is first enabled. See [issue #214](https://github.com/potmat/homebridge-google-nest-sdm/issues/214).

**Both patches live in `node_modules` and will be wiped by any `npm install`.** Save copies outside `node_modules` with a re-apply script.

### 7. Verify

```bash
docker restart homebridge
```

After ~30 seconds:
- Open Apple Home -- tiles should show real camera images
- Check the Homebridge log for `snapshot too stale` or `no warm snapshot` (should be zero)
- Snapshots refresh every ~20 seconds

## Things to Know

**Cameras that are off** are handled correctly. When a camera is switched off in the Google Home app, the warmer skips its stream, stale files are pruned after 2 minutes, and Camera.js rejects snapshots older than 90 seconds. The tile honestly shows the logo. When the camera turns back on, preload reconnects and snapshots resume automatically.

**Home/Away Assist** may automatically turn cameras off when you're home. This is the most common reason for cameras appearing to work intermittently. Check: Google Home app > Settings > Home & Away Routines.

**SDM quotas** are well within limits. Preload costs ~12 `ExtendWebRtcStream` calls/hour/camera (the device limit is 100/hour). The warmer makes zero SDM calls -- it reads from go2rtc's local cache.

**Live stream latency** also improves. With [PR #212](https://github.com/potmat/homebridge-google-nest-sdm/pull/212) by [@littlepope81](https://github.com/littlepope81) installed alongside `vEncoder: "copy"`, measured stream startup on a Pi 4: **first keyframe fully received at +2127ms** (down from ~8s stock). The remaining ~4s to tile-open is Apple's HAP/SRTP setup and isn't addressable from Homebridge.

## Credits

This guide builds on the work of:

- **[@AlexxIT](https://github.com/AlexxIT)** -- [go2rtc](https://github.com/AlexxIT/go2rtc), which provides the Nest WebRTC source, stream preload, RTSP output, and JPEG snapshot serving that make all of this possible
- **[@potmat](https://github.com/potmat)** -- [homebridge-google-nest-sdm](https://github.com/potmat/homebridge-google-nest-sdm), the Homebridge plugin that bridges Nest cameras to HomeKit
- **[@littlepope81](https://github.com/littlepope81)** -- [PR #212](https://github.com/potmat/homebridge-google-nest-sdm/pull/212), which dramatically reduces stream startup latency via FIR keyframe requests, frame-rate probe skipping, and REMB bandwidth signaling
- **[werift](https://github.com/nicktournux/werift-webrtc)** -- the WebRTC library used by `homebridge-google-nest-sdm` that handles the Nest WebRTC negotiation correctly where pion (used by go2rtc) currently has IPv6 issues
- **[pion/webrtc](https://github.com/pion/webrtc)** -- the Go WebRTC library used by go2rtc

## Related Issues

- [go2rtc #2311](https://github.com/AlexxIT/go2rtc/issues/2311) -- `nest: wrong status: 400` / ICE failure diagnosis and SDP validation data
- [homebridge-google-nest-sdm #214](https://github.com/potmat/homebridge-google-nest-sdm/issues/214) -- Api.js crash on `relationUpdate` events
- [homebridge-google-nest-sdm #215](https://github.com/potmat/homebridge-google-nest-sdm/issues/215) -- README corrections (subscriptionId/gcpProjectId confusion, self-hosted Pub/Sub, Node 24.17.0 regression, `aEncoder` non-option)
- [homebridge-google-nest-sdm PR #212](https://github.com/potmat/homebridge-google-nest-sdm/pull/212) -- stream startup latency fix

## License

go2rtc is MIT licensed. This fork adds a one-line patch to `pkg/nest/client.go`.
