package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"laya-api/internal/contract"
)

func TestClientPredictRoundTrip(t *testing.T) {
	listener, path := testUnixListener(t)
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		payload, err := ReadFrame(conn, MaxFrameSize)
		if err != nil {
			done <- err
			return
		}
		var request Envelope
		if err := json.Unmarshal(payload, &request); err != nil {
			done <- err
			return
		}
		if request.Method != "predict" || request.ID == "" || len(request.Params) == 0 {
			done <- errors.New("invalid predict envelope")
			return
		}
		result, _ := json.Marshal(contract.Result{
			Model:   "english",
			Answers: map[string]contract.Answer{"department": {"type": "choice", "choice": "billing"}},
		})
		response, _ := json.Marshal(Envelope{ID: request.ID, OK: true, Result: result})
		done <- WriteFrame(conn, response)
	}()

	client := NewClient(path, time.Second)
	defer client.Close()
	got, err := client.Predict(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "english" || got.Answers["department"]["choice"] != "billing" {
		t.Fatalf("result = %#v", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClientReady(t *testing.T) {
	listener, path := testUnixListener(t)
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		payload, err := ReadFrame(conn, MaxFrameSize)
		if err != nil {
			done <- err
			return
		}
		var request Envelope
		if err := json.Unmarshal(payload, &request); err != nil {
			done <- err
			return
		}
		if request.Method != "ready" {
			done <- errors.New("invalid ready envelope")
			return
		}
		response, _ := json.Marshal(Envelope{ID: request.ID, OK: true})
		done <- WriteFrame(conn, response)
	}()

	client := NewClient(path, time.Second)
	defer client.Close()
	if err := client.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClientMapsRemoteError(t *testing.T) {
	listener, path := testUnixListener(t)
	defer listener.Close()
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		payload, err := ReadFrame(conn, MaxFrameSize)
		if err != nil {
			return
		}
		var request Envelope
		if json.Unmarshal(payload, &request) != nil {
			return
		}
		response, _ := json.Marshal(Envelope{ID: request.ID, Error: &RemoteError{Type: "validation_error", Status: 422, Message: "bad question"}})
		_ = WriteFrame(conn, response)
	}()

	client := NewClient(path, time.Second)
	defer client.Close()
	_, err := client.Predict(context.Background(), testRequest())
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Status != 422 || rpcErr.Type != "validation_error" {
		t.Fatalf("error = %#v, want validation RPCError", err)
	}
}

func TestClientMapsTimeout(t *testing.T) {
	listener, path := testUnixListener(t)
	defer listener.Close()
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = ReadFrame(conn, MaxFrameSize)
		time.Sleep(100 * time.Millisecond)
	}()

	client := NewClient(path, 20*time.Millisecond)
	defer client.Close()
	_, err := client.Predict(context.Background(), testRequest())
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Status != 504 {
		t.Fatalf("error = %#v, want timeout RPCError", err)
	}
}

func TestClientReconnectsAfterDisconnect(t *testing.T) {
	listener, path := testUnixListener(t)
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		for attempt := 0; attempt < 2; attempt++ {
			conn, err := listener.AcceptUnix()
			if err != nil {
				done <- err
				return
			}
			payload, err := ReadFrame(conn, MaxFrameSize)
			if err != nil {
				conn.Close()
				done <- err
				return
			}
			var request Envelope
			if err := json.Unmarshal(payload, &request); err != nil {
				conn.Close()
				done <- err
				return
			}
			if attempt == 0 {
				conn.Close()
				continue
			}
			result, _ := json.Marshal(contract.Result{Model: "english"})
			response, _ := json.Marshal(Envelope{ID: request.ID, OK: true, Result: result})
			done <- WriteFrame(conn, response)
			conn.Close()
			return
		}
		done <- errors.New("client did not reconnect")
	}()

	client := NewClient(path, time.Second)
	defer client.Close()
	if _, err := client.Predict(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func testUnixListener(t *testing.T) (*net.UnixListener, string) {
	t.Helper()
	path := t.TempDir() + "/worker.sock"
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	return listener, path
}

func testRequest() contract.Request {
	return contract.Request{
		Model: "auto",
		State: json.RawMessage(`{"body":"charged twice"}`),
		Questions: []contract.Question{{
			ID: "department", Type: "choice", Instructions: "Which team?",
			Options: []contract.Option{{Name: "billing"}, {Name: "technical"}},
		}},
	}
}
