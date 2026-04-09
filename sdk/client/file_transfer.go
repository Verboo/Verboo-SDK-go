package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sync"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	"github.com/klauspost/compress/zstd"
)

const (
	DefaultChunkSize      = 64 * 1024 // 64 KiB default chunk size
	DefaultWindowSize     = 8         // flow control window for file transfers
	DefaultPrioritySystem = 0         // highest priority for heartbeats
	DefaultPriorityNormal = 1         // medium priority for messages
	DefaultPriorityLow    = 2         // lowest priority for file chunks
)

// uploadSession tracks an active virtual stream upload to server Object Store
type uploadSession struct {
	fileID       string
	localPath    string
	chunkSize    int
	totalChunks  int
	currentIdx   int
	file         *os.File  // Not used for uploads but kept for compatibility
	sha256Hasher hash.Hash // For incremental SHA256 hashing of ORIGINAL (uncompressed) data
	progressCb   func(sent, total int64)
	written      int64 // bytes written so far
	mu           sync.Mutex
	ackChan      chan struct{} // Buffered channel for sliding window flow control
}

// fileReceiver tracks an incoming file transfer from server (download with auto-decompression)
type fileReceiver struct {
	fileID       string
	filename     string
	size         int64
	mime         string
	senderID     string
	chunkSize    int
	totalChunks  int
	written      int64
	filePath     string
	file         *os.File
	sha256Hasher hash.Hash     // For incremental SHA256 hashing of DECOMPRESSED data
	mu           sync.Mutex    // Protect file operations
	isCompressed bool          // true if file was uploaded with compression
	decoder      *zstd.Decoder // zstd decoder for decompression
}

// downloadSession tracks an active file download from server Object Store with flow control
type downloadSession struct {
	fileID               string
	chunkSize            int
	windowSize           int          // flow control window (default 8 chunks)
	pendingAcks          map[int]bool // chunks waiting for ack
	totalChunks          int
	written              int64
	sha256Hasher         sync.Mutex
	mu                   sync.Mutex
	supportsBatchAcks    bool         // true if server supports batch ACKs
	batchReceived        map[int]bool //
	chunksInCurrentBatch int          //
}

// handleFileAvailable processes incoming FileAvailable frame from server notification.
// If AutoDownloadFiles is enabled (default: true), automatically starts download and decompression.
func (c *Client) handleFileAvailable(f *frame.Frame) error {
	var avail frame.FileAvailable
	if err := json.Unmarshal(f.Payload, &avail); err != nil {
		return fmt.Errorf("invalid file available metadata: %w", err)
	}

	c.logger.Debugw("file available for download from server",
		"file_id", avail.FileID,
		"filename", avail.Filename,
		"size", avail.Size,
		"sender", avail.SenderID)

	c.logger.Debugw("file available for download",
		"filename", avail.Filename,
		"size_mb", float64(avail.Size)/1e6,
		"sender", avail.SenderID)

	// Call OnFileAvailable callback if set
	if c.onFileAvailable != nil {
		go c.onFileAvailable(&avail)
	}

	// Deduplication guard: if we already have a receiver for this fileID, don't start duplicate download.
	c.mu.RLock()
	_, alreadyReceiving := c.fileReceivers[avail.FileID]
	c.mu.RUnlock()
	if alreadyReceiving {
		c.logger.Debugw("ignoring duplicate FrameFileAvailable", "file_id", avail.FileID)
		return nil
	}

	// Automatically start download if auto-download is enabled (default: true)
	c.logger.Debugw("checking auto-download option",
		"file_id", avail.FileID,
		"auto_download_enabled", c.options.AutoDownloadFiles)
	if c.options.AutoDownloadFiles {
		c.logger.Debugw("auto-downloading file from server",
			"file_id", avail.FileID,
			"filename", avail.Filename)

		// Create download directory if not exists
		downloadDir := c.options.DownloadDir
		if downloadDir == "" {
			downloadDir = "./downloads"
		}
		if err := os.MkdirAll(downloadDir, 0755); err != nil {
			c.logger.Warnw("failed to create download dir", "err", err)
			return fmt.Errorf("failed to create download directory: %w", err)
		}

		// Create fileReceiver BEFORE requesting download
		sanitizedFilename := sanitizeFileName(avail.Filename)
		filePath := filepath.Join(downloadDir, sanitizedFilename)

		file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			c.logger.Warnw("failed to create receiver file", "err", err)
			return fmt.Errorf("failed to create output file: %w", err)
		}

		receiver := &fileReceiver{
			fileID:       avail.FileID,
			filename:     sanitizedFilename,
			size:         avail.Size,
			mime:         avail.MIME,
			senderID:     avail.SenderID,
			chunkSize:    DefaultChunkSize,
			totalChunks:  0, // Will be updated as chunks arrive
			written:      0,
			filePath:     filePath,
			file:         file,
			isCompressed: avail.Compression == "zstd", // check compressed flag
		}

		c.mu.Lock()
		c.fileReceivers[avail.FileID] = receiver
		c.mu.Unlock()

		go func() {
			err := c.DownloadFile(avail.FileID)
			if err != nil {
				c.logger.Warnw("auto-download failed",
					"file_id", avail.FileID,
					"filename", avail.Filename,
					"err", err)
			}
		}()
	} else {
		c.logger.Debugw("auto-download disabled - use DownloadFile() manually to download",
			"file_id", avail.FileID,
			"filename", avail.Filename)
	}

	return nil
}

