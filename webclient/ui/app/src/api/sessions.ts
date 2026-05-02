import api from './client';
import type { BridgeImagePayload } from '@/lib/attachments';

export interface LastMessage {
  role: string;
  content: string;
  timestamp: string;
  images?: unknown[];
}

export interface SessionHistoryEntry {
  // Durable message fields (webclient backend may provide richer schema)
  id?: string;
  seq?: number;
  run_id?: string;
  user_message_id?: string;
  role: string;
  type?: 'text' | 'image' | 'file' | 'card' | 'buttons' | 'error' | string;
  content: string;
  timestamp?: string;
  created_at?: string;
  images?: unknown[];
  files?: unknown[];
  attachments?: unknown[];
  file?: unknown;
}

export interface Session {
  id: string;
  session_key: string;
  name: string;
  alias_mode?: string;
  alias_suffix?: string;
  platform: string;
  agent_type: string;
  active: boolean;
  live: boolean;
  created_at: string;
  updated_at: string;
  history_count: number;
  last_message: LastMessage | null;
  user_name?: string;
  chat_name?: string;
}

export interface SessionDetail extends Session {
  agent_session_id: string;
  history: SessionHistoryEntry[];
  run_events?: RunEvent[];
}

export type RunEventStatus = 'active' | 'completed' | 'error';

export interface RunEvent {
  id: string;
  seq?: number;
  run_id: string;
  user_message_id: string;
  session_id: string;
  type: string;
  content?: string;
  status: RunEventStatus;
  created_at: string;
  metadata?: Record<string, unknown>;
}

export const listSessions = (project: string) =>
  api.get<{ sessions: Session[]; active_keys: Record<string, string> }>(`/projects/${project}/sessions`);
export const getSession = (project: string, id: string, historyLimit?: number, runEventsLimit?: number) => {
  const params: Record<string, string> = {};
  if (historyLimit) params.history_limit = String(historyLimit);
  if (runEventsLimit) params.run_events_limit = String(runEventsLimit);
  return api.get<SessionDetail>(`/projects/${project}/sessions/${id}`, Object.keys(params).length ? params : undefined);
};
export const createSession = (project: string, body: { session_key: string; name?: string }) =>
  api.post<Session>(`/projects/${project}/sessions`, body);
export const updateSession = (project: string, id: string, body: { name: string }) =>
  api.patch(`/projects/${project}/sessions/${id}`, body);
export const deleteSession = (project: string, id: string) => api.delete(`/projects/${project}/sessions/${id}`);
export const switchSession = (project: string, body: { session_key: string; session_id: string }) =>
  api.post(`/projects/${project}/sessions/switch`, body);
export const sendMessage = (project: string, body: {
  session_key: string;
  session_id?: string;
  message?: string;
  action?: string;
  reply_ctx?: string;
  images?: BridgeImagePayload[];
}) => api.post(`/projects/${project}/send`, body);
