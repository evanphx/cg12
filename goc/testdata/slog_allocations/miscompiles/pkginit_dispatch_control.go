package main

import (
	"io"
	"log/slog"
)

var jsonLogger *slog.Logger

func main() {
	jsonLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	jsonLogger.Info("json handler")
	println("json ok")
}
