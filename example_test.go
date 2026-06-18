package gitsync_test

import (
	"context"
	"net/http"
	"time"

	"entire.io/entire/git-sync"
)

func ExampleClient_Sync() {
	client := gitsync.New(gitsync.Options{
		HTTPClient: &http.Client{},
		Auth: gitsync.StaticAuthProvider{
			Source: gitsync.EndpointAuth{Token: "source-token"},
			Target: gitsync.EndpointAuth{Token: "target-token"},
		},
	})

	// Bound the call with a context deadline so it can't hang on network I/O
	// when go test runs this example. The hosts below are RFC 2606 reserved
	// names that don't resolve, so the call fails fast and prints nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.Sync(ctx, gitsync.SyncRequest{
		Source: gitsync.Endpoint{URL: "https://github.example/source/repo.git"},
		Target: gitsync.Endpoint{URL: "https://git.example/target/repo.git"},
		Scope:  gitsync.RefScope{Branches: []string{"main"}},
		Policy: gitsync.SyncPolicy{
			IncludeTags: true,
			Protocol:    gitsync.ProtocolAuto,
		},
	}); err != nil {
		return // network error expected in example environment
	}
	// Output:
}
