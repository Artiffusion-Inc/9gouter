package provider

import (
	"testing"
)

func TestLookupGrokCli(t *testing.T) {
	p, err := Lookup("grok-cli")
	if err != nil {
		t.Fatalf("lookup grok-cli: %v", err)
	}
	if p.ID() != "grok-cli" {
		t.Fatalf("unexpected id %s", p.ID())
	}
}
