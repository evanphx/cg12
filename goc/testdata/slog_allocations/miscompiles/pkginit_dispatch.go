package main

import (
	"io"
	"log/slog"
)

var jsonLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

func main() {
	jsonLogger.Info("json handler")
	println("json ok")
}
