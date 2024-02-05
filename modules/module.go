package modules

import (
	"context"

	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/models"
)

// Module is the interface that describes a module that extends Hagall
// capabilities.
type Module interface {
	// Returns the module name.
	Name() string

	// Initializes the module.
	Init(*models.Session, *models.Participant)

	// Handles a given message. Modules are free to decide whether they handle a
	// message.
	//
	// Returning ErrModuleMsgSkip indicates that handling a message was skipped.
	//
	// Any other returned errors causes the current WebSocket client to be
	// disconnected.
	HandleMsg(context.Context, hwebsocket.ResponseSender, hwebsocket.Msg) error

	// Handles a client disconnection.
	HandleDisconnect()
}
