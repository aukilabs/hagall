# ROS2 Topic Relay

Relay can carry ROS2 topic messages between session participants, enabling teleoperation and telemetry without direct peer-to-peer connectivity. A remote operator and a robot both join the same Relay session over WebSocket, and ROS2 topic traffic flows between them through the relay.

## How it works

The ROS2 topic relay is implemented as a Relay module (`rosrelay`). Like all Relay modules, it hooks into the existing WebSocket message pipeline and uses the same session, authentication, and fan-out infrastructure as other message types.

The relay treats ROS2 payloads as **opaque bytes**. It does not parse, validate, or transform the payload content. Serialization format (CDR, JSON, etc.) is the client's concern.

## Wire format

Three protobuf message types are defined in `messages/rosrelaypb/`:

### RosTopicPublishRequest (type 400)

Sent by a client to publish a ROS2 topic message.

| Field | Type | Description |
|-------|------|-------------|
| `type` | MsgType | `MSG_TYPE_ROS_TOPIC_PUBLISH_REQUEST` (400) |
| `timestamp` | Timestamp | Client-side send time |
| `request_id` | uint32 | Correlates request to response |
| `topic` | string | ROS2 topic name (e.g. `/cmd_vel`) |
| `ros_message_type` | string | ROS2 message type (e.g. `geometry_msgs/msg/Twist`) |
| `payload` | bytes | Opaque serialized ROS2 message |
| `participant_ids` | repeated uint32 | Optional targeted delivery; empty = fan-out to all |

### RosTopicPublishResponse (type 401)

Acknowledgement returned to the sender.

| Field | Type | Description |
|-------|------|-------------|
| `type` | MsgType | `MSG_TYPE_ROS_TOPIC_PUBLISH_RESPONSE` (401) |
| `timestamp` | Timestamp | Server-side time |
| `request_id` | uint32 | Matches the request |

### RosTopicBroadcast (type 402)

Delivered to other session participants.

| Field | Type | Description |
|-------|------|-------------|
| `type` | MsgType | `MSG_TYPE_ROS_TOPIC_BROADCAST` (402) |
| `timestamp` | Timestamp | Server-side time |
| `origin_timestamp` | Timestamp | Original publish time from the sender |
| `topic` | string | ROS2 topic name |
| `ros_message_type` | string | ROS2 message type |
| `payload` | bytes | Opaque serialized ROS2 message |
| `origin_participant_id` | uint32 | Participant ID of the sender |

## Session model

- Clients join a session using the standard `ParticipantJoinRequest` flow.
- ROS topic messages are scoped to the session. Messages published in session A are never delivered to participants in session B.
- Targeted delivery: set `participant_ids` to forward only to specific participants (e.g. operator → one specific robot). Leave empty to fan out to all other participants.

## Size limit

Maximum payload size: **256 KiB** (262,144 bytes).

This is large enough for most control and sensor topics (Twist, LaserScan, CompressedImage thumbnails) while protecting the relay from unbounded memory use. Requests exceeding this limit are rejected with `ERROR_CODE_TOO_LARGE`.

Raw camera frames and full point clouds should be streamed out-of-band (e.g. via WebRTC data channels).

## Feature flag

The ROS topic relay can be fully disabled without affecting any other message types:

```
DISABLE_ROS_TOPIC_RELAY
```

When set, publish requests still receive a `RosTopicPublishResponse` acknowledgement, but no `RosTopicBroadcast` is generated.

## Data flow

```
   Remote operator                 Relay node                    Robot
   (WebSocket client)              (public WSS)              (WebSocket client)
        |                               |                            |
        |-- ParticipantJoin ----------->|                            |
        |<-- JoinResponse --------------|                            |
        |                               |<---- ParticipantJoin ------|
        |                               |----- JoinResponse -------->|
        |                               |                            |
        |-- RosTopicPublish(/cmd_vel) ->|                            |
        |<-- RosTopicPublishResponse ---|                            |
        |                               |-- RosTopicBroadcast ------>|
        |                               |                            |
        |                               |<-- RosTopicPublish(/odom) -|
        |                               |--- RosTopicPublishResponse>|
        |<-- RosTopicBroadcast ---------|                            |
```

## Operator walkthrough

1. **Start a Relay node** (or use an existing one). Ensure the `DISABLE_ROS_TOPIC_RELAY` flag is NOT set.

2. **Connect both endpoints** (operator and robot) to the same Relay node over WebSocket using the standard Relay client connection flow.

3. **Join the same session.** The first client creates a session (empty `session_id` in join request); the second joins with the returned `session_id`.

4. **Publish ROS2 topics.** Either side sends `RosTopicPublishRequest` with the topic name, message type, and serialized payload. The relay fans out the message as `RosTopicBroadcast` to all other session participants (or to targeted participants if `participant_ids` is set).

5. **Bridge to ROS2 locally.** Each endpoint runs a local bridge (not part of Relay) that subscribes to ROS2 topics, serializes messages, and sends them as `RosTopicPublishRequest` — and conversely, receives `RosTopicBroadcast` messages and publishes them to local ROS2 topics.

## Scope and limitations

- **ROS2 pub/sub only.** Services and actions are not supported.
- **No schema awareness.** The relay does not parse or validate payloads.
- **No ROS1 support.**
- **No companion bridge clients.** The WebSocket-to-ROS2 bridge on each endpoint is the client's responsibility.
