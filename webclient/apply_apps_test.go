package webclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApplyApps_AddRemoveDisabledAndDefaultSwitch(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
			{ID: "b", Platform: "web-b", DataNamespace: "b"},
		},
		DefaultApp: "a",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	// Create one session in default app via legacy route.
	{
		body := `{"session_key":"a:web:1","name":"a"}`
		res, err := ts.Client().Post(ts.URL+"/api/v1/projects/proj/sessions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST legacy create session: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("legacy create status=%d body=%s", res.StatusCode, string(b))
		}
	}
	// Create one session in b via namespaced route.
	{
		body := `{"session_key":"b:web:1","name":"b"}`
		res, err := ts.Client().Post(ts.URL+"/apps/b/api/v1/projects/proj/sessions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST b create session: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("b create status=%d body=%s", res.StatusCode, string(b))
		}
	}

	// Legacy list sees only default app sessions (a).
	assertLegacySessionKey := func(want string) {
		t.Helper()
		res, err := ts.Client().Get(ts.URL + "/api/v1/projects/proj/sessions")
		if err != nil {
			t.Fatalf("GET legacy list: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("legacy list status=%d body=%s", res.StatusCode, string(b))
		}
		var env struct {
			OK   bool `json:"ok"`
			Data struct {
				Sessions []struct {
					SessionKey string `json:"session_key"`
				} `json:"sessions"`
			} `json:"data"`
		}
		if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
			t.Fatalf("decode legacy list: %v", err)
		}
		if !env.OK || len(env.Data.Sessions) != 1 || env.Data.Sessions[0].SessionKey != want {
			t.Fatalf("legacy list unexpected: %+v", env)
		}
	}
	assertLegacySessionKey("a:web:1")

	disabled := false
	res, err := s.ApplyApps(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a", Enabled: &disabled}, // disable a
			{ID: "b", Platform: "web-b", DataNamespace: "b"},
			{ID: "c", Platform: "web-c", DataNamespace: "c"},
		},
		DefaultApp: "b",
	})
	if err != nil {
		t.Fatalf("ApplyApps: %v", err)
	}
	if res.DefaultAppID != "b" {
		t.Fatalf("DefaultAppID=%q want b", res.DefaultAppID)
	}

	// Disabled a returns 404 for new requests.
	{
		resp, err := ts.Client().Get(ts.URL + "/apps/a/api/projects/proj/sessions")
		if err != nil {
			t.Fatalf("GET removed app: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("removed app status=%d want %d", resp.StatusCode, http.StatusNotFound)
		}
	}
	// Added c is accessible.
	{
		resp, err := ts.Client().Get(ts.URL + "/apps/c/api/projects/proj/sessions")
		if err != nil {
			t.Fatalf("GET added app: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("added app status=%d body=%s", resp.StatusCode, string(b))
		}
	}

	// Default switch updates legacy routes.
	assertLegacySessionKey("b:web:1")
}

func TestApplyApps_PlatformRenameRestartsAdapterWhenStarted(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir:           tmp,
		ManagementBaseURL: "http://127.0.0.1:1", // enables adapter creation
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rt := s.apps["crm"]
	if rt == nil || rt.adapter == nil {
		t.Fatalf("expected adapter to be configured")
	}

	// Simulate server already started (we don't need an actual listener for this test).
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	old := rt.adapter
	old.Start()

	_, err = s.ApplyApps(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm-2", DataNamespace: "crm"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("ApplyApps: %v", err)
	}

	// New runtime should have a new adapter instance using the renamed platform.
	newRT := s.apps["crm"]
	if newRT == nil || newRT.adapter == nil {
		t.Fatalf("expected new runtime adapter")
	}
	if newRT.adapter == old {
		t.Fatalf("expected adapter to be restarted")
	}
	if got := newRT.adapter.platformName(); got != "web-crm-2" {
		t.Fatalf("platform=%q want web-crm-2", got)
	}
	if !newRT.adapter.started.Load() {
		t.Fatalf("expected new adapter to be started when server is started")
	}
}

func TestApplyApps_RejectsDataNamespaceChange(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	before := s.apps["crm"]

	_, err = s.ApplyApps(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm2"},
		},
		DefaultApp: "crm",
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if s.apps["crm"] != before {
		t.Fatalf("runtime changed despite error")
	}
}

func TestApplyApps_RejectsLegacyToMultiAppSwitch(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	beforeDefault := s.defaultAppID

	_, err = s.ApplyApps(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
		},
		DefaultApp: "crm",
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if s.defaultAppID != beforeDefault {
		t.Fatalf("default app changed unexpectedly")
	}
}

func TestApplyApps_RejectsMultiAppToLegacySwitch(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	beforeDefault := s.defaultAppID

	_, err = s.ApplyApps(Options{
		DataDir: tmp,
		Apps:    nil, // legacy
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if s.defaultAppID != beforeDefault {
		t.Fatalf("default app changed unexpectedly")
	}
}

func TestApplyApps_ConcurrentApplyDoesNotPanicAndKeepsServerUsable(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
		},
		DefaultApp: "a",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	opts1 := Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
			{ID: "b", Platform: "web-b", DataNamespace: "b"},
		},
		DefaultApp: "b",
	}
	opts2 := Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
			{ID: "c", Platform: "web-c", DataNamespace: "c"},
		},
		DefaultApp: "c",
	}

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			var o Options
			if i%2 == 0 {
				o = opts1
			} else {
				o = opts2
			}
			if _, err := s.ApplyApps(o); err != nil {
				t.Errorf("ApplyApps: %v", err)
			}
		}()
	}
	wg.Wait()

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)
	res, err := ts.Client().Get(ts.URL + "/api/v1/projects/proj/sessions")
	if err != nil {
		t.Fatalf("GET legacy list: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("legacy list status=%d want %d", res.StatusCode, http.StatusOK)
	}
}

