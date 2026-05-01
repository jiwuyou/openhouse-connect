package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var frontendIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)
var frontendServiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)

const (
	FrontendServiceStatusOffline = "offline"
	FrontendServiceStatusOnline  = "online"
	FrontendServiceStatusStale   = "stale"

	defaultFrontendServiceHeartbeatTTLSeconds = 60
	maxFrontendServiceHeartbeatTTLSeconds     = 24 * 60 * 60
)

// FrontendAppRegistry stores user-facing application entry points separately
// from config.toml so apps and frontend slots can be changed without restart.
type FrontendAppRegistry struct {
	path     string
	mu       sync.RWMutex
	services map[string]FrontendServiceState
}

type frontendRegistryFile struct {
	Version int                    `json:"version"`
	Apps    map[string]FrontendApp `json:"apps"`
}

// FrontendApp groups stable/beta/dev frontend slots for one application.
type FrontendApp struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Project     string                  `json:"project"`
	Description string                  `json:"description,omitempty"`
	Metadata    map[string]string       `json:"metadata,omitempty"`
	Slots       map[string]FrontendSlot `json:"slots,omitempty"`
	CreatedAt   time.Time               `json:"created_at"`
	UpdatedAt   time.Time               `json:"updated_at"`
}

// FrontendSlot describes one user-facing frontend entry point.
type FrontendSlot struct {
	Slot            string            `json:"slot"`
	Label           string            `json:"label,omitempty"`
	URL             string            `json:"url"`
	APIBase         string            `json:"api_base,omitempty"`
	AdapterPlatform string            `json:"adapter_platform,omitempty"`
	Enabled         bool              `json:"enabled"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// FrontendAppView is the management API view for an app. It merges persistent
// slot configuration with in-memory frontend service runtime state.
type FrontendAppView struct {
	ID          string                      `json:"id"`
	Name        string                      `json:"name"`
	Project     string                      `json:"project"`
	Description string                      `json:"description,omitempty"`
	Metadata    map[string]string           `json:"metadata,omitempty"`
	Slots       map[string]FrontendSlotView `json:"slots,omitempty"`
	CreatedAt   time.Time                   `json:"created_at"`
	UpdatedAt   time.Time                   `json:"updated_at"`
}

// FrontendSlotView is the management API view for one configured slot plus the
// backend frontend service currently occupying it, if any.
type FrontendSlotView struct {
	Slot            string               `json:"slot"`
	Label           string               `json:"label,omitempty"`
	URL             string               `json:"url"`
	APIBase         string               `json:"api_base,omitempty"`
	AdapterPlatform string               `json:"adapter_platform,omitempty"`
	Enabled         bool                 `json:"enabled"`
	Metadata        map[string]string    `json:"metadata,omitempty"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
	Service         FrontendServiceState `json:"service"`
}

