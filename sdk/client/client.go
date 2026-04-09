package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	"github.com/Verboo/Verboo-SDK-go/pkg/logger"
	"github.com/Verboo/Verboo-SDK-go/sdk/transport"

	"go.uber.org/zap"
)

// prioritizedFrame represents a frame with priority for send queue.
type prioritizedFrame struct {
	f    *frame.Frame
	prio int // 0 = high, 1 = normal, 2 = low
}

// Client is the main SDK VerbooRTC client.
type Client struct {
	ctx          context.Context
	cancel       context.CancelFunc
	transport    transport.Transport
	options      *Options
	recvCh       chan *frame.Frame
	highCh       chan *prioritizedFrame // heartbeat, control frames, Ack
	normalCh     chan *prioritizedFrame // messages, file metadata
	lowCh        chan *prioritizedFrame // file chunks
	token        string
	onFrame      func(*frame.Frame)
	onConnect    func()
	onDisconnect func(error)
	onMessages   []func(*frame.Frame) // list of raw frame handlers
	logger       *zap.SugaredLogger
	handshakeErr error

	presence        bool
	rooms           map[string]bool
	fileReceivers   map[string]*fileReceiver
	onFileReceived  func(*frame.ReceivedFile)
	onFileAvailable func(*frame.FileAvailable)
	uploadSessions  map[string]*uploadSession
	downloads       map[string]*downloadSession
	batchAckHandler *BatchAckHandler
	rttManager      *clientRTTManager
	mu              sync.RWMutex
}

