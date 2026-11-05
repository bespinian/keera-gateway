package gateway

import (
	"io"
	"strings"
	"testing"
)

// vllmChunk is a token as vLLM streams it, with the fields it always sends.
const vllmChunk = `{"id":"chatcmpl-8f2c1a9e4b7d","object":"chat.completion.chunk","created":1730000000,` +
	`"model":"served-name","choices":[{"index":0,"delta":{"content":" world"},"logprobs":null,` +
	`"finish_reason":null}],"usage":null}`

// BenchmarkFold is what a translated stream pays per token.
func BenchmarkFold(b *testing.B) {
	payload := []byte(vllmChunk)
	text := func(string) error { return nil }
	tool := func(int, oaiToolCallDelta) error { return nil }
	b.ReportAllocs()
	for b.Loop() {
		var c chatChunks
		if err := c.fold(payload, text, tool); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPipeSSE is a 300-token stream piped as it is.
func BenchmarkPipeSSE(b *testing.B) {
	chunks := make([]string, 0, 302)
	for range 300 {
		chunks = append(chunks, vllmChunk)
	}
	stream := sseStream(append(chunks, stopChunk, usageChunk)...)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := pipeSSE(io.Discard, func() {}, strings.NewReader(stream), "keera-code", true, 1<<20); err != nil {
			b.Fatal(err)
		}
	}
}
