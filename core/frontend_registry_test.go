package core

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFrontendRegistryServiceStatusStale(t *testing.T) {
	registry := NewFrontendAppRegistry(filepath.Join(t.TempDir(), "frontend_apps.json"))
	if _, err := registry.CreateApp(FrontendApp{
		ID:      "smallphone",
		Name:    "SmallPhone",
		Project: "smallphone-3e9fc251",
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, err := registry.UpsertSlot("smallphone", FrontendSlot{
		Slot:    "stable",
		URL:     "http://frontend.example/stable",
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert slot: %v", err)
	}
	if _, err := registry.RegisterSlotService("smallphone", "stable", FrontendServiceRegistration{
		ServiceID:           "frontend-smallphone-stable",
		HeartbeatTTLSeconds: 1,
	}); err != nil {
		t.Fatalf("register service: %v", err)
	}

	registry.mu.Lock()
	state := registry.services[frontendServiceKey("smallphone", "stable")]
	expiredAt := time.Now().UTC().Add(-time.Second)
	state.ExpiresAt = &expiredAt
	registry.services[frontendServiceKey("smallphone", "stable")] = state
	registry.mu.Unlock()

	slot, ok, err := registry.GetSlotView("smallphone", "stable")
	if err != nil {
		t.Fatalf("get slot view: %v", err)
	}
	if !ok {
		t.Fatal("expected stable slot")
	}
	if slot.Service.Status != FrontendServiceStatusStale {
		t.Fatalf("service status = %q, want stale", slot.Service.Status)
	}
}
