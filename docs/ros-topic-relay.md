# ROS Topic Relay

Relay can carry ROS2 pub/sub traffic through the existing session-scoped
`CustomMessage` path. This keeps the relay transport unchanged while allowing
robots and operators on different networks to exchange opaque ROS2 topic
payloads after they join the same Relay session.

## Transport

Clients publish ROS topic messages as `hagallpb.CustomMessage` frames:

- `Body` contains an encoded ROS topic envelope.
- `ParticipantIds` is optional. When set, Relay forwards only to those
  participant ids. When empty, Relay fans out to every other participant in the
  same session.
- Relay stamps the broadcast with the sender participant id in
  `CustomMessageBroadcast.ParticipantId`.

Relay only recognizes the JSON envelope fields needed to distinguish ROS topic
traffic from ordinary `CustomMessage` traffic when the ROS feature flag is
disabled. It does not parse, validate, or transform ROS payload bytes.

## Recommended Envelope

The recommended application-level envelope fields are:

```json
{
  "topic": "/cmd_vel",
  "ros_message_type": "geometry_msgs/msg/Twist",
  "payload_encoding": "cdr",
  "payload": "<base64 serialized ROS2 message>",
  "target_participant_ids": [42]
}
```

Relay derives `origin_participant_id` from the authenticated session
participant and adds it to the broadcast metadata. Clients should prefer the
broadcast metadata over trusting a client-supplied origin field.

## Session Model

ROS topic messages are isolated by Relay session:

1. The operator and robot both send `ParticipantJoinRequest`.
2. The publisher sends `CustomMessage` with the ROS envelope in `Body`.
3. Relay broadcasts inside that same session only.
4. Participants in other sessions never receive the broadcast.

## Size Limit

The default custom message body limit is 256 KiB. This is large enough for small
control and telemetry topics while still bounding relay memory and bandwidth
exposure. Oversized messages are rejected with `ERROR_CODE_TOO_LARGE`.

Operators can override the limit with:

```bash
HAGALL_CUSTOM_MESSAGE_MAX_SIZE=262144
```

Large image, point-cloud, bag, service, or action payloads should not be sent
through this path. Use a storage or streaming transport and send references
through Relay instead.

## Feature Flag

Set `DISABLE_ROS_TOPIC_RELAY` in `HAGALL_FEATURE_FLAGS` to disable this path:

```bash
HAGALL_FEATURE_FLAGS=DISABLE_ROS_TOPIC_RELAY
```

Only messages that match the JSON ROS topic envelope are blocked by this flag.
Ordinary `CustomMessage` traffic keeps using the existing
`DISABLE_CUSTOM_MESSAGE_BROADCAST` flag.

## Operator Walkthrough

1. Start Relay with the default 256 KiB limit, or set
   `HAGALL_CUSTOM_MESSAGE_MAX_SIZE` explicitly.
2. Have the operator client and robot client join the same session.
3. Encode each ROS topic publish into the recommended envelope.
4. Send it as a `CustomMessage`.
5. On `CustomMessageBroadcast`, decode `Body`, read
   `CustomMessageBroadcast.ParticipantId` as the origin participant, and publish
   the decoded payload onto the local ROS2 topic.
