// Package websocket provides real-time WebSocket communication for arkUI.
package websocket

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/alexjoedt/log"

	"github.com/gorilla/websocket"
)

// MessageType defines the type of WebSocket message.
type MessageType string

const (
	MessageTypeBackupStart     MessageType = "backup.started"
	MessageTypeBackupProgress  MessageType = "backup.progress"
	MessageTypeBackupComplete  MessageType = "backup.completed"
	MessageTypeBackupFailed    MessageType = "backup.failed"
	MessageTypeRestoreStart    MessageType = "restore.started"
	MessageTypeRestoreProgress MessageType = "restore.progress"
	MessageTypeRestoreComplete MessageType = "restore.completed"
	MessageTypeRestoreFailed   MessageType = "restore.failed"
	MessageTypeJobStart        MessageType = "job.started"
	MessageTypeJobComplete     MessageType = "job.completed"
	MessageTypeJobFailed       MessageType = "job.failed"
	MessageTypeStatusUpdate    MessageType = "system.status"
)

// Message represents a WebSocket message sent to clients.
type Message struct {
	Type      MessageType `json:"type"`
	Timestamp time.Time   `json:"timestamp"`
	Data      any         `json:"data,omitempty"`
}

// Hub manages WebSocket connections and broadcasts messages.
// It's a deep module - simple broadcast interface, complex connection management hidden.
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan Message
	register   chan *Client
	unregister chan *Client
	done       chan struct{}
	mu         sync.RWMutex
	logger     *log.Logger
}

// Client represents a WebSocket client connection.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// NewHub creates a new WebSocket hub.
func NewHub(logger *log.Logger) *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan Message, 256), // Buffered to prevent blocking
		register:   make(chan *Client),
		unregister: make(chan *Client),
		done:       make(chan struct{}),
		logger:     logger,
	}
}

// Run starts the hub's main event loop.
// This should be called in a goroutine. It returns when Stop is called.
func (h *Hub) Run() {
	for {
		select {
		case <-h.done:
			// Graceful shutdown: close all client connections
			h.mu.Lock()
			for client := range h.clients {
				close(client.send)
				_ = client.conn.Close()
				delete(h.clients, client)
			}
			h.mu.Unlock()
			h.logger.Info("websocket hub stopped")
			return

		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			h.logger.Info("websocket client registered", "total_clients", len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			h.logger.Info("websocket client unregistered", "total_clients", len(h.clients))

		case message := <-h.broadcast:
			h.broadcastMessage(message)
		}
	}
}

// Stop gracefully shuts down the hub, closing all client connections.
func (h *Hub) Stop() {
	close(h.done)
}

// broadcastMessage sends a message to all connected clients.
func (h *Hub) broadcastMessage(msg Message) {
	// Add timestamp if not set
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	// Marshal message once
	data, err := json.Marshal(msg)
	if err != nil {
		h.logger.Error("failed to marshal websocket message", "error", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	h.logger.Debug("broadcasting message", "type", msg.Type, "clients", len(h.clients))

	// Send to all clients
	for client := range h.clients {
		select {
		case client.send <- data:
			// Message sent successfully
		default:
			// Client's send buffer is full, close connection
			h.logger.Warn("client send buffer full, closing connection")
			go func(c *Client) {
				h.unregister <- c
				_ = c.conn.Close()
			}(client)
		}
	}
}

// Broadcast sends a message to all connected clients.
// This is the main public interface - simple to use, complexity hidden.
// The msgType parameter can be a string or MessageType constant.
func (h *Hub) Broadcast(msgType any, data any) {
	var mt MessageType
	switch v := msgType.(type) {
	case MessageType:
		mt = v
	case string:
		mt = MessageType(v)
	default:
		h.logger.Error("invalid message type", "type", msgType)
		return
	}

	h.broadcast <- Message{
		Type:      mt,
		Timestamp: time.Now(),
		Data:      data,
	}
}

// ClientCount returns the number of connected clients.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// RegisterClient registers a new WebSocket client.
func (h *Hub) RegisterClient(conn *websocket.Conn) *Client {
	client := &Client{
		hub:  h,
		conn: conn,
		send: make(chan []byte, 256),
	}
	h.register <- client
	return client
}

// readPump pumps messages from the WebSocket connection to the hub.
// It runs in a goroutine per client connection.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		_ = c.conn.Close()
	}()

	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.hub.logger.Error("websocket error", "error", err)
			}
			break
		}
		// We don't expect messages from clients, just ignore them
	}
}

// writePump pumps messages from the hub to the WebSocket connection.
// It runs in a goroutine per client connection.
func (c *Client) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// Hub closed the channel
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Start starts the client's read and write pumps.
// This should be called after registering the client.
func (c *Client) Start() {
	go c.writePump()
	go c.readPump()
}
