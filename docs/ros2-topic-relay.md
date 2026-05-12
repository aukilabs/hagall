# ROS2 Topic Relay Module

## Overview

The ROS2 Topic Relay module extends the Relay server to transport ROS2 topic messages between participants in the same session. This enables teleoperation and remote monitoring scenarios where robots and operators are on different networks.

## Use Cases

### 1. Teleoperation

```
┌──────────────────┐                        ┌──────────────────┐
│ Remote Operator  │                        │ Robot            │
│ (Web Interface)  │                        │ (ROS2 Node)      │
│                  │                        │                  │
│ Publish:         │                        │ Subscribe:       │
│ /cmd_vel         │────── Relay ──────────▶│ /cmd_vel         │
│ /arm_command     │      Server            │ /arm_command     │
│                  │                        │                  │
│ Subscribe:       │                        │ Publish:         │
│ /camera          │◀────── Relay ──────────│ /camera          │
│ /lidar           │      Server            │ /lidar           │
│ /status          │                        │ /status          │
└──────────────────┘                        └──────────────────┘
```

### 2. Multi-Robot Coordination

Multiple robots can share sensor data through the relay without direct connections.

### 3. Remote Monitoring

Operators can subscribe to robot telemetry without being on the same network.

## Message Protocol

### Subscribe to Topic

**Request:**
```json
{
  "type": "ROS2_TOPIC_SUBSCRIBE",
  "requestId": "uuid-123",
  "topic": "/cmd_vel",
  "messageType": "geometry_msgs/Twist"
}
```

**Response:**
```json
{
  "type": "ROS2_TOPIC_SUBSCRIBE_RESPONSE",
  "requestId": "uuid-123",
  "success": true,
  "topic": "/cmd_vel"
}
```

### Publish to Topic

**Request:**
```json
{
  "type": "ROS2_TOPIC_PUBLISH",
  "requestId": "uuid-456",
  "topic": "/cmd_vel",
  "messageType": "geometry_msgs/Twist",
  "data": "CgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",
  "timestamp": "2026-05-12T10:30:00Z"
}
```

**Broadcast to Subscribers:**
```json
{
  "type": "ROS2_TOPIC_MESSAGE",
  "topic": "/cmd_vel",
  "messageType": "geometry_msgs/Twist",
  "data": "CgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",
  "publisherId": "participant-abc",
  "timestamp": "2026-05-12T10:30:00Z"
}
```

### Unsubscribe from Topic

**Request:**
```json
{
  "type": "ROS2_TOPIC_UNSUBSCRIBE",
  "requestId": "uuid-789",
  "topic": "/cmd_vel"
}
```

## Integration with ROS2

### Python Example (Robot Side)

```python
import asyncio
import websockets
import json
import base64
from geometry_msgs.msg import Twist

class ROS2RelayClient:
    def __init__(self, relay_url, session_id, auth_token):
        self.relay_url = relay_url
        self.session_id = session_id
        self.auth_token = auth_token
        self.ws = None
    
    async def connect(self):
        headers = {
            "Authorization": f"Bearer {self.auth_token}",
            "X-Posemesh-Session-Id": self.session_id
        }
        self.ws = await websockets.connect(self.relay_url, extra_headers=headers)
        
        # Subscribe to cmd_vel
        await self.subscribe("/cmd_vel", "geometry_msgs/Twist")
    
    async def subscribe(self, topic, msg_type):
        msg = {
            "type": "ROS2_TOPIC_SUBSCRIBE",
            "topic": topic,
            "messageType": msg_type
        }
        await self.ws.send(json.dumps(msg))
    
    async def publish(self, topic, msg):
        # Serialize ROS2 message to bytes
        serialized = msg.serialize()
        encoded = base64.b64encode(serialized).decode()
        
        msg = {
            "type": "ROS2_TOPIC_PUBLISH",
            "topic": topic,
            "messageType": type(msg).__name__,
            "data": encoded
        }
        await self.ws.send(json.dumps(msg))
    
    async def listen(self):
        async for message in self.ws:
            data = json.loads(message)
            if data.get("type") == "ROS2_TOPIC_MESSAGE":
                # Deserialize and handle
                topic = data["topic"]
                msg_data = base64.b64decode(data["data"])
                await self.handle_message(topic, msg_data)
    
    async def handle_message(self, topic, data):
        print(f"Received on {topic}: {data}")

# Usage
async def main():
    client = ROS2RelayClient(
        relay_url="wss://relay.auki.network",
        session_id="5fx3a",
        auth_token="your-token"
    )
    await client.connect()
    asyncio.create_task(client.listen())
    
    # Publish velocity command
    twist = Twist(linear={'x': 1.0, 'y': 0.0, 'z': 0.0})
    await client.publish("/cmd_vel", twist)
    
    # Keep running
    await asyncio.sleep(3600)

asyncio.run(main())
```

