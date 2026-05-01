import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ArrowUp, Check, Folder, Home, RefreshCw } from 'lucide-react';
import { Button, Modal } from '@/components/ui';
import { listDirectories, type DirectoryEntry } from '@/api/projects';
import { cn } from '@/lib/utils';

interface DirectoryPickerModalProps {
  open: boolean;
  initialPath?: string;
  onClose: () => void;
  onSelect: (path: string) => void;
}

export default function DirectoryPickerModal({
  open,
  initialPath,
  onClose,
  onSelect,
}: DirectoryPickerModalProps) {
  const { t } = useTranslation();
  const [path, setPath] = useState('');
  const [inputPath, setInputPath] = useState('');
  const [parent, setParent] = useState('');
  const [home, setHome] = useState('');
  const [entries, setEntries] = useState<DirectoryEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  const load = async (nextPath?: string) => {
    setLoading(true);
    setError('');
    try {
      const data = await listDirectories(nextPath);
      setPath(data.path);
      setInputPath(data.path);
      setParent(data.parent || '');
      setHome(data.home || '');
      setEntries(data.entries || []);
    } catch (e: any) {
      setError(e?.message || String(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (open) load(initialPath || undefined);
  }, [open, initialPath]);

  const confirm = () => {
    if (!path) return;
    onSelect(path);
    onClose();
  };

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={t('setup.chooseDirectory', 'Choose folder')}
      className="max-w-2xl"
    >
      <div className="space-y-4">
        <div className="flex gap-2">
          <input
            value={inputPath}
            onChange={(e) => setInputPath(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') load(inputPath);
            }}
            className={cn(
              'w-full px-3 py-2 text-sm rounded-lg transition-all duration-200',
              'border border-gray-300/90 dark:border-white/[0.1]',
              'bg-white/90 dark:bg-[rgba(0,0,0,0.45)] text-gray-900 dark:text-white',
              'focus:outline-none focus:ring-2 focus:ring-accent/45 focus:border-accent'
            )}
          />
          <Button variant="secondary" onClick={() => load(inputPath)} loading={loading}>
            <RefreshCw size={14} /> {t('common.open', 'Open')}
          </Button>
        </div>

        <div className="flex flex-wrap gap-2">
          <Button variant="secondary" size="sm" onClick={() => load(home)} disabled={!home || loading}>
            <Home size={14} /> {t('common.home', 'Home')}
          </Button>
          <Button variant="secondary" size="sm" onClick={() => load(parent)} disabled={!parent || loading}>
            <ArrowUp size={14} /> {t('common.up', 'Up')}
          </Button>
        </div>

        {error && (
          <div className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-600 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-300">
            {error}
          </div>
        )}

        <div className="h-80 overflow-y-auto rounded-lg border border-gray-200 dark:border-white/[0.08]">
          {loading && entries.length === 0 ? (
            <div className="flex h-full items-center justify-center text-sm text-gray-400">
              {t('common.loading', 'Loading...')}
            </div>
          ) : entries.length === 0 ? (
            <div className="flex h-full items-center justify-center text-sm text-gray-400">
              {t('setup.noDirectories', 'No folders in this directory')}
            </div>
          ) : (
            <div className="divide-y divide-gray-100 dark:divide-white/[0.06]">
              {entries.map((entry) => (
                <button
                  key={entry.path}
                  type="button"
                  onClick={() => load(entry.path)}
                  className="flex w-full items-center gap-3 px-3 py-2.5 text-left text-sm hover:bg-gray-50 dark:hover:bg-white/[0.05]"
                >
                  <Folder size={16} className="shrink-0 text-amber-500" />
                  <span className={cn('truncate text-gray-800 dark:text-gray-200', entry.hidden && 'text-gray-400')}>
                    {entry.name}
                  </span>
                </button>
              ))}
            </div>
          )}
        </div>

        <div className="flex items-center justify-between gap-3">
          <p className="truncate text-xs text-gray-500 dark:text-gray-400">{path}</p>
          <div className="flex gap-2">
            <Button variant="secondary" onClick={onClose}>{t('common.cancel')}</Button>
            <Button onClick={confirm} disabled={!path}>
              <Check size={14} /> {t('setup.useThisDirectory', 'Use this folder')}
            </Button>
          </div>
        </div>
      </div>
    </Modal>
  );
}
