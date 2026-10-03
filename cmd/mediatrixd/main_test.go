package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestArgumentFailures(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{{}, {"--config", "/does/not/exist"}, {"unexpected"}, {"--token", "plaintext"}} {
		if err := run(context.Background(), args, io.Discard, logger); err == nil {
			t.Fatalf("bad arguments accepted: %v", args)
		}
	}
}
func TestVersion(t *testing.T) {
	if err := run(context.Background(), []string{"--version"}, io.Discard, slog.Default()); err != nil {
		t.Fatal(err)
	}
}
