package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Verboo/Verboo-SDK-go/sdk/transport"
	"time"

	"github.com/Verboo/Verboo-SDK-go/internal/logger"
	"github.com/Verboo/Verboo-SDK-go/pkg/frame"

	"go.uber.org/zap"
)

// Client represents the main VerbooRTC SDK client for connecting to the server.
type Client struct {
	ctx          context.Context
	cancel       context.CancelFunc
	transport    transport.Transport
	recvCh       chan *frame.Frame
	sendCh       chan *frame.Frame
	token        string
	onFrame      func(*frame.Frame)
	onConnect    func()
	onDisconnect func(error)
	onMessages   []func(*frame.Frame) // List of frame handlers (not parsed messages)
	logger       *zap.SugaredLogger
	handshakeErr error

	presence bool
	rooms    map[string]bool
}

// NewClient creates a new VerbooRTC client instance.
func NewClient(opts ...Option) (*Client, error) {
	options := newOptions(opts)

	// Validate required token for authentication if not using default user ID
	if options.Token == "" && options.UserID != "default-user" {
		return nil, errors.New("JWT token must be provided")
	}

	// Create transport based on configuration
	transport, err := transport.CreateTransport(transport.Options{
		Addr:     options.Addr,
		Token:    options.Token,
		Insecure: options.Insecure,
		Timeout:  options.Timeout,
		Mode:     options.Mode,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create transport: %w", err)
	}

	// Initialize client context
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		ctx:          ctx,
		cancel:       cancel,
		transport:    transport,
		token:        options.Token,
		logger:       options.Logger,
		handshakeErr: nil,
		presence:     false,
		rooms:        make(map[string]bool),
		onMessages:   []func(*frame.Frame){}, // Initialize empty handler list
	}

	if c.logger == nil {
		c.logger = logger.S()
	}

	// Setup channels for frame communication
	c.recvCh = make(chan *frame.Frame, 128)
	c.sendCh = make(chan *frame.Frame, 128)

	// Set up transport callbacks to handle incoming frames
	c.transport.OnFrame(func(f *frame.Frame) {
		if f.Type == frame.FrameHello && len(c.recvCh) > 0 {
			frame.PutFrame(f)
			return
		}
		c.handleIncoming(f)
	})

	return c, nil
}

// Connect establishes a connection to the server.
func (c *Client) Connect() error {
	// Establish transport connection first
	if err := c.transport.Connect(); err != nil {
		return fmt.Errorf("transport connect failed: %w", err)
	}

	// Perform handshake sequence (HELLO → AUTH)
	if err := c.performHandshake(); err != nil {
		c.handshakeErr = err
		return fmt.Errorf("handshake failed: %w", err)
	}

	// Start background goroutines for receiving, sending and heartbeats
	c.Start()

	return nil
}

// Start initializes all necessary background processes.
func (c *Client) Start() {
	go c.receiveLoop()
	go c.sendLoop()
	go c.heartbeatLoop()
}

// SendFrame sends a frame to the server over the established connection.
func (c *Client) SendFrame(f *frame.Frame) error {
	// Attempt to send through channel with backpressure handling
	select {
	case c.sendCh <- f:
		return nil
	default:
		frame.PutFrame(f)
		return errors.New("send channel full")
	}
}

// OnFrame sets a callback for raw frame processing (no parsing).
func (c *Client) OnFrame(handler func(*frame.Frame)) {
	c.onFrame = handler
}

