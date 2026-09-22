# Laya API

`laya-api` is a small Go HTTP front end for the Python Laya runtime. Go owns the public REST
listener and request validation; one persistent Python worker owns the loaded `laya.Router` and
communicates over a private length-prefixed JSON RPC Unix socket.

The worker uses Laya's `transformers`/PyTorch runtime and safetensor checkpoints. It does not use
vLLM. With the default preload list, the English and multilingual checkpoints are loaded once at
startup and remain resident for the life of the worker.

## Build and run

Build without a system Go runtime dependency:

```bash
cd ~/dev/laya/api
CGO_ENABLED=0 ../.tools/go1.27.1/bin/go build -trimpath -ldflags='-s -w' -o laya-api ./cmd/laya-api
```

Start the API and its worker from the project root:

```bash
cd ~/dev/laya
api/laya-api --host 127.0.0.1 --port 8011
```

The API starts listening immediately. During model download/preload, `GET /health` returns `503`;
it changes to `200` after the worker is ready. The default worker command is equivalent to:

```bash
uv run --project ~/dev/laya python ~/dev/laya/worker/laya_worker.py \
  --socket /tmp/laya-api.sock --preload english,multilingual
```

Useful flags:

```text
--host HOST                 HTTP bind host (default 127.0.0.1)
--port PORT                 HTTP bind port (default 8011)
--worker-socket PATH        private Unix socket (default /tmp/laya-api.sock)
--project-dir PATH          uv project directory
--worker-script PATH        Python worker path
--uv PATH                   uv executable (default uv)
--preload NAMES             comma-separated models (default english,multilingual)
--device DEVICE             PyTorch device, for example cuda or cpu
--api-key KEY               require Authorization: Bearer KEY
--worker-timeout DURATION   request timeout (default 60s)
--no-spawn-worker           connect to an already-running worker
```

To run the worker separately, start it with `uv` from the Laya project and point the API at the
same socket:

```bash
uv run --project ~/dev/laya python ~/dev/laya/worker/laya_worker.py \
  --socket /tmp/laya-worker.sock --preload english,multilingual
api/laya-api --worker-socket /tmp/laya-worker.sock --no-spawn-worker
```

## REST endpoint

`POST http://HOST:PORT/v1/systemone` accepts the djev/TypeSafe-style request envelope. `state`
may be a string, object, or array. Question order is preserved.

```json
{
  "model": "auto",
  "state": {"body": "I was charged twice; please refund the duplicate."},
  "questions": {
    "department": {
      "type": "choice",
      "instructions": "Which team should handle this?",
      "criteria": {
        "billing": "payments and refunds",
        "technical": "bugs and outages"
      }
    },
    "urgency": {
      "type": "score",
      "instructions": "How urgent is this?",
      "criteria": ["not urgent", "soon", "critical"]
    },
    "churn_risk": {
      "type": "noul",
      "instructions": "Does the user threaten to leave?"
    }
  }
}
```

The response contains `model`, `answers`, `usage`, and additive `routing` metadata. Internal Laya
`action` metadata is removed from answers. Choice answers include `choice` and probabilities,
score answers include `score`, and noul answers include `noul`.

```bash
curl -sS http://127.0.0.1:8011/health
curl -sS -X POST http://127.0.0.1:8011/v1/systemone \
  -H 'Content-Type: application/json' \
  --data @request.json | jq .
```

When configured with `--api-key`, add:

```bash
-H 'Authorization: Bearer YOUR_KEY'
```

Malformed schemas and unsupported vLLM/chat extensions are rejected by the Go layer with HTTP
`422`. Worker unavailability is `503`, timeouts are `504`, and inference/protocol failures are
`502`.

## Verification

Run the live model-backed smoke test from `api/`:

```bash
./smoke.sh
```

It starts the API on a temporary socket, waits for readiness, sends English and Hindi requests,
checks `choice`, `score`, `noul`, and verifies `routing.model` is `english` then `multilingual`.

## Typed-decisions benchmark result

The Go benchmark client at `cmd/laya-bench` was run against the local Go API using the public
[`LocalLLaMA/typed-decisions`](https://huggingface.co/datasets/LocalLLaMA/typed-decisions) test
split (`all`, 400 rows, five decisions per row, 2,000 decisions total). The API used the
`typed-decisions` checkpoint with one persistent HTTP client and one concurrent request. Model
loading was complete before timing began.

| Measurement | Result |
|---|---:|
| Decision agreement | 1,493 / 2,000 (74.65%) |
| Completely correct rows | 105 / 400 (26.25%) |
| Mean request latency | 79.17 ms |
| p50 request latency | 81.36 ms |
| p95 request latency | 121.87 ms |
| p99 request latency | 153.05 ms |
| Max request latency | 180.86 ms |
| Throughput | 12.61 requests/s; 63.06 decisions/s |
| API errors | 0 / 400 |

The exact machine-readable record is
[`benchmarks/typed-decisions-test-2026-09-22.json`](benchmarks/typed-decisions-test-2026-09-22.json).
Re-run it with:

```bash
cd ~/dev/laya/api
CGO_ENABLED=0 ../.tools/go1.27.1/bin/go run ./cmd/laya-bench \
  --api http://127.0.0.1:8012 \
  --model typed-decisions \
  --config all --split test --limit 400 --concurrency 1
```
