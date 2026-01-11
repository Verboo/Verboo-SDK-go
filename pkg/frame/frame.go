// Package frame implements a compact binary frame protocol with helpers for
// zero-copy decoding, pooled encoding, and small-header encoding into existing
// buffers. Callers must observe ownership contracts: DecodeInto and
// ZeroCopyDecode return payload slices that reference the input buffer and
// must be copied before long-term storage; EncodePooled returns a pooled
// []byte that the caller MUST return with ReleaseEncoded exactly once.
package frame

import (
	"encoding/binary" // big-endian helpers
	"errors"          // error values
	"sync"            // sync.Pool

	"github.com/Verboo/Verboo-SDK-go/pkg/config"
)

const HeaderLen = 12 // header size: Type(1)+Flags(1)+Version(2)+StreamID(4)+Len(4)

type FrameType uint8 // 1-byte frame type enum

// Core frame types for protocol operations (more detailed in comments below)
const (
	FrameOpenStream  FrameType = 0x01 // open stream
	FrameCloseStream FrameType = 0x02 // close stream
	FrameData        FrameType = 0x03 // application data (default)
	FrameAck         FrameType = 0x04 // transport level acknowledgement
	FrameNack        FrameType = 0x05 // transport level negative acknowledgement
	FramePing        FrameType = 0x06 // ping message (keepalive)
	FramePong        FrameType = 0x07 // pong response to ping
	FrameHello       FrameType = 0x08 // initial handshake message
	FrameAuth        FrameType = 0x09 // authentication payload
	FrameHeartbeat   FrameType = 0x0A // periodic heartbeat message (no ack required)
	FrameRoute       FrameType = 0x0B // route information after handshake
	FrameError       FrameType = 0x0C // error response during communication
	FrameDatagram    FrameType = 0x0D // connectionless datagram payload (best-effort)
)

// Extended frame types for messaging system
const (
	FrameMessageAck     FrameType = 0x0E // application level message delivery acknowledgement
	FrameMessageNack    FrameType = 0x0F // application level message delivery failure
	FrameMessageStatus  FrameType = 0x10 // message read/delivered status updates
	FrameJoinRoom       FrameType = 0x11 // join chat room/channel request
	FrameLeaveRoom      FrameType = 0x12 // leave chat room/channel request
	FrameRoomList       FrameType = 0x13 // request room list
	FrameRoomUpdate     FrameType = 0x14 // room information update (e.g. new member)
	FrameRoomMessage    FrameType = 0x15 // message scoped to specific room
	FramePresence       FrameType = 0x16 // user online/offline presence update
	FrameTyping         FrameType = 0x17 // typing indicators (in chat)
	FramePriorityUpdate FrameType = 0x18 // message priority update
)

// Priority levels for message prioritization (use in Frame.Flags)
const (
	FlagPrioritySystem = 0x01 // system messages, calls, critical notifications
	FlagPriorityHigh   = 0x02 // important messages, direct mentions, alerts
	FlagPriorityNormal = 0x04 // regular chat messages (default priority)
	FlagPriorityLow    = 0x08 // background sync, typing indicators, presence updates
	FlagPriorityBatch  = 0x10 // batch operations, history sync (low priority)
)

// Delivery status codes for message tracking
const (
	DeliveryStatusSent      = 0x20 // message sent from client (enqueued)
	DeliveryStatusDelivered = 0x40 // message delivered to recipient
	DeliveryStatusRead      = 0x60 // message read by recipient
	DeliveryStatusFailed    = 0x80 // message delivery failed
)

// Frame represents a single binary frame for network transmission
type Frame struct {
	Type     FrameType // 1-byte type identifier (see constants above)
	Flags    uint8     // 1-byte flags (priority + delivery status)
	Version  uint16    // 2-byte protocol version
	StreamID uint32    // 4-byte stream identifier (for multiplexing)
	Payload  []byte    // Variable length payload data
}

// MessageHeader provides application-level message metadata (embedded in Frame.Payload)
type MessageHeader struct {
	MessageID  string `json:"message_id,omitempty"` // unique message identifier (ULID format)
	Sequence   uint64 `json:"sequence,omitempty"`   // sequence number for ordering
	Timestamp  int64  `json:"ts,omitempty"`         // unix milliseconds timestamp
	SenderID   string `json:"from,omitempty"`       // sender user identifier
	TargetID   string `json:"to,omitempty"`         // recipient or room identifier
	Persistent bool   `json:"persistent,omitempty"` // whether to store in message history
}