// FrontendServiceState is runtime-only state for a backend frontend service
// registered into one slot. It is intentionally not persisted in the registry
// JSON file; service processes re-register and heartbeat after restart.
type FrontendServiceState struct {
	AppID               string            `json:"app_id"`
	Slot                string            `json:"slot"`
	ServiceID           string            `json:"service_id,omitempty"`
	Status              string            `json:"status"`
	URL                 string            `json:"url,omitempty"`
	APIBase             string            `json:"api_base,omitempty"`
	Version             string            `json:"version,omitempty"`
	Build               string            `json:"build,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	RegisteredAt        *time.Time        `json:"registered_at,omitempty"`
	LastSeenAt          *time.Time        `json:"last_seen_at,omitempty"`
	ExpiresAt           *time.Time        `json:"expires_at,omitempty"`
	HeartbeatTTLSeconds int               `json:"heartbeat_ttl_seconds,omitempty"`
}

// FrontendServiceSummary is the flattened management status view used by
// /api/v1/status so dashboards can monitor slots without inspecting bridge tabs.
type FrontendServiceSummary struct {
	AppID     string               `json:"app_id"`
	AppName   string               `json:"app_name"`
	Project   string               `json:"project"`
	Slot      string               `json:"slot"`
	Label     string               `json:"label,omitempty"`
	URL       string               `json:"url"`
	APIBase   string               `json:"api_base,omitempty"`
	Enabled   bool                 `json:"enabled"`
	Service   FrontendServiceState `json:"service"`
	UpdatedAt time.Time            `json:"updated_at"`
}

type FrontendAppUpdate struct {
	Name        *string
	Project     *string
	Description *string
	Metadata    map[string]string
}

type FrontendSlotUpdate struct {
	Label           *string
	URL             *string
	APIBase         *string
	AdapterPlatform *string
	Enabled         *bool
	Metadata        map[string]string
}

type FrontendServiceRegistration struct {
	ServiceID           string
	URL                 string
	APIBase             string
	Version             string
	Build               string
	HeartbeatTTLSeconds int
	Metadata            map[string]string
}

type FrontendServiceHeartbeat struct {
	ServiceID           string
	URL                 string
	APIBase             string
	Version             string
	Build               string
	HeartbeatTTLSeconds int
	Metadata            map[string]string
}

func NewFrontendAppRegistry(path string) *FrontendAppRegistry {
	return &FrontendAppRegistry{
		path:     path,
		services: map[string]FrontendServiceState{},
	}
}

func (r *FrontendAppRegistry) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

func (r *FrontendAppRegistry) ListApps() ([]FrontendApp, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := r.loadLocked()
	if err != nil {
		return nil, err
	}
	apps := make([]FrontendApp, 0, len(data.Apps))
	for _, app := range data.Apps {
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool {
		return strings.ToLower(apps[i].ID) < strings.ToLower(apps[j].ID)
	})
	return apps, nil
}

func (r *FrontendAppRegistry) ListAppViews() ([]FrontendAppView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := r.loadLocked()
	if err != nil {
		return nil, err
	}
	apps := make([]FrontendAppView, 0, len(data.Apps))
	for _, app := range data.Apps {
		apps = append(apps, r.frontendAppViewLocked(app))
	}
	sort.Slice(apps, func(i, j int) bool {
		return strings.ToLower(apps[i].ID) < strings.ToLower(apps[j].ID)
	})
	return apps, nil
}

func (r *FrontendAppRegistry) GetApp(id string) (FrontendApp, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = normalizeFrontendID(id)
	data, err := r.loadLocked()
	if err != nil {
		return FrontendApp{}, false, err
	}
	app, ok := data.Apps[id]
	return app, ok, nil
}

func (r *FrontendAppRegistry) GetAppView(id string) (FrontendAppView, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = normalizeFrontendID(id)
	data, err := r.loadLocked()
	if err != nil {
		return FrontendAppView{}, false, err
	}
	app, ok := data.Apps[id]
	if !ok {
		return FrontendAppView{}, false, nil
	}
	return r.frontendAppViewLocked(app), true, nil
}

func (r *FrontendAppRegistry) CreateApp(app FrontendApp) (FrontendApp, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := r.loadLocked()
	if err != nil {
		return FrontendApp{}, err
	}
	if app.ID == "" {
		app.ID = slugFrontendID(app.Name)
	}
	app.ID = normalizeFrontendID(app.ID)
	if err := validateFrontendApp(app); err != nil {
		return FrontendApp{}, err
	}
	if _, exists := data.Apps[app.ID]; exists {
		return FrontendApp{}, fmt.Errorf("frontend app %q already exists", app.ID)
	}
	now := time.Now().UTC()
	app.CreatedAt = now
	app.UpdatedAt = now
	if app.Slots == nil {
		app.Slots = map[string]FrontendSlot{}
	}
	data.Apps[app.ID] = app
	if err := r.saveLocked(data); err != nil {
		return FrontendApp{}, err
	}
	return app, nil
}

func (r *FrontendAppRegistry) UpdateApp(id string, update FrontendAppUpdate) (FrontendApp, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = normalizeFrontendID(id)
	data, err := r.loadLocked()
	if err != nil {
		return FrontendApp{}, err
	}
	app, ok := data.Apps[id]
	if !ok {
		return FrontendApp{}, fmt.Errorf("frontend app %q not found", id)
	}
	if update.Name != nil {
		app.Name = strings.TrimSpace(*update.Name)
	}
	if update.Project != nil {
		app.Project = strings.TrimSpace(*update.Project)
	}
	if update.Description != nil {
		app.Description = strings.TrimSpace(*update.Description)
	}
	if update.Metadata != nil {
		app.Metadata = update.Metadata
	}
	if err := validateFrontendApp(app); err != nil {
		return FrontendApp{}, err
	}
	app.UpdatedAt = time.Now().UTC()
	data.Apps[id] = app
	if err := r.saveLocked(data); err != nil {
		return FrontendApp{}, err
	}
	return app, nil
}

func (r *FrontendAppRegistry) DeleteApp(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = normalizeFrontendID(id)
	data, err := r.loadLocked()
	if err != nil {
		return err
	}
	if _, ok := data.Apps[id]; !ok {
		return fmt.Errorf("frontend app %q not found", id)
	}
	delete(data.Apps, id)
	r.ensureServicesLocked()
	for key, state := range r.services {
		if normalizeFrontendID(state.AppID) == id {
			delete(r.services, key)
		}
	}
	return r.saveLocked(data)
}

func (r *FrontendAppRegistry) ListSlots(appID string) ([]FrontendSlot, error) {
	app, ok, err := r.GetApp(appID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("frontend app %q not found", appID)
	}
	slots := make([]FrontendSlot, 0, len(app.Slots))
	for _, slot := range app.Slots {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool {
		return strings.ToLower(slots[i].Slot) < strings.ToLower(slots[j].Slot)
	})
	return slots, nil
}

func (r *FrontendAppRegistry) ListSlotViews(appID string) ([]FrontendSlotView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	appID = normalizeFrontendID(appID)
	data, err := r.loadLocked()
	if err != nil {
		return nil, err
	}
	app, ok := data.Apps[appID]
	if !ok {
		return nil, fmt.Errorf("frontend app %q not found", appID)
	}
	slots := make([]FrontendSlotView, 0, len(app.Slots))
	for _, slot := range app.Slots {
		slots = append(slots, r.frontendSlotViewLocked(appID, slot))
	}
	sort.Slice(slots, func(i, j int) bool {
		return strings.ToLower(slots[i].Slot) < strings.ToLower(slots[j].Slot)
	})
	return slots, nil
}

func (r *FrontendAppRegistry) GetSlotView(appID, slotName string) (FrontendSlotView, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	appID = normalizeFrontendID(appID)
	slotName = normalizeFrontendID(slotName)
	data, err := r.loadLocked()
	if err != nil {
		return FrontendSlotView{}, false, err
	}
	app, ok := data.Apps[appID]
	if !ok {
		return FrontendSlotView{}, false, fmt.Errorf("frontend app %q not found", appID)
	}
	slot, ok := app.Slots[slotName]
	if !ok {
		return FrontendSlotView{}, false, nil
	}
	return r.frontendSlotViewLocked(appID, slot), true, nil
}

func (r *FrontendAppRegistry) UpsertSlot(appID string, slot FrontendSlot) (FrontendSlot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	appID = normalizeFrontendID(appID)
	data, err := r.loadLocked()
	if err != nil {
		return FrontendSlot{}, err
	}
	app, ok := data.Apps[appID]
	if !ok {
		return FrontendSlot{}, fmt.Errorf("frontend app %q not found", appID)
	}
	slot.Slot = normalizeFrontendID(slot.Slot)
	if err := validateFrontendSlot(slot); err != nil {
		return FrontendSlot{}, err
	}
	now := time.Now().UTC()
	if app.Slots == nil {
		app.Slots = map[string]FrontendSlot{}
	}
	if existing, exists := app.Slots[slot.Slot]; exists {
		slot.CreatedAt = existing.CreatedAt
	} else {
		slot.CreatedAt = now
	}
	slot.UpdatedAt = now
	app.Slots[slot.Slot] = slot
	app.UpdatedAt = now
	data.Apps[appID] = app
	if err := r.saveLocked(data); err != nil {
		return FrontendSlot{}, err
	}
	return slot, nil
}

func (r *FrontendAppRegistry) UpdateSlot(appID, slotName string, update FrontendSlotUpdate) (FrontendSlot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	appID = normalizeFrontendID(appID)
	slotName = normalizeFrontendID(slotName)
	data, err := r.loadLocked()
	if err != nil {
		return FrontendSlot{}, err
	}
	app, ok := data.Apps[appID]
	if !ok {
		return FrontendSlot{}, fmt.Errorf("frontend app %q not found", appID)
	}
	slot, ok := app.Slots[slotName]
	if !ok {
		return FrontendSlot{}, fmt.Errorf("frontend slot %q not found", slotName)
	}
	if update.Label != nil {
		slot.Label = strings.TrimSpace(*update.Label)
	}
	if update.URL != nil {
		slot.URL = strings.TrimSpace(*update.URL)
	}
	if update.APIBase != nil {
		slot.APIBase = strings.TrimSpace(*update.APIBase)
	}
	if update.AdapterPlatform != nil {
		slot.AdapterPlatform = strings.TrimSpace(*update.AdapterPlatform)
	}
	if update.Enabled != nil {
		slot.Enabled = *update.Enabled
	}
	if update.Metadata != nil {
		slot.Metadata = update.Metadata
	}
	if err := validateFrontendSlot(slot); err != nil {
		return FrontendSlot{}, err
	}
	now := time.Now().UTC()
	slot.UpdatedAt = now
	app.Slots[slotName] = slot
	app.UpdatedAt = now
	data.Apps[appID] = app
	if err := r.saveLocked(data); err != nil {
		return FrontendSlot{}, err
	}
	return slot, nil
}

func (r *FrontendAppRegistry) DeleteSlot(appID, slotName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	appID = normalizeFrontendID(appID)
	slotName = normalizeFrontendID(slotName)
	data, err := r.loadLocked()
	if err != nil {
		return err
	}
	app, ok := data.Apps[appID]
	if !ok {
		return fmt.Errorf("frontend app %q not found", appID)
	}
	if _, ok := app.Slots[slotName]; !ok {
		return fmt.Errorf("frontend slot %q not found", slotName)
	}
	delete(app.Slots, slotName)
	r.ensureServicesLocked()
	delete(r.services, frontendServiceKey(appID, slotName))
	app.UpdatedAt = time.Now().UTC()
	data.Apps[appID] = app
	return r.saveLocked(data)
}

func (r *FrontendAppRegistry) PromoteSlot(appID, sourceSlot, targetSlot string) (FrontendSlot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	appID = normalizeFrontendID(appID)
	sourceSlot = normalizeFrontendID(sourceSlot)
	targetSlot = normalizeFrontendID(targetSlot)
	if targetSlot == "" {
		targetSlot = "stable"
	}
	data, err := r.loadLocked()
	if err != nil {
		return FrontendSlot{}, err
	}
	app, ok := data.Apps[appID]
	if !ok {
		return FrontendSlot{}, fmt.Errorf("frontend app %q not found", appID)
	}
	source, ok := app.Slots[sourceSlot]
	if !ok {
		return FrontendSlot{}, fmt.Errorf("frontend slot %q not found", sourceSlot)
	}
	if app.Slots == nil {
		app.Slots = map[string]FrontendSlot{}
	}
	promoted := source
	promoted.Slot = targetSlot
	if promoted.Label == "" || promoted.Label == source.Label {
		promoted.Label = targetSlot
	}
	now := time.Now().UTC()
	if existing, exists := app.Slots[targetSlot]; exists {
		promoted.CreatedAt = existing.CreatedAt
	} else {
		promoted.CreatedAt = now
	}
	promoted.UpdatedAt = now
	promoted.Metadata = cloneStringMap(source.Metadata)
	if promoted.Metadata == nil {
		promoted.Metadata = map[string]string{}
	}
	promoted.Metadata["promoted_from"] = sourceSlot
	promoted.Metadata["promoted_at"] = now.Format(time.RFC3339)
	if err := validateFrontendSlot(promoted); err != nil {
		return FrontendSlot{}, err
	}
	app.Slots[targetSlot] = promoted
	app.UpdatedAt = now
	data.Apps[appID] = app
	if err := r.saveLocked(data); err != nil {
		return FrontendSlot{}, err
	}
	return promoted, nil
}

func (r *FrontendAppRegistry) RegisterSlotService(appID, slotName string, reg FrontendServiceRegistration) (FrontendServiceState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	appID = normalizeFrontendID(appID)
	slotName = normalizeFrontendID(slotName)
	data, err := r.loadLocked()
	if err != nil {
		return FrontendServiceState{}, err
	}
	app, ok := data.Apps[appID]
	if !ok {
		return FrontendServiceState{}, fmt.Errorf("frontend app %q not found", appID)
	}
	slot, ok := app.Slots[slotName]
	if !ok {
		return FrontendServiceState{}, fmt.Errorf("frontend slot %q not found", slotName)
	}

	serviceID := strings.TrimSpace(reg.ServiceID)
	if serviceID == "" {
		serviceID = defaultFrontendServiceID(appID, slotName)
	}
	if err := validateFrontendServiceID(serviceID); err != nil {
		return FrontendServiceState{}, err
	}
	ttlSeconds := normalizeFrontendServiceTTLSeconds(reg.HeartbeatTTLSeconds)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(ttlSeconds) * time.Second)

	state := FrontendServiceState{
		AppID:               appID,
		Slot:                slot.Slot,
		ServiceID:           serviceID,
		Status:              FrontendServiceStatusOnline,
		URL:                 strings.TrimSpace(reg.URL),
		APIBase:             strings.TrimSpace(reg.APIBase),
		Version:             strings.TrimSpace(reg.Version),
		Build:               strings.TrimSpace(reg.Build),
		Metadata:            cloneStringMap(reg.Metadata),
		RegisteredAt:        timePtr(now),
		LastSeenAt:          timePtr(now),
		ExpiresAt:           timePtr(expiresAt),
		HeartbeatTTLSeconds: ttlSeconds,
	}

	r.ensureServicesLocked()
	key := frontendServiceKey(appID, slotName)
	if existing, ok := r.services[key]; ok && existing.ServiceID == serviceID && existing.RegisteredAt != nil {
		state.RegisteredAt = cloneTimePtr(existing.RegisteredAt)
	}
	r.services[key] = state
	return cloneFrontendServiceState(state), nil
}

func (r *FrontendAppRegistry) HeartbeatSlotService(appID, slotName string, heartbeat FrontendServiceHeartbeat) (FrontendServiceState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	appID = normalizeFrontendID(appID)
	slotName = normalizeFrontendID(slotName)
	data, err := r.loadLocked()
	if err != nil {
		return FrontendServiceState{}, err
	}
	app, ok := data.Apps[appID]
	if !ok {
		return FrontendServiceState{}, fmt.Errorf("frontend app %q not found", appID)
	}
	if _, ok := app.Slots[slotName]; !ok {
		return FrontendServiceState{}, fmt.Errorf("frontend slot %q not found", slotName)
	}

	r.ensureServicesLocked()
	key := frontendServiceKey(appID, slotName)
	state, ok := r.services[key]
	if !ok {
		return FrontendServiceState{}, fmt.Errorf("frontend service for slot %q is not registered", slotName)
	}
	serviceID := strings.TrimSpace(heartbeat.ServiceID)
	if serviceID != "" && state.ServiceID != "" && serviceID != state.ServiceID {
		return FrontendServiceState{}, fmt.Errorf("frontend service id %q does not match registered service %q", serviceID, state.ServiceID)
	}
	if serviceID != "" {
		if err := validateFrontendServiceID(serviceID); err != nil {
			return FrontendServiceState{}, err
		}
		state.ServiceID = serviceID
	}
	if ttl := normalizeOptionalFrontendServiceTTLSeconds(heartbeat.HeartbeatTTLSeconds); ttl > 0 {
		state.HeartbeatTTLSeconds = ttl
	}
	if state.HeartbeatTTLSeconds <= 0 {
		state.HeartbeatTTLSeconds = defaultFrontendServiceHeartbeatTTLSeconds
	}
	if url := strings.TrimSpace(heartbeat.URL); url != "" {
		state.URL = url
	}
	if apiBase := strings.TrimSpace(heartbeat.APIBase); apiBase != "" {
		state.APIBase = apiBase
	}
	if version := strings.TrimSpace(heartbeat.Version); version != "" {
		state.Version = version
	}
	if build := strings.TrimSpace(heartbeat.Build); build != "" {
		state.Build = build
	}
	if heartbeat.Metadata != nil {
		state.Metadata = cloneStringMap(heartbeat.Metadata)
	}

	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(state.HeartbeatTTLSeconds) * time.Second)
	state.Status = FrontendServiceStatusOnline
	state.LastSeenAt = timePtr(now)
	state.ExpiresAt = timePtr(expiresAt)
	r.services[key] = state
	return cloneFrontendServiceState(state), nil
}

func (r *FrontendAppRegistry) ClearSlotService(appID, slotName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	appID = normalizeFrontendID(appID)
	slotName = normalizeFrontendID(slotName)
	data, err := r.loadLocked()
	if err != nil {
		return err
	}
	app, ok := data.Apps[appID]
	if !ok {
		return fmt.Errorf("frontend app %q not found", appID)
	}
	if _, ok := app.Slots[slotName]; !ok {
		return fmt.Errorf("frontend slot %q not found", slotName)
	}
	r.ensureServicesLocked()
	delete(r.services, frontendServiceKey(appID, slotName))
	return nil
}

func (r *FrontendAppRegistry) ListServiceSummaries() ([]FrontendServiceSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := r.loadLocked()
	if err != nil {
		return nil, err
	}
	summaries := make([]FrontendServiceSummary, 0)
	for _, app := range data.Apps {
		for _, slot := range app.Slots {
			summaries = append(summaries, FrontendServiceSummary{
				AppID:     app.ID,
				AppName:   app.Name,
				Project:   app.Project,
				Slot:      slot.Slot,
				Label:     slot.Label,
				URL:       slot.URL,
				APIBase:   slot.APIBase,
				Enabled:   slot.Enabled,
				Service:   r.serviceStateLocked(app.ID, slot.Slot),
				UpdatedAt: slot.UpdatedAt,
			})
		}
	}
	sort.Slice(summaries, func(i, j int) bool {
		if !strings.EqualFold(summaries[i].AppID, summaries[j].AppID) {
			return strings.ToLower(summaries[i].AppID) < strings.ToLower(summaries[j].AppID)
		}
		return strings.ToLower(summaries[i].Slot) < strings.ToLower(summaries[j].Slot)
	})
	return summaries, nil
}

func (r *FrontendAppRegistry) loadLocked() (frontendRegistryFile, error) {
	data := frontendRegistryFile{Version: 1, Apps: map[string]FrontendApp{}}
	if r == nil || strings.TrimSpace(r.path) == "" {
		return data, fmt.Errorf("frontend app registry path is not configured")
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return data, nil
		}
		return data, fmt.Errorf("read frontend app registry: %w", err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return data, nil
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return data, fmt.Errorf("parse frontend app registry: %w", err)
	}
	if data.Version == 0 {
		data.Version = 1
	}
	if data.Apps == nil {
		data.Apps = map[string]FrontendApp{}
	}
	return data, nil
}

func (r *FrontendAppRegistry) saveLocked(data frontendRegistryFile) error {
	if r == nil || strings.TrimSpace(r.path) == "" {
		return fmt.Errorf("frontend app registry path is not configured")
	}
	if data.Version == 0 {
		data.Version = 1
	}
	if data.Apps == nil {
		data.Apps = map[string]FrontendApp{}
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("create frontend app registry dir: %w", err)
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode frontend app registry: %w", err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".frontend-apps-*.tmp")
	if err != nil {
		return fmt.Errorf("create frontend app registry temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write frontend app registry: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync frontend app registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close frontend app registry: %w", err)
	}
	if err := os.Rename(tmpPath, r.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replace frontend app registry: %w", err)
	}
	return nil
}

func validateFrontendApp(app FrontendApp) error {
	if !frontendIDPattern.MatchString(app.ID) {
		return fmt.Errorf("invalid frontend app id %q", app.ID)
	}
	if strings.TrimSpace(app.Name) == "" {
		return fmt.Errorf("frontend app name is required")
	}
	if strings.TrimSpace(app.Project) == "" {
		return fmt.Errorf("frontend app project is required")
	}
	return nil
}

func validateFrontendSlot(slot FrontendSlot) error {
	if !frontendIDPattern.MatchString(slot.Slot) {
		return fmt.Errorf("invalid frontend slot %q", slot.Slot)
	}
	if strings.TrimSpace(slot.URL) == "" {
		return fmt.Errorf("frontend slot url is required")
	}
	return nil
}

func validateFrontendServiceID(serviceID string) error {
	if !frontendServiceIDPattern.MatchString(serviceID) {
		return fmt.Errorf("invalid frontend service id %q", serviceID)
	}
	return nil
}

func (r *FrontendAppRegistry) frontendAppViewLocked(app FrontendApp) FrontendAppView {
	view := FrontendAppView{
		ID:          app.ID,
		Name:        app.Name,
		Project:     app.Project,
		Description: app.Description,
		Metadata:    cloneStringMap(app.Metadata),
		Slots:       map[string]FrontendSlotView{},
		CreatedAt:   app.CreatedAt,
		UpdatedAt:   app.UpdatedAt,
	}
	for name, slot := range app.Slots {
		view.Slots[name] = r.frontendSlotViewLocked(app.ID, slot)
	}
	return view
}

func (r *FrontendAppRegistry) frontendSlotViewLocked(appID string, slot FrontendSlot) FrontendSlotView {
	return FrontendSlotView{
		Slot:            slot.Slot,
		Label:           slot.Label,
		URL:             slot.URL,
		APIBase:         slot.APIBase,
		AdapterPlatform: slot.AdapterPlatform,
		Enabled:         slot.Enabled,
		Metadata:        cloneStringMap(slot.Metadata),
		CreatedAt:       slot.CreatedAt,
		UpdatedAt:       slot.UpdatedAt,
		Service:         r.serviceStateLocked(appID, slot.Slot),
	}
}

func (r *FrontendAppRegistry) serviceStateLocked(appID, slotName string) FrontendServiceState {
	r.ensureServicesLocked()
	state, ok := r.services[frontendServiceKey(appID, slotName)]
	if !ok {
		return FrontendServiceState{
			AppID:  normalizeFrontendID(appID),
			Slot:   normalizeFrontendID(slotName),
			Status: FrontendServiceStatusOffline,
		}
	}
	state = cloneFrontendServiceState(state)
	if frontendServiceExpired(state, time.Now().UTC()) {
		state.Status = FrontendServiceStatusStale
	}
	return state
}

func (r *FrontendAppRegistry) ensureServicesLocked() {
	if r.services == nil {
		r.services = map[string]FrontendServiceState{}
	}
}

func frontendServiceExpired(state FrontendServiceState, now time.Time) bool {
	return state.Status == FrontendServiceStatusOnline && state.ExpiresAt != nil && now.After(*state.ExpiresAt)
}

func frontendServiceKey(appID, slotName string) string {
	return normalizeFrontendID(appID) + "\x00" + normalizeFrontendID(slotName)
}

func defaultFrontendServiceID(appID, slotName string) string {
	return normalizeFrontendID(appID) + "/" + normalizeFrontendID(slotName)
}

func normalizeFrontendServiceTTLSeconds(ttl int) int {
	if ttl <= 0 {
		return defaultFrontendServiceHeartbeatTTLSeconds
	}
	if ttl > maxFrontendServiceHeartbeatTTLSeconds {
		return maxFrontendServiceHeartbeatTTLSeconds
	}
	return ttl
}

func normalizeOptionalFrontendServiceTTLSeconds(ttl int) int {
	if ttl <= 0 {
		return 0
	}
	return normalizeFrontendServiceTTLSeconds(ttl)
}

func normalizeFrontendID(value string) string {
	return strings.TrimSpace(value)
}

func slugFrontendID(value string) string {
	raw := strings.ToLower(strings.TrimSpace(value))
	if raw == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '_' || r == '-':
			if !lastDash && b.Len() > 0 {
				b.WriteRune(r)
				lastDash = r == '-'
			}
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	result := strings.Trim(b.String(), "-_")
	if len(result) > 80 {
		result = strings.Trim(result[:80], "-_")
	}
	return result
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	output := make(map[string]string, len(input))
	for k, v := range input {
		output[k] = v
	}
	return output
}

func cloneFrontendServiceState(input FrontendServiceState) FrontendServiceState {
	output := input
	output.Metadata = cloneStringMap(input.Metadata)
	output.RegisteredAt = cloneTimePtr(input.RegisteredAt)
	output.LastSeenAt = cloneTimePtr(input.LastSeenAt)
	output.ExpiresAt = cloneTimePtr(input.ExpiresAt)
	return output
}

func cloneTimePtr(input *time.Time) *time.Time {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func timePtr(input time.Time) *time.Time {
	value := input
	return &value
}
