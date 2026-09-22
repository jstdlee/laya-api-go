import json
import socket
import sys
import threading
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from worker.laya_worker import (
    WorkerError,
    _handle_connection,
    normalize_request,
    predict_request,
    read_frame,
    shape_result,
    write_frame,
)


class FakeRouter:
    def __init__(self, result):
        self.result = result
        self.calls = []

    def predict(self, state, questions, **kwargs):
        self.calls.append((state, questions, kwargs))
        return self.result


def request(model="auto"):
    return {
        "model": model,
        "state": {"body": "charged twice"},
        "questions": [
            {
                "id": "department",
                "type": "choice",
                "instructions": "Which team?",
                "options": [
                    {"name": "billing", "description": "payments"},
                    {"name": "technical", "description": None},
                ],
            },
            {
                "id": "urgency",
                "type": "score",
                "instructions": "How urgent?",
                "levels": ["not urgent", "soon", "critical"],
            },
            {"id": "risk", "type": "noul", "instructions": "Will they leave?"},
        ],
    }


def result():
    return {
        "model": "laya-rl-agent",
        "answers": {
            "department": {
                "type": "choice",
                "choice": "billing",
                "probabilities": {"billing": 0.9, "technical": 0.1},
                "confidence": 0.8,
                "action": {"act_probability": 0.3},
            },
            "urgency": {
                "type": "score",
                "score": 1.7,
                "legend": {"0": "not urgent", "1": "soon", "2": "critical"},
                "probabilities": {"0": 0.1, "1": 0.2, "2": 0.7},
                "confidence": 0.4,
                "action": {"act_probability": 0.2},
            },
            "risk": {
                "type": "noul",
                "noul": 0.2,
                "confidence": 0.6,
                "action": {"act_probability": 0.1},
            },
        },
        "usage": {"input_tokens": 42, "output_tokens": 0},
        "routing": {"model": "english", "reason": "English Latin text"},
    }


def test_normalize_request_preserves_option_order_and_types():
    state, questions, model = normalize_request(request())
    assert state == {"body": "charged twice"}
    assert model == "auto"
    assert list(questions) == ["department", "urgency", "risk"]
    assert list(questions["department"]["criteria"]) == ["billing", "technical"]
    assert questions["urgency"]["criteria"] == ["not urgent", "soon", "critical"]
    assert questions["risk"]["type"] == "noul"


def test_predict_request_uses_router_and_explicit_model():
    router = FakeRouter(result())
    shaped = predict_request(router, request("multilingual"))
    assert router.calls[0][0] == {"body": "charged twice"}
    assert router.calls[0][2] == {"model": "multilingual"}
    assert shaped["routing"]["model"] == "english"


def test_shape_result_removes_internal_action_metadata():
    shaped = shape_result(result(), request()["questions"])
    assert shaped["answers"]["department"]["choice"] == "billing"
    assert shaped["answers"]["urgency"]["score"] == 1.7
    assert shaped["answers"]["risk"]["noul"] == 0.2
    assert all("action" not in answer for answer in shaped["answers"].values())
    assert shaped["routing"]["model"] == "english"


def test_predict_request_rejects_missing_normalized_fields():
    router = FakeRouter(result())
    for bad in ({}, {"state": "x"}, {"questions": []}, {"state": "x", "questions": [], "model": "auto"}):
        try:
            predict_request(router, bad)
        except WorkerError:
            pass
        else:
            raise AssertionError("expected WorkerError")
    assert not router.calls


def test_shapes_are_json_serializable():
    assert json.loads(json.dumps(shape_result(result(), request()["questions"])))


def test_connection_does_not_reuse_request_id_after_bad_frame():
    client, worker = socket.socketpair()
    thread = threading.Thread(target=_handle_connection, args=(worker, FakeRouter(result())))
    thread.start()
    try:
        write_frame(client, json.dumps({"id": "first", "method": "ready"}).encode())
        assert json.loads(read_frame(client))["id"] == "first"
        write_frame(client, b"{")
        response = json.loads(read_frame(client))
        assert response["id"] == ""
        assert response["ok"] is False
    finally:
        client.close()
        thread.join(timeout=1)
        assert not thread.is_alive()


if __name__ == "__main__":
    tests = [
        test_normalize_request_preserves_option_order_and_types,
        test_predict_request_uses_router_and_explicit_model,
        test_shape_result_removes_internal_action_metadata,
        test_predict_request_rejects_missing_normalized_fields,
        test_shapes_are_json_serializable,
        test_connection_does_not_reuse_request_id_after_bad_frame,
    ]
    for test in tests:
        test()
        print(f"PASS {test.__name__}")
    print(f"{len(tests)} worker tests passed")