// DeliveryReceipt provides message delivery confirmation (used in Frame.Payload)
type DeliveryReceipt struct {
	MessageID string `json:"message_id,omitempty"` // original message identifier
	Status    uint8  `json:"status,omitempty"`     // delivery status code (DeliveryStatus*)
	Timestamp int64  `json:"ts,omitempty"`         // status change timestamp
	Error     string `json:"error,omitempty"`      // error description for failed delivery
}

// RoomInfo represents chat room or channel information (used in Frame.Payload)
type RoomInfo struct {
	RoomID      string   `json:"id,omitempty"`         // unique room identifier
	Name        string   `json:"name,omitempty"`       // display name
	Description string   `json:"desc,omitempty"`       // room description
	Type        string   `json:"type,omitempty"`       // room type: "direct", "group", "channel"
	Members     []string `json:"members,omitempty"`    // list of member user IDs
	CreatedAt   int64    `json:"created_at,omitempty"` // creation timestamp
	UpdatedAt   int64    `json:"updated_at,omitempty"` // last update timestamp
}

// PresenceStatus represents user presence information (used in Frame.Payload)
type PresenceStatus struct {
	UserID   string `json:"user_id,omitempty"`   // user identifier
	Online   bool   `json:"online,omitempty"`    // online/offline status
	LastSeen int64  `json:"last_seen,omitempty"` // last seen timestamp (ms)
	Device   string `json:"device,omitempty"`    // device type: web, mobile, desktop
	Status   string `json:"status,omitempty"`    // user status: away, busy, available
}

// SetPriority sets message priority level in frame flags (bits 0-4)
func (f *Frame) SetPriority(priority uint8) {
	f.Flags = (f.Flags & 0xE0) | (priority & 0x1F) // preserve delivery status bits
}

// GetPriority retrieves message priority level from frame flags (bits 0-4)
func (f *Frame) GetPriority() uint8 {
	return f.Flags & 0x1F // extract priority from lower 5 bits
}

// SetDeliveryStatus sets message delivery status in frame flags (bits 5-7)
func (f *Frame) SetDeliveryStatus(status uint8) {
	f.Flags = (f.Flags & 0x1F) | (status & 0xE0) // preserve priority bits
}

// GetDeliveryStatus retrieves message delivery status from frame flags (bits 5-7)
func (f *Frame) GetDeliveryStatus() uint8 {
	return f.Flags & 0xE0 // extract delivery status from upper 3 bits
}

// SetPersistent sets persistence flag in frame flags (bit 7)
func (f *Frame) SetPersistent(persist bool) {
	if persist {
		f.Flags |= 0x80 // set high bit (bit 7)
	} else {
		f.Flags &^= 0x80 // clear high bit
	}
}

// IsPersistent checks if frame has persistence flag set (bit 7)
func (f *Frame) IsPersistent() bool {
	return (f.Flags & 0x80) != 0 // check high bit for persistence
}

// encodePool provides reusable buffers for EncodePooled to reduce allocations.
var encodePool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 0, 8*1024) // zero-length slice with 8KiB capacity
	},
}

// framePool provides reusable *Frame objects for DecodeInto hot path.
var framePool = sync.Pool{
	New: func() interface{} { return &Frame{} }, // allocate new Frame when pool empty
}

var ErrPayloadTooLarge = errors.New("frame payload exceeds maximum allowed size")

// init pre-warms the pools with default capacity
func init() {
	const warm = 64 // Pre-seed pool (no guarantee these persist)
	// Note: sync.Pool may drop items during GC - this is best-effort warmup
	for i := 0; i < warm; i++ {
		encodePool.Put(make([]byte, 0, 8*1024)) // put zero-len buffers with capacity
	}
}

// GetFrame rents a *Frame from framePool.
func GetFrame() *Frame { return framePool.Get().(*Frame) }

// PutFrame returns a *Frame to framePool after clearing references.
func PutFrame(f *Frame) {
	f.Type = 0       // clear Type
	f.Flags = 0      // clear Flags
	f.Version = 0    // clear Version
	f.StreamID = 0   // clear StreamID
	f.Payload = nil  // drop payload reference
	framePool.Put(f) // put back to pool
}

