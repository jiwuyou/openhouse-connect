import { useEffect, useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Cable, Wifi, Globe2 } from 'lucide-react';
import { Card, Badge, EmptyState } from '@/components/ui';
import { listFrontendServices, type FrontendService } from '@/api/bridge';
import { formatTime } from '@/lib/utils';

function serviceStatusVariant(status?: string): 'default' | 'success' | 'warning' | 'danger' {
  if (status === 'online') return 'success';
  if (status === 'stale') return 'warning';
  if (status === 'offline') return 'danger';
  return 'default';
}

export default function BridgeAdapters() {
  const { t } = useTranslation();
  const [services, setServices] = useState<FrontendService[]>([]);
  const [loading, setLoading] = useState(true);

  const fetchData = useCallback(async () => {
    setLoading(true);
    try {
      const data = await listFrontendServices();
      setServices(data || []);
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

  if (loading && services.length === 0) {
    return <div className="flex items-center justify-center h-64 text-gray-400 animate-pulse">Loading...</div>;
  }

  if (services.length === 0) {
    return <EmptyState message={t('bridge.noFrontendServices', 'No frontend services registered')} icon={Cable} />;
  }

  return (
    <div className="space-y-4 animate-fade-in">
      {services.map((service) => {
        const state = service.service;
        const status = state?.status || 'offline';
        const serviceURL = state?.url || service.url;
        const apiBase = state?.api_base || service.api_base;
        const lastSeen = state?.last_seen_at || state?.registered_at || service.updated_at;
        return (
          <Card key={`${service.app_id}:${service.slot}`}>
            <div className="flex items-start justify-between gap-4">
              <div className="flex items-center gap-3 min-w-0">
                <div className="w-10 h-10 rounded-lg bg-blue-100 dark:bg-blue-900/30 flex items-center justify-center shrink-0">
                  <Globe2 size={20} className="text-blue-500" />
                </div>
                <div className="min-w-0">
                  <div className="flex items-center gap-2 flex-wrap">
                    <span className="font-medium text-gray-900 dark:text-white">{service.label || service.slot}</span>
                    <Badge variant={serviceStatusVariant(status)}>{t(`bridge.frontendStatus.${status}`, status)}</Badge>
                    <Badge variant="info">{service.app_name || service.app_id}</Badge>
                    {service.adapter_platform && <Badge>{service.adapter_platform}</Badge>}
                  </div>
                  <div className="flex flex-wrap gap-1 mt-1 items-center">
                    <Badge variant="default">{service.project}</Badge>
                    <Badge variant="outline">{service.slot}</Badge>
                    {state?.service_id && <span className="text-xs text-gray-500 dark:text-gray-400">id: {state.service_id}</span>}
                    {state?.version && <span className="text-xs text-gray-500 dark:text-gray-400">v{state.version}</span>}
                    {serviceURL && <span className="text-xs text-gray-500 dark:text-gray-400 truncate max-w-[60vw]">{serviceURL}</span>}
                    {apiBase && <span className="text-xs text-gray-500 dark:text-gray-400 truncate max-w-[60vw]">api: {apiBase}</span>}
                    {!service.enabled && <Badge variant="warning">{t('common.disabled', 'disabled')}</Badge>}
                  </div>
                </div>
              </div>
              <div className="flex items-center gap-1 text-xs text-gray-400 shrink-0">
                <Wifi size={12} />
                <span>{lastSeen ? formatTime(lastSeen) : t('bridge.managedService', 'managed')}</span>
              </div>
            </div>
          </Card>
        );
      })}
    </div>
  );
}
