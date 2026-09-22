package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"laya-api/internal/contract"
)

type Predictor interface {
	Predict(context.Context, contract.Request) (contract.Result, error)
}

type RPCError struct {
	Status  int
	Type    string
	Message string
	Err     error
}

func (e *RPCError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("worker RPC failed with status %d", e.Status)
	}
	return e.Message
}

func (e *RPCError) Unwrap() error { return e.Err }

type Client struct {
	socketPath string
	timeout    time.Duration

	mu     sync.Mutex
	conn   net.Conn
	nextID atomic.Uint64
	closed bool
}

func NewClient(socketPath string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{socketPath: socketPath, timeout: timeout}
}

func (c *Client) Predict(ctx context.Context, req contract.Request) (contract.Result, error) {
	params, err := json.Marshal(req)
	if err != nil {
		return contract.Result{}, &RPCError{Status: 502, Type: "serialization_error", Message: err.Error(), Err: err}
	}
	payload, err := c.call(ctx, "predict", params)
	if err != nil {
		return contract.Result{}, err
	}
	var result contract.Result
	if err := json.Unmarshal(payload, &result); err != nil {
		return contract.Result{}, &RPCError{Status: 502, Type: "protocol_error", Message: "worker returned an invalid result", Err: err}
	}
	return result, nil
}

func (c *Client) Ready(ctx context.Context) error {
	_, err := c.call(ctx, "ready", json.RawMessage(`{}`))
	return err
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.closeLocked()
}

func (c *Client) call(ctx context.Context, method string, params json.RawMessage) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, &RPCError{Status: 503, Type: "worker_unavailable", Message: "worker client is closed"}
	}

	for attempt := 0; attempt < 2; attempt++ {
		conn, err := c.connectionLocked(ctx)
		if err != nil {
			return nil, transportError(ctx, 503, "worker_unavailable", "worker is unavailable", err)
		}
		request := Envelope{
			ID:     fmt.Sprintf("req-%d", c.nextID.Add(1)),
			Method: method,
			Params: params,
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			return nil, &RPCError{Status: 502, Type: "serialization_error", Message: err.Error(), Err: err}
		}
		if err := conn.SetDeadline(deadline(ctx, c.timeout)); err != nil {
			c.closeLocked()
			return nil, &RPCError{Status: 503, Type: "worker_unavailable", Message: "worker connection is unavailable", Err: err}
		}
		if err := WriteFrame(conn, encoded); err != nil {
			c.closeLocked()
			if attempt == 0 {
				continue
			}
			return nil, transportError(ctx, 503, "worker_unavailable", "worker connection failed", err)
		}
		responseBytes, err := ReadFrame(conn, MaxFrameSize)
		if err != nil {
			c.closeLocked()
			if isContextDone(ctx, err) {
				return nil, transportError(ctx, 504, "worker_timeout", "worker request timed out", err)
			}
			if attempt == 0 {
				continue
			}
			return nil, transportError(ctx, 503, "worker_unavailable", "worker connection failed", err)
		}

		var response Envelope
		if err := json.Unmarshal(responseBytes, &response); err != nil {
			c.closeLocked()
			return nil, &RPCError{Status: 502, Type: "protocol_error", Message: "worker returned invalid RPC", Err: err}
		}
		if response.ID != request.ID {
			c.closeLocked()
			return nil, &RPCError{Status: 502, Type: "protocol_error", Message: "worker response ID did not match request"}
		}
		if !response.OK {
			if response.Error == nil {
				return nil, &RPCError{Status: 502, Type: "worker_error", Message: "worker returned an unspecified error"}
			}
			status := response.Error.Status
			if status == 0 {
				status = 502
			}
			return nil, &RPCError{Status: status, Type: response.Error.Type, Message: response.Error.Message}
		}
		return response.Result, nil
	}
	return nil, &RPCError{Status: 503, Type: "worker_unavailable", Message: "worker request failed"}
}

func (c *Client) connectionLocked(ctx context.Context) (net.Conn, error) {
	if c.conn != nil {
		return c.conn, nil
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return conn, nil
}

func (c *Client) closeLocked() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func deadline(ctx context.Context, fallback time.Duration) time.Time {
	if value, ok := ctx.Deadline(); ok {
		return value
	}
	return time.Now().Add(fallback)
}

func isContextDone(ctx context.Context, err error) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || isTimeoutError(err)
}

func transportError(ctx context.Context, status int, kind, message string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeoutError(err) {
		status = 504
		kind = "worker_timeout"
		message = "worker request timed out"
	}
	return &RPCError{Status: status, Type: kind, Message: message, Err: err}
}

func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
