export const WEBNEW_BRIDGE_PLATFORM = 'webnew';
export const DEFAULT_FRONTEND_SLOT = 'stable';
export const FRONTEND_SLOT_OPTIONS = ['stable', 'beta', 'web2', 'web3', 'web4', 'web5'];
export const DEFAULT_WEB_BRIDGE_PLATFORM = DEFAULT_FRONTEND_SLOT;
export const WEB_BRIDGE_PLATFORM_OPTIONS = FRONTEND_SLOT_OPTIONS;
export const WEB_BRIDGE_USER_ID = 'web-admin';
export const WEB_BRIDGE_USER_NAME = 'Web Admin';

export type FrontendSlotLike = {
  slot: string;
  label?: string;
  adapter_platform?: string;
  enabled?: boolean;
};

const STORAGE_KEY = 'cc_frontend_slot';
const LEGACY_STORAGE_KEY = 'cc_web2_bridge_platform';
const LEGACY_WEB_BRIDGE_PLATFORM_OPTIONS: string[] = [];
const FRONTEND_ID_PATTERN = /^[a-z0-9][a-z0-9_-]{0,79}$/;

function isLegacyWebRoute(value: string) {
  return value === 'bridge' || value === 'web';
}

export function normalizeWebBridgePlatform(value: string | null | undefined) {
  const normalized = (value || '').trim().toLowerCase();
  if (!normalized || isLegacyWebRoute(normalized)) return DEFAULT_FRONTEND_SLOT;
  if (FRONTEND_ID_PATTERN.test(normalized)) return normalized;
  return DEFAULT_WEB_BRIDGE_PLATFORM;
}

export function getWebBridgeTransportPlatform(route = DEFAULT_WEB_BRIDGE_PLATFORM) {
  return normalizeWebBridgePlatform(route);
}

export function frontendSlotOptionsFrom(slots: FrontendSlotLike[] = []) {
  const merged = new Map<string, FrontendSlotLike>();
  FRONTEND_SLOT_OPTIONS.forEach((slot) => merged.set(slot, { slot, enabled: true }));
  slots
    .filter((slot) => slot?.slot && slot.enabled !== false)
    .forEach((slot) => {
      const normalized = normalizeWebBridgePlatform(slot.slot);
      merged.set(normalized, { ...slot, slot: normalized });
    });
  return Array.from(merged.values()).sort((a, b) => {
    const ai = FRONTEND_SLOT_OPTIONS.indexOf(a.slot);
    const bi = FRONTEND_SLOT_OPTIONS.indexOf(b.slot);
    if (ai !== -1 || bi !== -1) return (ai === -1 ? Number.MAX_SAFE_INTEGER : ai) - (bi === -1 ? Number.MAX_SAFE_INTEGER : bi);
    return a.slot.localeCompare(b.slot);
  });
}

export function selectedFrontendSlot(slots: FrontendSlotLike[], value: string | null | undefined) {
  const options = frontendSlotOptionsFrom(slots);
  const normalized = normalizeWebBridgePlatform(value);
  return options.find((slot) => slot.slot === normalized) || options[0] || { slot: DEFAULT_FRONTEND_SLOT, enabled: true };
}

export function frontendServicePlatformForSlot(slots: FrontendSlotLike[], value: string | null | undefined) {
  const slot = selectedFrontendSlot(slots, value);
  return normalizeWebBridgePlatform(slot.slot);
}

export function getInitialWebBridgePlatform() {
  if (typeof window === 'undefined') return DEFAULT_WEB_BRIDGE_PLATFORM;
  const params = new URLSearchParams(window.location.search);
  const fromURL = params.get('frontend_slot') || params.get('slot') || params.get('web_platform') || params.get('platform');
  if (fromURL) return normalizeWebBridgePlatform(fromURL);
  return normalizeWebBridgePlatform(window.localStorage.getItem(STORAGE_KEY) || window.localStorage.getItem(LEGACY_STORAGE_KEY));
}

export function persistWebBridgePlatform(platform: string) {
  if (typeof window === 'undefined') return;
  const normalized = normalizeWebBridgePlatform(platform);
  window.localStorage.setItem(STORAGE_KEY, normalized);
  window.localStorage.removeItem(LEGACY_STORAGE_KEY);
}

export function platformFromSessionKey(sessionKey: string) {
  const idx = sessionKey.indexOf(':');
  return idx > 0 ? sessionKey.slice(0, idx) : '';
}

export function isWebBridgeSessionKey(sessionKey: string) {
  return sessionKey.startsWith(`${WEBNEW_BRIDGE_PLATFORM}:`) || isLegacyWebBridgeSessionKey(sessionKey);
}

export function isLegacyWebBridgeSessionKey(sessionKey: string) {
  const platform = platformFromSessionKey(sessionKey).toLowerCase();
  if (!sessionKey.includes(`:${WEB_BRIDGE_USER_ID}:`)) return false;
  return isLegacyWebRoute(platform) || FRONTEND_SLOT_OPTIONS.includes(platform);
}

export function webSessionKey(projectName: string) {
  return `${WEBNEW_BRIDGE_PLATFORM}:${WEB_BRIDGE_USER_ID}:${projectName}`;
}

export function webRouteSessionKey(projectName: string, platform = DEFAULT_WEB_BRIDGE_PLATFORM) {
  return `${normalizeWebBridgePlatform(platform)}:${WEB_BRIDGE_USER_ID}:${projectName}`;
}

export function legacyWebSessionKeys(projectName: string, preferredPlatform = DEFAULT_WEB_BRIDGE_PLATFORM) {
  const keys = [
    webRouteSessionKey(projectName, preferredPlatform),
    `bridge:${WEB_BRIDGE_USER_ID}:${projectName}`,
    `web:${WEB_BRIDGE_USER_ID}:${projectName}`,
    ...WEB_BRIDGE_PLATFORM_OPTIONS.map((platform) => webRouteSessionKey(projectName, platform)),
    ...LEGACY_WEB_BRIDGE_PLATFORM_OPTIONS.map((platform) => `${platform}:${WEB_BRIDGE_USER_ID}:${projectName}`),
  ];
  return Array.from(new Set(keys));
}