func TestApplyApps_StopAndApplyAppsConcurrent_NoRaceOrDeadlock(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
			{ID: "b", Platform: "web-b", DataNamespace: "b"},
		},
		DefaultApp: "a",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Simulate started server without binding a real port.
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	disabled := false
	opts := Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a", Enabled: &disabled},
			{ID: "b", Platform: "web-b", DataNamespace: "b"},
		},
		DefaultApp: "b",
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = s.ApplyApps(opts)
	}()
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()
	wg.Wait()
}

func TestApplyApps_RemoveAppReturns404BeforeAdapterStopCompletes(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
			{ID: "b", Platform: "web-b", DataNamespace: "b"},
		},
		DefaultApp: "a",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Install a blocking adapter so ApplyApps will stall on Stop().
	block := &adapterClient{
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	block.started.Store(true)

	s.mu.Lock()
	rt := s.apps["a"]
	if rt == nil {
		s.mu.Unlock()
		t.Fatalf("missing runtime a")
	}
	rt.adapter = block
	s.mu.Unlock()

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	disabled := false
	errCh := make(chan error, 1)
	go func() {
		_, err := s.ApplyApps(Options{
			DataDir: tmp,
			Apps: []AppOptions{
				{ID: "a", Platform: "web-a", DataNamespace: "a", Enabled: &disabled},
				{ID: "b", Platform: "web-b", DataNamespace: "b"},
			},
			DefaultApp: "b",
		})
		errCh <- err
	}()

	// Wait until ApplyApps begins stopping the adapter (this happens after it has
	// removed the app from the runtime map).
	select {
	case <-block.stopCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for adapter stop")
	}

	// Even though ApplyApps is still blocked on Stop(), new requests must see 404.
	resp, err := ts.Client().Get(ts.URL + "/apps/a/api/projects/proj/sessions")
	if err != nil {
		t.Fatalf("GET removed app: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want %d", resp.StatusCode, http.StatusNotFound)
	}

	close(block.doneCh)
	if err := <-errCh; err != nil {
		t.Fatalf("ApplyApps: %v", err)
	}
}

func TestApplyApps_ErrorPathLeavesOldAppAccessible(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	_, err = s.ApplyApps(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm2"},
		},
		DefaultApp: "crm",
	})
	if err == nil {
		t.Fatalf("expected error")
	}

	// Old app should still be accessible after the failed apply.
	resp, err := ts.Client().Get(ts.URL + "/apps/crm/api/projects/proj/sessions")
	if err != nil {
		t.Fatalf("GET app sessions: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestApplyApps_RejectsHostPortDataDirChange(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
		},
		DefaultApp: "a",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Host change
	if _, err := s.ApplyApps(Options{
		Host:    "127.0.0.1",
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
		},
		DefaultApp: "a",
	}); err == nil {
		t.Fatalf("expected host change error")
	}

	// Port change
	if _, err := s.ApplyApps(Options{
		Port:    9841,
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
		},
		DefaultApp: "a",
	}); err == nil {
		t.Fatalf("expected port change error")
	}

	// DataDir change
	tmp2 := t.TempDir()
	if _, err := s.ApplyApps(Options{
		DataDir: tmp2,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
		},
		DefaultApp: "a",
	}); err == nil {
		t.Fatalf("expected data_dir change error")
	}
}

func TestApplyApps_DefaultSwitchConcurrentLegacyPlatformSend_NoRace(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "a", Platform: "web-a", DataNamespace: "a"},
			{ID: "b", Platform: "web-b", DataNamespace: "b"},
		},
		DefaultApp: "a",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	p := newPlatform(s, "proj")
	rc := replyContext{Project: "proj", Session: "s1"}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = p.Send(context.Background(), rc, "hi")
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_, _ = s.ApplyApps(Options{
				DataDir: tmp,
				Apps: []AppOptions{
					{ID: "a", Platform: "web-a", DataNamespace: "a"},
					{ID: "b", Platform: "web-b", DataNamespace: "b"},
				},
				DefaultApp: "b",
			})
			_, _ = s.ApplyApps(Options{
				DataDir: tmp,
				Apps: []AppOptions{
					{ID: "a", Platform: "web-a", DataNamespace: "a"},
					{ID: "b", Platform: "web-b", DataNamespace: "b"},
				},
				DefaultApp: "a",
			})
		}
	}()

	wg.Wait()
}
