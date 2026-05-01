import api from './client';
import type { BridgeAdapter, FrontendService } from './bridge';

export interface SystemStatus {
  version: string;
  uptime_seconds: number;
  connected_platforms: string[];
  projects_count: number;
  bridge_adapters: BridgeAdapter[];
  frontend_services?: FrontendService[];
  bridge?: {
    enabled: boolean;
    port: number;
    path: string;
    token: string;
    token_set?: boolean;
    frontend_path?: string;
    client_path?: string;
    frontend_token?: string;
    service_platform?: string;
  };
}

export const getStatus = () => api.get<SystemStatus>('/status');
export const restartSystem = (body?: { session_key?: string; platform?: string }) => api.post('/restart', body);
export const reloadConfig = () => api.post<{ message: string; projects_added: string[]; projects_removed: string[]; projects_updated: string[] }>('/reload');
