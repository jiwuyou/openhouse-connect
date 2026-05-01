import { useEffect, useRef, useCallback, useState } from 'react';
import api from '@/api/client';
import { DEFAULT_WEB_BRIDGE_PLATFORM, WEB_BRIDGE_USER_ID, WEB_BRIDGE_USER_NAME, normalizeWebBridgePlatform } from '@/lib/webPlatform';

type BridgeScoped = { session_key: string; session_id?: string };
type BridgeReplyScoped = BridgeScoped & { reply_ctx: string };

export type BridgeIncoming =
  | { type: 'register_ack'; ok: boolean; error?: string }
  | (BridgeReplyScoped & { type: 'reply'; content: string; format?: string })
  | (BridgeReplyScoped & { type: 'reply_stream'; delta: string; full_text: string; preview_handle?: string; done: boolean })
  | (BridgeReplyScoped & { type: 'card'; card: any })
  | (BridgeReplyScoped & { type: 'buttons'; content: string; buttons: { text: string; data: string }[][] })
  | (BridgeScoped & { type: 'typing_start' })
  | (BridgeScoped & { type: 'typing_stop' })
  | (BridgeReplyScoped & { type: 'preview_start'; ref_id: string; content: string })
  | (BridgeScoped & { type: 'update_message'; preview_handle: string; content: string })
  | (BridgeScoped & { type: 'delete_message'; preview_handle: string })
  | { type: 'error'; code: string; message: string }
  | { type: 'pong'; ts: number }
  | { type: string; [key: string]: any };

export interface BridgeConfig {
  port: number;
  path: string;
  token: string;
  frontend_path?: string;
  client_path?: string;
  frontend_token?: string;
  service_platform?: string;
}

export type BridgeStatus = 'connecting' | 'registering' | 'connected' | 'disconnected' | 'error';

export interface UseBridgeSocketOptions {
  bridgeCfg: BridgeConfig | null;
  platformName?: string;
  routeName?: string;
  sessionKey: string;
  sessionId?: string;
  routeKey?: string;
  projectName?: string;
  onMessage: (msg: BridgeIncoming) => void;
}

export function useBridgeSocket({ bridgeCfg, platformName = DEFAULT_WEB_BRIDGE_PLATFORM, routeName, sessionKey, sessionId, routeKey, projectName, onMessage }: UseBridgeSocketOptions) {
  const wsRef = useRef<WebSocket | null>(null);
  const onMessageRef = useRef(onMessage);
  onMessageRef.current = onMessage;
  const pingRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const [status, setStatus] = useState<BridgeStatus>('disconnected');
  const visibleRoute = normalizeWebBridgePlatform(routeName || platformName || DEFAULT_WEB_BRIDGE_PLATFORM);
  const servicePlatform = normalizeWebBridgePlatform(platformName || visibleRoute);
  const transportSessionKey = routeKey || sessionKey;

  const send = useCallback((data: Record<string, any>) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(data));
    }
  }, []);

  const sendMessage = useCallback((content: string, overrideSessionId?: string) => {
    const targetSessionId = overrideSessionId || sessionId;
    send({
      type: 'message',
      msg_id: `web-${Date.now()}`,
      session_key: sessionKey,
      session_id: targetSessionId || undefined,
      transport_session_key: transportSessionKey,
      route: visibleRoute,
      user_id: WEB_BRIDGE_USER_ID,
      user_name: WEB_BRIDGE_USER_NAME,
      content,
      reply_ctx: sessionKey,
      project: projectName || '',
    });
  }, [send, sessionKey, sessionId, transportSessionKey, visibleRoute, projectName]);

  const sendCardAction = useCallback((action: string, overrideSessionId?: string) => {
    const targetSessionId = overrideSessionId || sessionId;
    send({
      type: 'card_action',
      session_key: sessionKey,
      session_id: targetSessionId || undefined,
      transport_session_key: transportSessionKey,
      route: visibleRoute,
      action,
      reply_ctx: sessionKey,
      project: projectName || '',
    });
  }, [send, sessionKey, sessionId, transportSessionKey, visibleRoute, projectName]);

  const sendPreviewAck = useCallback((refId: string, handle: string) => {
    send({ type: 'preview_ack', ref_id: refId, preview_handle: handle });
  }, [send]);

  useEffect(() => {
    if (!bridgeCfg) return;

    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    // Use current page host:port so the request goes through the Vite/nginx proxy
    // instead of directly hitting the bridge port (which may not be reachable).
    const wsPath = bridgeCfg.frontend_path || bridgeCfg.client_path || bridgeCfg.path;
    const wsToken = bridgeCfg.frontend_token || bridgeCfg.token;
    const wsUrl = `${proto}//${window.location.host}${wsPath}?token=${encodeURIComponent(wsToken)}`;

    let ws: WebSocket;
    let reconnectTimer: ReturnType<typeof setTimeout>;
    let alive = true;

    const connect = () => {
      if (!alive) return;
      setStatus('connecting');
      ws = new WebSocket(wsUrl);
      wsRef.current = ws;

      ws.onopen = () => {
        setStatus('registering');
        ws.send(JSON.stringify({
          type: 'frontend_connect',
          platform: servicePlatform,
          slot: visibleRoute,
          app: 'cc-connect-web',
          session_key: sessionKey,
          transport_session_key: transportSessionKey,
          route: visibleRoute,
          project: projectName || undefined,
          capabilities: ['text', 'card', 'buttons', 'typing', 'update_message', 'preview', 'reconstruct_reply'],
          metadata: {
            version: '1.0.0',
            description: 'Web Admin Dashboard',
            client_kind: 'frontend_browser',
            frontend_slot: visibleRoute,
            route: visibleRoute,
            service_platform: servicePlatform,
            transport_session_key: transportSessionKey,
            project: projectName || '',
          },
        }));
      };

      ws.onmessage = (evt) => {
        try {
          const msg = JSON.parse(evt.data) as BridgeIncoming;
          if (msg.type === 'register_ack') {
            if (msg.ok) {
              setStatus('connected');
              pingRef.current = setInterval(() => {
                send({ type: 'ping', ts: Date.now() });
              }, 25000);
            } else {
              setStatus('error');
            }
          }
          onMessageRef.current(msg);
        } catch { /* ignore parse errors */ }
      };

      ws.onclose = () => {
        setStatus('disconnected');
        wsRef.current = null;
        if (pingRef.current) clearInterval(pingRef.current);
        if (alive) reconnectTimer = setTimeout(connect, 3000);
      };

      ws.onerror = () => {
        setStatus('error');
      };
    };

    connect();

    return () => {
      alive = false;
      clearTimeout(reconnectTimer);
      if (pingRef.current) clearInterval(pingRef.current);
      if (wsRef.current) {
        wsRef.current.onclose = null;
        wsRef.current.close();
        wsRef.current = null;
      }
      setStatus('disconnected');
    };
  }, [bridgeCfg, servicePlatform, visibleRoute, sessionKey, transportSessionKey, projectName, send]);

  return { status, send, sendMessage, sendCardAction, sendPreviewAck };
}

// Fetch bridge config from the management API status endpoint.
export async function fetchBridgeConfig(): Promise<BridgeConfig | null> {
  try {
    const status = await api.get<any>('/status');
    if (status.bridge?.enabled) {
      return {
        port: status.bridge.port,
        path: status.bridge.path,
        token: status.bridge.token,
        frontend_path: status.bridge.frontend_path,
        client_path: status.bridge.client_path,
        frontend_token: status.bridge.frontend_token,
        service_platform: status.bridge.service_platform,
      };
    }
  } catch { /* bridge not available */ }
  return null;
}
