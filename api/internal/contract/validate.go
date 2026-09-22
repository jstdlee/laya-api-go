package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type objectField struct {
	name string
	raw  json.RawMessage
}

var unsupportedExtensions = map[string]struct{}{
	"images": {}, "samples": {}, "auto_max": {}, "auto_threshold": {},
	"steps": {}, "think": {}, "ask": {}, "chunk_rows": {},
	"chunk_prompt": {}, "sequential": {},
}

// ValidateAndNormalize parses the public djev/TypeSafe request while retaining
// question and choice order for the model's label alignment.
func ValidateAndNormalize(raw []byte) (Request, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return Request{}, validationf("request must be a JSON object: %v", err)
	}
	byName := fieldsByName(fields)

	for name := range unsupportedExtensions {
		if _, ok := byName[name]; ok {
			return Request{}, validationf("%s: unsupported by the Laya backend", name)
		}
	}

	model := "auto"
	if modelRaw, ok := byName["model"]; ok {
		var value string
		if err := json.Unmarshal(modelRaw, &value); err != nil || strings.TrimSpace(value) == "" {
			return Request{}, validationf("model: must be a non-empty string")
		}
		model = value
	}

	stateRaw, ok := byName["state"]
	if !ok || bytes.Equal(bytes.TrimSpace(stateRaw), []byte("null")) {
		return Request{}, validationf("state: required")
	}
	if err := validateState(stateRaw); err != nil {
		return Request{}, err
	}

	questionsRaw, ok := byName["questions"]
	if !ok {
		return Request{}, validationf("questions: needs a non-empty map of id -> question")
	}
	questionFields, err := decodeObject(questionsRaw)
	if err != nil || len(questionFields) == 0 {
		return Request{}, validationf("questions: needs a non-empty map of id -> question")
	}

	questions := make([]Question, 0, len(questionFields))
	seen := make(map[string]struct{}, len(questionFields))
	for _, field := range questionFields {
		if field.name == "" || strings.ContainsAny(field.name, ":\n") {
			return Request{}, validationf("question %q: id must be non-empty and contain no ':' or newline", field.name)
		}
		if _, exists := seen[field.name]; exists {
			return Request{}, validationf("duplicate question id %q", field.name)
		}
		seen[field.name] = struct{}{}
		question, err := parseQuestion(field.name, field.raw)
		if err != nil {
			return Request{}, err
		}
		questions = append(questions, question)
	}

	return Request{Model: model, State: append(json.RawMessage(nil), stateRaw...), Questions: questions}, nil
}

func parseQuestion(id string, raw []byte) (Question, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return Question{}, validationf("question %q: must be an object", id)
	}
	byName := fieldsByName(fields)

	typeRaw, ok := byName["type"]
	if !ok {
		return Question{}, validationf("question %q: unknown type", id)
	}
	var kind string
	if err := json.Unmarshal(typeRaw, &kind); err != nil {
		return Question{}, validationf("question %q: type must be a string", id)
	}

	instructions := ""
	if instructionsRaw, ok := byName["instructions"]; ok {
		instructions = compactInstruction(instructionsRaw)
	}
	question := Question{ID: id, Type: kind, Instructions: instructions}

	switch kind {
	case "choice":
		criteriaRaw, ok := byName["criteria"]
		if !ok {
			return Question{}, validationf("question %q: choice criteria must map option names to descriptions", id)
		}
		optionFields, err := decodeObject(criteriaRaw)
		if err != nil || len(optionFields) == 0 {
			return Question{}, validationf("question %q: choice criteria must map option names to descriptions", id)
		}
		question.Criteria = make(map[string]any, len(optionFields))
		question.Options = make([]Option, 0, len(optionFields))
		for _, option := range optionFields {
			var description any
			if err := decodeValue(option.raw, &description); err != nil {
				return Question{}, validationf("question %q: invalid criterion %q: %v", id, option.name, err)
			}
			question.Criteria[option.name] = description
			question.Options = append(question.Options, Option{Name: option.name, Description: description})
		}
	case "score":
		criteriaRaw, ok := byName["criteria"]
		if !ok {
			return Question{}, validationf("question %q: score criteria must be an ordered list of levels", id)
		}
		var levels []json.RawMessage
		if err := decodeValue(criteriaRaw, &levels); err != nil || len(levels) < 2 {
			return Question{}, validationf("question %q: score criteria must contain at least two levels", id)
		}
		question.Levels = make([]any, 0, len(levels))
		for _, levelRaw := range levels {
			var level any
			if err := decodeValue(levelRaw, &level); err != nil {
				return Question{}, validationf("question %q: invalid score level: %v", id, err)
			}
			question.Levels = append(question.Levels, level)
		}
	case "noul":
		if criteriaRaw, ok := byName["criteria"]; ok && !bytes.Equal(bytes.TrimSpace(criteriaRaw), []byte("null")) {
			if _, err := decodeObject(criteriaRaw); err != nil {
				return Question{}, validationf("question %q: noul criteria must be an object with true and false", id)
			}
		}
	default:
		return Question{}, validationf("question %q: unknown type %q", id, kind)
	}

	return question, nil
}

func validateState(raw []byte) error {
	var value any
	if err := decodeValue(raw, &value); err != nil {
		return validationf("state: invalid JSON: %v", err)
	}
	switch value.(type) {
	case string, []any, map[string]any:
		return nil
	default:
		return validationf("state: must be a string, object, or array")
	}
}

func compactInstruction(raw []byte) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return compact.String()
	}
	return strings.TrimSpace(string(raw))
}

func decodeValue(raw []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeObject(raw []byte) ([]objectField, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("expected object")
	}

	fields := make([]objectField, 0)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("object key must be a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, objectField{name: key, raw: value})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return fields, nil
}

func fieldsByName(fields []objectField) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(fields))
	for _, field := range fields {
		result[field.name] = field.raw
	}
	return result
}

func validationf(format string, args ...any) ValidationError {
	return ValidationError{Message: fmt.Sprintf(format, args...), Status: 422}
}