### C++ Example (Robot Side)

```cpp
#include <websocketpp/config/asio_no_tls_client.hpp>
#include <websocketpp/client.hpp>
#include <nlohmann/json.hpp>
#include <base64.hpp>

typedef websocketpp::client<websocketpp::config::asio_client> client;

class ROS2RelayClient {
public:
    void connect(const std::string& url, const std::string& session_id, 
                 const std::string& auth_token) {
        client c;
        
        c.set_open_handler([this, session_id](websocketpp::connection_hdl hdl) {
            this->subscribe(hdl, "/cmd_vel", "geometry_msgs/Twist");
        });
        
        c.set_message_handler([this](websocketpp::connection_hdl hdl, 
                                      client::message_ptr msg) {
            this->onMessage(msg->get_payload());
        });
        
        websocketpp::lib::error_code ec;
        auto con = c.get_connection(url, ec);
        
        con->append_header("Authorization", "Bearer " + auth_token);
        con->append_header("X-Posemesh-Session-Id", session_id);
        
        c.connect(con);
        c.run();
    }
    
    void subscribe(websocketpp::connection_hdl hdl, const std::string& topic,
                   const std::string& msg_type) {
        nlohmann::json msg = {
            {"type", "ROS2_TOPIC_SUBSCRIBE"},
            {"topic", topic},
            {"messageType", msg_type}
        };
        // Send via WebSocket
    }
    
    void publish(websocketpp::connection_hdl hdl, const std::string& topic,
                 const std::string& msg_type, const std::vector<uint8_t>& data) {
        std::string encoded = base64_encode(data);
        
        nlohmann::json msg = {
            {"type", "ROS2_TOPIC_PUBLISH"},
            {"topic", topic},
            {"messageType", msg_type},
            {"data", encoded}
        };
        // Send via WebSocket
    }
    
private:
    void onMessage(const std::string& payload) {
        auto msg = nlohmann::json::parse(payload);
        if (msg["type"] == "ROS2_TOPIC_MESSAGE") {
            std::string topic = msg["topic"];
            std::string data = msg["data"];
            // Process message
        }
    }
};
```

## Configuration

No additional server configuration is required. The module is automatically loaded when the Relay server starts.

### Optional Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ROS2_MAX_MESSAGE_SIZE` | `10240` | Maximum message size in bytes |
| `ROS2_MAX_TOPICS_PER_CLIENT` | `50` | Maximum topic subscriptions per client |
| `ROS2_ENABLE_LOGGING` | `false` | Enable debug logging |

## Security

- All messages are authenticated via Relay's existing authentication system
- Topic names are validated to prevent path traversal attacks
- Message size limits prevent DoS attacks
- Participants can only communicate within their session

## Performance

- **Latency**: < 10ms for message relay (same region)
- **Throughput**: Up to 1000 messages/second per session
- **Scalability**: Supports 100+ concurrent participants per session

## Troubleshooting

### Issue: Messages not being received

**Check:**
1. Subscriber is in the same session as publisher
2. Topic names match exactly (case-sensitive)
3. Message type is correctly specified

### Issue: Connection drops frequently

**Check:**
1. Network stability
2. Session timeout settings
3. Authentication token validity

## Limitations

- Best-effort delivery (no QoS guarantees like native ROS2)
- In-memory subscription storage (lost on server restart)
- No topic discovery (clients must know topic names in advance)
- No message persistence

## Future Enhancements

- [ ] ROS2 QoS policy support (reliability, durability, etc.)
- [ ] Topic discovery service
- [ ] Message persistence and replay
- [ ] Bridge to native ROS2 DDS/RTPS network
- [ ] Topic-level access control
- [ ] Message compression

## Support

For issues or questions, open a GitHub issue at [aukilabs/hagall](https://github.com/aukilabs/hagall/issues).
