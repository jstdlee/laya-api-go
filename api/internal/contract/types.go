package contract

import "encoding/json"

type Request struct {
	Model     string          `json:"model"`
	State     json.RawMessage `json:"state"`
	Questions []Question      `json:"questions"`
}

type Question struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Instructions string         `json:"instructions"`
	Criteria     map[string]any `json:"criteria,omitempty"`
	Options      []Option       `json:"options,omitempty"`
	Levels       []any          `json:"levels,omitempty"`
}

type Option struct {
	Name        string `json:"name"`
	Description any    `json:"description"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Answer map[string]any

type Result struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Routing map[string]any    `json:"routing,omitempty"`
}

type ValidationError struct {
	Message string
	Status  int
}

func (e ValidationError) Error() string { return e.Message }
