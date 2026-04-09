package client

// BatchAckHandler processes batch acknowledgments from the server
type BatchAckHandler struct {
	client               *Client
	fileDownloadComplete bool // Flag for tracking receipt of the final ACK with IsFileEnd
	supportsBatchAcks    bool
}

// NewBatchAckHandler creates a new packet ACK handler
func NewBatchAckHandler(client *Client) *BatchAckHandler {
	return &BatchAckHandler{
		client: client,

		// By default, we assume that the server supports packet ACKs
		supportsBatchAcks: true,
	}
}

// FileAckRaw v
type FileAckRaw struct {
	FileID         string `json:"file_id"`
	ChunkIndex     int    `json:"chunk_index,omitempty"`      // For single ACKs
	BatchSize      int    `json:"batch_size,omitempty"`       // Number of chunks in a batch (0 = single)
	LastChunkIndex int    `json:"last_chunk_index,omitempty"` // The last chunk in the batch
	Status         string `json:"status"`
	IsFileEnd      bool   `json:"is_file_end,omitempty"` // End of file flag
}

// HandleFileAck processes FrameFileAck from a server with packet ACK support
func (h *BatchAckHandler) HandleFileAck(ackRaw interface{}) error {
	// Convert to map to access fields
	data, ok := ackRaw.(map[string]interface{})
	if !ok {
		return ErrInvalidFileAck
	}

	fileID := ""
	batchSize := 0
	lastChunkIndex := -1
	isFileEnd := false

	if v, ok := data["file_id"]; ok {
		fileID = v.(string)
	}
	if v, ok := data["batch_size"]; ok {
		batchSize = int(v.(float64))
	}
	if v, ok := data["last_chunk_index"]; ok {
		lastChunkIndex = int(v.(float64))
	}
	if v, ok := data["is_file_end"]; ok {
		isFileEnd = v.(bool)
	}

	h.client.logger.Debugw("received file ack from server",
		"file_id", fileID,
		"batch_size", batchSize,
		"last_chunk_index", lastChunkIndex,
		"is_file_end", isFileEnd)

	// If the server sends IsFileEnd = true, this means that the file has been completely received.
	if isFileEnd {
		h.onFileDownloadComplete(fileID)
		return nil
	}

	// If BatchSize > 0, it is a batch ACK
	if batchSize > 0 && lastChunkIndex >= 0 {
		// Batch acknowledgment - the server received N chunks at once
		h.client.logger.Debugw("received batch acknowledgement",
			"file_id", fileID,
			"batch_size", batchSize,
			"last_chunk_index", lastChunkIndex)

		// Update the download state for the batch ACK
		return h.handleBatchAck(fileID, lastChunkIndex, batchSize)
	}

	return ErrInvalidFileAckFields
}

// handleBatchAck handles batch acknowledgment (N chunks at a time)
func (h *BatchAckHandler) handleBatchAck(fileID string, lastChunkIndex int, batchSize int) error {
	h.client.mu.Lock()
	defer h.client.mu.Unlock()

	// Find the download session for this file
	downloadSession, exists := h.client.downloads[fileID]
	if !exists {
		h.client.logger.Debugw("batch ack for unknown download session", "file_id", fileID)
		return nil
	}

	// Update the counter of received chunks in the batch
	downloadSession.mu.Lock()

	// Update the counter of received chunks in the batch // In batch mode, we may not know the exact indices of all chunks,
	// but we know that all chunks from 0 to lastChunkIndex have been received
	if downloadSession.batchReceived == nil {
		downloadSession.batchReceived = make(map[int]bool)
	}

	// We mark that the batch has been received (in simple terms, we just update the counter)
	downloadSession.chunksInCurrentBatch += batchSize

	h.client.logger.Debugw("batch ack processed",
		"file_id", fileID,
		"batch_size", batchSize,
		"chunks_in_batch", downloadSession.chunksInCurrentBatch)

	downloadSession.mu.Unlock()

	return nil
}

// onFileDownloadComplete handles the completion of file download upon receiving IsFileEnd
func (h *BatchAckHandler) onFileDownloadComplete(fileID string) {
	h.client.logger.Infow("file download completed - received FILE_END flag", "file_id", fileID)

	// We set the flag that the file has been received.
	h.fileDownloadComplete = true

	// onFileReceived is called by handleFileDownloadEnd — single source of truth
	h.client.mu.Lock()
	delete(h.client.downloads, fileID)
	h.client.mu.Unlock()
}

// IsDownloadComplete checks that the file has been completely received (via the IsFileEnd flag)
func (h *BatchAckHandler) IsDownloadComplete(fileID string) bool {
	// We check the flag from the final ACK
	if h.fileDownloadComplete {
		return true
	}

	// We also check the status of the download session.
	h.client.mu.RLock()
	defer h.client.mu.RUnlock()

	downloadSession, exists := h.client.downloads[fileID]
	if !exists || downloadSession == nil {
		return false
	}

	// If the ACK batch is not supported, check for pending Acks
	if !h.supportsBatchAcks && len(downloadSession.pendingAcks) == 0 {
		return true
	}

	return false
}

// Error definitions
var (
	ErrInvalidFileAck       = &ClientError{"invalid file ack frame"}
	ErrInvalidFileAckFields = &ClientError{"file ack missing required fields for batch mode"}
)

type ClientError struct {
	msg string
}

func (e *ClientError) Error() string {
	return e.msg
}
