import { useEffect, useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';
import {
  Server, Heart, ArrowRight, FolderKanban, Plus, Smartphone, Settings2, Globe,
  FolderOpen, FolderPlus, ChevronRight, ChevronUp, Home, RefreshCw,
} from 'lucide-react';
import { Card, Badge, Button, Input, Modal, EmptyState } from '@/components/ui';
import {
  createDirectory,
  createProject,
  listDirectories,
  listProjects,
  type DirectoryEntry,
  type DirectoryListing,
  type ProjectSummary,
} from '@/api/projects';
import { reloadConfig, restartSystem } from '@/api/status';
import PlatformSetupQR from './PlatformSetupQR';
import PlatformManualForm from './PlatformManualForm';
import { platformMeta } from '@/lib/platformMeta';
import { getAgentLabel } from '@/lib/providers';

const AGENT_OPTIONS = [
  { key: 'claudecode', label: 'Claude Code' },
  { key: 'codex', label: 'Codex' },
  { key: 'gemini', label: 'Gemini CLI' },
  { key: 'cursor', label: 'Cursor' },
  { key: 'acp', label: 'ACP' },
  { key: 'iflow', label: 'iFlow' },
  { key: 'opencode', label: 'OpenCode' },
  { key: 'qoder', label: 'Qoder' },
];

const PLATFORM_OPTIONS: { key: string; label: string; color: string; qr?: boolean }[] = [
  { key: 'web', label: 'Web Only', color: 'bg-slate-50 dark:bg-slate-900/30 text-slate-600 dark:text-slate-300' },
  { key: 'feishu', label: 'Feishu / Lark', color: 'bg-blue-50 dark:bg-blue-900/30 text-blue-600 dark:text-blue-400', qr: true },
  { key: 'weixin', label: 'WeChat', color: 'bg-green-50 dark:bg-green-900/30 text-green-600 dark:text-green-400', qr: true },
  { key: 'telegram', label: 'Telegram', color: 'bg-sky-50 dark:bg-sky-900/30 text-sky-600 dark:text-sky-400' },
  { key: 'discord', label: 'Discord', color: 'bg-indigo-50 dark:bg-indigo-900/30 text-indigo-600 dark:text-indigo-400' },
  { key: 'slack', label: 'Slack', color: 'bg-purple-50 dark:bg-purple-900/30 text-purple-600 dark:text-purple-400' },
  { key: 'dingtalk', label: 'DingTalk', color: 'bg-orange-50 dark:bg-orange-900/30 text-orange-600 dark:text-orange-400' },
  { key: 'wecom', label: 'WeChat Work', color: 'bg-emerald-50 dark:bg-emerald-900/30 text-emerald-600 dark:text-emerald-400' },
  { key: 'qq', label: 'QQ (OneBot)', color: 'bg-cyan-50 dark:bg-cyan-900/30 text-cyan-600 dark:text-cyan-400' },
  { key: 'qqbot', label: 'QQ Bot (Official)', color: 'bg-cyan-50 dark:bg-cyan-900/30 text-cyan-600 dark:text-cyan-400' },
  { key: 'line', label: 'LINE', color: 'bg-lime-50 dark:bg-lime-900/30 text-lime-600 dark:text-lime-400' },
];

type WorkspaceMode = 'new' | 'existing';

export default function ProjectList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [projects, setProjects] = useState<ProjectSummary[]>([]);
  const [loading, setLoading] = useState(true);

  // Add project wizard state
  const [showWizard, setShowWizard] = useState(false);
  const [wizStep, setWizStep] = useState<'name' | 'platform' | 'qr' | 'form' | 'done'>('name');
  const [newProjName, setNewProjName] = useState('');
  const [newWorkDir, setNewWorkDir] = useState('');
  const [workspaceMode, setWorkspaceMode] = useState<WorkspaceMode>('new');
  const [newAgentType, setNewAgentType] = useState('codex');
  const [selectedPlat, setSelectedPlat] = useState('');
  const [showRestartModal, setShowRestartModal] = useState(false);
  const [creatingWebOnly, setCreatingWebOnly] = useState(false);
  const [pendingProjectName, setPendingProjectName] = useState('');
  const [dirPickerOpen, setDirPickerOpen] = useState(false);
  const [dirListing, setDirListing] = useState<DirectoryListing | null>(null);
  const [dirLoading, setDirLoading] = useState(false);
  const [dirError, setDirError] = useState('');
  const [dirInput, setDirInput] = useState('');
  const [newFolderName, setNewFolderName] = useState('');
  const [creatingFolder, setCreatingFolder] = useState(false);
  const effectiveWorkDir = workspaceMode === 'existing' ? newWorkDir.trim() : '';

  const fetch = useCallback(async () => {
    try {
      setLoading(true);
      const data = await listProjects();
      setProjects(data.projects || []);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetch();
    const handler = () => fetch();
    window.addEventListener('cc:refresh', handler);
    return () => window.removeEventListener('cc:refresh', handler);
  }, [fetch]);

  const openWizard = () => {
    setShowWizard(true);
    setWizStep('name');
    setNewProjName('');
    setNewWorkDir('');
    setWorkspaceMode('new');
    setNewAgentType('codex');
    setSelectedPlat('');
    setDirError('');
  };

  const loadDirectories = useCallback(async (path?: string) => {
    setDirLoading(true);
    setDirError('');
    try {
      const data = await listDirectories(path);
      setDirListing(data);
      setDirInput(data.path);
    } catch (e: any) {
      setDirError(e?.message || String(e));
    } finally {
      setDirLoading(false);
    }
  }, []);

  const openDirectoryPicker = () => {
    setDirPickerOpen(true);
    setNewFolderName('');
    loadDirectories(newWorkDir.trim() || undefined);
  };

  const chooseDirectory = (path: string) => {
    setNewWorkDir(path);
    setWorkspaceMode('existing');
    setDirPickerOpen(false);
  };

  const handleCreateDirectory = async () => {
    if (!dirListing?.path || !newFolderName.trim()) return;
    setCreatingFolder(true);
    setDirError('');
    try {
      const data = await createDirectory({ parent: dirListing.path, name: newFolderName.trim() });
      setDirListing(data.listing);
      setDirInput(data.listing.path);
      setNewFolderName('');
      if (data.created?.path) {
        setNewWorkDir(data.created.path);
        setWorkspaceMode('existing');
      }
    } catch (e: any) {
      setDirError(e?.message || String(e));
    } finally {
      setCreatingFolder(false);
    }
  };

  const isQRPlatform = (type: string) => type === 'feishu' || type === 'lark' || type === 'weixin';

  const handlePlatformSelect = (key: string) => {
    setSelectedPlat(key);
    if (key === 'web') {
      setCreatingWebOnly(true);
      createProject({ display_name: newProjName, work_dir: effectiveWorkDir, agent_type: newAgentType })
        .then(async (res) => {
          setPendingProjectName(res.name || '');
          await reloadConfig().catch(() => undefined);
          await fetch();
          setShowWizard(false);
          if (res.restart_required) {
            setShowRestartModal(true);
            return;
          }
          if (res.name) {
            navigate(`/projects/${res.name}`);
          }
        })
        .catch((e: any) => {
          window.alert(e?.message || String(e));
        })
        .finally(() => setCreatingWebOnly(false));
    } else if (isQRPlatform(key)) {
      setWizStep('qr');
    } else if (platformMeta[key]) {
      setWizStep('form');
    } else {
      setWizStep('done');
    }
  };

  const handleQRComplete = () => {
    setShowWizard(false);
    fetch();
  };

  const handleManualDone = async () => {
    setShowWizard(false);
    fetch();
  };

  const waitForService = (maxMs: number) =>
    new Promise<void>((resolve) => {
      const start = Date.now();
      const poll = () => {
        window.fetch('/api/v1/status')
          .then((r) => { if (r.ok) resolve(); else throw new Error(); })
          .catch(() => {
            if (Date.now() - start > maxMs) { resolve(); return; }
            setTimeout(poll, 500);
          });
      };
      setTimeout(poll, 1500);
    });

  if (loading && projects.length === 0) {
    return <div className="flex items-center justify-center h-64 text-gray-400 animate-pulse">Loading...</div>;
  }

  return (
    <div className="animate-fade-in space-y-4">
      {/* Header */}
      <div className="flex items-center justify-between gap-3 flex-wrap">
        <h2 className="text-lg font-bold text-gray-900 dark:text-white">{t('projects.title')}</h2>
        <Button size="sm" onClick={openWizard}>
          <Plus size={14} /> {t('setup.addProject', 'Add project')}
        </Button>
      </div>

      {projects.length === 0 ? (
        <EmptyState message={t('projects.noProjects')} icon={FolderKanban} />
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
          {projects.map((p) => (
            <Link key={p.name} to={`/projects/${p.name}`}>
              <Card hover className="h-full flex flex-col">
                <div className="flex items-start justify-between mb-3">
                  <div className="flex items-center gap-2 min-w-0">
                    <Server size={18} className="text-gray-400" />
                    <h3 className="font-semibold text-gray-900 dark:text-white truncate">{p.display_name || p.name}</h3>
                  </div>
                  <ArrowRight size={16} className="text-gray-300 dark:text-gray-600" />
                </div>
                <div className="flex flex-wrap gap-1.5 mb-3">
                  <Badge variant="info">{getAgentLabel(p.agent_type)}</Badge>
                  {p.platforms?.map((pl) => <Badge key={pl}>{pl}</Badge>)}
                </div>
                <div className="flex items-center justify-between text-xs text-gray-500 dark:text-gray-400 mt-auto pt-3 border-t border-gray-100 dark:border-gray-800">
                  <span>{p.sessions_count} {t('nav.sessions').toLowerCase()}</span>
                  {p.heartbeat_enabled && (
                    <span className="flex items-center gap-1 text-emerald-500"><Heart size={12} /> {t('heartbeat.title')}</span>
                  )}
                </div>
              </Card>
            </Link>
          ))}
        </div>
      )}

      {/* Add Project Wizard Modal */}
      <Modal
        open={showWizard}
        onClose={() => setShowWizard(false)}
        title={t('setup.addProject', 'Add project')}
      >
        {wizStep === 'name' && (
          <div className="space-y-4 py-2">
            <Input
              label={t('setup.projectName', 'Project name')}
              value={newProjName}
              onChange={(e) => setNewProjName(e.target.value.replace(/[^a-zA-Z0-9_-]/g, ''))}
              placeholder="my-project"
              autoFocus
            />
            <div className="space-y-2">
              <label className="block text-xs font-medium text-gray-600 dark:text-gray-400">
                {t('setup.workspaceSource', 'Workspace')}
              </label>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
                <button
                  type="button"
                  onClick={() => setWorkspaceMode('new')}
                  className={`flex items-start gap-3 rounded-xl border p-3 text-left transition-all ${
                    workspaceMode === 'new'
                      ? 'border-accent/50 bg-accent/10 ring-1 ring-accent/20'
                      : 'border-gray-200 dark:border-gray-700 hover:border-accent/40 hover:bg-accent/5'
                  }`}
                >
                  <FolderPlus size={18} className="mt-0.5 shrink-0 text-accent" />
                  <span className="min-w-0">
                    <span className="block text-sm font-medium text-gray-900 dark:text-white">
                      {t('setup.workspaceNew', 'Create new workspace')}
                    </span>
                    <span className="mt-1 block text-xs leading-relaxed text-gray-500 dark:text-gray-400">
                      {t('setup.workspaceNewHint', 'Use the configured default project base path.')}
                    </span>
                  </span>
                </button>
                <button
                  type="button"
                  onClick={() => setWorkspaceMode('existing')}
                  className={`flex items-start gap-3 rounded-xl border p-3 text-left transition-all ${
                    workspaceMode === 'existing'
                      ? 'border-accent/50 bg-accent/10 ring-1 ring-accent/20'
                      : 'border-gray-200 dark:border-gray-700 hover:border-accent/40 hover:bg-accent/5'
                  }`}
                >
                  <FolderOpen size={18} className="mt-0.5 shrink-0 text-accent" />
                  <span className="min-w-0">
                    <span className="block text-sm font-medium text-gray-900 dark:text-white">
                      {t('setup.workspaceExisting', 'Use existing folder')}
                    </span>
                    <span className="mt-1 block text-xs leading-relaxed text-gray-500 dark:text-gray-400">
                      {t('setup.workspaceExistingHint', 'Select a folder that already contains your code.')}
                    </span>
                  </span>
                </button>
              </div>
            </div>

            {workspaceMode === 'existing' && (
              <div className="space-y-1.5">
                <label className="block text-xs font-medium text-gray-600 dark:text-gray-400">
                  {t('setup.workDir', 'Working directory')}
                </label>
                <div className="flex gap-2">
                  <input
                    value={newWorkDir}
                    onChange={(e) => setNewWorkDir(e.target.value)}
                    placeholder="/path/to/project"
                    className="min-w-0 flex-1 rounded-lg border border-gray-300/90 bg-white/90 px-3 py-2 text-sm text-gray-900 transition-all duration-200 placeholder:text-gray-400 focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent/45 dark:border-white/[0.1] dark:bg-[rgba(0,0,0,0.45)] dark:text-white dark:placeholder:text-gray-500"
                  />
                  <Button type="button" variant="secondary" onClick={openDirectoryPicker}>
                    <FolderOpen size={14} /> {t('setup.browseDir', 'Browse')}
                  </Button>
                </div>
              </div>
            )}
            <div>
              <label className="block text-xs font-medium text-gray-600 dark:text-gray-400 mb-1">
                {t('setup.agentType', 'Agent type')}
              </label>
              <select
                value={newAgentType}
                onChange={(e) => setNewAgentType(e.target.value)}
                className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-700 bg-white dark:bg-gray-800 text-gray-900 dark:text-white focus:outline-none focus:ring-2 focus:ring-accent/50"
              >
                {AGENT_OPTIONS.map(a => (
                  <option key={a.key} value={a.key}>{a.label}</option>
                ))}
              </select>
            </div>
            <div className="flex justify-end gap-2 pt-2 flex-wrap">
              <Button variant="secondary" className="max-sm:flex-1" onClick={() => setShowWizard(false)}>{t('common.cancel')}</Button>
              <Button
                className="max-sm:flex-1"
                onClick={() => setWizStep('platform')}
                disabled={workspaceMode === 'existing' && !newWorkDir.trim()}
              >
                {t('setup.next', 'Next')}
              </Button>
            </div>
          </div>
        )}

        {wizStep === 'platform' && (
          <div className="space-y-3 py-2">
            <p className="text-sm text-gray-500 dark:text-gray-400 mb-2">
              {t('setup.choosePlatform', 'Choose a platform to connect:')}
            </p>
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-2 max-h-[55dvh] sm:max-h-80 overflow-y-auto pr-1">
              {PLATFORM_OPTIONS.map(({ key, label, color, qr }) => (
                <button
                  key={key}
                  onClick={() => handlePlatformSelect(key)}
                  disabled={creatingWebOnly}
                  className="flex items-center gap-2.5 p-3 rounded-xl border border-gray-200 dark:border-gray-700 hover:border-accent/50 hover:bg-accent/5 transition-all text-left"
                >
                  <div className={`w-9 h-9 rounded-lg ${color} flex items-center justify-center shrink-0`}>
                    {key === 'web' ? <Globe size={16} /> : qr ? <Smartphone size={16} /> : <Settings2 size={16} />}
                  </div>
                  <div className="min-w-0">
                    <div className="text-sm font-medium text-gray-900 dark:text-white truncate">{label}</div>
                    <div className="text-[11px] text-gray-400">
                      {key === 'web' ? t('setup.webOnly', 'Web admin only') : qr ? t('setup.scanToConnect', 'Scan QR code') : t('setup.manualSetup', 'Manual setup')}
                    </div>
                  </div>
                </button>
              ))}
            </div>
            <div className="flex justify-start pt-2">
              <Button variant="secondary" size="sm" onClick={() => setWizStep('name')}>{t('common.back')}</Button>
            </div>
          </div>
        )}

        {wizStep === 'qr' && isQRPlatform(selectedPlat) && (
          <PlatformSetupQR
            platformType={selectedPlat as 'feishu' | 'weixin'}
            projectName={newProjName}
            workDir={effectiveWorkDir}
            agentType={newAgentType}
            onComplete={handleQRComplete}
            onCancel={() => setWizStep('platform')}
          />
        )}

        {wizStep === 'form' && platformMeta[selectedPlat] && (
          <PlatformManualForm
            platformType={selectedPlat}
            projectName={newProjName}
            workDir={effectiveWorkDir}
            agentType={newAgentType}
            onComplete={() => {
              setShowWizard(false);
              fetch();
            }}
            onCancel={() => setWizStep('platform')}
          />
        )}

        {wizStep === 'done' && !isQRPlatform(selectedPlat) && (
          <div className="space-y-4 py-4 text-center">
            <Settings2 size={40} className="mx-auto text-gray-400" />
            <p className="text-sm text-gray-600 dark:text-gray-400">
              {t('setup.manualHint', 'For {{platform}}, please configure credentials in config.toml or via the project detail page after creating the project.', { platform: PLATFORM_OPTIONS.find(o => o.key === selectedPlat)?.label || selectedPlat })}
            </p>
            <div className="flex justify-center gap-2 flex-wrap">
              <Button variant="secondary" onClick={() => setWizStep('platform')}>{t('common.back')}</Button>
            </div>
          </div>
        )}
      </Modal>

      <Modal
        open={dirPickerOpen}
        onClose={() => setDirPickerOpen(false)}
        title={t('setup.selectFolder', 'Select folder')}
        className="max-w-2xl"
      >
        <div className="space-y-4">
          <div className="flex gap-2">
            <input
              value={dirInput}
              onChange={(e) => setDirInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault();
                  loadDirectories(dirInput);
                }
              }}
              className="min-w-0 flex-1 rounded-lg border border-gray-300/90 bg-white/90 px-3 py-2 text-sm font-mono text-gray-900 transition-all duration-200 focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent/45 dark:border-white/[0.1] dark:bg-[rgba(0,0,0,0.45)] dark:text-white"
            />
            <Button type="button" variant="secondary" onClick={() => loadDirectories(dirInput)}>
              <RefreshCw size={14} />
            </Button>
          </div>

          <div className="flex flex-wrap gap-2">
            {dirListing?.parent && (
              <Button type="button" variant="ghost" size="sm" onClick={() => loadDirectories(dirListing.parent)}>
                <ChevronUp size={14} /> {t('setup.parentFolder', 'Up')}
              </Button>
            )}
            {dirListing?.home && (
              <Button type="button" variant="ghost" size="sm" onClick={() => loadDirectories(dirListing.home)}>
                <Home size={14} /> {t('setup.homeFolder', 'Home')}
              </Button>
            )}
            {dirListing?.path && (
              <Button type="button" size="sm" onClick={() => chooseDirectory(dirListing.path)}>
                <FolderOpen size={14} /> {t('setup.useThisFolder', 'Use this folder')}
              </Button>
            )}
          </div>

          {dirListing?.path && (
            <div className="rounded-xl border border-gray-200/80 bg-gray-50/80 p-3 dark:border-white/[0.08] dark:bg-white/[0.03]">
              <label className="mb-2 block text-xs font-medium text-gray-600 dark:text-gray-400">
                {t('setup.newFolder', 'New folder')}
              </label>
              <div className="flex gap-2 max-sm:flex-col">
                <input
                  value={newFolderName}
                  onChange={(e) => setNewFolderName(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault();
                      handleCreateDirectory();
                    }
                  }}
                  placeholder={t('setup.newFolderPlaceholder', 'Folder name')}
                  className="min-w-0 flex-1 rounded-lg border border-gray-300/90 bg-white/90 px-3 py-2 text-sm text-gray-900 transition-all duration-200 placeholder:text-gray-400 focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent/45 dark:border-white/[0.1] dark:bg-[rgba(0,0,0,0.45)] dark:text-white dark:placeholder:text-gray-500"
                />
                <Button
                  type="button"
                  variant="secondary"
                  className="max-sm:w-full"
                  disabled={!newFolderName.trim() || creatingFolder}
                  onClick={handleCreateDirectory}
                >
                  <FolderPlus size={14} /> {creatingFolder ? t('common.loading') : t('setup.createFolder', 'Create')}
                </Button>
              </div>
            </div>
          )}

          {dirError && (
            <div className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-600 dark:border-red-500/20 dark:bg-red-500/10 dark:text-red-400">
              {dirError}
            </div>
          )}

          <div className="max-h-[50dvh] overflow-y-auto rounded-xl border border-gray-200/80 bg-white/70 dark:border-white/[0.08] dark:bg-white/[0.03]">
            {dirLoading ? (
              <div className="p-6 text-center text-sm text-gray-400">{t('common.loading')}</div>
            ) : dirListing?.entries?.length ? (
              dirListing.entries.map((entry: DirectoryEntry) => (
                <button
                  key={entry.path}
                  type="button"
                  onClick={() => loadDirectories(entry.path)}
                  className="flex w-full items-center gap-3 border-b border-gray-100 px-3 py-2.5 text-left last:border-b-0 hover:bg-accent/5 dark:border-white/[0.05]"
                >
                  <FolderOpen size={16} className="shrink-0 text-accent" />
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm font-medium text-gray-900 dark:text-white">
                      {entry.name}
                    </div>
                    <div className="truncate text-[11px] text-gray-400">{entry.path}</div>
                  </div>
                  <ChevronRight size={14} className="shrink-0 text-gray-300" />
                </button>
              ))
            ) : (
              <div className="p-6 text-center text-sm text-gray-400">{t('setup.noFolders', 'No folders')}</div>
            )}
          </div>

          <div className="flex justify-end gap-2 flex-wrap">
            <Button variant="secondary" className="max-sm:flex-1" onClick={() => setDirPickerOpen(false)}>
              {t('common.cancel')}
            </Button>
            <Button
              className="max-sm:flex-1"
              disabled={!dirListing?.path}
              onClick={() => dirListing?.path && chooseDirectory(dirListing.path)}
            >
              {t('setup.useThisFolder', 'Use this folder')}
            </Button>
          </div>
        </div>
      </Modal>

      <Modal open={showRestartModal} onClose={() => setShowRestartModal(false)} title={t('setup.restartRequired', 'Restart required')}>
        <div className="space-y-4 py-2">
          <p className="text-sm text-gray-600 dark:text-gray-400">
            {t('setup.restartHint', 'Restart the service for the new platform to take effect.')}
          </p>
          <div className="flex justify-end gap-2 flex-wrap">
            <Button variant="secondary" className="max-sm:flex-1" onClick={() => { setShowRestartModal(false); setPendingProjectName(''); fetch(); }}>
              {t('setup.later', 'Later')}
            </Button>
            <Button className="max-sm:flex-1" onClick={async () => {
              await restartSystem();
              await waitForService(8000);
              await fetch();
              setShowRestartModal(false);
              if (pendingProjectName) {
                navigate(`/projects/${pendingProjectName}`);
                setPendingProjectName('');
              }
            }}>
              {t('setup.restartNow', 'Restart now')}
            </Button>
          </div>
        </div>
      </Modal>
    </div>
  );
}