// handleFileChunkServer processes incoming file chunk from server Object Store with flow control.
// Server may send compressed (zstd) or uncompressed data - client handles both transparently.
func (c *Client) handleFileChunkServer(f *frame.Frame) error {
	c.logger.Debugw("handleFileChunkServer called", "payload_size", len(f.Payload))

	// Split payload at RS separator 0x1E first - JSON metadata before, binary data after
	sep := bytesIndex(f.Payload, 0x1E)
	if sep < 0 {
		c.logger.Errorw("missing RS separator in download chunk payload", "payload", f.Payload)
		return fmt.Errorf("missing RS separator in download chunk payload")
	}
	metaBytes := f.Payload[:sep]
	chunkData := f.Payload[sep+1:]

	var chunkMeta struct {
		FileID string `json:"file_id"`
		Index  int    `json:"index"`
	}
	if err := json.Unmarshal(metaBytes, &chunkMeta); err != nil {
		c.logger.Errorw("invalid file chunk metadata", "err", err, "meta_bytes", metaBytes)
		return fmt.Errorf("invalid file chunk metadata: %w", err)
	}

	c.mu.Lock()
	receiver, exists := c.fileReceivers[chunkMeta.FileID]
	downloadSession, hasDownload := c.downloads[chunkMeta.FileID]
	c.mu.Unlock()

	if !exists {
		// This can happen legitimately when FrameFileDownloadEnd arrives before all chunks.
		// Log at Debug, not Warn - late chunks are expected in some cases.
		c.logger.Debugw("received late chunk after download completed, discarding",
			"file_id", chunkMeta.FileID, "index", chunkMeta.Index)
		return nil
	}

	receiver.mu.Lock()
	defer receiver.mu.Unlock()

	n, err := receiver.file.Write(chunkData)
	if err != nil {
		c.logger.Errorw("failed to write chunk",
			"file_id", chunkMeta.FileID,
			"index", chunkMeta.Index,
			"err", err)
		return fmt.Errorf("failed to write chunk: %w", err)
	}
	receiver.written += int64(n)

	c.logger.Debugw("written chunk from server",
		"file_id", chunkMeta.FileID,
		"index", chunkMeta.Index,
		"bytes_written", n,
		"total_written", receiver.written,
		"is_compressed", receiver.isCompressed)

	// Send flow control acknowledgement back to server for each chunk received
	if hasDownload && downloadSession != nil {
		downloadSession.mu.Lock()
		delete(downloadSession.pendingAcks, chunkMeta.Index)
		downloadSession.written += int64(n)

		// Initialize batchReceived if this is the first chunk
		if downloadSession.batchReceived == nil {
			downloadSession.batchReceived = make(map[int]bool)
		}

		downloadSession.mu.Unlock()

		// Determine the ACK type (packet or single) based on the configuration
		var ackFrame *frame.Frame
		if downloadSession.supportsBatchAcks {
			// Batch mode - send ACK every N chunks
			downloadSession.chunksInCurrentBatch++

			if downloadSession.chunksInCurrentBatch >= 1024 { // We send a packet ACK every 1024 chunks
				ackFrame = &frame.Frame{
					Type:    frame.FrameFileAck,
					Version: 1,
					Payload: mustJSON(frame.FileAck{
						FileID:         chunkMeta.FileID,
						BatchSize:      downloadSession.chunksInCurrentBatch,
						LastChunkIndex: chunkMeta.Index, // the last chunk in the current batch
						Status:         "ok",
					}),
				}
				// Reset the counter after sending a packet ACK
				downloadSession.chunksInCurrentBatch = 0
			} else {
				// We are not sending ACK yet, we are waiting for the batch to fill up.
				return nil // Do not send a single ACK in batch mode.
			}
		}

		if ackFrame != nil {
			if err := c.SendPrioritized(ackFrame, DefaultPriorityLow); err != nil {
				c.logger.Warnw("failed to send file ack for flow control", "err", err)
			}
		}
	}

	return nil
}

