# ROS2 Topic Relay Module

## Overview

This module extends the Relay server to carry ROS2 topic traffic between participants of the same session, enabling remote operators to publish to ROS2 topics and robots on different networks to subscribe without direct peer-to-peer connectivity.

## Architecture

```
┌─────────────────┐         WebSocket          ┌─────────────────┐
│  Remote         │      ┌───────────────┐     │  Robot          │
│  Operator       │──────│  Relay Server │─────│  (ROS2 Node)    │
│  (Web/Mobile)   │      │  + ROS2 Mod   │     │                 │
└─────────────────┘      └───────────────┘     └─────────────────┘
```

## Message Types

The module handles the following message types:

| Type | Direction | Description |
|------|-----------|-------------|
| `ROS2_TOPIC_SUBSCRIBE` | Client → Server | Subscribe to a ROS2 topic |
| `ROS2_TOPIC_UNSUBSCRIBE` | Client → Server | Unsubscribe from a topic |
| `ROS2_TOPIC_PUBLISH` | Client → Server | Publish a message to a topic |
| `ROS2_TOPIC_MESSAGE` | Server → Client | Broadcast message to subscribers |

## Usage

### Subscribing to a Topic

```json
{
  "type": "ROS2_TOPIC_SUBSCRIBE",
  "topic": "/cmd_vel",
  "messageType": "geometry_msgs/Twist"
}
```

### Publishing to a Topic

```json
{
  "type": "ROS2_TOPIC_PUBLISH",
  "topic": "/cmd_vel",
  "data": "base64-encoded-message",
  "messageType": "geometry_msgs/Twist"
}
```

### Receiving Messages

```json
{
  "type": "ROS2_TOPIC_MESSAGE",
  "topic": "/cmd_vel",
  "data": "base64-encoded-message",
  "messageType": "geometry_msgs/Twist",
  "publisherId": "participant-123"
}
```

## Configuration

No additional configuration required. The module is automatically loaded when the Relay server starts.

## Example: Teleoperation Setup

1. **Robot** subscribes to `/cmd_vel`:
   ```json
   {"type": "ROS2_TOPIC_SUBSCRIBE", "topic": "/cmd_vel", "messageType": "geometry_msgs/Twist"}
   ```

2. **Operator** publishes to `/cmd_vel`:
   ```json
   {"type": "ROS2_TOPIC_PUBLISH", "topic": "/cmd_vel", "data": "Cg..."}
   ```

3. **Relay** broadcasts to all subscribers

4. **Robot** receives the command and executes

## Security Considerations

- All messages are authenticated via the existing Relay authentication system
- Topic names should be validated to prevent injection attacks
- Message size limits apply (default: 10KB per message)

## Limitations

- Best-effort delivery (no QoS guarantees)
- In-memory subscription storage (lost on server restart)
- No topic discovery (clients must know topic names)

## Future Enhancements

- [ ] ROS2 QoS policy support
- [ ] Topic discovery service
- [ ] Message persistence
- [ ] Bridge to native ROS2 network
