# Signal to Info Metric Connector

The Signal to Info Metric connector makes Info metrics from facts in log records. It matches log records with
rules. It extracts facts with [OTTL] value expressions. It keeps the latest facts for each entity, for example for
each pod UID. An Info metric is a gauge with the value `1`. Its data point attributes carry the facts. When the
facts change, the connector emits the metric. It also emits the metric at each `emit_interval`. A storage
extension can persist the state, so that the facts stay available after an agent restart.

The first use case is the vLLM engine startup line. vLLM writes this line one time for each engine start. The
line contains the engine configuration:

```
Initializing a V1 LLM engine (v0.11.0) with config: model='...', tensor_parallel_size=4, ...
```

| Status                   |                                   |
| ------------------------ | --------------------------------- |
| Stability                | [alpha]                           |
| Supported pipeline types | exporter: logs, receiver: metrics |
| Distributions            | [amazon-cloudwatch-agent]         |

The connector does not change the logs that it consumes. Thus, it can share a logs pipeline with other exporters.

## Configuration

### Connector settings

| Name                     | Description                                                                                                                         | Default           | Bounds                                  |
|--------------------------|-------------------------------------------------------------------------------------------------------------------------------------|-------------------|-----------------------------------------|
| `storage`                | ID of a storage extension that persists the state, for example `file_storage/state`. If not set, the state is in memory only.      | not set           |                                         |
| `emit_interval`          | Time between two emissions of the current Info metrics. Rules can override it.                                                      | `60s`             | `1s` to `24h`                           |
| `emit_on_change`         | If `true`, a change of facts causes an immediate emission of the Info metric.                                                       | `true`            |                                         |
| `max_state_bytes`        | Maximum size of the serialized state.                                                                                               | `1048576` (1 MiB) | `65536` (64 KiB) to `67108864` (64 MiB) |
| `max_value_length`       | Maximum length of an extracted attribute value.                                                                                     | `256`             | `1` to `1024` (CloudWatch limit)        |
| `error_mode`             | Action on OTTL evaluation errors at runtime: `propagate`, `ignore`, or `silent`. OTTL parse errors always stop the startup.         | `ignore`          | One of the three values.                |
| `identity_output_prefix` | If set, the connector also emits each identity value as the resource attribute `<prefix><identity key>`.                           | `""`              |                                         |
| `rules`                  | The rules. Refer to the next table.                                                                                                 | none              | Minimum one rule.                       |

### Rule settings

| Name                          | Description                                                                                                                                                                       | Default   | Bounds                                                 |
|-------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------|--------------------------------------------------------|
| `name`                        | Metric name. It is also the key of the state of the rule.                                                                                                                         | none      | Required. Maximum 255 bytes. Unique across the rules.  |
| `description`                 | Metric description.                                                                                                                                                               | `""`      |                                                        |
| `unit`                        | Metric unit.                                                                                                                                                                      | `""`      |                                                        |
| `body_contains`               | Substrings to find in the body. This is a plain substring check on a string body, with no OTTL and no regex. It runs before the conditions.                                       | `[]`      | Entries must not be empty.                             |
| `conditions`                  | OTTL conditions on the log context, joined with OR.                                                                                                                               | `[]`      | Each condition must parse.                             |
| `identity`                    | Resource attribute keys that identify an entity, for example `k8s.pod.uid`. The connector keeps the latest facts for each entity.                                                | none      | Minimum one key. No empty or duplicate keys.           |
| `include_resource_attributes` | Resource attribute keys that the connector copies to the emitted metric. If `identity_output_prefix` is empty, this list must contain all identity keys. Otherwise, two entities can produce the same series. | `[]`      | No empty or duplicate keys.                            |
| `attributes`                  | The facts to extract. Refer to the next table.                                                                                                                                    | none      | Minimum one. Maximum `max_attributes`.                 |
| `emit_interval`               | Overrides the connector `emit_interval` for this rule. `0` uses the connector value.                                                                                             | `0`       | If set: `1s` to `24h`.                                 |
| `max_entities`                | Maximum number of entities that this rule tracks. `0` uses the default.                                                                                                          | `0` (500) | If set: `1` to `100000`.                               |
| `max_attributes`              | Maximum number of attributes in this rule. `0` uses the default.                                                                                                                 | `0` (32)  | If set: `1` to `128`.                                  |

A rule must set `body_contains`, `conditions`, or both. For a record to match a rule, both of these checks must pass:

- If `body_contains` is set, the body contains at least one of the substrings.
- If `conditions` is set, at least one condition is true.

### Attribute settings

| Name            | Description                                                                                                                                      | Default | Bounds                                                  |
|-----------------|--------------------------------------------------------------------------------------------------------------------------------------------------|---------|---------------------------------------------------------|
| `key`           | Data point attribute key.                                                                                                                        | none    | Required. Unique in the rule.                           |
| `value`         | OTTL value expression for the matching record. If empty, the value is `log.attributes["<key>"]`.                                                 | `""`    | Must parse.                                             |
| `default_value` | If the value expression gives an error, `nil`, or an empty string, the connector uses this value. If empty, `optional` controls the result.     | `""`    | Maximum `max_value_length` bytes.                       |
| `optional`      | If `true`, a matching record can have no value for this fact. The emitted metric then does not have this attribute. If `false`, the connector does not use a record that has no value for this fact. | `false` | Not permitted together with `default_value`.            |

