package main

import (
	"log/slog"
	"os"

	"example.com/planet/internal/bootstrap"
)

func main() {
	a, err := bootstrap.New()
	if err == nil {
		err = a.Execute()
	}
	if err != nil {
		slog.Error("server exit", "err", err)
		os.Exit(1)
	}
}
