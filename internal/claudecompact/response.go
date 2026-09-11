package claudecompact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
)

// MarkerSSE returns a valid Anthropic message stream containing only the opaque marker.
func MarkerSSE(marker Marker) []byte {
	messageID := randomMessageID()
	start := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": messageID, "type": "message", "role": "assistant", "content": []any{},
			"model": "claude-code-compact", "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}
	blockStart := map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}
	delta := map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": marker.String()}}
	blockStop := map[string]any{"type": "content_block_stop", "index": 0}
	messageDelta := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}}
	messageStop := map[string]any{"type": "message_stop"}
	var out []byte
	for _, event := range []struct {
		name    string
		payload any
	}{
		{"message_start", start}, {"content_block_start", blockStart}, {"content_block_delta", delta},
		{"content_block_stop", blockStop}, {"message_delta", messageDelta}, {"message_stop", messageStop},
	} {
		encoded, _ := json.Marshal(event.payload)
		out = append(out, []byte("event: "+event.name+"\ndata: ")...)
		out = append(out, encoded...)
		out = append(out, '\n', '\n')
	}
	return out
}

// WriteMarkerSSE writes the local stream and stops promptly when the request is cancelled.
func WriteMarkerSSE(ctx context.Context, writer io.Writer, marker Marker) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if writer == nil {
		return fmtError(ErrProtocolError, 500, "compact response writer is unavailable")
	}
	data := MarkerSSE(marker)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	_, err := writer.Write(data)
	return err
}

func randomMessageID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "msg_compact"
	}
	return "msg_compact_" + hex.EncodeToString(value[:])
}