The connector gets each fact in this order: first the `value` expression, then `default_value`. If the two give
no value, `optional` controls the result.

### Label budget

Each data point has `len(attributes) + len(include_resource_attributes)` labels. If `identity_output_prefix` is
set, add `len(identity)`. The total must be 150 or less. This is the CloudWatch limit for each data point.

### OTTL

Conditions and values use the OTTL log context with the [standard converters][OTTL functions]. Editors are not
available. You can write paths with or without the context name. For example, `body` and `log.body` are equal.
Also, `attributes["k"]` and `log.attributes["k"]` are equal. Resource attributes are `resource.attributes["k"]`.

An index into a converter result with a missing key gives an evaluation error, not `nil`. For example,
`ExtractPatterns(log.body, "x=(?P<v>\\d+)")["v"]` fails on a record that the pattern does not match. Thus,
`Coalesce` cannot supply a fallback value for it. Use `default_value` for a fallback value.

## Example

```yaml
extensions:
  file_storage/state:
    directory: /var/lib/amazon-cloudwatch-agent/state

connectors:
  signal_to_info_metric/vllm:
    storage: file_storage/state
    emit_interval: 30s
    identity_output_prefix: cwagent.info.identity.
    rules:
      - name: vllm_engine_config_info
        description: vLLM engine configuration from the engine startup log line.
        # vllm_version still needs ") with config", so a line with only the phrase gives no series.
        # A prefix check does not work, because vLLM writes "(EngineCore pid=N) INFO ..." or "INFO ..." first.
        body_contains:
          - "Initializing an LLM engine (v"
          - "Initializing a V0 LLM engine (v"
          - "Initializing a V1 LLM engine (v"
        identity: [k8s.pod.uid]
        include_resource_attributes: [k8s.pod.name, k8s.namespace.name, k8s.container.name]
        attributes:
          - key: vllm_version
            value: 'ExtractPatterns(log.body, "\\(v(?P<v>[^)]+)\\) with config")["v"]'
          # A missing key in ExtractPatterns is an evaluation error, so the default applies to "Initializing an LLM engine" (v0.6.x) lines.
          - key: vllm_engine
            value: 'ExtractPatterns(log.body, "Initializing a (?P<v>V[01]) LLM engine")["v"]'
            default_value: V0
          - key: tensor_parallel_size
            value: 'ExtractPatterns(log.body, ", tensor_parallel_size=(?P<v>\\d+),")["v"]'
            optional: true

service:
  extensions: [file_storage/state]
  pipelines:
    logs:
      receivers: [filelog]
      exporters: [awscloudwatchlogs, signal_to_info_metric/vllm]
    metrics/info:
      receivers: [signal_to_info_metric/vllm]
      exporters: [otlphttp]
```

## Behavior

### Matching and facts

For each record and rule, the connector first does the `body_contains` check. A record without a string body
does not match a rule that sets `body_contains`. Most records fail this check, so it does not allocate memory.
Then the connector evaluates the `conditions`. Then it reads the identity from the resource attributes, one time
for each resource and rule. If an identity key is missing or its value is empty, the rule does not use records of
that resource. Then the connector gets the facts in the configured order.

Values that are not strings are converted as by `pcommon.Value.AsString`. For example, the integer 4 becomes `"4"`.
Each value, also each resource attribute and identity value, is cut to `max_value_length` bytes. The cut is at a
UTF-8 character boundary. A value that is empty after the cut counts as missing.

With `error_mode: propagate`, the connector stops the batch at an evaluation error. This applies to the
conditions, and to a required attribute without `default_value`. The connector returns the error. It still
applies the records before that record. With `ignore` or `silent`, the connector does not use the record. The
connector does not change the logs that it consumes.

### Entity state

The connector keeps the latest facts for each rule and identity. The record time is the record timestamp, else
the observed timestamp, else the current time. For a known entity:

- If the record is older than the stored facts, the connector does not use it. After a container restart,
  kubelet keeps both log files, and the receiver can read them in any order.
- If the facts and the included resource attributes are the same, the entity stays the same series.
- If they are different, the connector replaces them and starts a new series. The start time of the series is
  the record time.

A rule tracks at most `max_entities` entities. A new entity causes the eviction of the entity of that rule that
has the oldest activity. `max_state_bytes` limits an estimate of the size of the serialized state, for all rules
together. If the state is too large, the connector evicts the entities with the oldest activity, from any rule.
The connector does not keep an entity that alone is larger than `max_state_bytes`.

### Emission

Each entity is one series: a gauge with the value `1` and the rule name as metric name. The resource has the
included resource attributes. If `identity_output_prefix` is set, the resource also has the identity. The data
point attributes are the facts. The data point time is the emission time. The start time is the start of the
series. The record time is never the data point time, because CloudWatch rejects data points that are older than
14 days.

If `emit_on_change` is `true`, a new entity or a change of facts causes an immediate emission. If the next
consumer returns an error, the connector logs it, at most one time each minute. The error does not go back to
the logs pipeline.

TODO: Describe the periodic emission and the persistence.

[OTTL]: https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/pkg/ottl/README.md
[OTTL functions]: https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/pkg/ottl/ottlfuncs/README.md#converters
