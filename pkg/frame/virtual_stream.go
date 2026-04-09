package frame

// VirtualStreamInit represents initialization frame for a new virtual stream.
// Contains metadata about the file being uploaded via streaming.
type VirtualStreamInit struct {
	FileID      string `json:"file_id"`               // unique file ID: "{sender}-{timestamp_ns}"
	Filename    string `json:"filename"`              // original filename with extension
	Size        int64  `json:"size"`                  // ORIGINAL uncompressed size in bytes
	MIME        string `json:"mime,omitempty"`        // MIME type of the file
	Recipient   string `json:"recipient"`             // target user ID for direct transfer or empty for server storage
	ChunkSize   int    `json:"chunk_size"`            // chunk size in bytes (default: 65536)
	Compression string `json:"compression,omitempty"` // "zstd" if client compressed, "" otherwise
}

// VirtualStreamData represents a data chunk sent within an active virtual stream.
type VirtualStreamData struct {
	FileID string `json:"file_id"` // same file ID from init frame
	Index  int    `json:"index"`   // chunk index (0-based)
}

// VirtualStreamEnd represents end-of-stream confirmation with checksum.
type VirtualStreamEnd struct {
	FileID      string `json:"file_id"`          // same file ID from init frame
	TotalChunks int    `json:"total_chunks"`     // total number of chunks sent
	SHA256      string `json:"sha256,omitempty"` // SHA256 hash of COMPLETE FILE (after decompression)
}

// VirtualStreamAck represents acknowledgment for a stream chunk.
type VirtualStreamAck struct {
	FileID     string `json:"file_id"`         // file ID being acknowledged
	ChunkIndex int    `json:"chunk_index"`     // chunk index being acknowledged
	Status     string `json:"status"`          // "ok" or "error"
	Error      string `json:"error,omitempty"` // error description if status is "error"
}