// DecodeInto fills f from b and sets Payload as a zero-copy view into b.
// WARNING: f.Payload is a subslice of the provided buffer b; the lifetime of
// that payload is tied to b. If the payload will be used asynchronously or stored
// beyond the lifetime of the input buffer, make an explicit copy (see CopyPayload).
func DecodeInto(f *Frame, b []byte) error {
	if len(b) < HeaderLen { // header must be present
		return errors.New("buffer too small")
	}
	pl := binary.BigEndian.Uint32(b[8:12]) // read payload length

	// Security check: prevent DoS by rejecting frames with oversized payload
	if int64(pl) > config.MaxFramePayloadSize() {
		return ErrPayloadTooLarge
	}

	if uint32(len(b)) < HeaderLen+pl { // validate full payload present
		return errors.New("incomplete payload")
	}
	f.Type = FrameType(b[0])                     // Type from first byte
	f.Flags = b[1]                               // Flags from second byte
	f.Version = binary.BigEndian.Uint16(b[2:4])  // Version from bytes 2-3
	f.StreamID = binary.BigEndian.Uint32(b[4:8]) // StreamID from bytes 4-7
	if pl > 0 {
		f.Payload = b[12 : 12+pl] // zero-copy slice into b for payload
	} else {
		f.Payload = nil // no payload
	}
	return nil // success
}

// Encode allocates a new buffer and returns header+payload copy.
func Encode(f *Frame) ([]byte, error) {
	if f == nil { // validate input
		return nil, errors.New("nil frame")
	}
	need := HeaderLen + len(f.Payload)                            // total size required
	buf := make([]byte, HeaderLen+len(f.Payload))                 // allocate exact-sized buffer
	buf[0] = byte(f.Type)                                         // write Type at position 0
	buf[1] = f.Flags                                              // write Flags at position 1
	binary.BigEndian.PutUint16(buf[2:4], f.Version)               // write Version BE at 2-3
	binary.BigEndian.PutUint32(buf[4:8], f.StreamID)              // write StreamID BE at 4-7
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(f.Payload))) // write payload length BE at 8-11
	copy(buf[12:], f.Payload)                                     // copy payload starting at 12
	_ = need                                                      // keep var used
	return buf, nil                                               // return allocated buffer
}

// Decode copies bytes into new *Frame (safe copy)
func Decode(b []byte) (*Frame, error) {
	if len(b) < HeaderLen { // header check
		return nil, errors.New("buffer too small")
	}
	pl := binary.BigEndian.Uint32(b[8:12]) // payload length

	// Security check: prevent DoS by rejecting frames with oversized payload
	if int64(pl) > config.MaxFramePayloadSize() {
		return nil, ErrPayloadTooLarge
	}

	if uint32(len(b)) < HeaderLen+pl { // full payload check
		return nil, errors.New("incomplete payload")
	}
	f := &Frame{
		Type:     FrameType(b[0]),                 // read Type from first byte
		Flags:    b[1],                            // read Flags from second byte
		Version:  binary.BigEndian.Uint16(b[2:4]), // read Version from bytes 2-3
		StreamID: binary.BigEndian.Uint32(b[4:8]), // read StreamID from bytes 4-7
	}
	if pl > 0 {
		f.Payload = make([]byte, pl) // allocate payload slice
		copy(f.Payload, b[12:12+pl]) // copy payload bytes
	}
	return f, nil // return new Frame
}

// ZeroCopyDecode returns newly allocated *Frame that references input buffer for payload.
func ZeroCopyDecode(b []byte) (*Frame, error) {
	if len(b) < HeaderLen { // header check
		return nil, errors.New("buffer too small")
	}
	pl := binary.BigEndian.Uint32(b[8:12]) // payload length

	// Security check: prevent DoS by rejecting frames with oversized payload
	if int64(pl) > config.MaxFramePayloadSize() {
		return nil, ErrPayloadTooLarge
	}

	if uint32(len(b)) < HeaderLen+pl { // full payload check
		return nil, errors.New("incomplete payload")
	}
	f := &Frame{
		Type:     FrameType(b[0]),                 // read Type from first byte
		Flags:    b[1],                            // read Flags from second byte
		Version:  binary.BigEndian.Uint16(b[2:4]), // read Version from bytes 2-3
		StreamID: binary.BigEndian.Uint32(b[4:8]), // read StreamID from bytes 4-7
		Payload:  b[12 : 12+pl],                   // payload view into b (zero-copy)
	}
	return f, nil // return frame referencing b
}

