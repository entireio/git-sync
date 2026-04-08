package main

import (
	"encoding/json"
	"testing"

	"github.com/soph/git-sync/internal/syncer"
)

func TestMarshalOutput_JSONShape(t *testing.T) {
	data, err := marshalOutput(syncer.FetchResult{
		SourceURL:      "https://example.com/source.git",
		RequestedMode:  "auto",
		Protocol:       "v2",
		FetchedObjects: 42,
	})
	if err != nil {
		t.Fatalf("marshalOutput returned error: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal marshaled output: %v", err)
	}

	if got := decoded["SourceURL"]; got != "https://example.com/source.git" {
		t.Fatalf("unexpected SourceURL: %#v", got)
	}
	if got := decoded["Protocol"]; got != "v2" {
		t.Fatalf("unexpected Protocol: %#v", got)
	}
	if got := decoded["FetchedObjects"]; got != float64(42) {
		t.Fatalf("unexpected FetchedObjects: %#v", got)
	}
}
