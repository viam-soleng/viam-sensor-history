# Module sensor-replay 

A Viam `sensor` component for historical data replay.

## Model hunter:sensor-replay:sensor

This model implements the `rdk:component:sensor` API simulating a live sensor by fetching a dataset from a specified time range and replaying it in real-time.

### Configuration
The following attribute template can be used to configure this model:

```json
{
  "source_component_name": "<string>",
  "source_component_type": "<string>",
  "organization_id": "<string>",
  "start_time_utc": "<string>",
  "end_time_utc": "<string>",
  "api_key_id": "<your-key-id>",
  "api_key": "<your-api-key>",
  "loop": false
}
```

#### Attributes

The following attributes are available for this model:

| Name          | Type   | Inclusion | Description                |
|---------------|--------|-----------|----------------------------|
| `source_component_name` | string  | Required  | The name of the original sensor component whose data will be replayed (e.g., "cpu"). |
| `source_component_type` | string | Required  | The type of the original sensor component (e.g., "rdk:component:sensor"). |
| `organization_id` | string | Required  | The ID of the Viam organization where the data is stored. |
| `start_time_utc` | string | Required  | The start of the historical time window in RFC3339 format (e.g., "2025-08-14T15:30:00Z"). |
| `end_time_utc` | string | Required  | The end of the historical time window in RFC3339 format. |
| `api_key_id` | string | Required  | The ID of the Viam API key with permissions to read data. |
| `api_key` | string | Required  | The Viam API key. |
| `loop` | boolean | Optional  | If `true`, the replay will restart from the beginning upon completion. Defaults to `false`. |

#### Example Configuration

```json
{
  "source_component_name": "cpu",
  "source_component_type": "rdk:component:sensor",
  "organization_id": "31641b32-74e8-4e1c-9509-2cc6d45b45ff",
  "start_time_utc": "2025-08-14T15:30:00Z",
  "end_time_utc": "2025-08-14T15:45:00Z",
  "api_key_id": "api-key-id",
  "api_key": "api-key",
  "loop": false
}
```

### DoCommand

If your model implements DoCommand, provide an example payload of each command that is supported and the arguments that can be used. If your model does not implement DoCommand, remove this section.

#### Example DoCommand

```json
{
  "command_name": {
    "arg1": "foo",
    "arg2": 1
  }
}
```