// EncodePooled writes header+payload into a pooled buffer and returns it.
// Ownership contract (See pkg/frame/OWNERSHIP.md.):
//   - The returned []byte may be backed by a shared pool and MUST NOT be mutated
//     after being handed to other goroutines unless you own the buffer.
//   - The caller that receives the buffer becomes its owner and MUST call
//     ReleaseEncoded(buf) exactly once when done, unless ownership is explicitly
//     transferred.
func EncodePooled(f *Frame) ([]byte, error) {
	if f == nil { // validate input
		return nil, errors.New("nil frame")
	}
	need := HeaderLen + len(f.Payload) // total bytes required

	b := encodePool.Get().([]byte) // rent a []byte from the pool
	if cap(b) < need {             // if rented buffer capacity is insufficient
		b = make([]byte, need) // allocate new buffer with length == need
	} else {
		b = b[:need] // reuse buffer and set length to final size
	}

	b[0] = byte(f.Type)           // Type at offset 0
	b[1] = f.Flags                // Flags at offset 1
	b[2] = byte(f.Version >> 8)   // Version high byte at offset 2
	b[3] = byte(f.Version)        // Version low byte at offset 3
	b[4] = byte(f.StreamID >> 24) // StreamID byte 0 at offset 4
	b[5] = byte(f.StreamID >> 16) // StreamID byte 1 at offset 5
	b[6] = byte(f.StreamID >> 8)  // StreamID byte 2 at offset 6
	b[7] = byte(f.StreamID)       // StreamID byte 3 at offset 7
	l := uint32(len(f.Payload))   // payload length
	b[8] = byte(l >> 24)          // length byte 0 at offset 8
	b[9] = byte(l >> 16)          // length byte 1 at offset 9
	b[10] = byte(l >> 8)          // length byte 2 at offset 10
	b[11] = byte(l)               // length byte 3 at offset 11

	copy(b[HeaderLen:], f.Payload) // copy payload into reserved region after header

	return b, nil // return pooled buffer to caller
}

// ReleaseEncoded returns a pooled buffer back to the pool.
// It is noop-safe for heap buffers (calling ReleaseEncoded on a non-pooled buffer is allowed).
func ReleaseEncoded(b []byte) {
	if b == nil {
		return
	}
	encodePool.Put(b[:0]) // reset length, preserve capacity, put back
}

// EncodeInto writes header+payload into dst if dst has enough spare capacity.
// It performs no allocations when dst capacity is sufficient. The returned
// slice is dst extended with header+payload. On insufficient capacity the
// function returns an error and leaves dst unchanged.
func EncodeInto(dst []byte, f *Frame) ([]byte, error) {
	if f == nil { // validate input
		return nil, errors.New("nil frame")
	}
	need := HeaderLen + len(f.Payload) // bytes required
	if cap(dst)-len(dst) < need {      // check spare capacity
		return nil, errors.New("dst buffer too small")
	}
	start := len(dst)                     // current end of dst
	dst = dst[:start+HeaderLen]           // extend dst to include header
	dst[start+0] = byte(f.Type)           // write Type
	dst[start+1] = f.Flags                // write Flags
	dst[start+2] = byte(f.Version >> 8)   // Version high byte
	dst[start+3] = byte(f.Version)        // Version low byte
	dst[start+4] = byte(f.StreamID >> 24) // StreamID byte 0
	dst[start+5] = byte(f.StreamID >> 16) // StreamID byte 1
	dst[start+6] = byte(f.StreamID >> 8)  // StreamID byte 2
	dst[start+7] = byte(f.StreamID)       // StreamID byte 3
	l := uint32(len(f.Payload))           // payload length
	dst[start+8] = byte(l >> 24)          // length byte 0
	dst[start+9] = byte(l >> 16)          // length byte 1
	dst[start+10] = byte(l >> 8)          // length byte 2
	dst[start+11] = byte(l)               // length byte 3
	dst = append(dst, f.Payload...)       // append payload into dst
	_ = need                              // keep var referenced
	return dst, nil                       // return extended dst
}