// AddOnMessage adds a message parser handler that processes frames and filters according to options.
// AddOnMessage adds a message parser handler that processes frames and filters according to options.
func (c *Client) AddOnMessage(handler func(*ParsedMessage), opts ...ParseOption) {
	// Create internal handler for parsing with the specified options
	internalHandler := func(f *frame.Frame) {
		// Only process FrameData and FrameRoomMessage types
		if f.Type != frame.FrameData && f.Type != frame.FrameRoomMessage {
			return
		}

		msg, err := ParseMessage(f, opts...)
		if err != nil {
			c.logger.Debug("failed to parse message", "err", err, "type", f.Type)
			return
		}
		handler(msg)
	}

	// Add the internal handler to our list of handlers
	c.onMessages = append(c.onMessages, internalHandler)

	// If this is the first handler, set up a single transport callback for all messages
	if len(c.onMessages) == 1 {
		// Clear previous OnFrame if it existed
		c.transport.OnFrame(nil)

		// Set new callback to handle all frames and route to our handlers
		c.transport.OnFrame(func(f *frame.Frame) {
			if f.Type == frame.FrameHello && len(c.recvCh) > 0 {
				frame.PutFrame(f)
				return
			}

			for _, handler := range c.onMessages {
				handler(f)
			}
		})
	}
}

// OnConnect sets a callback that will be called when the client connects and completes handshake.
func (c *Client) OnConnect(cb func()) {
	c.onConnect = cb
}

// OnDisconnect sets a callback that will be called when the connection is closed or disconnected.
func (c *Client) OnDisconnect(cb func(error)) {
	c.onDisconnect = cb
}

// Close terminates all client connections and releases resources.
func (c *Client) Close() error {
	defer c.cancel()
	return c.transport.Close()
}

// SetPresence updates the user's online status in presence system.
func (c *Client) SetPresence(online bool) error {
	// Create presence update frame
	presence := frame.PresenceStatus{
		UserID:   c.token,
		Online:   online,
		LastSeen: time.Now().UnixMilli(),
	}

	// Marshal to JSON for payload
	payload, err := json.Marshal(presence)
	if err != nil {
		return fmt.Errorf("failed to marshal presence: %w", err)
	}

	// Create frame with the presence data
	f := &frame.Frame{
		Type:     frame.FramePresence,
		Version:  1,
		StreamID: 0,
		Payload:  payload,
	}

	// Send via client's send channel
	return c.SendFrame(f)
}

// JoinRoom adds the user to a specific chat room.
func (c *Client) JoinRoom(roomID string) error {
	// Prepare join request as JSON
	payload, err := json.Marshal(map[string]string{"room_id": roomID})
	if err != nil {
		return fmt.Errorf("failed to marshal join request: %w", err)
	}

	// Create frame for joining the room
	f := &frame.Frame{
		Type:     frame.FrameJoinRoom,
		Version:  1,
		StreamID: 0,
		Payload:  payload,
	}

	// Send via client's send channel
	return c.SendFrame(f)
}

// LeaveRoom removes the user from a specific chat room.
func (c *Client) LeaveRoom(roomID string) error {
	// Prepare leave request as JSON
	payload, err := json.Marshal(map[string]string{"room_id": roomID})
	if err != nil {
		return fmt.Errorf("failed to marshal leave request: %w", err)
	}

	// Create frame for leaving the room
	f := &frame.Frame{
		Type:     frame.FrameLeaveRoom,
		Version:  1,
		StreamID: 0,
		Payload:  payload,
	}

	// Send via client's send channel
	return c.SendFrame(f)
}

// SendToRoom sends a message to all members in the specified room.
func (c *Client) SendToRoom(roomID, msg string) error {
	// Build header with metadata for the message
	header := frame.MessageHeader{
		MessageID:  "sdk-" + time.Now().Format("150405.000"),
		SenderID:   c.token,
		TargetID:   "#" + roomID,
		Timestamp:  time.Now().UnixMilli(),
		Persistent: true,
	}

	// Build payload in required format (headerJSON + RS + body)
	payload := buildPayload(header, []byte(msg))

	// Create frame with the message
	f := &frame.Frame{
		Type:     frame.FrameData,
		Version:  1,
		StreamID: 1,
		Payload:  payload,
	}

	// Send via client's send channel
	return c.SendFrame(f)
}

