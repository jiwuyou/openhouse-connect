import { useEffect, useRef, useCallback, useState } from 'react';
import api from '@/api/client';
import { sendMessage as sendV1Message } from '@/api/sessions';
import type { BridgeImagePayload } from '@/lib/attachments';

type BridgeScoped = { session_key: string; session_id?: string };
type BridgeReplyScoped = BridgeScoped & { reply_ctx: string };

export type BridgeIncoming =
  | { type: 'register_ack'; ok: boolean; error?: string }
  | (BridgeReplyScoped & { type: 'reply'; content: string; format?: string; images?: unknown[] })
  | (BridgeScoped & { type: 'image'; content?: string; image?: unknown; images?: unknown[]; reply_ctx?: string })
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

// Compatibility hook: historically this connected a browser tab to /bridge/ws via
// frontend_connect. For the 9840 webclient, the primary path is now the local
// backend API + SSE stream. We keep the interface so existing pages (e.g.
// SessionChat) don't need invasive edits.
export function useBridgeSocket({ bridgeCfg: _bridgeCfg, sessionKey, sessionId, projectName, onMessage }: UseBridgeSocketOptions) {
  const onMessageRef = useRef(onMessage);
  onMessageRef.current = onMessage;
  const [status, setStatus] = useState<BridgeStatus>('disconnected');
  const esRef = useRef<EventSource | null>(null);

  const send = useCallback((data: Record<string, any>) => {
    void data;
  }, []);

  const sendMessage = useCallback((content: string, overrideSessionId?: string, images?: BridgeImagePayload[], msgId?: string) => {
    void msgId; // backend generates durable IDs
    if (!projectName) return;
    const targetSessionId = overrideSessionId || sessionId;
    sendV1Message(projectName, {
      session_key: sessionKey,
      session_id: targetSessionId || undefined,
      message: content,
      images: images && images.length > 0 ? images : undefined,
    }).catch(() => {});
  }, [projectName, sessionKey, sessionId]);

  const sendCardAction = useCallback((action: string, overrideSessionId?: string) => {
    if (!projectName) return;
    const targetSessionId = overrideSessionId || sessionId;
    sendV1Message(projectName, {
      session_key: sessionKey,
      session_id: targetSessionId || undefined,
      action,
      reply_ctx: sessionKey,
    }).catch(() => {});
  }, [projectName, sessionKey, sessionId]);

  const sendPreviewAck = useCallback((refId: string, handle: string) => {
    // preview is acked by the 9840 backend; keep method for legacy callers
    void refId;
    void handle;
  }, [send]);

  useEffect(() => {
    let alive = true;
    if (!projectName || !sessionId) {
      setStatus('disconnected');
      return;
    }

    const token = api.getToken();
    const base = `/api/projects/${encodeURIComponent(projectName)}/sessions/${encodeURIComponent(sessionId)}/events`;
    const url = token ? `${base}?token=${encodeURIComponent(token)}` : base;

    setStatus('connecting');
    const es = new EventSource(url);
    esRef.current = es;

    es.onopen = () => {
      if (alive) setStatus('connected');
    };
    es.onerror = () => {
      if (alive) setStatus('error');
    };
    es.addEventListener('message', (evt: MessageEvent) => {
      try {
        const m = JSON.parse(String((evt as any).data || '{}')) as any;
        if (String(m.role || '') !== 'assistant') return;

        const attachments = Array.isArray(m.attachments) ? m.attachments : [];
        const images = attachments
          .filter((a: any) => String(a.kind || '').toLowerCase() === 'image')
          .map((a: any) => ({
            id: a.id,
            mime_type: a.mime_type || a.mimeType,
            url: a.url,
            file_name: a.file_name || a.fileName,
            size: a.size,
          }));

        onMessageRef.current({
          type: 'reply',
          session_key: sessionKey,
          session_id: sessionId,
          reply_ctx: sessionKey,
          content: String(m.content || ''),
          format: 'markdown',
          images: images.length ? images : undefined,
        } as any);
      } catch {
        // ignore parse errors
      }
    });

    return () => {
      alive = false;
      if (esRef.current) {
        esRef.current.close();
        esRef.current = null;
      }
      setStatus('disconnected');
    };
  }, [projectName, sessionId, sessionKey]);

  return { status, send, sendMessage, sendCardAction, sendPreviewAck };
}

// Fetch bridge config from the management API status endpoint.
export async function fetchBridgeConfig(): Promise<BridgeConfig | null> {
  try {
    const status = await api.get<any>('/status');
    const token = String(status.bridge?.token || api.getToken() || '');
    return {
      port: Number(status.bridge?.port || 0),
      path: String(status.bridge?.path || '/bridge/ws'),
      token,
      frontend_path: status.bridge?.frontend_path,
      client_path: status.bridge?.client_path,
      frontend_token: status.bridge?.frontend_token,
      service_platform: status.bridge?.service_platform,
    };
  } catch {
    // Return a stub so legacy pages don't block chat when bridge is disabled.
    return { port: 0, path: '/bridge/ws', token: api.getToken() || '' };
  }
}
