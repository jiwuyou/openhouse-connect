import api from './client';

export type RunTraceMode = 'auto' | 'expanded' | 'collapsed' | 'hidden';

export interface GlobalSettings {
  language: string;
  attachment_send: string;
  log_level: string;
  idle_timeout_mins: number;
  thinking_messages: boolean;
  thinking_max_len: number;
  tool_messages: boolean;
  tool_max_len: number;
  stream_preview_enabled: boolean;
  stream_preview_interval_ms: number;
  rate_limit_max_messages: number;
  rate_limit_window_secs: number;

  // Webclient-only display preferences. Backend may expose either
  // `run_trace_mode` (flat) or `webclient_display.run_trace_mode` (nested).
  run_trace_mode?: RunTraceMode;
  webclient_display?: {
    run_trace_mode?: RunTraceMode;
  };
}

export const getGlobalSettings = () => api.get<GlobalSettings>('/settings');
export const updateGlobalSettings = (body: Partial<GlobalSettings>) => api.patch<GlobalSettings>('/settings', body);
