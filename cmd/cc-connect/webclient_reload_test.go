package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/webclient"
)

type fakeWebClientApplier struct {
	calls int
	got   webclient.Options
	err   error
}

func (f *fakeWebClientApplier) ApplyApps(opts webclient.Options) (*webclient.ApplyAppsResult, error) {
	f.calls++
	f.got = opts
	return &webclient.ApplyAppsResult{}, f.err
}

func TestApplyWebClientAppsHotReloadIfSupported_CallsApplyApps(t *testing.T) {
	f := &fakeWebClientApplier{}
	opts := webclient.Options{
		Host:  "0.0.0.0",
		Port:  9840,
		Token: "tok",
		Apps: []webclient.AppOptions{
			{ID: "a", Platform: "plat-a", DataNamespace: "ns-a"},
		},
	}

	if err := applyWebClientAppsHotReloadIfSupported(f, opts); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want 1", f.calls)
	}
	if f.got.Port != 9840 || len(f.got.Apps) != 1 || f.got.Apps[0].ID != "a" {
		t.Fatalf("got opts = %+v, want port=9840 apps[0].id=a", f.got)
	}
}

func TestApplyWebClientAppsHotReloadIfSupported_Nil_NoError(t *testing.T) {
	if err := applyWebClientAppsHotReloadIfSupported(nil, webclient.Options{Port: 1}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestApplyWebClientAppsHotReloadIfSupported_ErrorReturned(t *testing.T) {
	want := errors.New("boom")
	f := &fakeWebClientApplier{err: want}
	if err := applyWebClientAppsHotReloadIfSupported(f, webclient.Options{Port: 1}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestApplyWebClientHotReloadFromConfig_BuildsManagementFacade(t *testing.T) {
	enabled := true

	cfg := &config.Config{
		WebClient: config.WebClientConfig{
			Enabled: &enabled,
			Host:    "0.0.0.0",
			Port:    9840,
			Token:   "wc-token",
			DataDir: "/tmp/data",
			Apps: []config.WebClientAppConfig{
				{ID: "app-a", Platform: "plat-a", DataNamespace: "ns-a"},
			},
		},
		Management: config.ManagementConfig{
			Enabled: &enabled,
			Host:    "0.0.0.0",
			Port:    0, // defaulting logic should pick 9820
			Token:   "mgmt-token",
		},
	}

	f := &fakeWebClientApplier{}
	if err := applyWebClientHotReloadFromConfig(cfg, f); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want 1", f.calls)
	}
	if got := f.got.ManagementBaseURL; got != "http://127.0.0.1:9820" {
		t.Fatalf("ManagementBaseURL = %q, want %q", got, "http://127.0.0.1:9820")
	}
	if got := f.got.ManagementToken; got != "mgmt-token" {
		t.Fatalf("ManagementToken = %q, want %q", got, "mgmt-token")
	}
}

func TestReloadConfigFrom_AppliesWebClient(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		Projects: []config.ProjectConfig{
			{Name: "p1"},
		},
		WebClient: config.WebClientConfig{
			Enabled: &enabled,
			Host:    "0.0.0.0",
			Port:    9840,
			Token:   "wc-token",
			DataDir: "/tmp/data",
			Apps: []config.WebClientAppConfig{
				{ID: "app-a", Platform: "plat-a", DataNamespace: "ns-a"},
			},
		},
	}

	engine := core.NewEngine("p1", nil, nil, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	f := &fakeWebClientApplier{}

	if _, err := reloadConfigFrom(cfg, "p1", engine, f); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want 1", f.calls)
	}
}
