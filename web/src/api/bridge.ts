import api from './client';
import { FRONTEND_SLOT_OPTIONS } from '@/lib/webPlatform';

export interface BridgeAdapter {
  platform: string;
  project: string;
  capabilities: string[];
  connected_at?: string;
}

export type FrontendServiceStatus = 'offline' | 'online' | 'stale' | string;

export interface FrontendServiceState {
  app_id: string;
  slot: string;
  service_id?: string;
  status: FrontendServiceStatus;
  url?: string;
  api_base?: string;
  version?: string;
  build?: string;
  metadata?: Record<string, string>;
  registered_at?: string;
  last_seen_at?: string;
  expires_at?: string;
  heartbeat_ttl_seconds?: number;
}

export interface FrontendSlot {
  slot: string;
  label?: string;
  url?: string;
  api_base?: string;
  adapter_platform?: string;
  enabled?: boolean;
  service?: FrontendServiceState;
  metadata?: Record<string, string>;
  created_at?: string;
  updated_at?: string;
}

export interface FrontendApp {
  id: string;
  name: string;
  project: string;
  description?: string;
  metadata?: Record<string, string>;
  slots?: Record<string, FrontendSlot>;
  created_at?: string;
  updated_at?: string;
}

export interface FrontendService extends FrontendSlot {
  app_id: string;
  app_name: string;
  project: string;
  service: FrontendServiceState;
}

export function isBrowserTabBridgeAdapter(platform: string) {
  return platform.toLowerCase().split('-').includes('tab');
}

export const listBridgeAdapters = async () => {
  const data = await api.get<{ adapters: BridgeAdapter[] }>('/bridge/adapters');
  return {
    adapters: (data.adapters || []).filter((adapter) => !isBrowserTabBridgeAdapter(adapter.platform)),
  };
};

export const listFrontendApps = () => api.get<{ apps: FrontendApp[] }>('/apps');

export const listFrontendSlots = (appID: string) => (
  api.get<{ slots: FrontendSlot[] }>(`/apps/${encodeURIComponent(appID)}/slots`)
);

function slotRank(slot: string) {
  const idx = FRONTEND_SLOT_OPTIONS.indexOf(slot);
  return idx === -1 ? Number.MAX_SAFE_INTEGER : idx;
}

function slotServiceState(appID: string, slot: FrontendSlot): FrontendServiceState {
  return slot.service || {
    app_id: appID,
    slot: slot.slot,
    status: 'offline',
  };
}

function slotsFromApps(apps: FrontendApp[], projectName?: string): FrontendService[] {
  const normalizedProject = (projectName || '').trim();
  const matchingApps = normalizedProject
    ? apps.filter((app) => app.project === normalizedProject || app.id === normalizedProject)
    : apps;
  const sourceApps = matchingApps;
  const services = sourceApps.flatMap((app) => (
    Object.values(app.slots || {}).map((slot) => ({
      ...slot,
      app_id: app.id,
      app_name: app.name,
      project: app.project,
      service: slotServiceState(app.id, slot),
    }))
  ));
  return services
    .filter((slot) => slot.slot && slot.enabled !== false)
    .sort((a, b) => {
      const rankDelta = slotRank(a.slot) - slotRank(b.slot);
      if (rankDelta !== 0) return rankDelta;
      return `${a.app_id}:${a.slot}`.localeCompare(`${b.app_id}:${b.slot}`);
    });
}

export async function listProjectFrontendSlots(projectName?: string): Promise<FrontendSlot[]> {
  try {
    const data = await listFrontendApps();
    return slotsFromApps(data.apps || [], projectName);
  } catch {
    return [];
  }
}

export async function listFrontendServices(): Promise<FrontendService[]> {
  try {
    const data = await listFrontendApps();
    return slotsFromApps(data.apps || []);
  } catch {
    const data = await listBridgeAdapters();
    return (data.adapters || []).map((adapter) => ({
      app_id: adapter.platform,
      app_name: adapter.platform,
      project: adapter.project,
      slot: adapter.platform,
      adapter_platform: adapter.platform,
      enabled: true,
      updated_at: adapter.connected_at,
      service: {
        app_id: adapter.platform,
        slot: adapter.platform,
        service_id: adapter.platform,
        status: 'online',
        registered_at: adapter.connected_at,
        last_seen_at: adapter.connected_at,
      },
      metadata: {
        capabilities: adapter.capabilities?.join(',') || '',
      },
    }));
  }
}
