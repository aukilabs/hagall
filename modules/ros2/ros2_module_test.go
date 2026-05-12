package ros2

import (
	"fmt"
	"testing"

	"github.com/aukilabs/hagall/models"
	"github.com/stretchr/testify/assert"
)

func TestNewModule(t *testing.T) {
	mod := NewModule()
	assert.NotNil(t, mod)
	assert.Equal(t, ModuleName, mod.Name())
}

func TestModule_Init(t *testing.T) {
	mod := NewModule().(*Module)
	
	session := &models.Session{ID: "test-session"}
	participant := &models.Participant{ID: "test-participant"}
	
	mod.Init(session, participant)
	
	assert.Equal(t, session, mod.session)
	assert.Equal(t, participant, mod.participant)
}

func TestModule_Subscribe(t *testing.T) {
	mod := NewModule().(*Module)
	session := &models.Session{ID: "test-session"}
	participant := &models.Participant{ID: "test-participant"}
	mod.Init(session, participant)
	
	mod.Subscribe("/cmd_vel", "geometry_msgs/Twist")
	
	subs := mod.GetSubscriptions("/cmd_vel")
	assert.Len(t, subs, 1)
	assert.Equal(t, "/cmd_vel", subs[0].TopicName)
	assert.Equal(t, "geometry_msgs/Twist", subs[0].MessageType)
	assert.Equal(t, "test-participant", subs[0].ParticipantID)
}

func TestModule_Unsubscribe(t *testing.T) {
	mod := NewModule().(*Module)
	session := &models.Session{ID: "test-session"}
	participant := &models.Participant{ID: "test-participant"}
	mod.Init(session, participant)
	
	mod.Subscribe("/cmd_vel", "geometry_msgs/Twist")
	mod.Unsubscribe("/cmd_vel")
	
	subs := mod.GetSubscriptions("/cmd_vel")
	assert.Len(t, subs, 0)
}

func TestModule_GetAllTopics(t *testing.T) {
	mod := NewModule().(*Module)
	session := &models.Session{ID: "test-session"}
	participant := &models.Participant{ID: "test-participant"}
	mod.Init(session, participant)
	
	mod.Subscribe("/cmd_vel", "geometry_msgs/Twist")
	mod.Subscribe("/odom", "nav_msgs/Odometry")
	mod.Subscribe("/scan", "sensor_msgs/LaserScan")
	
	topics := mod.GetAllTopics()
	assert.Len(t, topics, 3)
	assert.Contains(t, topics, "/cmd_vel")
	assert.Contains(t, topics, "/odom")
	assert.Contains(t, topics, "/scan")
}

func TestModule_HandleDisconnect(t *testing.T) {
	mod := NewModule().(*Module)
	session := &models.Session{ID: "test-session"}
	participant := &models.Participant{ID: "test-participant"}
	mod.Init(session, participant)
	
	// Create subscriptions
	mod.Subscribe("/cmd_vel", "geometry_msgs/Twist")
	mod.Subscribe("/odom", "nav_msgs/Odometry")
	
	// Simulate disconnect
	mod.HandleDisconnect()
	
	// All subscriptions should be cleaned up
	topics := mod.GetAllTopics()
	assert.Len(t, topics, 0)
}

func TestModule_MultipleParticipants(t *testing.T) {
	// Create module for participant 1
	mod1 := NewModule().(*Module)
	session := &models.Session{ID: "test-session"}
	participant1 := &models.Participant{ID: "participant-1"}
	mod1.Init(session, participant1)
	
	// Create module for participant 2
	mod2 := NewModule().(*Module)
	participant2 := &models.Participant{ID: "participant-2"}
	mod2.Init(session, participant2)
	
	// Both subscribe to same topic
	mod1.Subscribe("/cmd_vel", "geometry_msgs/Twist")
	mod2.Subscribe("/cmd_vel", "geometry_msgs/Twist")
	
	// Verify both subscriptions exist
	subs1 := mod1.GetSubscriptions("/cmd_vel")
	subs2 := mod2.GetSubscriptions("/cmd_vel")
	
	assert.Len(t, subs1, 1)
	assert.Len(t, subs2, 1)
	assert.Equal(t, "participant-1", subs1[0].ParticipantID)
	assert.Equal(t, "participant-2", subs2[0].ParticipantID)
	
	// Disconnect participant 1
	mod1.HandleDisconnect()
	
	// Participant 2 should still be subscribed
	subs2After := mod2.GetSubscriptions("/cmd_vel")
	assert.Len(t, subs2After, 1)
	assert.Equal(t, "participant-2", subs2After[0].ParticipantID)
}

func TestModule_GetSubscriptionCount(t *testing.T) {
	mod := NewModule().(*Module)
	session := &models.Session{ID: "test-session"}
	participant := &models.Participant{ID: "test-participant"}
	mod.Init(session, participant)
	
	assert.Equal(t, 0, mod.GetSubscriptionCount())
	
	mod.Subscribe("/cmd_vel", "geometry_msgs/Twist")
	mod.Subscribe("/odom", "nav_msgs/Odometry")
	mod.Subscribe("/scan", "sensor_msgs/LaserScan")
	
	assert.Equal(t, 3, mod.GetSubscriptionCount())
}

func TestModule_MaxTopicsLimit(t *testing.T) {
	mod := NewModule().(*Module)
	session := &models.Session{ID: "test-session"}
	participant := &models.Participant{ID: "test-participant"}
	mod.Init(session, participant)
	
	// Subscribe to MaxTopicsPerClient topics
	for i := 0; i < MaxTopicsPerClient; i++ {
		mod.Subscribe(fmt.Sprintf("/topic%d", i), "std_msgs/String")
	}
	
	assert.Equal(t, MaxTopicsPerClient, mod.GetSubscriptionCount())
	
	// Try to subscribe to one more - should be rejected in handleSubscribe
	// This is tested at the handler level
}
