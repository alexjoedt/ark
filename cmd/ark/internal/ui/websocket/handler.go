package websocket

import (
	"net/http"

	"github.com/alexjoedt/log"
	"github.com/gorilla/websocket"
)

// A nil CheckOrigin rejects cross-origin upgrades, so another site cannot
// open a socket with the session cookie of a logged-in user.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// Handler creates an HTTP handler for WebSocket connections.
// It integrates with chi router and session authentication.
func Handler(hub *Hub, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Upgrade HTTP connection to WebSocket
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Error("failed to upgrade websocket", "error", err)
			return
		}

		// Register client with hub
		client := hub.RegisterClient(conn)

		// Start client pumps
		client.Start()

		logger.Info("websocket client connected", "remote_addr", r.RemoteAddr)
	}
}