// NewClient creates a new client instance.
func NewClient(opts ...Option) (*Client, error) {
	options := newOptions(opts)

	if options.Token == "" && options.UserID != "default-user" {
		return nil, errors.New("JWT token must be provided")
	}

	tp, err := transport.CreateTransport(transport.Options{
		Addr:     options.Addr,
		Token:    options.Token,
		Insecure: options.Insecure,
		Timeout:  options.Timeout,
		Mode:     options.Mode,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create transport: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		ctx:             ctx,
		cancel:          cancel,
		transport:       tp,
		options:         options,
		token:           options.Token,
		logger:          options.Logger,
		handshakeErr:    nil,
		presence:        false,
		rooms:           make(map[string]bool),
		onMessages:      []func(*frame.Frame){},
		fileReceivers:   make(map[string]*fileReceiver),
		uploadSessions:  make(map[string]*uploadSession),
		downloads:       make(map[string]*downloadSession),
		batchAckHandler: nil,
		rttManager:      newClientRTTManager(),
		mu:              sync.RWMutex{},
	}

	if c.logger == nil {
		c.logger = logger.S()
	}

	// Channels with priorities
	c.highCh = make(chan *prioritizedFrame, 1024)   //
	c.normalCh = make(chan *prioritizedFrame, 2048) //
	c.lowCh = make(chan *prioritizedFrame, 65536)   // large buffer for chunks
	c.recvCh = make(chan *frame.Frame, 32768)       //

	// Callback for incoming frames from transport
	c.transport.OnFrame(func(f *frame.Frame) {
		if f.Type == frame.FrameHello && len(c.recvCh) > 0 {
			frame.PutFrame(f)
			return
		}
		c.handleIncoming(f)
	})
	// Wire WS reconnect: after the transport re-establishes the WebSocket
	if wsT, ok := tp.(*transport.WsTransport); ok && options.Reconnect {
		wsT.OnReconnect(func() {
			c.logger.Infow("ws reconnected, performing handshake")
			if err := c.performHandshake(); err != nil {
				c.logger.Warnw("ws reconnect handshake failed", "err", err)
				if c.onDisconnect != nil {
					c.onDisconnect(fmt.Errorf("reconnect handshake failed: %w", err))
				}
			}
		})
	}

	return c, nil
}

// Connect establishes connection and performs handshake.
func (c *Client) Connect() error {
	if err := c.transport.Connect(); err != nil {
		return fmt.Errorf("transport connect failed: %w", err)
	}

	if err := c.performHandshake(); err != nil {
		c.handshakeErr = err
		return fmt.Errorf("handshake failed: %w", err)
	}

	c.Start()
	return nil
}

// Start starts background goroutines.
func (c *Client) Start() {
	go c.receiveLoop()
	go c.sendLoop()
	go c.heartbeatLoop()
}

func (c *Client) SendPrioritized(f *frame.Frame, prio int) error {
	if f == nil {
		return errors.New("nil frame")
	}

	var ch chan *prioritizedFrame
	switch prio {
	case 0:
		ch = c.highCh
	case 1:
		ch = c.normalCh
	default:
		ch = c.lowCh
	}

	select {
	case ch <- &prioritizedFrame{f: f, prio: prio}:
		return nil
	default:
		frame.PutFrame(f)
		return errors.New("send channel full")
	}
}

func (c *Client) OnFrame(handler func(*frame.Frame)) {
	c.onFrame = handler
}

func (c *Client) AddOnMessage(handler func(*ParsedMessage), opts ...ParseOption) {
	internalHandler := func(f *frame.Frame) {
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

	c.onMessages = append(c.onMessages, internalHandler)

	if len(c.onMessages) == 1 {
		c.transport.OnFrame(func(f *frame.Frame) {
			if f.Type == frame.FrameHello && len(c.recvCh) > 0 {
				frame.PutFrame(f)
				return
			}

			c.handleSystemFrames(f)

			for _, h := range c.onMessages {
				h(f)
			}

			frame.PutFrame(f)
		})
	}
}

// OnConnect callback on successful connection and handshake.
func (c *Client) OnConnect(cb func()) {
	c.onConnect = cb
}

// OnDisconnect callback on connection break.
func (c *Client) OnDisconnect(cb func(error)) {
	c.onDisconnect = cb
}

// Close closes the connection and releases resources.
func (c *Client) Close() error {
	defer c.cancel()
	return c.transport.Close()
}

// SetPresence updates presence status.
func (c *Client) SetPresence(online bool) error {
	presence := frame.PresenceStatus{
		UserID:   c.token,
		Online:   online,
		LastSeen: time.Now().UnixMilli(),
	}

	payload, err := json.Marshal(presence)
	if err != nil {
		return fmt.Errorf("failed to marshal presence: %w", err)
	}

	f := &frame.Frame{
		Type:     frame.FramePresence,
		Version:  1,
		StreamID: 0,
		Payload:  payload,
	}

	return c.SendPrioritized(f, 1)
}

// JoinRoom joins the user to a room.
func (c *Client) JoinRoom(roomID string) error {
	payload, err := json.Marshal(map[string]string{"room_id": roomID})
	if err != nil {
		return fmt.Errorf("failed to marshal join request: %w", err)
	}

	f := &frame.Frame{
		Type:     frame.FrameJoinRoom,
		Version:  1,
		StreamID: 0,
		Payload:  payload,
	}

	return c.SendPrioritized(f, 1)
}

// LeaveRoom leaves the room.
func (c *Client) LeaveRoom(roomID string) error {
	payload, err := json.Marshal(map[string]string{"room_id": roomID})
	if err != nil {
		return fmt.Errorf("failed to marshal leave request: %w", err)
	}

	f := &frame.Frame{
		Type:     frame.FrameLeaveRoom,
		Version:  1,
		StreamID: 0,
		Payload:  payload,
	}

	return c.SendPrioritized(f, 1)
}

// SendToRoom sends a message to a room.
func (c *Client) SendToRoom(roomID, msg string) error {
	header := frame.MessageHeader{
		MessageID:  "sdk-" + time.Now().Format("150405.000"),
		SenderID:   c.token,
		TargetID:   "#" + roomID,
		Timestamp:  time.Now().UnixMilli(),
		Persistent: true,
	}

	payload := buildPayload(header, []byte(msg))

	f := &frame.Frame{
		Type:     frame.FrameData,
		Version:  1,
		StreamID: 1,
		Payload:  payload,
	}

	return c.SendPrioritized(f, 1)
}

// SendTextMessage sends a personal text message.
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
	return c.SendPrioritized(f, 1)
}

// handleSystemFrames handles system frames (files, errors, etc.).
func (c *Client) handleSystemFrames(f *frame.Frame) {
	switch f.Type {
	case frame.FrameHeartbeat:
		c.rttManager.recordPong()
	case frame.FrameFileMetadata:
		c.logger.Debug("FrameFileMetadata received")
		_ = c.handleFileAvailable(f) // Treat as notification only
	case frame.FrameFileAvailable:
		_ = c.handleFileAvailable(f)
	case frame.FrameFileChunkServer:
		c.logger.Debug("FrameFileChunkServer received")
		_ = c.handleFileChunkServer(f) // Download from server (with auto-decompression)
	case frame.FrameFileDownloadEnd:
		c.logger.Debug("FrameFileDownloadEnd received")
		_ = c.handleFileDownloadEnd(f)
	case frame.FrameFileAck:
		var ackData map[string]interface{}
		if err := json.Unmarshal(f.Payload, &ackData); err != nil {
			c.logger.Debugw("failed to unmarshal file ack", "err", err)
			frame.PutFrame(f)
			return
		}

		// create batchAckHandler if not exists
		if c.batchAckHandler == nil {
			c.batchAckHandler = NewBatchAckHandler(c)
		}

		err := c.batchAckHandler.HandleFileAck(ackData)
		if err != nil {
			c.logger.Warnw("failed to handle file ack", "err", err, "file_id", ackData["file_id"])
		}

		//  IsFileEnd = true -> file complete
		if isFileEnd := ackData["is_file_end"]; isFileEnd == true {
			c.logger.Infow("file download completed - received FILE_END flag",
				"file_id", ackData["file_id"],
				"batch_size", ackData["batch_size"],
				"last_chunk_index", ackData["last_chunk_index"])
		}

		if isFileEnd := ackData["is_file_end"]; !(isFileEnd == true) {
			status, _ := ackData["status"].(string)
			c.mu.RLock()
			fileID, _ := ackData["file_id"].(string)
			session, exists := c.uploadSessions[fileID]
			c.mu.RUnlock()

			if exists && status == "ok" {
				// Signal sliding window that a chunk was processed (upload)
				select {
				case session.ackChan <- struct{}{}:
				default:
					// Channel full - client is not waiting for ack, drop it gracefully
				}
			}
		}

		frame.PutFrame(f)
		return
	// Virtual stream file transfer frames (new high-performance streaming)
	case frame.FrameVirtualStreamInit:
		c.logger.Debug("FrameVirtualStreamInit received")
		_ = c.handleVirtualStreamInit(f) // handle virtual stream init ack
	case frame.FrameVirtualStreamEnd:
		c.logger.Debug("FrameVirtualStreamEnd received")
		_ = c.handleVirtualStreamEnd(f) // handle virtual stream end (for downloads)
	case frame.FrameError:
		if c.onDisconnect != nil {
			c.onDisconnect(fmt.Errorf("server error: %s", string(f.Payload)))
		}
	case frame.FrameData:
		if c.onFrame != nil {
			c.onFrame(f)
		}
	case frame.FrameRoute:
		if c.onConnect != nil {
			go c.onConnect()
		}
	}
}

// handleIncoming handles incoming frames (called from receiveLoop).
func (c *Client) handleIncoming(f *frame.Frame) {
	defer frame.PutFrame(f)
	c.handleSystemFrames(f)
}

// performHandshake performs HELLO -> AUTH sequence.
func (c *Client) performHandshake() error {
	if err := c.sendHello(); err != nil {
		return fmt.Errorf("hello failed: %w", err)
	}
	return c.sendAuth()
}

func (c *Client) sendHello() error {
	hello := &frame.Frame{
		Type:    frame.FrameHello,
		Version: 1,
		Payload: []byte(`{"version":1,"features":["messaging","rooms","presence"]}`),
	}
	return c.transport.SendFrame(hello)
}

func (c *Client) sendAuth() error {
	auth := map[string]string{
		"token":   c.token,
		"user_id": c.token,
	}

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

func (c *Client) heartbeatLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			// Heartbeat — makes session alive
			hb := frame.GetFrame()
			hb.Type = frame.FrameHeartbeat
			hb.Version = 1
			hb.StreamID = 0
			hb.Payload = []byte{}
			c.rttManager.recordPing()
			if err := c.SendPrioritized(hb, DefaultPrioritySystem); err != nil {
				c.logger.Warnw("heartbeat enqueue failed", "err", err)
			}
		}
	}
}

