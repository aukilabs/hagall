package ros2

import (
	"context"
	"encoding/base64"
	"sync"
	"time"

	"github.com/aukilabs/go-tooling/pkg/logs"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/models"
	"github.com/aukilabs/hagall/modules"
)

const (
	// ModuleName is the name of the ROS2 Topic Relay module
	ModuleName = "ros2-topic-relay"
	
	// Message types
	MsgTypeSubscribe      = "ROS2_TOPIC_SUBSCRIBE"
	MsgTypeUnsubscribe    = "ROS2_TOPIC_UNSUBSCRIBE"
	MsgTypePublish        = "ROS2_TOPIC_PUBLISH"
	MsgTypeMessage        = "ROS2_TOPIC_MESSAGE"
	MsgTypeSubscribeResp  = "ROS2_TOPIC_SUBSCRIBE_RESPONSE"
	MsgTypeUnsubscribeResp = "ROS2_TOPIC_UNSUBSCRIBE_RESPONSE"
	
	// Limits
	MaxMessageSize = 10240
	MaxTopicsPerClient = 50
)

// TopicSubscription represents a subscription to a ROS2 topic
type TopicSubscription struct {
	TopicName     string
	ParticipantID string
	MessageType   string
	CreatedAt     time.Time
}

// ROS2Message represents a ROS2 topic message
type ROS2Message struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
	Topic     string `json:"topic"`
	MessageType string `json:"messageType"`
	Data      string `json:"data,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	PublisherID string `json:"publisherId,omitempty"`
	Success   bool   `json:"success,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Module implements the ROS2 Topic Relay functionality
type Module struct {
	name          string
	session       *models.Session
	participant   *models.Participant
	subscriptions map[string][]*TopicSubscription // topic -> subscriptions
	mu            sync.RWMutex
}

// NewModule creates a new ROS2 Topic Relay module
func NewModule() modules.Module {
	return &Module{
		name:          ModuleName,
		subscriptions: make(map[string][]*TopicSubscription),
	}
}

// Name returns the module name
func (m *Module) Name() string {
	return m.name
}

// Init initializes the module with session and participant info
func (m *Module) Init(session *models.Session, participant *models.Participant) {
	m.session = session
	m.participant = participant
	logs.Infof("[ROS2] Module initialized for participant %s in session %s", participant.ID, session.ID)
}

// HandleMsg handles incoming WebSocket messages
func (m *Module) HandleMsg(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	// Try to parse as ROS2 message
	var ros2Msg ROS2Message
	if err := msg.DataTo(&ros2Msg); err != nil {
		// Not a ROS2 message, skip
		return modules.ErrModuleMsgSkip
	}
	
	// Route based on message type
	switch ros2Msg.Type {
	case MsgTypeSubscribe:
		return m.handleSubscribe(ctx, respond, msg, ros2Msg)
	case MsgTypeUnsubscribe:
		return m.handleUnsubscribe(ctx, respond, msg, ros2Msg)
	case MsgTypePublish:
		return m.handlePublish(ctx, respond, msg, ros2Msg)
	default:
		return modules.ErrModuleMsgSkip
	}
}

// handleSubscribe processes topic subscription requests
func (m *Module) handleSubscribe(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg, req ROS2Message) error {
	// Validate topic name
	if req.Topic == "" {
		respond.Send(&ROS2Message{
			Type:    MsgTypeSubscribeResp,
			Success: false,
			Error:   "topic name required",
		})
		return nil
	}
	
	// Check subscription limit
	m.mu.RLock()
	count := 0
	for _, subs := range m.subscriptions {
		for _, sub := range subs {
			if sub.ParticipantID == m.participant.ID {
				count++
			}
		}
	}
	m.mu.RUnlock()
	
	if count >= MaxTopicsPerClient {
		respond.Send(&ROS2Message{
			Type:    MsgTypeSubscribeResp,
			Success: false,
			Error:   "max topic limit reached",
		})
		return nil
	}
	
	// Add subscription
	m.Subscribe(req.Topic, req.MessageType)
	
	// Send success response
	respond.Send(&ROS2Message{
		Type:      MsgTypeSubscribeResp,
		RequestID: req.RequestID,
		Success:   true,
		Topic:     req.Topic,
	})
	
	logs.Infof("[ROS2] Participant %s subscribed to topic %s", m.participant.ID, req.Topic)
	return nil
}

