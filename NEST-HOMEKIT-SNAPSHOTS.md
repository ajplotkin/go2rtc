# Google Nest Cameras in Apple HomeKit — With Real Tile Images

You have Google Nest cameras. You want them in Apple HomeKit. And you want to actually *see* what the camera sees on the tile — a real, refreshing image — not a blank tile or a placeholder logo.

This is harder than it should be. Google does not offer Nest cameras through HomeKit natively, and when they migrated Nest devices to the Google Home app, they removed the API that provided still images. No integration — commercial or open-source — can request a snapshot from these cameras anymore. The only way to get a real picture is to grab a frame from a live video stream.

This guide walks through the full setup from scratch: getting API access to your Nest cameras, bridging them into HomeKit, and then solving the snapshot problem by keeping a warm stream and serving frames from it. By the end you'll have:

- **Real camera images on your HomeKit tiles** — refreshed every 10 seconds, and instantly on motion or doorbell events (so the tile shows *who's there*, not a stale frame)
- **~2 second live stream startup** — down from ~8 seconds stock
- **Motion and doorbell event notifications** in Apple Home
- **Automatic camera discovery** — new cameras appear without editing config files

**Everything here is open source and runs on a Raspberry Pi.**

## What You'll Set Up

There are four layers. Each builds on the last:

1. **Google Device Access** — Google's [official API](https://developers.google.com/nest/device-access) for accessing Nest devices programmatically. One-time $5 registration. This gives you the credentials everything else needs.

2. **[Homebridge](https://homebridge.io/)** + **[homebridge-google-nest-sdm](https://github.com/potmat/homebridge-google-nest-sdm)** — [Homebridge](https://github.com/homebridge/homebridge) (by the [@homebridge](https://github.com/homebridge) team) is an open-source HomeKit bridge that runs on a Pi or any server. The Nest plugin (by [@potmat](https://github.com/potmat)) connects to Google's SDM API and presents your cameras as HomeKit accessories. After this step, your cameras appear in Apple Home and live streams work — but tiles show a placeholder logo because there's no snapshot API.

3. **[go2rtc](https://github.com/AlexxIT/go2rtc)** (patched) — go2rtc (by [@AlexxIT](https://github.com/AlexxIT)) is a streaming tool that can connect to Nest cameras via WebRTC, keep the connection alive, re-serve the stream over RTSP, and produce JPEG snapshots on demand. This is the engine that makes real tile images possible. (A one-line patch is needed on many home networks — explained below.)

4. **Snapshot warmer + plugin patches** — A small script that pulls a JPEG from each warm stream every 10 seconds (and immediately on motion/doorbell events), plus a patch to the Homebridge plugin that serves those images instead of the placeholder. This is the glue that connects go2rtc's capabilities to your HomeKit tiles.

## Already have Homebridge + homebridge-google-nest-sdm working?

If your Nest cameras are already in Apple HomeKit via Homebridge and you just want to fix the snapshot/tile-image problem, skip to [Part 3: go2rtc](#part-3-go2rtc--warm-streams-and-snapshots). You already have the credentials and the plugin — you just need the warm-stream layer and the patches.

If you also want faster live stream startup (~2s instead of ~8s), check the `vEncoder: "copy"` and PR #212 notes in [Part 2](#part-2-homebridge-and-the-nest-plugin) first.

---

## Part 1: Google Device Access

Google provides the [Smart Device Management (SDM) API](https://developers.google.com/nest/device-access) for programmatic access to Nest devices. You need to register for it before any integration can talk to your cameras.

Follow Google's own [Get Started guide](https://developers.google.com/nest/device-access/get-started). The key steps are:

1. Create a Google Cloud Platform (GCP) project and enable the SDM API
2. Create OAuth 2.0 credentials (a client ID and client secret)
3. Register for Device Access at [console.nest.google.com/device-access](https://console.nest.google.com/device-access) — this costs a one-time $5 fee
4. Authorize your Google account and obtain a refresh token

Google's docs cover this well. Here are the gotchas they don't emphasize:

**The Device Access project ID is NOT the GCP project ID.** You'll end up with two IDs that look similar. The Device Access project ID is a UUID like `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`. The GCP project ID is a name like `my-project-123456`. Mixing them up produces a `404: Requested project not found` that's hard to diagnose because the plugin only logs "Plugin initialization failed" without the underlying error. (See [issue #215](https://github.com/potmat/homebridge-google-nest-sdm/issues/215) for details.)

**You must use a personal Google account (@gmail.com).** Google Workspace accounts (@yourdomain.com) cannot register for Device Access. The console will show "Your account doesn't meet the requirements" and offer to switch accounts. Once a project is linked to an account, it cannot be moved.

**Projects created after January 2025 must self-host their Pub/Sub topic.** Google stopped offering hosted topics for new projects ([release notes, 2025-01-23](https://developers.google.com/nest/device-access/release-notes)). You'll need to create a topic in your GCP project, grant `group:sdm-publisher@googlegroups.com` the `roles/pubsub.publisher` role, create a pull subscription, and register the topic in the Device Access Console. This is only needed for motion/doorbell events — camera streaming works without it.

**Include the `pubsub` scope in your refresh token** if you want motion and doorbell notifications. The authorization URL should include both `https://www.googleapis.com/auth/sdm.service` and `https://www.googleapis.com/auth/pubsub`.

## Part 2: Homebridge and the Nest Plugin

[Homebridge](https://homebridge.io/) bridges non-HomeKit devices into Apple Home. Install it following the [official guide](https://github.com/homebridge/homebridge/wiki) — Docker is the easiest path on a Pi.

Then install the Nest plugin by [@potmat](https://github.com/potmat):

```bash
# From the Homebridge UI (Settings > Plugins > Search), or:
npm install homebridge-google-nest-sdm
```

Configure it with your Device Access credentials in `config.json`:

```json
{
    "platform": "homebridge-google-nest-sdm",
    "clientId": "YOUR_GCP_CLIENT_ID",
    "clientSecret": "YOUR_GCP_CLIENT_SECRET",
    "projectId": "YOUR_DEVICE_ACCESS_PROJECT_UUID",
    "refreshToken": "YOUR_REFRESH_TOKEN",
    "subscriptionId": "projects/YOUR_GCP_PROJECT/subscriptions/YOUR_SUB_NAME",
    "gcpProjectId": "YOUR_GCP_PROJECT_ID",
    "vEncoder": "copy"
}
```

Two things to note:

**Set `vEncoder` to `"copy"`.** Your Nest cameras send H264 video. HomeKit wants H264 video. The default setting re-encodes it with x264, which wastes CPU and adds seconds of latency. `"copy"` passes the video through untouched. The plugin's README already mentions this option but understates how much it helps.

**For faster live stream startup**, install [PR #212](https://github.com/potmat/homebridge-google-nest-sdm/pull/212) by [@littlepope81](https://github.com/littlepope81). This unmerged PR adds three optimizations — FIR keyframe requests instead of PLI, frame-rate probe skipping (`-fpsprobesize 0`), and REMB bandwidth signaling — that reduce the time to first video frame from ~8 seconds to ~2 seconds. Measured on a Pi 4: first keyframe fully received at **+2127ms**. (The remaining ~4s to see the tile is Apple's own HomeKit setup, not addressable from Homebridge.)

```bash
# Install PR #212 from the author's branch:
npm install https://github.com/littlepope81/homebridge-google-nest-sdm/tarball/feature/configurable-analyzeduration
```

After restarting Homebridge, your cameras should appear in Apple Home. Live streams will work. But the tiles show a Google logo or a blank image — that's the problem this guide exists to solve.

### Pub/Sub event handler bug

If you've set up Pub/Sub events, be aware of a bug in the plugin: the event handler crashes on `relationUpdate` events (which Google sends when device/room relationships change, including right after you enable events). The fix is a 3-line guard in `dist/sdm/Api.js` — add this before `if (event.resourceUpdate.events) {`:

```javascript
if (!event || !event.resourceUpdate) {
    this.log.debug('Event without resourceUpdate (e.g. relationUpdate), ignoring');
    return;
}
```

See [issue #214](https://github.com/potmat/homebridge-google-nest-sdm/issues/214) for the full stack trace.

## Part 3: go2rtc — Warm Streams and Snapshots

[go2rtc](https://github.com/AlexxIT/go2rtc) by [@AlexxIT](https://github.com/AlexxIT) is a streaming tool that supports dozens of camera protocols, including Google Nest via the SDM API. It can:

- Connect to your Nest cameras over WebRTC
- Automatically extend the stream before Google's 5-minute expiry
- Keep streams permanently warm via its `preload:` feature
- Serve JPEG snapshots from a warm stream via `/api/frame.jpeg`
- Re-serve the stream over RTSP for other consumers

This is the piece that makes real snapshots possible: keep one stream warm per camera, and grab a frame whenever HomeKit asks.

### The IPv6 bug

**Stock go2rtc (v1.9.14 as of this writing) cannot stream Nest cameras on many home networks.** Its `nest:` source uses [pion/webrtc](https://github.com/pion/webrtc) for WebRTC negotiation, and pion gathers ICE candidates on all network types including IPv6. On hosts where IPv6 addresses exist but have no working route — which is extremely common — the ICE agent fails silently and no media flows. You'll see `nest: wrong status: 400 Bad Request` in the logs or streams that start but never produce video.

The `webrtc: filters:` YAML config exists for restricting network types, but `pkg/nest/client.go` bypasses it by calling `webrtc.NewAPI()` with nil filters. There is no config-only workaround. See [go2rtc #2311](https://github.com/AlexxIT/go2rtc/issues/2311) for discussion and diagnostic data.

**Try stock go2rtc first.** If it works, you don't need the patch. If you see the symptoms above, use this fork which fixes it with one line — forcing IPv4-only ICE:

```go
// was: rtcAPI, err := webrtc.NewAPI()
rtcAPI, err := webrtc.NewServerAPI("", "", &webrtc.Filters{Networks: []string{"udp4"}})
```

The fork also removes an inner retry loop in `rtcConn` that burned ~130 SDM API calls/hour per offline camera (over Google's documented 100/hour quota).

### Build from this fork

```bash
git clone https://github.com/ajplotkin/go2rtc.git
cd go2rtc
git checkout fix/nest-ipv6-ice-failure

# Build natively on a Pi (arm64, ~3 min):
docker run --rm -v "$PWD":/src -w /src \
  -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod \
  golang:1.24-alpine sh -c \
  "CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o go2rtc_patched ."
```

Create a Docker image using [@AlexxIT](https://github.com/AlexxIT)'s base (which provides the ffmpeg needed for JPEG transcoding):

```bash
mkdir -p ~/go2rtc-nest
cp go2rtc_patched ~/go2rtc-nest/

cat > ~/go2rtc-nest/Dockerfile <<'EOF'
FROM alexxit/go2rtc:1.9.14
COPY go2rtc_patched /usr/local/bin/go2rtc
EOF

docker build -t go2rtc-nestfix:1.9.14 ~/go2rtc-nest/
```

### Discover cameras and generate the config

The script below reads your Nest credentials from Homebridge's own `config.json` (single source of truth — no duplicated secrets), discovers cameras via the SDM API, and writes a go2rtc config with a warm stream per camera.

**Important:** The `nest:` source URL must be properly URL-encoded. The refresh token contains `//` which must be encoded as `%2F%2F`, and `protocols=WEB_RTC` must be present. Hand-written URLs will fail with a 400. This script handles encoding automatically. If you skip it, use go2rtc's own `GET /api/nest` endpoint to generate correctly-encoded URLs.

Save as `~/scripts/nest-go2rtc-sync.py`:

```python
#!/usr/bin/env python3
"""
Auto-discovers Nest cameras from the SDM API and generates go2rtc.yaml.
Reads credentials from Homebridge's config.json.

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
ap.add_argument("--container", default="go2rtc", help="Docker container to restart")
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
        sys.exit(f"ERROR: duplicate room key '{k}' -- two devices in same room")
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

Run it:

```bash
python3 ~/scripts/nest-go2rtc-sync.py \
  --hb-config /path/to/homebridge/config.json \
  --out ~/go2rtc-nest/go2rtc.yaml
```

### Start go2rtc

```bash
docker run -d --name go2rtc \
  --restart unless-stopped \
  --network host \
  -v ~/go2rtc-nest/go2rtc.yaml:/config/go2rtc.yaml \
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

# Grab a test snapshot
curl -o /tmp/test.jpg "http://127.0.0.1:1985/api/frame.jpeg?src=front_door&cache=30s"
file /tmp/test.jpg   # should say "JPEG image data"
```

If streams show "cold", check the go2rtc logs (`docker logs go2rtc`). Common causes: camera switched off in the Google Home app (`FAILED_PRECONDITION`), IPv6 issue (see above), or URL encoding problems.

## Part 4: The Snapshot Warmer

You now have go2rtc serving JPEG snapshots via HTTP. The obvious approach is to have the Homebridge plugin call that endpoint directly whenever HomeKit asks for a snapshot. **Don't do this.** It fails under real-world conditions:

- HomeKit polls tiles roughly every 10 seconds. go2rtc's JPEG cache lasts 30 seconds. Every third poll is a **cache miss**, which spins up ffmpeg and takes ~1.5 seconds — triggering Homebridge's "snapshot handler is slow to respond" warning.
- Worse: **two concurrent cache misses return HTTP 500**, and the plugin's error path falls back to the placeholder logo. The logo flashes back intermittently.

The solution is a warmer script that pre-fetches a JPEG every 10 seconds and writes it to a file. The plugin reads the file (~1ms, never races, never 500s). On motion or doorbell events, the plugin signals the warmer to grab a fresh frame immediately — so the tile shows *who's there*, not a 10-second-old empty porch.

### SD card wear

**If your system runs on an SD card** (most Raspberry Pis), put the snapshots in tmpfs (RAM). Writing ~100KB JPEGs every 10 seconds per camera is ~1.5 GB/day of flash writes for data that's pure cache — the warmer rebuilds it in seconds after a reboot. tmpfs costs about 200KB of RAM.

```bash
echo 'd /run/nest-snaps 0755 1000 1000 -' | sudo tee /etc/tmpfiles.d/nest-snaps.conf
sudo systemd-tmpfiles --create /etc/tmpfiles.d/nest-snaps.conf
```

### The warmer script

The warmer auto-discovers streams from go2rtc (no hardcoded camera list) and only polls streams that have active media — cameras that are off are skipped, avoiding wasted SDM quota. Stale files are pruned after 2 minutes so an off camera shows the honest placeholder rather than a frozen frame.

It also watches for an **event trigger**: when the plugin receives a motion or doorbell event, it touches a signal file, and the warmer grabs a fresh frame within 1 second instead of waiting for the next cycle.

Save as `~/scripts/go2rtc-snapshot-warmer.sh`:

```bash
#!/bin/bash
# Baseline: refresh every 10s (HomeKit polls ~10s, go2rtc cache 30s -> always a hit).
# Event-triggered: touch /run/nest-snaps/.refresh for an immediate cycle.
DIR=/run/nest-snaps
API=http://127.0.0.1:1985
INTERVAL=10
mkdir -p "$DIR"
refresh_all() {
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
}
while true; do
  refresh_all
  for i in $(seq 1 $INTERVAL); do
    if [ -f "$DIR/.refresh" ]; then
      rm -f "$DIR/.refresh"
      refresh_all
      break
    fi
    sleep 1
  done
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

### Patch the Homebridge plugin

Mount the snapshot directory into the Homebridge container. **Without this mount, the plugin can't see the files and tiles will show the placeholder:**

```bash
docker run -d --name homebridge \
  ... \
  -v /path/to/homebridge:/homebridge \
  -v /run/nest-snaps:/homebridge/nest-snaps \
  homebridge/homebridge:latest
```

Patch two files in `node_modules/homebridge-google-nest-sdm/dist/sdm/`:

**Camera.js** — two changes:

First, add this at the top of `getSnapshot()`, before the existing logo-fallback code:

```javascript
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

The 90-second mtime check prevents a camera that was turned off from showing an indefinitely stale frame. After 90 seconds without a refresh, it falls back to the placeholder — which is the honest answer when the camera is off.

The key derivation (`toLowerCase()`, non-alphanum to `_`, strip leading/trailing `_`) must match the Python `stream_key()` function in the sync script. Both derive from the SDM room name (the `displayName` in `parentRelations`).

Second, in the `event()` method, find the `if (this.onMotion)` line (inside the `CameraMotion`/`CameraPerson` case) and add this just before it:

```javascript
try { require('child_process').execSync('touch /homebridge/nest-snaps/.refresh'); } catch(e) {}
```

This signals the warmer to grab a fresh frame immediately when motion or a person is detected, so the tile shows who's there rather than a stale frame from the last cycle.

**Both patches live in `node_modules` and will be wiped by any `npm install` of the plugin.** Save copies outside `node_modules` with a script that re-applies them.

## Part 5: Verify

```bash
docker restart homebridge
```

After about 30 seconds:

1. **Check warm streams:** `curl -s http://127.0.0.1:1985/api/streams` — each camera should show receiver bytes increasing
2. **Check snapshot files:** `ls -la /run/nest-snaps/` — a `.jpg` per camera, refreshing every ~10 seconds (and immediately on motion events)
3. **Open Apple Home** — tiles should show real camera images instead of the placeholder

### Troubleshooting

**Tiles show the placeholder for some cameras:** Check if those cameras are switched off in the Google Home app. Google returns `FAILED_PRECONDITION: "The camera is not available for streaming"` for off cameras. The system handles this gracefully (warmer skips them, stale files are pruned, plugin shows the placeholder), but the camera needs to actually be on.

**Google Home's "Home/Away Assist"** may automatically turn cameras off when you're home. This is the most common reason for cameras that work sometimes and not others. Check: Google Home app > Settings > Home & Away Routines.

**`nest: wrong status: 400 Bad Request`** in go2rtc logs: Most likely the IPv6 issue described above. Use the patched fork. Can also be a URL encoding problem — always generate URLs via the sync script or go2rtc's `GET /api/nest` endpoint.

**Node v24.17.0 breaks the plugin entirely** with `ERR_STREAM_PREMATURE_CLOSE` on every OAuth call. This is a Node.js regression ([nodejs/node#63989](https://github.com/nodejs/node/issues/63989)), not a plugin bug. Fixed in Node 24.18.0. If you're on the official Homebridge Docker image, pull the latest.

## Reference

### SDM API Quotas

Source: [developers.google.com/nest/device-access/project/limits](https://developers.google.com/nest/device-access/project/limits)

| Limit | Value |
|---|---|
| `devices.executeCommand` per project/user | 10 QPM |
| Per trait command per device | 5 QPM |
| CAMERA/DOORBELL device instance | 30 QPM or 100 QPH |

Preload costs ~12 `ExtendWebRtcStream` calls/hour/camera — well within the 100 QPH device limit. The warmer makes zero SDM calls (it reads from go2rtc's local cache).

### Performance (measured on Raspberry Pi 4, arm64)

| Metric | Value |
|---|---|
| Cached snapshot served | ~26 ms |
| CPU (idle, with 3 warm streams) | ~0% (brief spikes during 10s transcode cycle) |
| Bandwidth per camera | ~1.5 Mbps continuous |
| RAM for snapshot files | ~200 KB |
| Stream startup (with PR #212 + `vEncoder: "copy"`) | First keyframe at +2127ms |

### Related Issues and PRs

- [go2rtc #2311](https://github.com/AlexxIT/go2rtc/issues/2311) — `nest: wrong status: 400` / IPv6 ICE failure diagnosis
- [homebridge-google-nest-sdm #214](https://github.com/potmat/homebridge-google-nest-sdm/issues/214) — Api.js crash on `relationUpdate` events
- [homebridge-google-nest-sdm #215](https://github.com/potmat/homebridge-google-nest-sdm/issues/215) — README corrections (project ID confusion, self-hosted Pub/Sub, Node regression)
- [homebridge-google-nest-sdm PR #212](https://github.com/potmat/homebridge-google-nest-sdm/pull/212) — stream startup latency fix by [@littlepope81](https://github.com/littlepope81)

## License

go2rtc is [MIT licensed](https://github.com/AlexxIT/go2rtc/blob/master/LICENSE). This fork adds a one-line patch to `pkg/nest/client.go`.
