package backend

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeBoundedJSON(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		var got struct {
			Status string `json:"status"`
		}
		if err := decodeBoundedJSON(strings.NewReader(`{"status":"success"}`), &got); err != nil || got.Status != "success" {
		    t.Fatalf("decode = %q, %v", got.Status, err)
		}
	})
	t.Run("exact_limit", func(t *testing.T) {
		// JSON string including its quotes fits exactly.
		payload := "\"" + strings.Repeat("a", maxSuccessBodyBytes-2) + "\""
		var got string
		if err := decodeBoundedJSON(strings.NewReader(payload), &got); err != nil {
		    t.Fatalf("exact limit rejected: %v", err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		var got any
		err := decodeBoundedJSON(bytes.NewReader(bytes.Repeat([]byte("x"), maxSuccessBodyBytes+1)), &got)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
		    t.Fatalf("expected size error, got %v", err)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		var got json.RawMessage
		if err := decodeBoundedJSON(strings.NewReader("{"), &got); err == nil {
		    t.Fatal("expected JSON error")
		}
	})
}
