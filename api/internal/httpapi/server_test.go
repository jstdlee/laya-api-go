package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"laya-api/internal/contract"
	"laya-api/internal/rpc"
)

type fakePredictor struct {
	result contract.Result
	err    error
	calls  int
}

func (f *fakePredictor) Predict(context.Context, contract.Request) (contract.Result, error) {
	f.calls++
	return f.result, f.err
}

type fakeReadyPredictor struct {
	fakePredictor
	readyErr error
}

func (f *fakeReadyPredictor) Ready(context.Context) error { return f.readyErr }

func TestSystemOneReturnsAllPrimitiveShapes(t *testing.T) {
	predictor := &fakePredictor{result: contract.Result{
		Model: "english",
		Answers: map[string]contract.Answer{
			"department": {"type": "choice", "choice": "billing", "probabilities": map[string]float64{"billing": 0.9, "technical": 0.1}, "confidence": 0.8},
			"urgency":    {"type": "score", "score": 1.7, "legend": map[string]any{"0": "not urgent", "1": "soon", "2": "critical"}, "probabilities": map[string]float64{"0": 0.1, "1": 0.2, "2": 0.7}, "confidence": 0.4},
			"churn":      {"type": "noul", "noul": 0.88, "confidence": 0.76},
		},
		Usage: contract.Usage{InputTokens: 42},
	}}
	server := httptest.NewServer(NewServer(predictor, "", 1<<20, time.Second))
	defer server.Close()

	body := `{"state":{"body":"charged twice"},"questions":{"department":{"type":"choice","instructions":"Which team?","criteria":{"billing":"payments","technical":null}},"urgency":{"type":"score","instructions":"How urgent?","criteria":["not urgent","soon","critical"]},"churn":{"type":"noul","instructions":"Will they leave?"}}}`
	response, err := http.Post(server.URL+"/v1/systemone", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	answers := result["answers"].(map[string]any)
	choice := answers["department"].(map[string]any)
	if choice["type"] != "choice" || choice["choice"] != "billing" || choice["confidence"] != 0.8 {
		t.Fatalf("choice = %#v", choice)
	}
	score := answers["urgency"].(map[string]any)
	if score["type"] != "score" || score["score"] != 1.7 {
		t.Fatalf("score = %#v", score)
	}
	noul := answers["churn"].(map[string]any)
	if noul["type"] != "noul" || noul["noul"] != 0.88 {
		t.Fatalf("noul = %#v", noul)
	}
	if predictor.calls != 1 {
		t.Fatalf("predict calls = %d", predictor.calls)
	}
}

func TestHealthAndRoutingErrors(t *testing.T) {
	predictor := &fakeReadyPredictor{readyErr: nil}
	server := httptest.NewServer(NewServer(predictor, "", 1<<20, time.Second))
	defer server.Close()

	response, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}

	response, err = http.Get(server.URL + "/v1/systemone")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", response.StatusCode)
	}

	response, err = http.Post(server.URL+"/missing", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown status = %d", response.StatusCode)
	}
}

func TestCORSPreflight(t *testing.T) {
	server := httptest.NewServer(NewServer(&fakePredictor{}, "", 1<<20, time.Second))
	defer server.Close()

	req, err := http.NewRequest(http.MethodOptions, server.URL+"/v1/systemone", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://127.0.0.1:8011")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow-origin = %q", got)
	}
	if got := response.Header.Get("Access-Control-Allow-Headers"); got != "Content-Type, Authorization" {
		t.Fatalf("allow-headers = %q", got)
	}
}

func TestHealthReportsWorkerUnavailable(t *testing.T) {
	predictor := &fakeReadyPredictor{readyErr: &rpc.RPCError{Status: 503, Type: "worker_unavailable", Message: "not ready"}}
	server := httptest.NewServer(NewServer(predictor, "", 1<<20, time.Second))
	defer server.Close()
	response, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d", response.StatusCode)
	}
}

func TestSystemOneValidationAuthAndBodyErrors(t *testing.T) {
	predictor := &fakePredictor{}
	server := httptest.NewServer(NewServer(predictor, "secret", 1<<20, time.Second))
	defer server.Close()

	for name, body := range map[string]string{
		"malformed":         `{"state":`,
		"missing questions": `{"state":"x"}`,
		"bad choice":        `{"state":"x","questions":{"q":{"type":"choice","criteria":{}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/systemone", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer secret")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d", response.StatusCode)
			}
		})
	}

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/systemone", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d", response.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/systemone", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	response, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong auth status = %d", response.StatusCode)
	}

	smallServer := httptest.NewServer(NewServer(predictor, "", 32, time.Second))
	defer smallServer.Close()
	req, _ = http.NewRequest(http.MethodPost, smallServer.URL+"/v1/systemone", strings.NewReader(strings.Repeat("x", 100)))
	response, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body limit status = %d", response.StatusCode)
	}
}

func TestSystemOneMapsWorkerErrors(t *testing.T) {
	for _, want := range []int{502, 503, 504} {
		t.Run(string(rune('0'+want%10)), func(t *testing.T) {
			predictor := &fakePredictor{err: &rpc.RPCError{Status: want, Type: "worker_error", Message: "worker failed"}}
			server := httptest.NewServer(NewServer(predictor, "", 1<<20, time.Second))
			defer server.Close()
			body := `{"state":"x","questions":{"q":{"type":"noul"}}}`
			response, err := http.Post(server.URL+"/v1/systemone", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != want {
				t.Fatalf("status = %d, want %d", response.StatusCode, want)
			}
		})
	}
}
