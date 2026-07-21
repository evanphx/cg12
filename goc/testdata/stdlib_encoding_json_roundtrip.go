package main

import (
	"bytes"
	"encoding/json"
)

type jsonStatus struct {
	Name     string            `json:"name"`
	Sequence int               `json:"sequence"`
	Labels   map[string]string `json:"labels"`
	Values   []int             `json:"values"`
	Payload  json.RawMessage   `json:"payload"`
	Ignored  string            `json:"ignored,omitempty"`
}

func main() {
	status := jsonStatus{
		Name:     "cg12",
		Sequence: 42,
		Labels: map[string]string{
			"compiler": "go",
			"backend":  "cg12",
		},
		Values:  []int{3, 5, 8, 13},
		Payload: json.RawMessage(`{"ready":true,"count":2}`),
	}

	encoded, err := json.Marshal(status)
	if err != nil {
		panic("json Marshal failed")
	}
	if bytes.Contains(encoded, []byte("ignored")) {
		panic("json omitempty field was encoded")
	}

	var decoded jsonStatus
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		panic("json Unmarshal failed")
	}
	if decoded.Name != status.Name || decoded.Sequence != status.Sequence {
		panic("json scalar field mismatch")
	}
	if decoded.Labels["compiler"] != "go" || decoded.Labels["backend"] != "cg12" {
		panic("json map field mismatch")
	}
	if len(decoded.Values) != 4 || decoded.Values[2] != 8 {
		panic("json slice field mismatch")
	}

	decoder := json.NewDecoder(bytes.NewReader([]byte(`{"number":9007199254740993,"text":"\u03bb"}`)))
	decoder.UseNumber()
	var dynamic map[string]any
	if err := decoder.Decode(&dynamic); err != nil {
		panic("json Decoder failed")
	}
	number, ok := dynamic["number"].(json.Number)
	if !ok || number.String() != "9007199254740993" {
		panic("json number interface mismatch")
	}
	text, ok := dynamic["text"].(string)
	if !ok || text != "λ" {
		panic("json unicode interface mismatch")
	}
}
