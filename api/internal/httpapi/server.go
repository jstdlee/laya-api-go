package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"laya-api/internal/contract"
	"laya-api/internal/rpc"
)

const defaultRequestLimit = 8 << 20

type Server struct {
	predictor    rpc.Predictor
	apiKey       string
	requestLimit int64
	timeout      time.Duration
}

func NewServer(predictor rpc.Predictor, apiKey string, requestLimit int, timeout time.Duration) http.Handler {
	if requestLimit <= 0 {
		requestLimit = defaultRequestLimit
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Server{predictor: predictor, apiKey: apiKey, requestLimit: int64(requestLimit), timeout: timeout}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		s.health(w, r)
	case "/v1/systemone":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		s.systemOne(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown route")
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if ready, ok := s.predictor.(interface{ Ready(context.Context) error }); ok {
		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		defer cancel()
		if err := ready.Ready(ctx); err != nil {
			writePredictorError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) systemOne(w http.ResponseWriter, r *http.Request) {
	if s.apiKey != "" && !authorized(r.Header.Get("Authorization"), s.apiKey) {
		writeError(w, http.StatusUnauthorized, "authentication_error", "missing or wrong bearer token")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.requestLimit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "invalid_request_error", "unable to read request body")
		return
	}

	req, err := contract.ValidateAndNormalize(raw)
	if err != nil {
		writeContractError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	result, err := s.predictor.Predict(ctx, req)
	if err != nil {
		writePredictorError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func authorized(header, expected string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := []byte(strings.TrimSpace(strings.TrimPrefix(header, prefix)))
	want := []byte(expected)
	return len(provided) == len(want) && subtle.ConstantTimeCompare(provided, want) == 1
}

func writeContractError(w http.ResponseWriter, err error) {
	var validation contract.ValidationError
	if errors.As(err, &validation) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", validation.Message)
		return
	}
	writeError(w, http.StatusUnprocessableEntity, "invalid_request_error", "invalid request")
}

func writePredictorError(w http.ResponseWriter, err error) {
	var rpcErr *rpc.RPCError
	if errors.As(err, &rpcErr) {
		status := rpcErr.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		typ := rpcErr.Type
		if typ == "" {
			typ = "server_error"
		}
		writeError(w, status, typ, rpcErr.Message)
		return
	}
	writeError(w, http.StatusBadGateway, "server_error", "worker request failed")
}

func writeError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": typ},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"message":"failed to encode response","type":"server_error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
