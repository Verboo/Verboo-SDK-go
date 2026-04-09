package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	"github.com/klauspost/compress/zstd"
)

const (
	VirtualStreamWindowSize = 32 // Number of chunks that can be sent without acks
)

// handleVirtualStreamInit processes incoming virtual stream initialization acknowledgment from server.
func (c *Client) handleVirtualStreamInit(f *frame.Frame) error {
	var ack struct {
		FileID     string `json:"file_id"`
		Status     string `json:"status"`
		ResumeFrom int    `json:"resume_from"`
	}
	if err := json.Unmarshal(f.Payload, &ack); err != nil {
		c.logger.Debugw("failed to unmarshal virtual stream init ack", "err", err)
		return fmt.Errorf("invalid virtual stream initialization ack: %w", err)
	}

	c.logger.Debugw("received virtual stream initialization acknowledgment from server",
		"file_id", ack.FileID,
		"status", ack.Status,
		"resume_from", ack.ResumeFrom)

	if ack.Status != "ready" {
		return fmt.Errorf("virtual stream not ready: status=%s", ack.Status)
	}

	c.mu.Lock()
	session, exists := c.uploadSessions[ack.FileID]
	c.mu.Unlock()

	if !exists {
		c.logger.Warnw("received virtual stream init ack for unknown session", "file_id", ack.FileID)
		return errors.New("unknown upload session")
	}

	// If resuming, update the current index
	if ack.ResumeFrom > 0 {
		session.currentIdx = ack.ResumeFrom
		c.logger.Debugw("resuming virtual stream from chunk",
			"file_id", ack.FileID,
			"chunk_index", ack.ResumeFrom)
	}

	return nil
}

// handleVirtualStreamEnd processes incoming VirtualStreamEnd frame (should only come from server during download).
func (c *Client) handleVirtualStreamEnd(f *frame.Frame) error {
	var end frame.VirtualStreamEnd
	if err := json.Unmarshal(f.Payload, &end); err != nil {
		return fmt.Errorf("invalid virtual stream end metadata: %w", err)
	}

	c.mu.Lock()
	session, exists := c.uploadSessions[end.FileID]
	receiver, hasReceiver := c.fileReceivers[end.FileID]
	c.mu.Unlock()

	if !exists && !hasReceiver {
		c.logger.Warnw("received VirtualStreamEnd for unknown session", "file_id", end.FileID)
		return errors.New("unknown virtual stream session")
	}

	// If this is an upload session, we already completed it
	if exists {
		session.mu.Lock()
		fileSize := session.written
		session.mu.Unlock()

		c.logger.Debugw("virtual stream upload to server completed",
			"file_id", end.FileID,
			"bytes_uploaded", fileSize)
		return nil
	}

	// If this is a download (server → client), process as normal file chunk end
	if hasReceiver {
		receiver.mu.Lock()
		defer receiver.mu.Unlock()

		if err := receiver.file.Close(); err != nil {
			c.logger.Warnw("failed to close received file", "err", err)
		}

		fileSize := receiver.written

		c.mu.Lock()
		delete(c.fileReceivers, end.FileID)
		c.mu.Unlock()

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

			c.logger.Infow("virtual stream download completed",
				"file_id", end.FileID,
				"filename", rcvdFile.Filename,
				"size_mb", float64(rcvdFile.Size)/1e6)
		}
	}

	return nil
}