// SendTextMessage sends a message to member
func (c *Client) SendTextMessage(targetUserID string, message string) error {
	header := frame.MessageHeader{
		MessageID:  "sdk-" + time.Now().Format("150405.000"),
		SenderID:   c.token,
		TargetID:   targetUserID,
		Timestamp:  time.Now().UnixMilli(),
		Persistent: true,
	}

	payload := buildPayload(header, []byte(message))
	f := &frame.Frame{
		Type:     frame.FrameData,
		Version:  1,
		StreamID: 1,
		Payload:  payload,
	}
	return c.SendFrame(f)
}

// handleIncoming processes incoming frames from the server.
func (c *Client) handleIncoming(f *frame.Frame) {
	defer frame.PutFrame(f)

	// Handle different frame types based on type
	if f.Type == frame.FrameData || f.Type == frame.FrameHeartbeat {
		c.onFrame(f)

		// If this is a FrameRoute, trigger OnConnect callback
		if f.Type == frame.FrameRoute && c.onConnect != nil {
			go c.onConnect()
		}
	} else if f.Type == frame.FrameError {
		// Handle error frames (e.g., authentication failure)
		if c.onDisconnect != nil {
			c.onDisconnect(fmt.Errorf("server error: %s", string(f.Payload)))
		}
	}
}

// performHandshake executes the full handshake sequence with the server.
func (c *Client) performHandshake() error {
	// Send HELLO frame to initiate connection
	if err := c.sendHello(); err != nil {
		return fmt.Errorf("hello failed: %w", err)
	}

	// Send AUTH frame for authentication
	return c.sendAuth()
}

// sendHello sends the initial HELLO frame to start handshake.
func (c *Client) sendHello() error {
	hello := &frame.Frame{
		Type:    frame.FrameHello,
		Version: 1,
		Payload: []byte(`{"version":1,"features":["messaging","rooms","presence"]}`),
	}
	return c.transport.SendFrame(hello)
}

// sendAuth sends the AUTH frame with JWT token for authentication.
func (c *Client) sendAuth() error {
	// Prepare authentication payload
	auth := map[string]string{
		"token":   c.token,
		"user_id": c.token,
	}

	// Marshal to JSON and create frame
	data, err := json.Marshal(auth)
	if err != nil {
		return fmt.Errorf("auth payload marshal failed: %w", err)
	}

	authF := &frame.Frame{
		Type:    frame.FrameAuth,
		Version: 1,
		Payload: data,
	}
	return c.transport.SendFrame(authF)
}

// heartbeatLoop manages periodic heartbeats to maintain connection.
func (c *Client) heartbeatLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			c.logger.Debug("heartbeat loop stopped")
			return
		case <-ticker.C:
			// Send heartbeat frame to maintain connection
			if err := c.transport.SendFrame(&frame.Frame{
				Type:     frame.FrameHeartbeat,
				Version:  1,
				StreamID: 0,
				Payload:  []byte{},
			}); err != nil {
				c.logger.Warnw("heartbeat failed", "err", err)
				continue
			}
		}
	}
}

// receiveLoop processes incoming frames from the server.
func (c *Client) receiveLoop() {
	for f := range c.recvCh {
		if f == nil {
			break
		}
		c.handleIncoming(f)
	}
}

// sendLoop sends outgoing frames to the server.
func (c *Client) sendLoop() {
	for f := range c.sendCh {
		if err := c.transport.SendFrame(f); err != nil {
			c.logger.Warnw("send failed", "err", err, "type", f.Type)

			select {
			case c.sendCh <- f:
				continue
			default:
				frame.PutFrame(f)
			}
		}
	}
}

// buildPayload constructs payload in the required format (headerJSON + RS + body).
func buildPayload(header frame.MessageHeader, body []byte) []byte {
	hdrJSON, _ := json.Marshal(header)
	payload := make([]byte, 0, len(hdrJSON)+1+len(body))
	payload = append(payload, hdrJSON...)
	payload = append(payload, 0x1E)
	payload = append(payload, body...)
	return payload
}
