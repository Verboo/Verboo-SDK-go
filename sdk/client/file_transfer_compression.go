package client

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// determineMIMEType determines MIME type from file extension
func determineMIMEType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	case ".js":
		return "application/javascript"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".zip":
		return "application/zip"
	case ".tar", ".gz":
		return "application/x-tar"
	default:
		return "application/octet-stream"
	}
}

// decideCompression returns (shouldCompress, method) based on MIME type and heuristic test.
// Returns true with "zstd" if compression ratio is expected to be < 0.85 (>=15% saving).
func (c *Client) decideCompression(mime string, localPath string, fileSize int64) (bool, string) {
	mime = strings.ToLower(mime)

	// Always compress text-based and common formats that benefit from compression
	switch {
	case strings.HasPrefix(mime, "text/"),
		mime == "application/json",
		mime == "application/xml",
		mime == "application/javascript",
		strings.Contains(mime, "+json"),
		strings.Contains(mime, "+xml"):
		return true, "zstd"

	// Never compress already-compressed formats
	case strings.HasPrefix(mime, "image/"),
		strings.HasPrefix(mime, "video/"),
		strings.HasPrefix(mime, "audio/"),
		mime == "application/zip",
		mime == "application/gzip",
		mime == "application/x-tar":
		return false, ""
	}

	// For unknown MIME types or application/octet-stream:
	// Use heuristic on first 64KB to estimate compression ratio
	if fileSize < int64(DefaultChunkSize) {
		// Small files - always try to compress (overhead is minimal)
		return true, "zstd"
	}

	// Read first chunk for heuristic test
	file, err := os.Open(localPath)
	if err != nil {
		c.logger.Warnw("failed to open file for compression test", "err", err)
		return false, ""
	}
	defer file.Close()

	firstChunk := make([]byte, DefaultChunkSize)
	n, err := io.ReadFull(file, firstChunk)
	if err != nil && !errors.Is(err, io.EOF) {
		c.logger.Warnw("failed to read sample for compression test", "err", err)
		return false, ""
	}
	firstChunk = firstChunk[:n]

	// Test compression ratio on sample
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		c.logger.Warnw("failed to create zstd encoder", "err", err)
		return false, ""
	}
	defer enc.Close()

	compressed := enc.EncodeAll(firstChunk, nil)
	ratio := float64(len(compressed)) / float64(len(firstChunk))

	// Compress if ratio < 0.85 (>=15% space saving expected)
	if ratio < 0.85 {
		return true, "zstd"
	}

	c.logger.Debugw("compression skipped - file not compressible",
		"ratio", fmt.Sprintf("%.2f", ratio))
	return false, ""
}
