import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  Bot,
  CheckCircle2,
  Clock,
  FolderKanban,
  MessageSquare,
  Plug,
  Settings,
  ShieldCheck,
  Terminal,
  Zap,
} from 'lucide-react';
import { Card, Badge } from '@/components/ui';
import { cn } from '@/lib/utils';

const quickStart = [
  'createProject',
  'chooseAgent',
  'connectPlatform',
  'openChat',
  'manageSessions',
];

const workflows = [
  { key: 'projects', icon: FolderKanban, to: '/projects' },
  { key: 'providers', icon: Plug, to: '/providers' },
  { key: 'chat', icon: MessageSquare, to: '/chat' },
  { key: 'cron', icon: Clock, to: '/cron' },
  { key: 'system', icon: Settings, to: '/system' },
];

const commands = [
  ['/new', 'new'],
  ['/list', 'list'],
  ['/switch', 'switch'],
  ['/model', 'model'],
  ['/provider', 'provider'],
  ['/status', 'status'],
  ['/doctor', 'doctor'],
  ['/help', 'help'],
] as const;

export default function Help() {
  const { t } = useTranslation();

  return (
    <div className="space-y-6 animate-fade-in">
      <section className="rounded-2xl border border-gray-200/80 bg-white/70 p-5 sm:p-6 dark:border-white/[0.08] dark:bg-white/[0.03]">
        <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
          <div className="min-w-0">
            <div className="mb-3 inline-flex items-center gap-2 rounded-full bg-accent/10 px-3 py-1 text-xs font-medium text-accent">
              <ShieldCheck size={13} />
              {t('help.badge')}
            </div>
            <h1 className="text-2xl font-bold tracking-tight text-gray-900 dark:text-white">
              {t('help.title')}
            </h1>
            <p className="mt-2 max-w-2xl text-sm leading-relaxed text-gray-500 dark:text-gray-400">
              {t('help.subtitle')}
            </p>
          </div>
          <div className="flex shrink-0 items-center gap-2 rounded-xl bg-gray-950 px-3 py-2 text-xs font-medium text-white dark:bg-white dark:text-gray-950">
            <Bot size={14} />
            openhouse-connect
          </div>
        </div>
      </section>

      <section>
        <div className="mb-3 flex items-center gap-2">
          <CheckCircle2 size={16} className="text-accent" />
          <h2 className="text-sm font-semibold text-gray-900 dark:text-white">{t('help.quickStart.title')}</h2>
        </div>
        <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
          {quickStart.map((key, index) => (
            <Card key={key} className="h-full">
              <div className="mb-3 flex items-center justify-between">
                <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-accent/10 text-xs font-semibold text-accent">
                  {index + 1}
                </span>
                <Badge>{t(`help.quickStart.${key}.tag`)}</Badge>
              </div>
              <h3 className="text-sm font-semibold text-gray-900 dark:text-white">
                {t(`help.quickStart.${key}.title`)}
              </h3>
              <p className="mt-2 text-xs leading-relaxed text-gray-500 dark:text-gray-400">
                {t(`help.quickStart.${key}.body`)}
              </p>
            </Card>
          ))}
        </div>
      </section>

      <section>
        <div className="mb-3 flex items-center gap-2">
          <Zap size={16} className="text-accent" />
          <h2 className="text-sm font-semibold text-gray-900 dark:text-white">{t('help.workflows.title')}</h2>
        </div>
        <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
          {workflows.map(({ key, icon: Icon, to }) => (
            <Link key={key} to={to} className="block">
              <Card hover className="h-full">
                <div className="flex items-start gap-3">
                  <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-gray-900/90 text-white dark:bg-white/10 dark:text-accent">
                    <Icon size={18} />
                  </div>
                  <div className="min-w-0">
                    <h3 className="text-sm font-semibold text-gray-900 dark:text-white">
                      {t(`help.workflows.${key}.title`)}
                    </h3>
                    <p className="mt-1 text-xs leading-relaxed text-gray-500 dark:text-gray-400">
                      {t(`help.workflows.${key}.body`)}
                    </p>
                    <span className="mt-3 inline-flex text-xs font-medium text-accent">
                      {t(`help.workflows.${key}.cta`)}
                    </span>
                  </div>
                </div>
              </Card>
            </Link>
          ))}
        </div>
      </section>

      <section className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(280px,380px)]">
        <Card>
          <div className="mb-4 flex items-center gap-2">
            <Terminal size={16} className="text-accent" />
            <h2 className="text-sm font-semibold text-gray-900 dark:text-white">{t('help.commands.title')}</h2>
          </div>
          <div className="grid gap-2 sm:grid-cols-2">
            {commands.map(([cmd, key]) => (
              <div
                key={cmd}
                className="flex items-start gap-3 rounded-xl border border-gray-200/70 bg-white/60 px-3 py-2.5 dark:border-white/[0.06] dark:bg-white/[0.03]"
              >
                <code className="w-20 shrink-0 rounded-lg bg-accent/10 px-2 py-1 text-xs font-semibold text-accent">
                  {cmd}
                </code>
                <p className="min-w-0 text-xs leading-relaxed text-gray-500 dark:text-gray-400">
                  {t(`help.commands.${key}`)}
                </p>
              </div>
            ))}
          </div>
        </Card>

        <Card>
          <div className="mb-4 flex items-center gap-2">
            <ShieldCheck size={16} className="text-accent" />
            <h2 className="text-sm font-semibold text-gray-900 dark:text-white">{t('help.tips.title')}</h2>
          </div>
          <div className="space-y-3">
            {['permissions', 'restart', 'mobile', 'secrets'].map((key) => (
              <div key={key} className="flex gap-3">
                <span className={cn('mt-1 h-2 w-2 shrink-0 rounded-full bg-accent')} />
                <p className="text-xs leading-relaxed text-gray-500 dark:text-gray-400">
                  {t(`help.tips.${key}`)}
                </p>
              </div>
            ))}
          </div>
        </Card>
      </section>
    </div>
  );
}
