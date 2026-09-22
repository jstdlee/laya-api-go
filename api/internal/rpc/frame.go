package rpc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const MaxFrameSize = 8 << 20

type RemoteError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Status  int    `json:"status,omitempty"`
}

type Envelope struct {
	ID     string          `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RemoteError    `json:"error,omitempty"`
}

func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return errors.New("rpc frame payload is empty")
	}
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("rpc frame payload is %d bytes, maximum is %d", len(payload), MaxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, payload)
}

func ReadFrame(r io.Reader, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > MaxFrameSize {
		maxBytes = MaxFrameSize
	}
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint32(header[:]))
	if size == 0 {
		return nil, errors.New("rpc frame payload is empty")
	}
	if size > maxBytes {
		return nil, fmt.Errorf("rpc frame payload is %d bytes, maximum is %d", size, maxBytes)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func writeAll(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if n > 0 {
			payload = payload[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
