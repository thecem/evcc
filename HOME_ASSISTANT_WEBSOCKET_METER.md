# Home Assistant WebSocket Meter for evcc

## Overview

This implementation provides **real-time WebSocket integration** between evcc and Home Assistant, eliminating HTTP polling delays and providing instant power updates for optimal solar charging control.

## Features

✅ **Real-time updates** via WebSocket connection  
✅ **Zero polling latency** - instant state changes  
✅ **Automatic reconnection** on connection loss  
✅ **Secure authentication** with Home Assistant tokens  
✅ **Template-based configuration** following evcc patterns  
✅ **Energy interface support** (optional)  

## Required Files

### 1. Core Implementation
- **`/workspaces/evcc/meter/homeassistant_ws.go`** - Main WebSocket meter implementation
- **`/workspaces/evcc/templates/definition/meter/homeassistant-ws.yaml`** - Template definition

### 2. Configuration
- **`evcc.yaml`** - Your main evcc configuration file

## File Structure

```
evcc/
├── meter/
│   └── homeassistant_ws.go          # WebSocket meter implementation
├── templates/definition/meter/
│   └── homeassistant-ws.yaml        # Template definition
└── evcc.yaml                        # Main configuration
```

## Configuration Guide

### 1. Home Assistant Setup

#### Create Long-Lived Access Token
1. Go to **Home Assistant → Profile → Security → Long-lived access tokens**
2. Click **"Create Token"**
3. Name it: `evcc-websocket-access`
4. Copy the token (you'll need this for evcc.yaml)

#### Verify Entity IDs
Make sure your sensor entities exist in Home Assistant:
- **Power sensors:**
  - Grid power: `sensor.grid_power` or `sensor.sml_0100100700ff`
  - PV power: `sensor.solar_pv_power_total`
  - Battery power: `sensor.battery_power` (optional)
- **Energy sensors (optional):**
  - Grid energy: `sensor.grid_energy_total` or `sensor.sml_0100011000ff`
  - PV energy: `sensor.solar_pv_energy_total`
  - Battery energy: `sensor.battery_energy_total`

### 2. evcc Configuration

Add WebSocket meters to your `evcc.yaml`:

```yaml
meters:
  # Grid meter with real-time WebSocket updates AND energy
  - name: ha_grid_websocket
    type: template
    template: homeassistant-ws
    usage: grid
    uri: ws://192.168.1.100:8123/api/websocket
    token: eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9...  # Your token
    entity: sensor.sml_0100100700ff              # Power sensor
    energy: sensor.grid_energy_total             # Energy sensor (optional)
    scale: 1

  # PV meter with real-time WebSocket updates AND energy
  - name: ha_pv_websocket
    type: template
    template: homeassistant-ws
    usage: pv
    uri: ws://192.168.1.100:8123/api/websocket
    token: eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9...  # Your token
    entity: sensor.solar_pv_power_total          # Power sensor
    energy: sensor.solar_pv_energy_total         # Energy sensor (optional)
    scale: 1

site:
  title: My Home
  meters:
    grid: ha_grid_websocket    # Use WebSocket meter with energy
    pv: ha_pv_websocket        # Use WebSocket meter with energy
```

### 3. Template Parameters

| Parameter | Required | Description | Example |
|-----------|----------|-------------|---------|
| `uri` | ✅ | Home Assistant WebSocket endpoint | `ws://192.168.1.100:8123/api/websocket` |
| `token` | ✅ | Long-lived access token | `eyJ0eXAiOiJKV1Q...` |
| `entity` | ✅ | Power sensor entity ID | `sensor.grid_power` |
| `energy` | ❌ | Energy sensor entity ID (optional) | `sensor.grid_energy_total` |
| `scale` | ❌ | Value scaling factor | `1` (default), `-1` to invert, `1000` for kW→W |

## How It Works

### 1. Connection Flow
```
evcc startup → WebSocket dial → Home Assistant
             ↓
    auth_required ← Home Assistant  
             ↓
    Send token → Home Assistant
             ↓  
       auth_ok ← Home Assistant
             ↓
   Subscribe to state_changed events
             ↓
   Real-time power updates ⚡
```

### 2. Real-time Updates
- **WebSocket connection** establishes on evcc startup
- **Subscribes** to `state_changed` events for your entities
- **Instant updates** when sensor values change in Home Assistant
- **Automatic reconnection** if connection drops

### 3. Data Flow
```
Home Assistant Sensors → state_changed events → WebSocket → evcc meter → Site control
                      ↓
Power: sensor.grid_power → Real-time W updates ⚡
Energy: sensor.grid_energy → Real-time kWh updates 🔋
```

## Advanced Configuration

### Multiple Entity Support
```yaml
- name: ha_grid_advanced
  type: template
  template: homeassistant-ws
  usage: grid
  uri: ws://192.168.1.100:8123/api/websocket
  token: eyJ0eXAiOiJKV1Q...
  entity: sensor.grid_power_watts         # Power sensor
  energy: sensor.grid_energy_kwh          # Energy sensor (optional)
  scale: 1  # Already in Watts/kWh
```

### Scaling Examples
```yaml
# Convert kW to W
scale: 1000

# Invert values (export = negative)
scale: -1

# Convert W to kW (not recommended, evcc expects W)
scale: 0.001
```

### SSL/TLS Support
```yaml
uri: wss://my-homeassistant.duckdns.org:8123/api/websocket  # HTTPS/WSS
```

## Energy Support 🔋

### What's Included
- ✅ **Real-time energy tracking** via WebSocket
- ✅ **Optional energy entities** - no errors if not configured  
- ✅ **Same scaling support** as power values
- ✅ **Implements `api.MeterEnergy` interface**

### Energy Configuration
```yaml
meters:
  - name: ha_grid_with_energy
    type: template
    template: homeassistant-ws
    usage: grid
    uri: ws://192.168.1.100:8123/api/websocket
    token: your_token_here
    entity: sensor.grid_power               # Required: Power in W
    energy: sensor.grid_energy_total        # Optional: Energy in kWh
    scale: 1
```

### Common Energy Sensors
| Meter Type | Power Entity | Energy Entity |
|------------|--------------|---------------|
| **Grid** | `sensor.grid_power` | `sensor.grid_energy_total` |
| **PV** | `sensor.solar_power` | `sensor.solar_energy_today` |
| **Battery** | `sensor.battery_power` | `sensor.battery_energy` |
| **SML** | `sensor.sml_0100100700ff` | `sensor.sml_0100011000ff` |

### Energy Units
- **Power**: Watts (W) - instant power consumption/generation
- **Energy**: Kilowatt-hours (kWh) - cumulative energy over time
- **Scaling**: Use same `scale` parameter for both power and energy

### How It Works
1. **Power updates** → Instant WebSocket notifications for current usage
2. **Energy updates** → Cumulative tracking for daily/total consumption
3. **Both optional** → Power required, energy is optional enhancement

## Troubleshooting

### Common Issues

#### 1. "Connection refused"
- ✅ Check Home Assistant IP address and port
- ✅ Verify WebSocket API is enabled
- ✅ Test: `ws://IP:8123/api/websocket` in browser

#### 2. "Authentication failed"  
- ✅ Verify token is correct and not expired
- ✅ Create new long-lived access token
- ✅ Check token permissions

#### 3. "Entity not found"
- ✅ Verify entity ID exists in Home Assistant
- ✅ Check **Developer Tools → States** for exact entity ID
- ✅ Ensure sensor produces numeric values

#### 4. "Scale issues"
- ✅ Check sensor units (W vs kW vs MW)
- ✅ Adjust scale factor accordingly
- ✅ Use `-1` for inverted values

### Debug Logging

Enable debug logging in `evcc.yaml`:
```yaml
log: debug
```

Look for these log messages:
- `🔗 Connected to ws://...` - Connection successful
- `🔐 Authenticated successfully` - Token works
- `📡 Subscribed to real-time updates` - Subscription active
- `⚡ Power updated: sensor.xxx = 123.4` - Real-time power data
- `🔋 Energy updated: sensor.xxx_energy = 456.7` - Real-time energy data

## Performance Benefits

| Method | Latency | Efficiency | Real-time |
|--------|---------|------------|-----------|
| **HTTP Polling** | 3-30 seconds | ❌ Wasteful | ❌ Delayed |
| **WebSocket** | ~50ms | ✅ Efficient | ✅ Instant |

### Real Impact
- **Instant response** to power changes
- **Better PV optimization** with immediate data
- **Reduced Home Assistant load** (no constant polling)
- **More accurate charging decisions**

## Example Working Configuration

Here's a complete example that works:

```yaml
network:
  port: 7070

log: debug

meters:
  # Real-time WebSocket meters
  - name: ha_grid_rt
    type: template
    template: homeassistant-ws
    usage: grid
    uri: ws://192.168.76.100:8123/api/websocket
    token: eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9.eyJpc3MiOiIyMWJhYzJlZGNiZTQ0YWVhYWUwMTg3ZGFiODM2ODkxMCIsImlhdCI6MTY1NzQ0NjQ3MCwiZXhwIjoxOTcyODA2NDcwfQ.EtakTBpSJ1Sr61U1jmLb4PQH77NDxNnZcMC6_w1cCx0
    entity: sensor.sml_0100100700ff
    scale: 1

  - name: ha_pv_rt  
    type: template
    template: homeassistant-ws
    usage: pv
    uri: ws://192.168.76.100:8123/api/websocket
    token: eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9.eyJpc3MiOiIyMWJhYzJlZGNiZTQ0YWVhYWUwMTg3ZGFiODM2ODkxMCIsImlhdCI6MTY1NzQ0NjQ3MCwiZXhwIjoxOTcyODA2NDcwfQ.EtakTBpSJ1Sr61U1jmLb4PQH77NDxNnZcMC6_w1cCx0
    entity: sensor.solar_pv_power_total
    energy: sensor.solar_pv_energy_total    # Add energy tracking
    scale: 1

site:
  title: Real-time Solar Home
  meters:
    grid: ha_grid_rt
    pv: ha_pv_rt

loadpoints:
  - title: Garage
    charger: my_charger
    mode: pv
```

## Migration from HTTP Polling

### Before (HTTP):
```yaml
- name: ha_grid_old
  type: custom
  power:
    source: http
    uri: http://192.168.1.100:8123/api/states/sensor.grid_power
    headers:
      - authorization: Bearer TOKEN
    jq: .state | tonumber
    timeout: 10s
```

### After (WebSocket):
```yaml
- name: ha_grid_new
  type: template
  template: homeassistant-ws
  uri: ws://192.168.1.100:8123/api/websocket
  token: TOKEN
  entity: sensor.grid_power
```

**Result**: 30-second delays → Instant updates! 🚀

## Conclusion

The Home Assistant WebSocket integrations provide **complete real-time monitoring** for evcc:

### 🔋 **WebSocket Meters:**
- ⚡ **Instant power updates** for grid, PV, battery meters
- 🔄 **Zero polling overhead** 
- 📊 **Better PV optimization**
- 🏠 **Perfect home energy management**

### 🚗 **WebSocket Vehicles:**
- 🔋 **Real-time SoC monitoring** 
- 🛣️ **Instant range updates**
- ⚡ **Live charging status**
- 🌡️ **Climate state tracking**
- 📏 **Odometer monitoring**

This is the **optimal solution** for integrating evcc with Home Assistant sensors and vehicles!

## WebSocket Vehicle Implementation

The same WebSocket technology is also available for vehicles! 

### Required Files (Vehicle):
- **`/workspaces/evcc/vehicle/homeassistant_ws.go`** - WebSocket vehicle implementation
- **`/workspaces/evcc/templates/definition/vehicle/homeassistant-ws.yaml`** - Vehicle template

### Vehicle Configuration Example:
```yaml
vehicles:
  - name: my_ev_websocket
    type: template
    template: homeassistant-ws
    title: My Electric Vehicle (Real-time)
    uri: ws://192.168.1.100:8123/api/websocket
    token: eyJ0eXAiOiJKV1Q...
    soc: sensor.vehicle_state_of_charge       # Required
    range: sensor.vehicle_range               # Optional
    status: sensor.vehicle_charging_state     # Optional  
    limitSoc: number.vehicle_target_soc       # Optional
    odometer: sensor.vehicle_odometer         # Optional
    climater: binary_sensor.vehicle_climate   # Optional
    start_charging: script.vehicle_start      # Optional
    stop_charging: script.vehicle_stop        # Optional
    wakeup: script.vehicle_wakeup            # Optional
```

**Result**: **Instant vehicle data updates** instead of 30-second HTTP polling delays! 🚀