func (c *Client) receiveLoop() {
	for f := range c.recvCh {
		if f == nil {
			break
		}
		c.handleIncoming(f)
	}
}

func (c *Client) sendLoop() {
	for {
		// High priority - always first
		select {
		case <-c.ctx.Done():
			return
		case pf := <-c.highCh:
			if err := c.transport.SendFrame(pf.f); err != nil {
				c.logger.Warnw("high send failed", "err", err, "type", pf.f.Type)
				frame.PutFrame(pf.f)
			}
			continue
		default:
		}

		// Normal priority
		select {
		case <-c.ctx.Done():
			return
		case pf := <-c.normalCh:
			if err := c.transport.SendFrame(pf.f); err != nil {
				c.logger.Warnw("normal send failed", "err", err, "type", pf.f.Type)
				frame.PutFrame(pf.f)
			}
			continue
		default:
		}

		// Low priority or again high/normal (if appeared)
		select {
		case <-c.ctx.Done():
			return
		case pf := <-c.highCh:
			if err := c.transport.SendFrame(pf.f); err != nil {
				frame.PutFrame(pf.f)
			}
		case pf := <-c.normalCh:
			if err := c.transport.SendFrame(pf.f); err != nil {
				frame.PutFrame(pf.f)
			}
		case pf := <-c.lowCh:
			if err := c.transport.SendFrame(pf.f); err != nil {
				c.logger.Warnw("low send failed", "err", err, "type", pf.f.Type)
				frame.PutFrame(pf.f)
			}
		}
	}
}

func buildPayload(header frame.MessageHeader, body []byte) []byte {
	hdrJSON, _ := json.Marshal(header)
	payload := make([]byte, 0, len(hdrJSON)+1+len(body))
	payload = append(payload, hdrJSON...)
	payload = append(payload, 0x1E)
	payload = append(payload, body...)
	return payload
}
