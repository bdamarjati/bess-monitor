# BESS Monitor

A small web app for monitoring all-in-one battery energy storage units (battery, solar, grid and load).
Devices publish telemetry over MQTT. The app stores it in SQLite and serves a dashboard behind a sign-in page.
It ships as one static Go binary, with no Docker, no JS framework and no frontend build step.

```
Device -> Mosquitto (MQTT) -> bess-monitor (subscriber + API + dashboard) -> SQLite
Browser -> Caddy (HTTPS) -> bess-monitor (127.0.0.1:8080)
```

## Features

- **Dashboard**: system status (OK / Fault and error code), a stale-data warning, and live energy flow between solar, grid, inverter, load and battery.
- **Battery**: SoC gauge, SoH, voltage, current, temperatures, and per-cell voltages with the lowest and highest cell highlighted.
- **History**: SoC, power, battery current and cell range charts over 1h, 6h, 24h, 7d, 30d, 90d or 1y.
- **Data tiers**: raw readings are rolled up into 1-minute and 1-hour averages. A fault is kept as the max, so it is never averaged away.
- **Sign-in**: one user, signed session cookie, and a lockout after repeated failed attempts.

## Requirements

- Go 1.25+ (only to build; the server just needs the binary)
- An MQTT broker, e.g. Mosquitto
- `sqlite3` CLI (optional, for inspecting data)

## Quick start (local)

```sh
cp .env.example .env              # then set DASH_PASS
mosquitto -p 1883                 # terminal 1: local broker
go run ./cmd/bess-monitor         # terminal 2: the app
go run ./cmd/fakepub -devices MOS-AIO-30001,MOS-AIO-30002   # terminal 3: fake devices
open http://localhost:8080        # sign in with DASH_USER / DASH_PASS
```

Readings are written to the database every 10 s, so the first values appear within a few seconds.

## Configuration

Set these in the environment or in `.env`. Real environment variables override `.env`.
Keep `.env` private (`chmod 600 .env`).

| Variable | Default | Description |
|---|---|---|
| `MQTT_URL` | `tcp://127.0.0.1:1883` | Broker URL. Use `ssl://host:8883` for TLS. |
| `MQTT_USER`, `MQTT_PASS` | (empty) | Broker credentials. |
| `MQTT_TOPIC` | `bess/+/telemetry` | Subscription topic (wildcard). |
| `DB_PATH` | `bess.db` | SQLite file. |
| `HTTP_ADDR` | `127.0.0.1:8080` | Listen address. Keep it on localhost behind Caddy. |
| `DASH_USER`, `DASH_PASS` | (none, required) | Dashboard sign-in. The app will not start without them. |
| `SESSION_SECRET` | random per start | Signs session cookies. Set it (`openssl rand -hex 32`) to keep users signed in across restarts. |
| `RETENTION_RAW_DAYS` | `7` | Days to keep raw readings. |
| `RETENTION_1M_DAYS` | `90` | Days to keep 1-minute averages. |
| `RETENTION_1H_DAYS` | `1825` | Days to keep 1-hour averages (5 years). |

## Telemetry

Topic: `bess/{device_id}/telemetry`. JSON payload, sent every 5–10 s:

```json
{
  "device_id": "MOS-AIO-30001",
  "status": 0,
  "battery": {
    "error_code": 0, "soc": 88, "soh": 99, "voltage": 24.8, "current": 1.2,
    "temperature": [28.1, 28.2],
    "cell_voltage": [3210, 3209, 3211, 3210, 3210, 3212, 3208]
  },
  "solar": { "voltage": 26.2, "current": 5.2, "power": 136.24 },
  "grid":  { "voltage": 220.3, "current": 1.2, "power": 264.36, "freq": 50.1 },
  "load":  { "voltage": 220.3, "current": 1.2, "power": 264.36, "freq": 50.1 }
}
```

- `status`: `0` = OK, `255` = Fault.
- Battery `current`: positive = charging, negative = discharging.
- Units: V, A, W, Hz, °C. Cell voltages are in mV.
- The payload has no timestamp: the server stamps each message when it arrives.
- Each message is validated. Malformed messages are logged and dropped.

All decoding lives in [internal/ingest/decode.go](internal/ingest/decode.go), so a wire-format change only touches that file.

## API

All endpoints except `/healthz` and `/login` require a signed-in session.
Without one, `/api/*` returns 401 and pages redirect to `/login`.

| Endpoint | Returns |
|---|---|
| `GET /api/devices` | Known devices with last-seen time: `[{"device_id":"…","last_ts":…}]` |
| `GET /api/latest?device_id=` | Newest reading, same shape as the payload plus `ts`. |
| `GET /api/history?device_id=&hours=24` | Columns for charts (`ts`, `bat_soc`, `bat_current`, `bat_voltage`, `solar_power`, `grid_power`, `load_power`, `cell_min_mv`, `cell_max_mv`), at most 500 points. `hours` is 1–43800. |
| `GET /healthz` | `ok` |

If `device_id` is left out and only one device exists, that device is used.
History reads raw data for ranges up to 24 h, 1-minute averages up to 90 days, and 1-hour averages beyond that.
`null` values mark gaps in the data.

## Data retention

| Table | Resolution | Kept for | Rolled up |
|---|---|---|---|
| `readings` | as received (~5 s) | 7 days | every 5 min into `readings_1m` |
| `readings_1m` | 1 minute | 90 days | every 10 min into `readings_1h` |
| `readings_1h` | 1 hour | 5 years | (last tier) |

Rollups catch up on startup after any downtime, and re-running them gives the same result.
Cleanup runs daily. It only deletes rows that are already in the next tier, so no data is lost before it is rolled up.

## Fake publisher

`cmd/fakepub` simulates one or more devices through a full day: solar rising and falling, varying load, the battery charging from surplus and discharging otherwise, and the grid covering the rest.

| Flag | Default | Description |
|---|---|---|
| `-devices` | `MOS-AIO-30001` | Comma-separated device IDs. |
| `-interval` | `5s` | Publish interval. |
| `-speed` | `60` | Simulated seconds per real second (60 = one day in 24 minutes). |
| `-fault` | `0` | Chance (0–1) per message of starting a fault. |
| `-bad` | `0` | Chance (0–1) per message of sending a malformed payload. |

It uses the same `MQTT_*` settings as the app.

## Development

```sh
go test ./...                     # unit tests
go vet ./...
```

Build for the server:

```sh
GOOS=linux GOARCH=amd64 go build -o bess-monitor ./cmd/bess-monitor
```

### Project layout

```
cmd/bess-monitor/   entry point: wires MQTT, storage, jobs and HTTP; graceful shutdown on SIGTERM
cmd/fakepub/        fake device publisher for testing
internal/config/    settings from the environment and .env
internal/ingest/    MQTT subscriber, payload decode/validation, batched writes
internal/store/     SQLite schema, queries, downsampling, rollups and cleanup
internal/api/       HTTP handlers and static file serving
internal/auth/      sign-in, session cookies, lockout
web/                index.html, login.html, vendored uPlot (embedded in the binary)
```

## Deployment

The app is designed for a single VPS:

- **Mosquitto**, installed with apt: no anonymous access, a username and password per device, and TLS on port 8883.
- **Caddy**, installed with apt: HTTPS and a reverse proxy to `127.0.0.1:8080`. Caddy does not need basic auth, because the app has its own sign-in.
- **bess-monitor**, as a systemd service, with `.env` next to the binary.
- **Firewall**: open only ports 80, 443 and 8883.

The deployment files (systemd unit, Caddyfile, Mosquitto config) are planned and not yet in this repository.
