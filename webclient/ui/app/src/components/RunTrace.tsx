import { memo, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { ChevronDown, ChevronRight, Activity, AlertTriangle, CheckCircle2 } from 'lucide-react';
import { cn } from '@/lib/utils';
import type { RunTraceMode } from '@/api/settings';
import type { RunEvent, RunEventStatus } from '@/api/sessions';

function deriveStatus(events: RunEvent[]): RunEventStatus {
  if (!events.length) return 'completed';

  const hasSeq = events.some((e) => typeof e.seq === 'number');
  let last = events[0];
  let lastRank = hasSeq
    ? (typeof last.seq === 'number' ? last.seq : 0)
    : (() => {
      const ts = Date.parse(last.created_at || '');
      return Number.isFinite(ts) ? ts : 0;
    })();

  for (let i = 1; i < events.length; i++) {
    const e = events[i];
    const rank = hasSeq
      ? (typeof e.seq === 'number' ? e.seq : 0)
      : (() => {
        const ts = Date.parse(e.created_at || '');
        return Number.isFinite(ts) ? ts : i;
      })();
    if (rank >= lastRank) {
      last = e;
      lastRank = rank;
    }
  }

  return last.status || 'active';
}

function compactText(s: string, max = 180) {
  const t = (s || '').replace(/\s+/g, ' ').trim();
  if (!t) return '';
  if (t.length <= max) return t;
  return `${t.slice(0, max - 1)}…`;
}

export const RunTrace = memo(function RunTrace({
  mode,
  events,
  open,
  onToggle,
}: {
  mode: RunTraceMode;
  events: RunEvent[];
  open?: boolean;
  onToggle?: () => void;
}) {
  const { t } = useTranslation();
  const status = useMemo(() => deriveStatus(events), [events]);

  const badge = status === 'active'
    ? { icon: <Activity size={12} className="text-amber-500" />, label: t('runTrace.active', 'Running') }
    : status === 'error'
      ? { icon: <AlertTriangle size={12} className="text-red-500" />, label: t('runTrace.error', 'Error') }
      : { icon: <CheckCircle2 size={12} className="text-emerald-500" />, label: t('runTrace.done', 'Done') };

  if (mode === 'hidden') return null;
  if (!events.length) return null;

  const shouldShowExpanded = mode === 'expanded' || mode === 'auto' || (mode === 'collapsed' && open);
  const shouldShowBar = mode === 'collapsed';

  return (
    <div className="flex justify-end">
      <div className="max-w-[86%] sm:max-w-[70%] w-full">
        {shouldShowBar && (
          <button
            type="button"
            onClick={onToggle}
            className={cn(
              'w-full flex items-center justify-between gap-2 px-3 py-2 rounded-xl',
              'bg-gray-50 dark:bg-gray-800/60 border border-gray-200 dark:border-gray-700/60',
              'text-xs text-gray-600 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-800 transition-colors',
            )}
          >
            <span className="flex items-center gap-2 min-w-0">
              {open ? <ChevronDown size={14} className="text-gray-400" /> : <ChevronRight size={14} className="text-gray-400" />}
              <span className="font-medium">{t('runTrace.title', 'Run trace')}</span>
              <span className="text-gray-400">{events.length}</span>
            </span>
            <span className="flex items-center gap-1.5 shrink-0">
              {badge.icon}
              <span className="text-[11px] text-gray-500">{badge.label}</span>
            </span>
          </button>
        )}

        {shouldShowExpanded && (
          <div className={cn(
            'mt-2 px-3 py-2 rounded-xl',
            'bg-gray-50 dark:bg-gray-800/60 border border-gray-200 dark:border-gray-700/60',
          )}>
            {!shouldShowBar && (
              <div className="flex items-center justify-between mb-2">
                <div className="text-xs font-medium text-gray-600 dark:text-gray-300">{t('runTrace.title', 'Run trace')}</div>
                <div className="flex items-center gap-1.5">
                  {badge.icon}
                  <span className="text-[11px] text-gray-500">{badge.label}</span>
                </div>
              </div>
            )}
            <div className="space-y-1.5">
              {events.map((e) => (
                <div key={e.id} className="flex items-start gap-2 text-[11px] text-gray-600 dark:text-gray-300">
                  <span className="shrink-0 w-24 text-gray-400 font-mono truncate">{e.type}</span>
                  <span className="min-w-0 break-words">
                    {e.content ? compactText(e.content) : (
                      e.metadata ? compactText(JSON.stringify(e.metadata)) : ''
                    )}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  );
});