// SendVirtualStream sends a file to the specified target using virtual stream transfer via server Object Store.
func (c *Client) SendVirtualStream(targetID, localPath string) error {
	// Handle relative paths without extension
	if !filepath.IsAbs(localPath) && !strings.Contains(localPath, ".") {
		localPath = "./" + localPath
	}

	fileInfo, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	fileID := fmt.Sprintf("%s-%d", c.token, time.Now().UnixNano())
	chunkSize := c.options.FileChunkSize
	if chunkSize == 0 {
		chunkSize = DefaultChunkSize
	}

	filename := filepath.Base(localPath)
	sanitizedFilename := sanitizeFileName(filename)

	// Decide compression based on MIME type and heuristic test
	compress, compressionMethod := c.decideCompression(
		determineMIMEType(localPath), localPath, fileInfo.Size())

	c.logger.Debugw("virtual stream compression decision",
		"file", sanitizedFilename,
		"mime", determineMIMEType(localPath),
		"should_compress", compress)

	// Send VirtualStreamInit to server (short initialization frame with metadata only)
	initFrame := &frame.Frame{
		Type:    frame.FrameVirtualStreamInit,
		Version: 1,
		Payload: mustJSON(frame.VirtualStreamInit{
			FileID:      fileID,
			Filename:    sanitizedFilename,
			Size:        fileInfo.Size(),
			MIME:        determineMIMEType(localPath),
			Recipient:   targetID,
			ChunkSize:   chunkSize,
			Compression: compressionMethod,
		}),
	}

	if err := c.SendPrioritized(initFrame, DefaultPriorityLow); err != nil {
		return fmt.Errorf("failed to send virtual stream init: %w", err)
	}

	c.logger.Debugw("virtual stream init sent to server",
		"file_id", fileID,
		"filename", sanitizedFilename,
		"size", fileInfo.Size(),
		"recipient", targetID)

	initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer initCancel()

	uploadCtx, uploadCancel := context.WithCancel(context.Background())
	defer uploadCancel()

	initAckReceived := make(chan struct{})
	go func() {
		c.mu.RLock()
		session, exists := c.uploadSessions[fileID]
		c.mu.RUnlock()

		if !exists {
			close(initAckReceived)
			return
		}

		for {
			select {
			case <-initCtx.Done():
				close(initAckReceived)
				return
			default:
				session.mu.Lock()
				currentIdx := session.currentIdx
				session.mu.Unlock()

				if currentIdx > 0 {
					close(initAckReceived)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()

	select {
	case <-initAckReceived:
		// Continue with upload
	case <-initCtx.Done():
		return fmt.Errorf("virtual stream init timeout")
	}

	// Create upload session tracking structure
	session := &uploadSession{
		fileID:       fileID,
		localPath:    localPath,
		chunkSize:    chunkSize,
		totalChunks:  int((fileInfo.Size() + int64(chunkSize) - 1) / int64(chunkSize)),
		currentIdx:   0,
		sha256Hasher: sha256.New(),
		progressCb:   c.options.FileProgressCallback,
		ackChan:      make(chan struct{}, VirtualStreamWindowSize),
	}

	// Create streaming pipe for compressed upload (if needed)
	var readerForUpload io.Reader = file
	if compress {
		pr, pw := io.Pipe()
		encoder, err := zstd.NewWriter(pw, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			return fmt.Errorf("failed to create zstd encoder: %w", err)
		}
		readerForUpload = pr
		go func() {
			defer pw.Close()
			tee := io.TeeReader(file, session.sha256Hasher)
			if _, err := io.Copy(encoder, tee); err != nil {
				pw.CloseWithError(err)
				return
			}
			encoder.Close()
		}()
	}

	c.mu.Lock()
	c.uploadSessions[fileID] = session
	c.mu.Unlock()

	// Create error channel for upload completion status (separate from session)
	errChan := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Errorw("panic in virtual stream upload", "err", r)
				errChan <- fmt.Errorf("upload panic: %v", r)
			}
		}()

		buf := make([]byte, chunkSize)
		totalChunks := session.totalChunks
		sentCount := 0
		inFlight := 0 // Track chunks in-flight for sliding window backpressure
		windowSize := c.rttManager.windowSize(chunkSize)
		c.logger.Debugw("upload window size", "window", windowSize, "chunk_size", chunkSize)
		c.logger.Debugw("starting virtual stream upload (sliding window)",
			"file_id", fileID,
			"filename", sanitizedFilename,
			"total_chunks", totalChunks,
			"chunk_size_bytes", chunkSize,
			"window_size", VirtualStreamWindowSize)
		ackTimer := time.NewTimer(30 * time.Second)
		defer ackTimer.Stop()
		for {
			select {
			case <-uploadCtx.Done():
				errChan <- uploadCtx.Err()
				return
			default:
			}

			// Read chunk from file/pipe
			n, err := readerForUpload.Read(buf)
			if n > 0 {
				// Update progress tracking
				session.mu.Lock()
				session.written += int64(n)
				if !compress {
					// in case compress=true the hash is calculated in a goroutine compressor (via TeeReader)
					session.sha256Hasher.Write(buf[:n])
				}
				currentIdx := session.currentIdx
				session.mu.Unlock()

				sentCount++

				// Send chunk with sliding window backpressure control
				chunkPayloadJSON := mustJSON(struct {
					FileID string `json:"file_id"`
					Index  int    `json:"index"`
				}{FileID: fileID, Index: currentIdx})

				fullChunkPayload := make([]byte, len(chunkPayloadJSON)+1+n)
				copy(fullChunkPayload, chunkPayloadJSON)
				fullChunkPayload[len(chunkPayloadJSON)] = 0x1E // RS separator
				copy(fullChunkPayload[len(chunkPayloadJSON)+1:], buf[:n])

				chunkFrame := &frame.Frame{
					Type:    frame.FrameVirtualStreamData,
					Version: 1,
					Payload: fullChunkPayload,
				}

				// Sliding window: wait if we have too many chunks in-flight
				// Wait for ack from ackChan (server will send FileAck eventually) or use timeout
				for {
					select {
					case <-uploadCtx.Done():
						errChan <- uploadCtx.Err()
						return
					default:
					}

					// Check if we've exceeded window size - wait for ack before sending more
					if inFlight >= windowSize {
						if !ackTimer.Stop() {
							select {
							case <-ackTimer.C:
							default:
							}
						}
						ackTimer.Reset(30 * time.Second)

						select {
						case <-uploadCtx.Done():
							errChan <- uploadCtx.Err()
							return
						case <-session.ackChan: // Wait for ack (this will be sent by server eventually)
							inFlight--
						case <-ackTimer.C:
							errChan <- fmt.Errorf("ack timeout: server did not acknowledge chunk within 30s (file_id=%s, in_flight=%d)", fileID, inFlight)
							return
						}
					} else {
						// Within window, send immediately
						break
					}
				}

				if err := c.sendPrioritizedBlocking(uploadCtx, chunkFrame, DefaultPriorityLow); err != nil {
					errChan <- fmt.Errorf("failed to send chunk %d: %w", currentIdx, err)
					return
				}
				inFlight++ // Increment in-flight count after successful enqueue

				session.mu.Lock()
				session.currentIdx++
				session.mu.Unlock()

				if sentCount%5 == 0 {
					c.logger.Debugw("virtual stream progress",
						"file_id", fileID,
						"chunks_sent", sentCount,
						"total_chunks", totalChunks,
						"progress_percent", float64(sentCount)/float64(totalChunks)*100)
				}

				if err != nil {
					if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
						break // End of file reached
					}
					c.logger.Errorw("error reading virtual stream data", "err", err)
					errChan <- fmt.Errorf("read error: %w", err)
					return
				}
			} else if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					break // End of file reached
				}
				c.logger.Errorw("error reading virtual stream data", "err", err)
				errChan <- fmt.Errorf("read error: %w", err)
				return
			}
		}

		// Send VirtualStreamEnd with SHA256 of ORIGINAL (uncompressed) file
		computedHash := session.sha256Hasher.Sum(nil)
		endFrame := &frame.Frame{
			Type:    frame.FrameVirtualStreamEnd,
			Version: 1,
			Payload: mustJSON(frame.VirtualStreamEnd{
				FileID:      fileID,
				TotalChunks: sentCount,
				SHA256:      hex.EncodeToString(computedHash),
			}),
		}

		if err := c.SendPrioritized(endFrame, DefaultPriorityLow); err != nil {
			c.logger.Warnw("failed to send virtual stream end", "err", err)
		} else {
			c.logger.Infow("virtual stream upload completed (sliding window)",
				"file_id", fileID,
				"filename", sanitizedFilename,
				"total_chunks", sentCount,
				"bytes_uploaded", session.written,
				"original_size", fileInfo.Size())
		}

		// Clean up session after completion
		c.mu.Lock()
		delete(c.uploadSessions, fileID)
		c.mu.Unlock()

		// Signal success
		errChan <- nil
	}()

	// Wait for upload to complete (with timeout)
	select {
	case err := <-errChan:
		if err != nil {
			return fmt.Errorf("virtual stream upload failed: %w", err)
		}
		c.logger.Debugw("virtual stream upload completed successfully")
		return nil
	case <-uploadCtx.Done():
		return fmt.Errorf("virtual stream upload timeout")
	}
}
