package applyinator

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestGzipByteSliceRoundTrip(t *testing.T) {
	t.Parallel()

	input := []byte(`{"hello":"world"}`)

	gzipped, err := gzipByteSlice(input)
	if err != nil {
		t.Fatalf("gzipByteSlice returned error: %v", err)
	}

	buf, err := generateByteBufferFromBytes(gzipped)
	if err != nil {
		t.Fatalf("generateByteBufferFromBytes returned error: %v", err)
	}

	if !bytes.Equal(buf.Bytes(), input) {
		t.Errorf("expected round-tripped bytes %q, got %q", input, buf.Bytes())
	}
}

func TestGenerateByteBufferFromBytesInvalidGzip(t *testing.T) {
	t.Parallel()

	if _, err := generateByteBufferFromBytes([]byte("not gzip data")); err == nil {
		t.Error("expected error decoding non-gzip data, got nil")
	}
}

func TestStreamLogs(t *testing.T) {
	t.Parallel()

	reader := strings.NewReader("line one\nline two\n")
	var outputBuffer bytes.Buffer
	lock := &sync.Mutex{}

	if err := streamLogs("[test]", &outputBuffer, reader, lock); err != nil {
		t.Fatalf("streamLogs returned error: %v", err)
	}

	expected := "line one\nline two\n"
	if outputBuffer.String() != expected {
		t.Errorf("expected buffer %q, got %q", expected, outputBuffer.String())
	}
}
