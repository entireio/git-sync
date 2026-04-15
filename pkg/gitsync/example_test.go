package gitsync_test

import (
	"context"
	"net/http"

	"github.com/entirehq/git-sync/pkg/gitsync"
)

func ExampleClient_Sync() {
	client := gitsync.New(gitsync.Options{
		HTTPClient: &http.Client{},
		Auth: gitsync.StaticAuthProvider{
			Source: gitsync.EndpointAuth{Token: "source-token"},
			Target: gitsync.EndpointAuth{Token: "target-token"},
		},
	})

	_, _ = client.Sync(context.Background(), gitsync.SyncRequest{
		Source: gitsync.Endpoint{URL: "https://github.example/source/repo.git"},
		Target: gitsync.Endpoint{URL: "https://git.example/target/repo.git"},
		Scope:  gitsync.RefScope{Branches: []string{"main"}},
		Policy: gitsync.SyncPolicy{
			IncludeTags: true,
			Protocol:    gitsync.ProtocolAuto,
		},
	})

	// Output:
}
