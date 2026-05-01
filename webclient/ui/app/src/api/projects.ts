import api from './client';

export interface ProjectSummary {
  name: string;
  display_name?: string;
  agent_type: string;
  platforms: string[];
  sessions_count: number;
  heartbeat_enabled: boolean;
}

export interface PlatformConfigInfo {
  type: string;
  allow_from?: string;
}

export interface PermissionModeInfo {
  key: string;
  name?: string;
  nameZh?: string;
  desc?: string;
  descZh?: string;
}

export interface ProjectDetail {
  name: string;
  display_name?: string;
  agent_type: string;
  work_dir?: string;
  agent_mode?: string;
  permission_modes?: PermissionModeInfo[];
  show_context_indicator?: boolean;
  reply_footer?: boolean;
  provider_refs?: string[];
  platform_configs?: PlatformConfigInfo[];
  platforms: { type: string; connected: boolean }[];
  sessions_count: number;
  active_session_keys: string[];
  heartbeat: {
    enabled: boolean;
    paused: boolean;
    interval_mins: number;
    session_key: string;
  };
  settings: {
    admin_from: string;
    language: string;
    disabled_commands: string[];
  };
}

export interface ProjectSettingsUpdate {
  display_name?: string;
  language?: string;
  admin_from?: string;
  disabled_commands?: string[];
  work_dir?: string;
  mode?: string;
  show_context_indicator?: boolean;
  reply_footer?: boolean;
  platform_allow_from?: Record<string, string>;
}

export interface DirectoryEntry {
  name: string;
  path: string;
  hidden: boolean;
}

export interface DirectoryListing {
  path: string;
  parent: string;
  home: string;
  separator: string;
  entries: DirectoryEntry[];
}

export const listProjects = () => api.get<{ projects: ProjectSummary[] }>('/projects');
export const listDirectories = (path?: string) => api.get<DirectoryListing>(
  '/filesystem/directories',
  path ? { path } : undefined,
);
export const createDirectory = (body: { parent: string; name: string }) =>
  api.post<{ created: DirectoryEntry; listing: DirectoryListing }>('/filesystem/directories', body);
export const createProject = (body: { name?: string; display_name?: string; work_dir?: string; agent_type?: string }) =>
  api.post<{ message: string; name?: string; display_name?: string; restart_required: boolean }>('/projects', body);
export const getProject = (name: string) => api.get<ProjectDetail>(`/projects/${name}`);
export const updateProject = (name: string, body: ProjectSettingsUpdate) => api.patch(`/projects/${name}`, body);

export const addPlatformToProject = (projectName: string, body: {
  type: string; options: Record<string, any>; work_dir?: string; agent_type?: string;
}) => api.post<{ message: string; restart_required: boolean }>(`/projects/${projectName}/add-platform`, body);

export const deleteProjectPlatform = (projectName: string, selector: string) =>
  api.delete<{ message: string; restart_required: boolean }>(
    `/projects/${encodeURIComponent(projectName)}/platforms/${encodeURIComponent(selector)}`,
  );

export const deleteProject = (name: string) =>
  api.delete<{ message: string; restart_required: boolean }>(`/projects/${name}`);
