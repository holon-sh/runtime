package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunNeedsACommand(t *testing.T) {
	if err := run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run = %v", err)
	}
}
