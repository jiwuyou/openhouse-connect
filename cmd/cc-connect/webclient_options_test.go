package main

import (
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

func TestBuildWebClientOptions_LegacyNoAppsUnchanged(t *testing.T) {
	cfg := &config.Config{
		WebClient: config.WebClientConfig{
			Host:          "0.0.0.0",
			Port:          9840,
			Token:         "tok",
			DataDir:       "/tmp/data",
			PublicURL:     "http://example.test",
			DefaultApp:    "ignored-default",
			Platform:      "legacy-platform",
			DataNamespace: "legacy-ns",
			Apps:          nil,
		},
	}

	opts := buildWebClientOptions(cfg, "http://mgmt.local:9820", "mgmt-token")

	if got := opts.ManagementBaseURL; got != "http://mgmt.local:9820" {
		t.Fatalf("ManagementBaseURL = %q, want %q", got, "http://mgmt.local:9820")
	}
	if got := opts.ManagementToken; got != "mgmt-token" {
		t.Fatalf("ManagementToken = %q, want %q", got, "mgmt-token")
	}
	if opts.DefaultApp != "" {
		t.Fatalf("DefaultApp = %q, want empty in legacy mode", opts.DefaultApp)
	}
	if len(opts.Apps) != 0 {
		t.Fatalf("Apps len = %d, want 0 in legacy mode", len(opts.Apps))
	}
	if opts.Platform != "legacy-platform" {
		t.Fatalf("Platform = %q, want %q", opts.Platform, "legacy-platform")
	}
	if opts.DataNamespace != "legacy-ns" {
		t.Fatalf("DataNamespace = %q, want %q", opts.DataNamespace, "legacy-ns")
	}
}

func TestBuildWebClientOptions_MultiAppMapsAndSkipsDisabled(t *testing.T) {
	enabled := true
	disabled := false

	cfg := &config.Config{
		WebClient: config.WebClientConfig{
			DefaultApp:    "app-a",
			Platform:      "default-platform",
			DataNamespace: "default-ns",
			Apps: []config.WebClientAppConfig{
				{ID: "app-a", Platform: "plat-a", DataNamespace: "ns-a", Enabled: nil},
				{ID: "app-b", Platform: "plat-b", DataNamespace: "ns-b", Enabled: &enabled},
				{ID: "app-c", Platform: "plat-c", DataNamespace: "ns-c", Enabled: &disabled},
			},
		},
	}

	opts := buildWebClientOptions(cfg, "", "")

	if opts.DefaultApp != "app-a" {
		t.Fatalf("DefaultApp = %q, want %q", opts.DefaultApp, "app-a")
	}
	if len(opts.Apps) != 2 {
		t.Fatalf("Apps len = %d, want 2 (disabled app skipped)", len(opts.Apps))
	}

	if opts.Apps[0].ID != "app-a" || opts.Apps[0].Platform != "plat-a" || opts.Apps[0].DataNamespace != "ns-a" || opts.Apps[0].Enabled != nil {
		t.Fatalf("apps[0] = %+v, want app-a with explicit values and nil Enabled", opts.Apps[0])
	}
	if opts.Apps[1].ID != "app-b" || opts.Apps[1].Platform != "plat-b" || opts.Apps[1].DataNamespace != "ns-b" || opts.Apps[1].Enabled != &enabled {
		t.Fatalf("apps[1] = %+v, want app-b with explicit values and Enabled pointer preserved", opts.Apps[1])
	}
}
