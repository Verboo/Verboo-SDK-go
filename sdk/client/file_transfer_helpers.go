package client

import (
	"encoding/json"
	"strings"
)

// sanitizeFileName replaces unsafe characters with underscore
func sanitizeFileName(name string) string {
	sanitized := strings.ReplaceAll(name, "\\", "_")
	sanitized = strings.ReplaceAll(sanitized, "/", "_")
	sanitized = strings.ReplaceAll(sanitized, ":", "_")
	sanitized = strings.ReplaceAll(sanitized, "*", "_")
	sanitized = strings.ReplaceAll(sanitized, "?", "_")
	sanitized = strings.ReplaceAll(sanitized, "\"", "_")
	sanitized = strings.ReplaceAll(sanitized, "<", "_")
	sanitized = strings.ReplaceAll(sanitized, ">", "_")
	sanitized = strings.ReplaceAll(sanitized, "|", "_")
	return sanitized
}

// bytesIndex finds index of byte in slice (optimized version)
func bytesIndex(data []byte, b byte) int {
	for i := 0; i < len(data); i++ {
		if data[i] == b {
			return i
		}
	}
	return -1
}

// mustJSON marshals to JSON and returns []byte (error ignored for internal use)
func mustJSON(v interface{}) []byte {
	data, _ := json.Marshal(v)
	return data
}
