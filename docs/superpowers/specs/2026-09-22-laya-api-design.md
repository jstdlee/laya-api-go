# Laya API Server Design

## Status

Approved direction: Go public API with a persistent local Laya worker.

## Goal

Expose Laya through a djev-spark/TypeSafe-compatible REST API while keeping
the public HTTP server in a high-performance non-Python stack. The API must
validate the request syntax before inference and support all Laya decision
primitives: `choice`, `score`, and `noul`.

## Architecture

```text
HTTP client
    |
    |  Go TCP listener: configurable host and port
    v
Go laya-api
    |  request validation, auth, timeouts, response shaping
    |  persistent length-prefixed JSON RPC over Unix socket
    v
Python laya-worker
    |  one Router with selected checkpoints preloaded per process
    |  router.predict(state, questions)
    v
Loaded Laya checkpoints in PyTorch memory
```

The Go binary owns the public REST surface. The worker is not an HTTP server;
it is a model adapter using the existing Laya module. Model weights are loaded
once when the worker starts and remain resident until the worker exits.

The internal bridge uses a Unix stream socket and a 4-byte big-endian payload
length followed by one UTF-8 JSON envelope. A request and response both carry
an ID so the Go side can correlate responses. Persistent connections avoid a
connect/close cycle per prediction. JSON is retained at this boundary because
the payload is already JSON-shaped and model inference dominates bridge cost.

## Public API

### `GET /health`

Returns `200` and `{"status":"ok"}` when the API is serving. The response may
include worker readiness details as additive fields. If the worker is absent or
not ready, return `503` with the standard error object.

### `POST /v1/systemone`

The request follows the TypeSafe/djev-spark shape:

```json
{
  "model": "auto",
  "state": {"body": "I was charged twice"},
  "questions": {
    "department": {
      "type": "choice",
      "instructions": "Which team should handle this?",
      "criteria": {
        "billing": "Charges, invoices, refunds",
        "technical": "Bugs and outages"
      }
    },
    "urgency": {
      "type": "score",
      "instructions": "How urgent is this?",
      "criteria": ["not urgent", "soon", "critical"]
    },
    "churn_risk": {
      "type": "noul",
      "instructions": "Will the customer cancel?"
    }
  }
}
```

`state` is required and must be a string, object, or array. `model` is optional;
`auto` (or omission) lets the Laya Router choose. Explicit Laya model aliases
are forwarded to the worker.

Supported question validation:

- `choice`: non-empty `criteria` object; criterion descriptions may be null,
  strings, objects, or arrays.
- `score`: `criteria` must be an ordered array with at least two levels.
- `noul`: `criteria` is optional; when present it must be an object whose
  `true`/`false` values are descriptions.
- Question IDs must be non-empty, unique, and contain neither `:` nor a
  newline, matching the djev-spark parser.
- Unknown question types, malformed criteria, missing state/questions, and
  unsupported image or sampling extensions receive `422`.

The response preserves djev-spark answer shapes:

```json
{
  "model": "english",
  "answers": {
    "department": {
      "type": "choice",
      "choice": "billing",
      "probabilities": {"billing": 0.9, "technical": 0.1},
      "confidence": 0.72
    },
    "urgency": {
      "type": "score",
      "score": 1.7,
      "legend": {"0": "not urgent", "1": "soon", "2": "critical"},
      "probabilities": {"0": 0.1, "1": 0.2, "2": 0.7},
      "confidence": 0.4
    },
    "churn_risk": {
      "type": "noul",
      "noul": 0.88,
      "confidence": 0.76
    }
  },
  "usage": {"input_tokens": 42, "output_tokens": 0},
  "routing": {"model": "english", "reason": "English Latin text"}
}
```

`routing` is an additive Laya extension. The standard answer fields remain
compatible with TypeSafe's primitive contract. Laya's internal `action`
metadata is not exposed in the initial REST response.

The first implementation intentionally does not provide djev-spark's
vLLM-specific raw chat passthrough, image inputs, diffusion sampling, or
dependency/chunk/thought extensions. These are rejected clearly rather than
silently ignored. A future `/v1/chat/completions` adapter can translate a
two-message structured request into `/v1/systemone` without changing the
model worker protocol.

## Authentication and configuration

- `--host` defaults to `127.0.0.1`.
- `--port` defaults to `8011`.
- `--worker-socket` selects the Unix socket path.
- `--api-key` or `API_KEY` enables `Authorization: Bearer ...` on POST routes.
- Request and upstream timeouts are bounded and return `503`/`504` rather than
  leaving connections hanging indefinitely.
- A bounded in-memory queue prevents unbounded request growth under load.

## Worker lifecycle

The API process starts or connects to one persistent worker. The worker loads
only the selected model set at startup; the default production profile
preloads English and multilingual checkpoints, while typed-decisions can be
explicitly enabled. A worker restart reloads the models and temporarily makes
the API unready.

The Go server validates all public inputs once. The worker performs a small
defensive shape check before invoking Laya, then returns structured errors with
the request ID. Model failures become `502`/`500` responses without leaking
tracebacks to clients.

## Testing and verification

Tests will cover:

1. Validation for `choice`, `score`, `noul`, malformed criteria, and unsupported
   extensions.
2. HTTP status and error-body compatibility.
3. Length-prefixed Unix-socket request/response framing.
4. Worker response shaping for all three primitive types using a test worker.
5. A live smoke test that starts the real worker, sends an English request and
   a Hindi request, and verifies Router routing metadata.
6. Go formatting, unit tests, and a standalone build with no Python, Node, or
   Rust dependency in the Go API binary.
