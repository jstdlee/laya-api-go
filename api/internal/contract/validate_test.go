package contract

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestValidateAndNormalizePrimitives(t *testing.T) {
	raw := []byte(`{"model":"auto","state":{"body":"charged twice\n🙂"},"questions":{
        "department":{"type":"choice","instructions":"Which team?","criteria":{"billing":"payments","technical":null}},
        "urgency":{"type":"score","instructions":"How urgent?","criteria":["not urgent","soon","critical"]},
        "churn":{"type":"noul","instructions":"Will they leave?"}
    }}`)
	req, err := ValidateAndNormalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "auto" {
		t.Fatalf("model = %q, want auto", req.Model)
	}
	if got := string(req.State); !bytes.Contains([]byte(got), []byte("charged twice")) {
		t.Fatalf("state = %s", got)
	}
	if len(req.Questions) != 3 || req.Questions[0].ID != "department" || req.Questions[1].ID != "urgency" || req.Questions[2].ID != "churn" {
		t.Fatalf("question order = %#v", req.Questions)
	}
	if got := req.Questions[0].Criteria["billing"]; got != "payments" {
		t.Fatalf("billing criterion = %#v", got)
	}
	if len(req.Questions[0].Options) != 2 || req.Questions[0].Options[0].Name != "billing" || req.Questions[0].Options[1].Name != "technical" {
		t.Fatalf("choice option order = %#v", req.Questions[0].Options)
	}
	if len(req.Questions[1].Levels) != 3 || req.Questions[1].Levels[2] != "critical" {
		t.Fatalf("levels = %#v", req.Questions[1].Levels)
	}
	if req.Questions[2].Type != "noul" {
		t.Fatalf("noul type = %q", req.Questions[2].Type)
	}
}

func TestValidateInstructionsCanBeStructured(t *testing.T) {
	raw := []byte(`{"state":"ticket","questions":{"q":{"type":"choice","instructions":{"what":"team?","examples":["refund"]},"criteria":{"billing":null,"other":null}}}}`)
	req, err := ValidateAndNormalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if req.Questions[0].Instructions != `{"what":"team?","examples":["refund"]}` {
		t.Fatalf("instructions = %q", req.Questions[0].Instructions)
	}
}

func TestValidateRejectsMalformedRequests(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"malformed json", `{"state":`},
		{"top level array", `[]`},
		{"missing state", `{"questions":{"q":{"type":"noul"}}}`},
		{"null state", `{"state":null,"questions":{"q":{"type":"noul"}}}`},
		{"scalar state", `{"state":42,"questions":{"q":{"type":"noul"}}}`},
		{"missing questions", `{"state":"x"}`},
		{"questions array", `{"state":"x","questions":[]}`},
		{"empty questions", `{"state":"x","questions":{}}`},
		{"empty question id", `{"state":"x","questions":{"":{"type":"noul"}}}`},
		{"colon question id", `{"state":"x","questions":{"bad:id":{"type":"noul"}}}`},
		{"newline question id", "{\"state\":\"x\",\"questions\":{\"bad\\nid\":{\"type\":\"noul\"}}}"},
		{"unknown type", `{"state":"x","questions":{"q":{"type":"rank"}}}`},
		{"choice criteria missing", `{"state":"x","questions":{"q":{"type":"choice"}}}`},
		{"choice criteria empty", `{"state":"x","questions":{"q":{"type":"choice","criteria":{}}}}`},
		{"choice criteria array", `{"state":"x","questions":{"q":{"type":"choice","criteria":["a","b"]}}}`},
		{"score criteria missing", `{"state":"x","questions":{"q":{"type":"score"}}}`},
		{"score criteria one", `{"state":"x","questions":{"q":{"type":"score","criteria":["only"]}}}`},
		{"score criteria object", `{"state":"x","questions":{"q":{"type":"score","criteria":{"a":"b"}}}}`},
		{"noul criteria array", `{"state":"x","questions":{"q":{"type":"noul","criteria":[]}}}`},
		{"unsupported images", `{"state":"x","images":[],"questions":{"q":{"type":"noul"}}}`},
		{"unsupported samples", `{"state":"x","samples":2,"questions":{"q":{"type":"noul"}}}`},
		{"duplicate question id", `{"state":"x","questions":{"q":{"type":"noul"},"q":{"type":"noul"}}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateAndNormalize([]byte(tt.raw))
			if err == nil {
				t.Fatal("expected validation error")
			}
			ve, ok := err.(ValidationError)
			if !ok {
				t.Fatalf("error type = %T, want ValidationError: %v", err, err)
			}
			if ve.Status != 422 {
				t.Fatalf("status = %d, want 422", ve.Status)
			}
		})
	}
}

func TestValidationErrorIsSafeToMarshal(t *testing.T) {
	ve := ValidationError{Message: "bad question", Status: 422}
	data, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": ve.Message, "type": "validation_error"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"error":{"message":"bad question","type":"validation_error"}}` {
		t.Fatalf("body = %s", data)
	}
}
