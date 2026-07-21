package main

import (
	"bytes"
	"encoding/gob"
)

type gobPayload struct {
	Enabled bool
	Count   int
}

type gobRecord struct {
	Name    string
	Values  []int
	Labels  map[string]string
	Payload any
}

func main() {
	gob.Register(gobPayload{})
	original := gobRecord{
		Name:   "cg12",
		Values: []int{3, 5, 8, 13},
		Labels: map[string]string{
			"compiler": "goc",
			"backend":  "cg12",
		},
		Payload: gobPayload{Enabled: true, Count: 42},
	}

	var encoded bytes.Buffer
	encoder := gob.NewEncoder(&encoded)
	if err := encoder.Encode(original); err != nil {
		panic("gob Encode failed: " + err.Error())
	}

	var decoded gobRecord
	decoder := gob.NewDecoder(&encoded)
	if err := decoder.Decode(&decoded); err != nil {
		panic("gob Decode failed: " + err.Error())
	}
	if decoded.Name != original.Name {
		panic("gob string field mismatch")
	}
	if len(decoded.Values) != 4 || decoded.Values[2] != 8 {
		panic("gob slice field mismatch")
	}
	if decoded.Labels["compiler"] != "goc" || decoded.Labels["backend"] != "cg12" {
		panic("gob map field mismatch")
	}
	payload, ok := decoded.Payload.(gobPayload)
	if !ok || !payload.Enabled || payload.Count != 42 {
		panic("gob interface payload mismatch")
	}
}
