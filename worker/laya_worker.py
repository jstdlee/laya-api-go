#!/usr/bin/env python3
"""Persistent Unix-socket adapter for the Laya Python runtime.

The public HTTP API lives in Go. This process owns the loaded Router and
speaks only a small length-prefixed JSON protocol over its private socket.
"""

import argparse
import json
import os
import socket
import struct
import sys
import threading
import traceback

from laya import Router


MAX_FRAME_SIZE = 8 << 20


class WorkerError(ValueError):
    status = 422
    error_type = "validation_error"


def read_frame(conn):
    header = _read_exact(conn, 4)
    if not header:
        return None
    size = struct.unpack(">I", header)[0]
    if size == 0 or size > MAX_FRAME_SIZE:
        raise WorkerError("invalid RPC frame size")
    return _read_exact(conn, size)


def write_frame(conn, payload):
    if not payload or len(payload) > MAX_FRAME_SIZE:
        raise ValueError("invalid RPC frame payload size")
    conn.sendall(struct.pack(">I", len(payload)) + payload)


def _read_exact(conn, size):
    chunks = []
    remaining = size
    while remaining:
        chunk = conn.recv(remaining)
        if not chunk:
            if not chunks:
                return None
            raise EOFError("truncated RPC frame")
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def normalize_request(params):
    if not isinstance(params, dict):
        raise WorkerError("params must be an object")
    if "state" not in params:
        raise WorkerError("state: required")
    raw_questions = params.get("questions")
    if not isinstance(raw_questions, list) or not raw_questions:
        raise WorkerError("questions: needs a non-empty array")

    questions = {}
    for raw in raw_questions:
        if not isinstance(raw, dict):
            raise WorkerError("question must be an object")
        qid = raw.get("id")
        kind = raw.get("type")
        if not isinstance(qid, str) or not qid or ":" in qid or "\n" in qid:
            raise WorkerError(f"question {qid!r}: invalid id")
        if qid in questions:
            raise WorkerError(f"duplicate question id {qid!r}")
        if not isinstance(kind, str):
            raise WorkerError(f"question {qid!r}: type is required")
        question = {
            "type": kind,
            "instructions": str(raw.get("instructions", "")),
        }
        if kind == "choice":
            options = raw.get("options")
            if not isinstance(options, list) or not options:
                raise WorkerError(f"question {qid!r}: options are required")
            criteria = {}
            for option in options:
                if not isinstance(option, dict) or not isinstance(option.get("name"), str):
                    raise WorkerError(f"question {qid!r}: invalid option")
                name = option["name"]
                if name in criteria:
                    raise WorkerError(f"question {qid!r}: duplicate option {name!r}")
                criteria[name] = option.get("description")
            question["criteria"] = criteria
        elif kind == "score":
            levels = raw.get("levels")
            if not isinstance(levels, list) or len(levels) < 2:
                raise WorkerError(f"question {qid!r}: at least two score levels are required")
            question["criteria"] = levels
        elif kind == "noul":
            criteria = raw.get("criteria")
            if criteria is not None and not isinstance(criteria, dict):
                raise WorkerError(f"question {qid!r}: noul criteria must be an object")
            if criteria is not None:
                question["criteria"] = criteria
        else:
            raise WorkerError(f"question {qid!r}: unknown type {kind!r}")
        questions[qid] = question

    model = params.get("model", "auto")
    if not isinstance(model, str) or not model:
        raise WorkerError("model: must be a non-empty string")
    return params["state"], questions, model


def predict_request(router, params):
    state, questions, model = normalize_request(params)
    kwargs = {} if model in ("auto", "router") else {"model": model}
    result = router.predict(state, questions, **kwargs)
    return shape_result(result, params["questions"])


def shape_result(result, requested_questions):
    if not isinstance(result, dict) or not isinstance(result.get("answers"), dict):
        raise WorkerError("worker returned an invalid result")
    answer_map = result["answers"]
    answers = {}
    for question in requested_questions:
        qid = question.get("id") if isinstance(question, dict) else None
        if qid not in answer_map:
            raise WorkerError(f"worker result is missing answer {qid!r}")
        answer = answer_map[qid]
        if not isinstance(answer, dict):
            raise WorkerError(f"worker answer {qid!r} is not an object")
        answers[qid] = {key: value for key, value in answer.items() if key != "action"}

    shaped = {key: value for key, value in result.items() if key != "answers"}
    shaped["answers"] = answers
    routing = shaped.get("routing")
    if isinstance(routing, dict) and isinstance(routing.get("model"), str):
        shaped["model"] = routing["model"]
    return shaped


def serve(socket_path, preload_names, device=None):
    names = [name.strip() for name in preload_names if name and name.strip()]
    if not names:
        names = ["english", "multilingual"]
    router = Router(max_loaded=len(names), device=device)
    router.preload(names)

    _remove_stale_socket(socket_path)
    parent = os.path.dirname(socket_path)
    if parent:
        os.makedirs(parent, exist_ok=True)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(socket_path)
    os.chmod(socket_path, 0o600)
    server.listen(128)
    try:
        while True:
            conn, _ = server.accept()
            threading.Thread(target=_handle_connection, args=(conn, router), daemon=True).start()
    finally:
        server.close()
        _remove_stale_socket(socket_path)


def _handle_connection(conn, router):
    with conn:
        while True:
            request = {}
            try:
                payload = read_frame(conn)
                if payload is None:
                    return
                request = json.loads(payload)
                response = _dispatch(router, request)
            except Exception as exc:
                if not isinstance(exc, WorkerError):
                    traceback.print_exc(file=sys.stderr)
                response = _error_response(request, exc)
            try:
                write_frame(conn, json.dumps(response, ensure_ascii=False, separators=(",", ":")).encode("utf-8"))
            except (BrokenPipeError, ConnectionResetError, OSError):
                return


def _dispatch(router, request):
    if not isinstance(request, dict):
        raise WorkerError("RPC request must be an object")
    method = request.get("method")
    if method == "ready":
        return {"id": request.get("id", ""), "ok": True, "result": {"status": "ready"}}
    if method != "predict":
        raise WorkerError(f"unknown RPC method {method!r}")
    result = predict_request(router, request.get("params"))
    return {"id": request.get("id", ""), "ok": True, "result": result}


def _error_response(request, exc):
    if isinstance(exc, WorkerError):
        status, error_type = exc.status, exc.error_type
    else:
        status, error_type = 502, "server_error"
    return {
        "id": request.get("id", "") if isinstance(request, dict) else "",
        "ok": False,
        "error": {"status": status, "type": error_type, "message": str(exc)},
    }


def _remove_stale_socket(path):
    try:
        mode = os.stat(path).st_mode
    except FileNotFoundError:
        return
    if not os.path.exists(path) or not stat_is_socket(mode):
        raise RuntimeError(f"refusing to remove non-socket path {path!r}")
    os.unlink(path)


def stat_is_socket(mode):
    import stat

    return stat.S_ISSOCK(mode)


def main():
    parser = argparse.ArgumentParser(description="Persistent Laya Unix-socket worker")
    parser.add_argument("--socket", required=True, dest="socket_path")
    parser.add_argument("--preload", default="english,multilingual")
    parser.add_argument("--device", default=None)
    args = parser.parse_args()
    serve(args.socket_path, args.preload.split(","), args.device)


if __name__ == "__main__":
    main()
