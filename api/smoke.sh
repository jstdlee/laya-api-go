#!/usr/bin/env bash
set -euo pipefail

api_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
project_dir=$(cd "$api_dir/.." && pwd)
cd "$api_dir"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/laya-api-smoke.XXXXXX")
socket_path="$tmp_dir/worker.sock"
log_path="$tmp_dir/api.log"
english_request="$tmp_dir/english.json"
hindi_request="$tmp_dir/hindi.json"
english_response="$tmp_dir/english-response.json"
hindi_response="$tmp_dir/hindi-response.json"
pid=""

cleanup() {
    set +e
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
        kill -TERM "$pid" 2>/dev/null
        for _ in {1..50}; do
            kill -0 "$pid" 2>/dev/null || break
            sleep 0.1
        done
        kill -KILL "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    fi
    rm -rf "$tmp_dir"
}
trap cleanup EXIT

port=$((18000 + RANDOM % 10000))
cat >"$english_request" <<'JSON'
{
  "model": "auto",
  "state": {"body": "I was charged twice and want a refund today."},
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
JSON

jq '.state.body = "मुझसे दो बार शुल्क लिया गया और मुझे धनवापसी चाहिए।"' \
    "$english_request" >"$hindi_request"

"$api_dir/laya-api" \
    --host 127.0.0.1 \
    --port "$port" \
    --project-dir "$project_dir" \
    --worker-socket "$socket_path" \
    --preload english,multilingual \
    >"$log_path" 2>&1 &
pid=$!

ready=0
for _ in {1..600}; do
    if ! kill -0 "$pid" 2>/dev/null; then
        echo "laya-api exited before readiness" >&2
        cat "$log_path" >&2
        exit 1
    fi
    if curl -fsS "http://127.0.0.1:$port/health" >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done
if [[ "$ready" -ne 1 ]]; then
    echo "laya-api did not become ready" >&2
    cat "$log_path" >&2
    exit 1
fi

post_json() {
    local request=$1 response=$2 status
    status=$(curl -sS -o "$response" -w '%{http_code}' \
        -X POST "http://127.0.0.1:$port/v1/systemone" \
        -H 'Content-Type: application/json' \
        --data-binary "@$request")
    if [[ "$status" != "200" ]]; then
        echo "unexpected HTTP status $status for $request" >&2
        cat "$response" >&2
        exit 1
    fi
}

post_json "$english_request" "$english_response"
jq -e '
    .model == "english" and
    .routing.model == "english" and
    (.answers.department.choice | type) == "string" and
    (.answers.urgency.score | type) == "number" and
    (.answers.churn_risk.noul | type) == "number" and
    ([.answers[] | has("action")] | any) == false
' "$english_response" >/dev/null

post_json "$hindi_request" "$hindi_response"
jq -e '.model == "multilingual" and .routing.model == "multilingual"' \
    "$hindi_response" >/dev/null

echo "laya-api smoke: PASS"
