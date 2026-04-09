# eebus-go-rest (WebSocket + EEBUS Runtime)

This backend provides the EEBUS runtime for the project and exposes a WebSocket API for device discovery, simulation control, and LPC/MPC related communication.

## Attribution

This project is based on and forked from work created by **TumbleOwlee**.
The underlying module path and core structure are inherited from that original project:

- `github.com/tumbleowlee/eebus-go-rest`

Thank you to TumbleOwlee for the original implementation.

## What eebus-go-rest does

- Starts an EEBUS service using `enbility/eebus-go`
- Generates a local certificate/SKI at startup (current implementation)
- Continuously discovers EEBUS devices via mDNS (`_ship._tcp`)
- Maintains a live list of discovered devices and connection state
- Exposes a WebSocket endpoint used by the frontend
- Accepts control messages for simulation and LPC commands
- Broadcasts updates (discovery, connection state, MPC/LPC updates) to all connected clients

## Tech stack

- Go `1.23`
- [gin](https://gin-gonic.com/) (HTTP server)
- [gorilla/websocket](https://github.com/gorilla/websocket) (WebSocket transport)
- [gin-contrib/cors](https://github.com/gin-contrib/cors) (CORS)
- [enbility/eebus-go](https://github.com/enbility/eebus-go), [ship-go](https://github.com/enbility/ship-go), [spine-go](https://github.com/enbility/spine-go)
- [grandcat/zeroconf](https://github.com/grandcat/zeroconf) (mDNS discovery)

## Project structure

```text
eebus-go-rest/
	README.md
	src/
		go.mod
		cmd/
			main.go                # App entrypoint (HTTP + WebSocket)
		config/
			config.go              # Config model + cert generation helper
		server/
			eebus/
				runtime.go           # EEBUS runtime + hub + discovery + control methods
				config.go            # Runtime config struct
			model/
				*.go                 # API payload and domain models
```

## Prerequisites

- Go `1.23+`
- Local network/multicast support for mDNS discovery
- At least one EEBUS-capable peer on the network (optional, but needed for meaningful runtime interaction)

## Run locally

From repository root:

```bash
cd backend/src
go mod download
go run ./cmd
```

By default, eebus-go-rest listens on:

- `http://localhost:8080`
- WebSocket endpoint: `ws://localhost:8080/ws`

## Frontend integration notes

- CORS currently allows: `http://localhost:4321`
- If your frontend runs on another origin, update `AllowOrigins` in `src/cmd/main.go`

## Runtime behavior

- A new certificate/SKI is created on each start in the current flow.
- A random `SIM-XXXXXXXXXX` serial is generated at startup.
- Continuous discovery runs every 5 seconds.
- Hub broadcasts include a `timestamp` field (unix seconds).

## WebSocket message protocol

All payloads follow this envelope:

```json
{
	"type": "message_type",
	"data": {}
}
```

### Client -> Server messages

- `get_local_ski`
- `get_remote_skis`
- `register_ski` with `{ "ski": "..." }`
- `register_skis` with `{ "skis": ["...", "..."] }`
- `get_lpp`
- `get_lpc`
- `get_log_level`
- `set_log_level` with `{ "level": "0|1|2|3" }`
- `mdns_discovery`
- `start_simulation` with `{ "devices": ["ski1", "ski2"] }`
- `stop_simulation`
- `add_device` with `{ "ski": "..." }`
- `remove_device` with `{ "ski": "..." }`
- `lpc_limit` with `{ "ski": "...", "limit": 5000, "duration": 60, "active": true }`
- `lpc_failsafe_value` with `{ "ski": "...", "value": 3000 }`
- `lpc_failsafe_duration` with `{ "ski": "...", "duration": 120 }`

### Server -> Client messages (examples)

- `connected`
- `local_ski`
- `remote_skis`
- `ski_registered`, `skis_registered`
- `log_level`, `log_level_changed`
- `mdns_discovery`, `new_devices_discovered`
- `connection_state_update`
- `simulation_started`, `simulation_stopped`, `simulation_error`
- `device_added`, `device_removed`
- `lpc_update`, `lpc_limit_set`, `lpc_failsafe_value_set`, `lpc_failsafe_duration_set`
- `mpc_update`
- `error`

## Example: quick WebSocket check

Use `wscat` to verify connectivity:

```bash
npx wscat -c ws://localhost:8080/ws
```

Then send:

```json
{"type":"get_local_ski"}
```

## Troubleshooting

- No devices discovered:
	- Ensure the host network allows multicast/mDNS.
	- Ensure peer devices publish `_ship._tcp` on `local.`.
- Frontend cannot connect:
	- Verify eebus-go-rest is running on port `8080`.
	- Verify frontend origin is included in CORS `AllowOrigins`.
- Commands fail with SKI errors:
	- Discover devices first and confirm the SKI exists.
	- Start simulation with target device SKIs before expecting simulation-scoped updates.

## Security and development caveats

- WebSocket origin check currently allows all origins (`CheckOrigin` returns `true`) and should be tightened for production.
- TLS and certificate persistence strategy is development-oriented right now; persisting certs/SKI is recommended for stable identity.

## License

See `LICENSE`.
