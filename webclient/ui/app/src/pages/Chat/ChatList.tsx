import { useEffect, useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { MessageSquare, Bot, User, Circle, ArrowRight, FolderKanban } from 'lucide-react';
import { Card, EmptyState, Badge } from '@/components/ui';
import { listProjects, type ProjectSummary } from '@/api/projects';
import { listSessions, type Session } from '@/api/sessions';
import { getAgentLabel } from '@/lib/providers';
import { cn } from '@/lib/utils';

interface ChatEntry {
  project: ProjectSummary;
  latestSession: Session | null;
  sessions: Session[];
}

type ChatListView = 'projects' | 'grouped';
const VIEW_KEY = 'cc_chat_list_view';

function timeAgo(iso: string, t: (k: string) => string): string {
  if (!iso) return '';
  const diff = Date.now() - new Date(iso).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return t('sessions.justNow');
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

export default function ChatList() {
  const { t } = useTranslation();
  const [entries, setEntries] = useState<ChatEntry[]>([]);
  const [view, setView] = useState<ChatListView>(() => {
    const saved = localStorage.getItem(VIEW_KEY);
    return saved === 'grouped' ? 'grouped' : 'projects';
  });
  const [loading, setLoading] = useState(true);

  const setPreferredView = (next: ChatListView) => {
    setView(next);
    localStorage.setItem(VIEW_KEY, next);
  };

  const fetchData = useCallback(async () => {
    setLoading(true);
    try {
      const { projects: projs } = await listProjects();
      if (!projs?.length) {
        setEntries([]);
        return;
      }
      const results = await Promise.all(
        projs.map(async (p) => {
          try {
            const { sessions } = await listSessions(p.name);
            const sorted = (sessions || []).sort(
              (a, b) => (b.updated_at || b.created_at || '').localeCompare(a.updated_at || a.created_at || ''),
            );
            return { project: p, latestSession: sorted[0] || null, sessions: sorted };
          } catch {
            return { project: p, latestSession: null, sessions: [] };
          }
        }),
      );
      results.sort((a, b) => {
        const ta = a.latestSession?.updated_at || a.latestSession?.created_at || '';
        const tb = b.latestSession?.updated_at || b.latestSession?.created_at || '';
        return tb.localeCompare(ta);
      });
      setEntries(results);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchData();
    const handler = () => fetchData();
    window.addEventListener('cc:refresh', handler);
    return () => window.removeEventListener('cc:refresh', handler);
  }, [fetchData]);

  if (loading && entries.length === 0) {
    return <div className="flex items-center justify-center h-64 text-gray-400 animate-pulse">Loading...</div>;
  }

  return (
    <div className="animate-fade-in space-y-4">
      <div className="flex items-center justify-between gap-3 flex-wrap">
        <div>
          <h2 className="text-lg font-bold text-gray-900 dark:text-white">{t('nav.chat')}</h2>
          <p className="mt-1 text-xs text-gray-500 dark:text-gray-400">
            {view === 'grouped' ? t('chat.groupedHint') : t('chat.projectsHint')}
          </p>
        </div>
        <div className="flex w-full sm:w-auto gap-1 rounded-xl bg-gray-100 p-1 dark:bg-white/[0.06]">
          <button
            type="button"
            onClick={() => setPreferredView('projects')}
            className={cn(
              'flex-1 sm:flex-none rounded-lg px-3 py-2 text-xs font-medium transition-all',
              view === 'projects'
                ? 'bg-white text-gray-900 shadow-sm dark:bg-white/10 dark:text-white'
                : 'text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200',
            )}
          >
            {t('chat.viewProjects')}
          </button>
          <button
            type="button"
            onClick={() => setPreferredView('grouped')}
            className={cn(
              'flex-1 sm:flex-none rounded-lg px-3 py-2 text-xs font-medium transition-all',
              view === 'grouped'
                ? 'bg-white text-gray-900 shadow-sm dark:bg-white/10 dark:text-white'
                : 'text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200',
            )}
          >
            {t('chat.viewGrouped')}
          </button>
        </div>
      </div>

      {entries.length === 0 ? (
        <EmptyState message={t('chat.noChats')} icon={MessageSquare} />
      ) : view === 'projects' ? (
        <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
          {entries.map(({ project, latestSession }) => {
            const hasLive = latestSession?.live;
            const lastMsg = latestSession?.last_message;
            const ts = latestSession?.updated_at || latestSession?.created_at || '';

            return (
              <Link key={project.name} to={`/chat/${project.name}`}>
                <Card hover className="h-full flex flex-col">
                  <div className="flex items-start justify-between gap-3 mb-3">
                    <div className="flex items-center gap-2 min-w-0">
                      <MessageSquare size={18} className="text-accent" />
                      <h3 className="font-semibold text-gray-900 dark:text-white truncate">{project.display_name || project.name}</h3>
                      {hasLive && <Circle size={6} className="fill-emerald-500 text-emerald-500" />}
                    </div>
                    <ArrowRight size={16} className="text-gray-300 dark:text-gray-600" />
                  </div>

                  <div className="flex-1 min-h-[2rem] mb-3">
                    {lastMsg ? (
                      <p className="text-xs text-gray-500 dark:text-gray-400 line-clamp-2 leading-relaxed">
                        {lastMsg.role === 'user' ? (
                          <User size={10} className="inline mr-1 -mt-0.5 opacity-60" />
                        ) : (
                          <Bot size={10} className="inline mr-1 -mt-0.5 opacity-60" />
                        )}
                        {lastMsg.content.replace(/\n/g, ' ').slice(0, 120)}
                      </p>
                    ) : (
                      <p className="text-xs text-gray-400 dark:text-gray-500 italic">
                        {t('chat.noMessages')}
                      </p>
                    )}
                  </div>

                  <div className="flex items-center justify-between gap-2 text-xs text-gray-500 dark:text-gray-400 mt-auto pt-3 border-t border-gray-100 dark:border-gray-800">
                    <div className="flex items-center gap-1.5 flex-wrap min-w-0">
                      <Badge className="text-[9px]">{getAgentLabel(project.agent_type)}</Badge>
                      {project.platforms?.map((pl) => <Badge key={pl}>{pl}</Badge>)}
                    </div>
                    <div className="flex items-center gap-2">
                      <span>{project.sessions_count} {t('chat.sessions', 'sessions')}</span>
                      {ts && <span className="text-gray-400">{timeAgo(ts, t)}</span>}
                    </div>
                  </div>
                </Card>
              </Link>
            );
          })}
        </div>
      ) : (
        <div className="space-y-4">
          {entries.map(({ project, sessions }) => (
            <section
              key={project.name}
              className="rounded-xl border border-gray-200/80 bg-white/70 p-3 sm:p-4 dark:border-white/[0.08] dark:bg-white/[0.03]"
            >
              <div className="mb-3 flex items-center justify-between gap-3">
                <div className="flex min-w-0 items-center gap-2">
                  <FolderKanban size={16} className="shrink-0 text-accent" />
                  <div className="min-w-0">
                    <h3 className="truncate text-sm font-semibold text-gray-900 dark:text-white">
                      {project.display_name || project.name}
                    </h3>
                    <div className="mt-1 flex flex-wrap gap-1.5">
                      <Badge className="text-[9px]">{getAgentLabel(project.agent_type)}</Badge>
                      {project.platforms?.map((pl) => <Badge key={pl}>{pl}</Badge>)}
                    </div>
                  </div>
                </div>
                <Link
                  to={`/chat/${project.name}`}
                  className="shrink-0 rounded-lg px-2.5 py-1.5 text-xs font-medium text-accent hover:bg-accent/10"
                >
                  {t('chat.openProject')}
                </Link>
              </div>

              {sessions.length === 0 ? (
                <div className="rounded-lg border border-dashed border-gray-200 px-3 py-4 text-center text-xs text-gray-400 dark:border-white/[0.08]">
                  {t('chat.noMessages')}
                </div>
              ) : (
                <div className="grid grid-cols-1 gap-2 md:grid-cols-2 xl:grid-cols-3">
                  {sessions.map((session) => {
                    const lastMsg = session.last_message;
                    const ts = session.updated_at || session.created_at || '';
                    return (
                      <Link
                        key={session.id}
                        to={`/chat/${project.name}?session=${encodeURIComponent(session.id)}`}
                        className="block"
                      >
                        <div className="h-full rounded-xl border border-gray-200/80 bg-white/75 p-3 transition-all hover:border-accent/40 hover:shadow-md hover:shadow-accent/5 dark:border-white/[0.06] dark:bg-white/[0.03]">
                          <div className="mb-2 flex items-start justify-between gap-2">
                            <div className="flex min-w-0 items-center gap-1.5">
                              <MessageSquare size={14} className={session.live ? 'shrink-0 text-accent' : 'shrink-0 text-gray-400'} />
                              <span className="truncate text-sm font-medium text-gray-900 dark:text-white">
                                {session.name || session.user_name || session.id.slice(0, 8)}
                              </span>
                              {session.live && <Circle size={5} className="shrink-0 fill-emerald-500 text-emerald-500" />}
                            </div>
                            {ts && <span className="shrink-0 text-[10px] text-gray-400">{timeAgo(ts, t)}</span>}
                          </div>
                          <div className="min-h-[2.5rem]">
                            {lastMsg ? (
                              <p className="line-clamp-2 text-xs leading-relaxed text-gray-500 dark:text-gray-400">
                                {lastMsg.role === 'user' ? (
                                  <User size={10} className="inline mr-1 -mt-0.5 opacity-60" />
                                ) : (
                                  <Bot size={10} className="inline mr-1 -mt-0.5 opacity-60" />
                                )}
                                {lastMsg.content.replace(/\n/g, ' ').slice(0, 120)}
                              </p>
                            ) : (
                              <p className="text-xs italic text-gray-400">{t('chat.noMessages')}</p>
                            )}
                          </div>
                          <div className="mt-2 flex items-center justify-between gap-2 border-t border-gray-100 pt-2 text-[10px] text-gray-400 dark:border-white/[0.05]">
                            {session.platform ? <Badge variant="info" className="text-[9px]">{session.platform}</Badge> : <span />}
                            <span>{session.history_count} msgs</span>
                          </div>
                        </div>
                      </Link>
                    );
                  })}
                </div>
              )}
            </section>
          ))}
        </div>
      )}
    </div>
  );
}