// handleUnsubscribe processes topic unsubscription requests
func (m *Module) handleUnsubscribe(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg, req ROS2Message) error {
	if req.Topic == "" {
		respond.Send(&ROS2Message{
			Type:    MsgTypeUnsubscribeResp,
			Success: false,
			Error:   "topic name required",
		})
		return nil
	}
	
	m.Unsubscribe(req.Topic)
	
	respond.Send(&ROS2Message{
		Type:      MsgTypeUnsubscribeResp,
		RequestID: req.RequestID,
		Success:   true,
		Topic:     req.Topic,
	})
	
	logs.Infof("[ROS2] Participant %s unsubscribed from topic %s", m.participant.ID, req.Topic)
	return nil
}

// handlePublish processes topic publish requests and broadcasts to subscribers
func (m *Module) handlePublish(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg, req ROS2Message) error {
	if req.Topic == "" {
		return nil
	}
	
	// Validate message size
	if len(req.Data) > MaxMessageSize {
		logs.Warnf("[ROS2] Message too large for topic %s: %d bytes", req.Topic, len(req.Data))
		return nil
	}
	
	// Validate base64 data
	if _, err := base64.StdEncoding.DecodeString(req.Data); err != nil {
		logs.Warnf("[ROS2] Invalid base64 data for topic %s", req.Topic)
		return nil
	}
	
	// Broadcast to all subscribers
	m.Broadcast(req.Topic, req.Data, req.MessageType)
	
	logs.Debugf("[ROS2] Published to topic %s: %d bytes", req.Topic, len(req.Data))
	return nil
}

// Broadcast sends a message to all subscribers of a topic
func (m *Module) Broadcast(topicName, data, messageType string) {
	m.mu.RLock()
	subs := m.subscriptions[topicName]
	m.mu.RUnlock()
	
	if len(subs) == 0 {
		return
	}
	
	// Create broadcast message
	broadcastMsg := ROS2Message{
		Type:        MsgTypeMessage,
		Topic:       topicName,
		MessageType: messageType,
		Data:        data,
		PublisherID: m.participant.ID,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	
	// In a real implementation, you'd send this to the WebSocket dispatcher
	// For now, log the broadcast
	for _, sub := range subs {
		if sub.ParticipantID != m.participant.ID {
			logs.Infof("[ROS2] Broadcasting to %s on topic %s", sub.ParticipantID, topicName)
		}
	}
}

// HandleDisconnect handles client disconnection
func (m *Module) HandleDisconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	// Clean up all subscriptions for this participant
	for topic, subs := range m.subscriptions {
		var cleaned []*TopicSubscription
		for _, sub := range subs {
			if sub.ParticipantID != m.participant.ID {
				cleaned = append(cleaned, sub)
			}
		}
		if len(cleaned) == 0 {
			delete(m.subscriptions, topic)
		} else {
			m.subscriptions[topic] = cleaned
		}
	}
	
	logs.Infof("[ROS2] Participant %s disconnected, cleaned up subscriptions", m.participant.ID)
}

// Subscribe adds a subscription to a topic
func (m *Module) Subscribe(topicName, messageType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	sub := &TopicSubscription{
		TopicName:     topicName,
		ParticipantID: m.participant.ID,
		MessageType:   messageType,
		CreatedAt:     time.Now(),
	}
	
	m.subscriptions[topicName] = append(m.subscriptions[topicName], sub)
	logs.Infof("[ROS2] Participant %s subscribed to topic %s (%s)", m.participant.ID, topicName, messageType)
}

// Unsubscribe removes a subscription from a topic
func (m *Module) Unsubscribe(topicName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	var subs []*TopicSubscription
	for _, sub := range m.subscriptions[topicName] {
		if sub.ParticipantID != m.participant.ID {
			subs = append(subs, sub)
		}
	}
	
	if len(subs) == 0 {
		delete(m.subscriptions, topicName)
	} else {
		m.subscriptions[topicName] = subs
	}
}

// GetSubscriptions returns all subscriptions for a topic
func (m *Module) GetSubscriptions(topicName string) []*TopicSubscription {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	subs := m.subscriptions[topicName]
	result := make([]*TopicSubscription, len(subs))
	copy(result, subs)
	return result
}

// GetAllTopics returns all topics with active subscriptions
func (m *Module) GetAllTopics() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	topics := make([]string, 0, len(m.subscriptions))
	for topic := range m.subscriptions {
		if len(m.subscriptions[topic]) > 0 {
			topics = append(topics, topic)
		}
	}
	return topics
}

// GetSubscriptionCount returns the total number of subscriptions for this participant
func (m *Module) GetSubscriptionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	count := 0
	for _, subs := range m.subscriptions {
		for _, sub := range subs {
			if sub.ParticipantID == m.participant.ID {
				count++
			}
		}
	}
	return count
}
