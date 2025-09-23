# Sensor History Module

A Viam `sensor` component for sensor history from Viam's data management service.

## Model hunter:sensor-history:sensor

This model implements the `rdk:component:sensor` API by simulating a live sensor and fetching a dataset from a specified time range in real-time with configurable speed and looping.

### Configuration

```json
{
  "source_component_name": "<string>",
  "source_component_type": "<string>",
  "start_time_utc": "<string>",
  "end_time_utc": "<string>",
  "loop": false,
  "speed_multiplier": 1.0,
  "cache_size": 10000
}
```

#### Attributes

The following attributes are available for this model:

| Name          | Type   | Inclusion | Description                |
|---------------|--------|-----------|----------------------------|
| `source_component_name` | string  | Required  | The name of the original sensor component for data history (e.g., "cpu"). |
| `source_component_type` | string | Required  | The type of the original sensor component (e.g., "rdk:component:sensor"). |
| `start_time_utc` | string | Required  | The start of the historical time window in RFC3339 format (e.g., "2025-08-14T15:30:00Z"). |
| `end_time_utc` | string | Required  | The end of the historical time window in RFC3339 format. |
| `loop` | boolean | Optional  | If `true`, the history will restart from the beginning upon completion. Defaults to `false`. |
| `speed_multiplier` | float | Optional  | History speed multiplier (e.g., 2.0 for double speed, 0.5 for half speed). Defaults to 1.0. |
| `cache_size` | integer | Optional  | Maximum number of data points to cache in memory. Defaults to 10000. |

#### Example Configuration

```json
{
  "source_component_name": "cpu",
  "source_component_type": "rdk:component:sensor",
  "start_time_utc": "2025-08-14T15:30:00Z",
  "end_time_utc": "2025-08-14T15:45:00Z",
  "loop": true,
  "speed_multiplier": 60.0,
  "cache_size": 10000
}
```

### DoCommand

The sensor-history model supports the following DoCommand operations:

#### Get Statistics

Returns current history statistics.

```json
{
  "command": "get_stats"
}
```

Response:

```json
{
  "total_data_points": 3600,
  "fetch_duration_ms": 2500,
  "is_ready": true,
  "current_index": 1800,
  "loops_completed": 1,
  "last_reading_time": "2025-08-14T16:00:00Z"
}
```

#### Reset History

Resets the history to the beginning of the dataset.

```json
{
  "command": "reset"
}
```

Response:

```json
{
  "status": "reset"
}
```

#### Jump to Percent

Jumps to a specific perentage through the dataset.

```json
{
  "command": "jump_to_percent",
  "percent": 50.0
}
```

Response:

```json
{
  "status": "jumped",
  "index": 1800,
  "timestamp": "2025-08-14T15:35:30Z"
}
```

