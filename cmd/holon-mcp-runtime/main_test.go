package main

import (
	"context"
	"strings"
	"testing"
)

func TestInvalidLaunchInputs(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--"}, "missing MCP executable"},
		{[]string{"--origin=https://holon.example", "--", "/missing/sensitive"}, "could not start MCP server"},
	} {
		err := run(context.Background(), test.args)
		if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("expected %s without values, got %v", test.want, err)
		}
	}
}