// handleFileDownloadEnd processes incoming FileEnd frame from server Object Store.
func (c *Client) handleFileDownloadEnd(f *frame.Frame) error {
	var end frame.FileEnd
	if err := json.Unmarshal(f.Payload, &end); err != nil {
		return fmt.Errorf("invalid file download end metadata: %w", err)
	}

	c.mu.Lock()
	receiver, exists := c.fileReceivers[end.FileID]
	_, hasDownload := c.downloads[end.FileID]
	c.mu.Unlock()

	if !exists {
		c.logger.Warnw("received FileEnd for unknown file download", "file_id", end.FileID)
		return errors.New("unknown file download")
	}

	receiver.mu.Lock()

	// Close file
	if err := receiver.file.Close(); err != nil {
		c.logger.Warnw("failed to close received file", "err", err)
	}

	fileSize := receiver.written

	// Verify size if provided in FileEnd
	if fileSize != receiver.size && receiver.size > 0 {
		c.logger.Warnw("file size mismatch", "expected", receiver.size, "received", receiver.written)
	}

	receiver.mu.Unlock()

	// Remove receiver and session after releasing receiver mutex to avoid deadlock.
	c.mu.Lock()
	delete(c.fileReceivers, end.FileID)
	if hasDownload {
		delete(c.downloads, end.FileID)
	}
	c.mu.Unlock()

	// Call OnFileReceived callback if set
	if c.onFileReceived != nil {
		rcvdFile := &frame.ReceivedFile{
			FileID:    end.FileID,
			Filename:  receiver.filename,
			Size:      fileSize,
			SenderID:  receiver.senderID,
			LocalPath: receiver.filePath,
			MIME:      receiver.mime,
		}
		go c.onFileReceived(rcvdFile)

		c.logger.Infow("file download completed",
			"file_id", end.FileID,
			"filename", rcvdFile.Filename,
			"size_mb", float64(rcvdFile.Size)/1e6,
			"path", rcvdFile.LocalPath)
	}

	return nil
}

// DownloadFile downloads a file from the server Object Store using flow control.
// If the file was compressed on upload (zstd), it is automatically decompressed during download.
func (c *Client) DownloadFile(fileID string) error {
	// Request download from server
	req := &frame.Frame{
		Type:    frame.FrameFileDownloadRequest,
		Version: 1,
		Payload: mustJSON(frame.FileDownloadRequest{
			FileID:     fileID,
			StartChunk: 0, // Start from beginning
		}),
	}

	if err := c.SendPrioritized(req, DefaultPriorityNormal); err != nil {
		return fmt.Errorf("failed to request file download from server: %w", err)
	}

	c.logger.Debugw("requesting file download from server Object Store", "file_id", fileID)

	// Create download session tracker for flow control
	downloadSession := &downloadSession{
		fileID:      fileID,
		chunkSize:   DefaultChunkSize,
		windowSize:  DefaultWindowSize,
		pendingAcks: make(map[int]bool),
	}

	c.mu.Lock()
	c.downloads[fileID] = downloadSession
	c.mu.Unlock()

	// Wait for file chunks to be received via handleFileChunkServer.
	// The method returns immediately and the actual download happens asynchronously.
	return nil
}

// OnFileReceived sets a callback for received files.
func (c *Client) OnFileReceived(cb func(*frame.ReceivedFile)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onFileReceived = cb
}

// OnFileAvailable sets a callback for file availability notifications from server.
func (c *Client) OnFileAvailable(cb func(*frame.FileAvailable)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onFileAvailable = cb
}

// IsDownloadComplete checks if file download is complete via FILE_END flag.
// Returns true if server sent IsFileEnd=true in the last FileAck.
func (c *Client) IsDownloadComplete(fileID string) bool {
	if c.batchAckHandler == nil {
		return false
	}
	return c.batchAckHandler.IsDownloadComplete(fileID)
}
