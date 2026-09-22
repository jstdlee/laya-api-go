package rpc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	want := []byte("{\"text\":\"line one\\nline two — 🙂\"}")
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buffer, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func TestFrameUsesBigEndianLength(t *testing.T) {
	var buffer bytes.Buffer
	payload := []byte("hello")
	if err := WriteFrame(&buffer, payload); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(&buffer, header); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(header); got != uint32(len(payload)) {
		t.Fatalf("length = %d, want %d", got, len(payload))
	}
}

func TestFrameRejectsEmptyPayload(t *testing.T) {
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, nil); err == nil {
		t.Fatal("expected empty payload error")
	}
}

func TestFrameRejectsOversizedPayload(t *testing.T) {
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, []byte("123456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrame(&buffer, 8); err == nil {
		t.Fatal("expected size-limit error")
	}
}

func TestFrameRejectsTruncatedPayload(t *testing.T) {
	var buffer bytes.Buffer
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, 10)
	buffer.Write(header)
	buffer.WriteString("short")
	if _, err := ReadFrame(&buffer, 100); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
}

func TestFrameWorksOverPipe(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	want := []byte("payload with\nnewlines and unicode: 你好")
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- WriteFrame(client, want)
	}()
	got, err := ReadFrame(server, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}
