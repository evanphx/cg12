package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
)

type compilerState struct {
	packageName string
	files       int
}

func (state compilerState) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("package", state.packageName),
		slog.Int("files", state.files),
	)
}

func main() {
	var jsonOutput bytes.Buffer
	options := &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, attribute slog.Attr) slog.Attr {
			if len(groups) == 0 && attribute.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attribute
		},
	}
	logger := slog.New(slog.NewJSONHandler(&jsonOutput, options)).With("component", "cg12")
	logger.Info(
		"compiled",
		"state", compilerState{packageName: "runtime", files: 42},
		"success", true,
	)

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(jsonOutput.Bytes()), &record); err != nil {
		panic(err)
	}
	if record[slog.LevelKey] != "INFO" || record[slog.MessageKey] != "compiled" {
		panic("slog JSON record metadata mismatch")
	}
	if record["component"] != "cg12" || record["success"] != true {
		panic("slog JSON attributes mismatch")
	}
	state, ok := record["state"].(map[string]any)
	if !ok {
		panic("slog LogValuer did not produce a group")
	}
	if state["package"] != "runtime" || state["files"] != float64(42) {
		panic("slog LogValuer group mismatch")
	}

	var textOutput bytes.Buffer
	textLogger := slog.New(slog.NewTextHandler(&textOutput, options)).WithGroup("build")
	textLogger.Warn("retry", "attempt", 2)
	text := textOutput.String()
	if !strings.Contains(text, "level=WARN") ||
		!strings.Contains(text, "msg=retry") ||
		!strings.Contains(text, "build.attempt=2") {
		panic("slog text output mismatch")
	}
}
