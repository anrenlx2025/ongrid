import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  AlertTriangle,
  BarChart3,
  ChevronDown,
  ChevronRight,
  Clock,
  Download,
  FileSearch,
  Loader2,
  Pause,
  Play,
  RefreshCw,
  Search,
  X,
} from 'lucide-react';
import {
  getLogContext,
  getLogHistogram,
  listLogFieldValues,
  listLogFields,
  searchLogs,
  type LogHistogramBucket,
  type LogMatchMode,
  type LogRecord,
  type LogScope,
  type LogSearchRequest,
} from '@/api/logs';
import { ApiError } from '@/api/client';
import { listEdges, type Edge, type EdgeRole } from '@/api/edges';
import { onDevicesChanged } from '@/lib/events';
import { RoleSelect } from '@/components/ui';
import { cn } from '@/lib/cn';
import { useI18n } from '@/i18n/locale';

const RANGE_PRESETS = [
  { value: '5m', zh: '5 分钟', en: '5 min' },
  { value: '15m', zh: '15 分钟', en: '15 min' },
  { value: '1h', zh: '1 小时', en: '1 hour' },
  { value: '6h', zh: '6 小时', en: '6 hours' },
  { value: '24h', zh: '1 天', en: '1 day' },
  { value: '7d', zh: '7 天', en: '7 days' },
  { value: 'custom', zh: '自定义', en: 'Custom' },
];

const PAGE_LIMIT = 200;
const MAX_EXPORT_ROWS = 1000;
const LIVE_INTERVAL_MS = 5000;
const INPUT = 'h-[34px] w-full rounded-md border border-zinc-800 bg-zinc-950 px-2 text-xs text-zinc-100 placeholder:text-zinc-600 focus:border-zinc-600 focus:outline-none';

type ScopeKey =
  | 'cluster_ids'
  | 'namespaces'
  | 'workloads'
  | 'pods'
  | 'containers'
  | 'nodes'
  | 'service_names'
  | 'source_ids'
  | 'severities'
  | 'files'
  | 'units';

type ScopeDraft = Record<ScopeKey, string>;

const EMPTY_SCOPE: ScopeDraft = {
  cluster_ids: '',
  namespaces: '',
  workloads: '',
  pods: '',
  containers: '',
  nodes: '',
  service_names: '',
  source_ids: '',
  severities: '',
  files: '',
  units: '',
};

const QUICK_SEARCHES = [
  { zh: '最近错误', en: 'Recent errors', value: 'error panic fatal', mode: 'any' as LogMatchMode },
  { zh: 'OOM', en: 'OOM', value: 'Out of memory OOM oom-killer', mode: 'any' as LogMatchMode },
  { zh: '服务重启', en: 'Service restart', value: 'Started Stopping systemd', mode: 'any' as LogMatchMode },
  { zh: '超时', en: 'Timeouts', value: 'timeout deadline exceeded', mode: 'any' as LogMatchMode },
];

function rangeToMs(value: string): number {
  const match = /^(\d+)([mhd])$/.exec(value);
  if (!match) return 60 * 60 * 1000;
  const scale = match[2] === 'm' ? 60_000 : match[2] === 'h' ? 3_600_000 : 86_400_000;
  return Number(match[1]) * scale;
}

function splitValues(value: string): string[] {
  return value.split(/[\n,]+/).map((item) => item.trim()).filter(Boolean);
}

function keywordValues(value: string, mode: LogMatchMode): string[] {
  const trimmed = value.trim();
  if (!trimmed) return [];
  if (mode === 'phrase') return [trimmed];
  const values: string[] = [];
  const pattern = /"([^"]+)"|'([^']+)'|([^\s]+)/g;
  for (const match of trimmed.matchAll(pattern)) {
    const item = (match[1] ?? match[2] ?? match[3] ?? '').trim();
    if (item) values.push(item);
  }
  return values;
}

function histogramInterval(windowMs: number): string {
  if (windowMs <= 15 * 60_000) return '15s';
  if (windowMs <= 60 * 60_000) return '1m';
  if (windowMs <= 6 * 60 * 60_000) return '5m';
  if (windowMs <= 24 * 60 * 60_000) return '30m';
  if (windowMs <= 7 * 24 * 60 * 60_000) return '3h';
  return '12h';
}

function recordKey(record: LogRecord): string {
  return record.id || `${record.timestamp}:${record.backend}:${record.message}`;
}

function errorMessage(error: unknown): string {
  return error instanceof ApiError ? error.message : (error as Error).message;
}

function scopeValue(record: LogRecord, ...keys: string[]): string {
  for (const key of keys) {
    const value = record.resource_attributes?.[key] ?? record.attributes?.[key];
    if (value) return value;
  }
  return '';
}

function formatLogTime(timestamp: Date): string {
  const base = timestamp.toLocaleTimeString(undefined, { hour12: false });
  return `${base}.${String(timestamp.getMilliseconds()).padStart(3, '0')}`;
}

export default function LogsPage() {
  const { tr } = useI18n();
  const [range, setRange] = useState('1h');
  const [customStart, setCustomStart] = useState('');
  const [customEnd, setCustomEnd] = useState('');
  const [query, setQuery] = useState('');
  const [committedQuery, setCommittedQuery] = useState('');
  const [exclude, setExclude] = useState('');
  const [committedExclude, setCommittedExclude] = useState('');
  const [matchMode, setMatchMode] = useState<LogMatchMode>('any');
  const [committedMode, setCommittedMode] = useState<LogMatchMode>('any');
  const [deviceID, setDeviceID] = useState('');
  const [role, setRole] = useState<'' | EdgeRole>('');
  const [scopeDraft, setScopeDraft] = useState<ScopeDraft>(EMPTY_SCOPE);
  const [committedScope, setCommittedScope] = useState<ScopeDraft>(EMPTY_SCOPE);
  const [advanced, setAdvanced] = useState(false);
  const [edges, setEdges] = useState<Edge[]>([]);
  const [fieldValues, setFieldValues] = useState<Record<string, string[]>>({});
  const [records, setRecords] = useState<LogRecord[]>([]);
  const [histogram, setHistogram] = useState<LogHistogramBucket[]>([]);
  const [backends, setBackends] = useState<string[]>([]);
  const [tookMS, setTookMS] = useState(0);
  const [nextCursor, setNextCursor] = useState('');
  const [loading, setLoading] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [live, setLive] = useState(false);
  const [refreshKey, setRefreshKey] = useState(0);
  const [selected, setSelected] = useState<LogRecord | null>(null);
  const [contextRows, setContextRows] = useState<LogRecord[]>([]);
  const [contextLoading, setContextLoading] = useState(false);
  const requestSeq = useRef(0);

  const resolveWindow = useCallback(() => {
    if (range === 'custom') {
      const start = new Date(customStart);
      const end = new Date(customEnd);
      if (!customStart || !customEnd || Number.isNaN(start.getTime()) || Number.isNaN(end.getTime()) || end <= start) return null;
      return { start: start.toISOString(), end: end.toISOString(), duration: end.getTime() - start.getTime() };
    }
    const end = new Date();
    const duration = rangeToMs(range);
    return { start: new Date(end.getTime() - duration).toISOString(), end: end.toISOString(), duration };
  }, [customEnd, customStart, range]);

  const selectedDeviceIDs = useMemo(() => {
    if (deviceID) return [Number(deviceID)].filter((id) => Number.isInteger(id) && id > 0);
    if (!role) return [];
    return edges
      .filter((edge) => Array.isArray(edge.roles) && (edge.roles as string[]).includes(role) && edge.device_id != null)
      .map((edge) => Number(edge.device_id))
      .filter((id) => Number.isInteger(id) && id > 0);
  }, [deviceID, edges, role]);

  const buildScope = useCallback((draft: ScopeDraft): LogScope => {
    const scope: LogScope = {};
    if (selectedDeviceIDs.length > 0) scope.device_ids = selectedDeviceIDs;
    for (const key of Object.keys(draft) as ScopeKey[]) {
      const values = splitValues(draft[key]);
      if (values.length > 0) scope[key] = values;
    }
    return scope;
  }, [selectedDeviceIDs]);

  const buildRequest = useCallback((cursor = ''): LogSearchRequest | null => {
    const timeWindow = resolveWindow();
    if (!timeWindow) return null;
    return {
      start: timeWindow.start,
      end: timeWindow.end,
      scope: buildScope(committedScope),
      keywords: {
        include: keywordValues(committedQuery, committedMode),
        exclude: keywordValues(committedExclude, 'any'),
        mode: committedMode,
      },
      limit: PAGE_LIMIT,
      cursor: cursor || undefined,
      direction: 'backward',
    };
  }, [buildScope, committedExclude, committedMode, committedQuery, committedScope, resolveWindow]);

  const runSearch = useCallback(async (quiet = false) => {
    const input = buildRequest();
    const timeWindow = resolveWindow();
    if (!input || !timeWindow) {
      setError(tr('请选择有效的自定义起止时间', 'Choose a valid custom start and end time'));
      return;
    }
    const seq = ++requestSeq.current;
    const controller = new AbortController();
    if (!quiet) setLoading(true);
    setError(null);
    try {
      const [result, buckets] = await Promise.all([
        searchLogs(input, controller.signal),
        getLogHistogram({ ...input, limit: 1, cursor: undefined }, histogramInterval(timeWindow.duration), controller.signal),
      ]);
      if (seq !== requestSeq.current) return;
      setRecords(result.records ?? []);
      setNextCursor(result.next_cursor ?? '');
      setBackends(result.backends ?? []);
      setTookMS(result.took_ms ?? 0);
      setHistogram(buckets ?? []);
    } catch (err) {
      if (seq !== requestSeq.current || (err as Error).name === 'AbortError') return;
      setError(errorMessage(err));
      if (!quiet) {
        setRecords([]);
        setHistogram([]);
        setNextCursor('');
      }
    } finally {
      if (seq === requestSeq.current && !quiet) setLoading(false);
    }
  }, [buildRequest, resolveWindow, tr]);

  const loadMore = useCallback(async () => {
    if (!nextCursor || loadingMore) return;
    const input = buildRequest(nextCursor);
    if (!input) return;
    setLoadingMore(true);
    setError(null);
    try {
      const result = await searchLogs(input);
      setRecords((current) => {
        const seen = new Set(current.map(recordKey));
        return current.concat((result.records ?? []).filter((record) => !seen.has(recordKey(record)))).slice(0, MAX_EXPORT_ROWS);
      });
      setNextCursor(result.next_cursor ?? '');
      setBackends((current) => Array.from(new Set(current.concat(result.backends ?? []))));
      setTookMS((current) => current + (result.took_ms ?? 0));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoadingMore(false);
    }
  }, [buildRequest, loadingMore, nextCursor]);

  const submit = (event?: React.FormEvent) => {
    event?.preventDefault();
    setCommittedQuery(query);
    setCommittedExclude(exclude);
    setCommittedMode(matchMode);
    setCommittedScope(scopeDraft);
    setRefreshKey((value) => value + 1);
  };

  useEffect(() => {
    void runSearch();
  }, [refreshKey, runSearch]);

  useEffect(() => {
    if (!live) return;
    const timer = window.setInterval(() => void runSearch(true), LIVE_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [live, runSearch]);

  useEffect(() => {
    let cancelled = false;
    const load = () => {
      void listEdges().then((result) => {
        if (!cancelled) setEdges(result.items ?? []);
      }).catch(() => undefined);
    };
    load();
    const unsubscribe = onDevicesChanged(load);
    return () => {
      cancelled = true;
      unsubscribe();
    };
  }, []);

  useEffect(() => {
    const timeWindow = resolveWindow();
    if (!timeWindow) return;
    let cancelled = false;
    void (async () => {
      try {
        const fields = await listLogFields({ start: timeWindow.start, end: timeWindow.end });
        const names = new Set(fields.filter((field) => field.aggregatable).map((field) => field.name));
        const requested = ['cluster_id', 'namespace', 'service_name', 'source_id', 'severity', 'file', 'unit'].filter((name) => names.has(name));
        const values = await Promise.all(requested.map(async (field) => [field, await listLogFieldValues({ field, start: timeWindow.start, end: timeWindow.end, limit: 100 })] as const));
        if (!cancelled) setFieldValues(Object.fromEntries(values));
      } catch {
        // Facet discovery is best-effort; free-form filters remain usable.
      }
    })();
    return () => { cancelled = true; };
  }, [resolveWindow, refreshKey]);

  const openContext = async (record: LogRecord) => {
    setSelected(record);
    setContextLoading(true);
    setContextRows([]);
    try {
      const scope: LogScope = {};
      const device = scopeValue(record, 'device_id');
      const cluster = scopeValue(record, 'cluster_id', 'k8s.cluster.name');
      const namespace = scopeValue(record, 'namespace', 'k8s.namespace.name');
      const workload = scopeValue(record, 'workload', 'k8s.deployment.name', 'k8s.statefulset.name', 'k8s.daemonset.name', 'k8s.job.name', 'k8s.cronjob.name');
      const pod = scopeValue(record, 'pod', 'k8s.pod.name');
      const container = scopeValue(record, 'container', 'k8s.container.name');
      const node = scopeValue(record, 'node', 'k8s.node.name');
      const service = scopeValue(record, 'service_name', 'service.name');
      const source = scopeValue(record, 'source_id', 'ongrid_source');
      const file = scopeValue(record, 'file', 'filename', 'log.file.path');
      const unit = scopeValue(record, 'unit', 'systemd.unit');
      if (device && Number(device) > 0) scope.device_ids = [Number(device)];
      if (cluster) scope.cluster_ids = [cluster];
      if (namespace) scope.namespaces = [namespace];
      if (workload) scope.workloads = [workload];
      if (pod) scope.pods = [pod];
      if (container) scope.containers = [container];
      if (node) scope.nodes = [node];
      if (service) scope.service_names = [service];
      if (source) scope.source_ids = [source];
      if (file) scope.files = [file];
      if (unit) scope.units = [unit];
      setContextRows(await getLogContext({ timestamp: record.timestamp, scope, before: 30, after: 30 }));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setContextLoading(false);
    }
  };

  const exportJSONL = () => {
    const rows = records.slice(0, MAX_EXPORT_ROWS);
    const blob = new Blob([rows.map((record) => JSON.stringify(record)).join('\n')], { type: 'application/x-ndjson' });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = `ongrid-logs-${new Date().toISOString().replace(/[:.]/g, '-')}.jsonl`;
    anchor.click();
    URL.revokeObjectURL(url);
  };

  const maxBucket = Math.max(1, ...histogram.map((bucket) => bucket.count));
  const backendLabel = backends.length === 0 ? tr('日志后端', 'Log backend') : backends.join(' + ');

  return (
    <main className="anim-fade flex min-h-0 flex-1 flex-col overflow-hidden">
      <header className="app-header border-b border-zinc-800/60 px-6 py-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <div className="flex items-center gap-2">
              <h1 className="text-base font-semibold text-zinc-100">{tr('日志中心', 'Log center')}</h1>
              <span className="rounded-full border border-zinc-700 bg-zinc-900 px-2 py-0.5 text-[10px] uppercase tracking-wide text-zinc-400">{backendLabel}</span>
            </div>
            <p className="mt-1 text-xs text-zinc-500">
              {tr('统一检索 Loki 历史与 Elasticsearch 新日志；查询不暴露后端 DSL。', 'Search Loki history and new Elasticsearch logs through one backend-neutral query model.')}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <button type="button" onClick={() => setLive((value) => !value)} className={cn('inline-flex h-8 items-center gap-1.5 rounded-md border px-2.5 text-xs', live ? 'border-emerald-500/50 bg-emerald-500/10 text-emerald-300' : 'border-zinc-700 bg-zinc-900 text-zinc-300')}>
              {live ? <Pause size={12} /> : <Play size={12} />}{live ? tr('实时中', 'Live') : tr('实时', 'Live')}
            </button>
            <button type="button" onClick={exportJSONL} disabled={records.length === 0} className="inline-flex h-8 items-center gap-1.5 rounded-md border border-zinc-700 bg-zinc-900 px-2.5 text-xs text-zinc-300 disabled:opacity-40">
              <Download size={12} /> {tr('导出当前结果', 'Export loaded')}
            </button>
            <button type="button" onClick={() => setRefreshKey((value) => value + 1)} disabled={loading} className="inline-flex h-8 items-center gap-1.5 rounded-md border border-zinc-700 bg-zinc-900 px-2.5 text-xs text-zinc-300 disabled:opacity-40">
              <RefreshCw size={12} className={cn(loading && 'animate-spin')} /> {tr('刷新', 'Refresh')}
            </button>
          </div>
        </div>

        <form onSubmit={submit} className="mt-4 space-y-2">
          <div className="flex flex-wrap items-end gap-2">
            <label className="min-w-[280px] flex-1">
              <span className="mb-1 block text-[11px] text-zinc-500">{tr('日志正文关键词', 'Message keywords')}</span>
              <div className="flex">
                <select value={matchMode} onChange={(event) => setMatchMode(event.target.value as LogMatchMode)} className="h-[34px] rounded-l-md border border-r-0 border-zinc-800 bg-zinc-900 px-2 text-xs text-zinc-300 focus:outline-none">
                  <option value="any">{tr('包含任一', 'Match any')}</option>
                  <option value="all">{tr('包含全部', 'Match all')}</option>
                  <option value="phrase">{tr('精确短语', 'Exact phrase')}</option>
                </select>
                <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder={matchMode === 'phrase' ? tr('例：connection refused', 'e.g. connection refused') : tr('空格分隔；短语可用引号包裹', 'Space-separated; quote multi-word terms')} className={cn(INPUT, 'rounded-l-none font-mono')} />
              </div>
            </label>
            <label className="w-64">
              <span className="mb-1 block text-[11px] text-zinc-500">{tr('排除关键词', 'Exclude keywords')}</span>
              <input value={exclude} onChange={(event) => setExclude(event.target.value)} placeholder={tr('例：debug heartbeat', 'e.g. debug heartbeat')} className={cn(INPUT, 'font-mono')} />
            </label>
            <label className="w-36">
              <span className="mb-1 block text-[11px] text-zinc-500"><Clock size={10} className="mr-1 inline" />{tr('时间范围', 'Time range')}</span>
              <select value={range} onChange={(event) => setRange(event.target.value)} className={INPUT}>
                {RANGE_PRESETS.map((item) => <option key={item.value} value={item.value}>{tr(item.zh, item.en)}</option>)}
              </select>
            </label>
            <button type="submit" disabled={loading} className="inline-flex h-[34px] items-center gap-1.5 rounded-md bg-zinc-100 px-4 text-xs font-medium text-zinc-900 hover:bg-white disabled:opacity-50">
              {loading ? <Loader2 size={12} className="animate-spin" /> : <Search size={12} />}{tr('搜索', 'Search')}
            </button>
          </div>

          <div className="flex flex-wrap items-end gap-2">
            <RoleSelect variant="block" omitUnknown value={role} onChange={(value) => setRole(value as '' | EdgeRole)} className="w-36 shrink-0" />
            <FilterSelect label={tr('设备', 'Device')} value={deviceID} onChange={setDeviceID} options={edges.filter((edge) => edge.device_id != null).map((edge) => ({ value: String(edge.device_id), label: `${edge.name} (#${edge.device_id})` }))} empty={tr('全部设备', 'All devices')} />
            <FilterInput label={tr('集群', 'Cluster')} value={scopeDraft.cluster_ids} onChange={(value) => setScopeDraft((current) => ({ ...current, cluster_ids: value }))} suggestions={fieldValues.cluster_id} />
            <FilterInput label="Namespace" value={scopeDraft.namespaces} onChange={(value) => setScopeDraft((current) => ({ ...current, namespaces: value }))} suggestions={fieldValues.namespace} />
            <FilterInput label={tr('级别', 'Severity')} value={scopeDraft.severities} onChange={(value) => setScopeDraft((current) => ({ ...current, severities: value }))} suggestions={fieldValues.severity} />
            <FilterInput label={tr('文件', 'File')} value={scopeDraft.files} onChange={(value) => setScopeDraft((current) => ({ ...current, files: value }))} suggestions={fieldValues.file} wide />
            <button type="button" onClick={() => setAdvanced((value) => !value)} className="inline-flex h-[34px] items-center gap-1 rounded-md border border-zinc-800 bg-zinc-900 px-2 text-[11px] text-zinc-400 hover:text-zinc-200">
              <ChevronDown size={12} className={cn('transition-transform', advanced && 'rotate-180')} />{tr('更多筛选', 'More filters')}
            </button>
          </div>

          {advanced && (
            <div className="grid grid-cols-2 gap-2 rounded-lg border border-zinc-800/70 bg-zinc-950/50 p-3 md:grid-cols-3 xl:grid-cols-5">
              <FilterInput label="Workload" value={scopeDraft.workloads} onChange={(value) => setScopeDraft((current) => ({ ...current, workloads: value }))} />
              <FilterInput label="Pod" value={scopeDraft.pods} onChange={(value) => setScopeDraft((current) => ({ ...current, pods: value }))} />
              <FilterInput label="Container" value={scopeDraft.containers} onChange={(value) => setScopeDraft((current) => ({ ...current, containers: value }))} />
              <FilterInput label="Node" value={scopeDraft.nodes} onChange={(value) => setScopeDraft((current) => ({ ...current, nodes: value }))} />
              <FilterInput label="systemd unit" value={scopeDraft.units} onChange={(value) => setScopeDraft((current) => ({ ...current, units: value }))} suggestions={fieldValues.unit} />
              <FilterInput label="Service" value={scopeDraft.service_names} onChange={(value) => setScopeDraft((current) => ({ ...current, service_names: value }))} suggestions={fieldValues.service_name} />
              <FilterInput label="Source" value={scopeDraft.source_ids} onChange={(value) => setScopeDraft((current) => ({ ...current, source_ids: value }))} suggestions={fieldValues.source_id} />
            </div>
          )}

          {range === 'custom' && (
            <div className="flex gap-2">
              <input type="datetime-local" value={customStart} onChange={(event) => setCustomStart(event.target.value)} className={cn(INPUT, 'w-52')} />
              <span className="self-center text-xs text-zinc-600">→</span>
              <input type="datetime-local" value={customEnd} onChange={(event) => setCustomEnd(event.target.value)} className={cn(INPUT, 'w-52')} />
            </div>
          )}

          <div className="flex flex-wrap items-center gap-1.5 pt-1">
            <span className="text-[11px] text-zinc-600">{tr('快捷：', 'Quick:')}</span>
            {QUICK_SEARCHES.map((item) => (
              <button key={item.en} type="button" onClick={() => { setQuery(item.value); setMatchMode(item.mode); setCommittedQuery(item.value); setCommittedMode(item.mode); setRefreshKey((value) => value + 1); }} className="rounded-full border border-zinc-800 bg-zinc-900 px-2 py-0.5 text-[11px] text-zinc-300 hover:border-zinc-600">
                {tr(item.zh, item.en)}
              </button>
            ))}
          </div>
        </form>
      </header>

      <section className="border-b border-zinc-800/60 bg-zinc-950/40 px-6 py-2">
        <div className="mb-1 flex items-center justify-between text-[10px] text-zinc-500">
          <span className="inline-flex items-center gap-1"><BarChart3 size={11} />{tr('日志量', 'Log volume')}</span>
          <span>{records.length} {tr('条已加载', 'loaded')} · {tookMS} ms{nextCursor ? ` · ${tr('还有更多', 'more available')}` : ''}</span>
        </div>
        <div className="flex h-12 items-end gap-px" aria-label={tr('日志时间直方图', 'Log time histogram')}>
          {histogram.length === 0 ? <div className="h-px w-full self-center bg-zinc-800" /> : histogram.map((bucket) => (
            <div key={bucket.start} title={`${new Date(bucket.start).toLocaleString()} · ${bucket.count}`} className="min-w-[2px] flex-1 rounded-t-sm bg-sky-500/65 hover:bg-sky-400" style={{ height: `${Math.max(3, (bucket.count / maxBucket) * 100)}%` }} />
          ))}
        </div>
      </section>

      <div className="flex min-h-0 flex-1 overflow-hidden">
        <section className="min-w-0 flex-1 overflow-y-auto px-4 py-3">
          {error && <div className="mb-3 flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-300"><AlertTriangle size={13} className="mt-0.5 shrink-0" /><span>{error}</span></div>}
          {loading && records.length === 0 && <div className="flex h-40 items-center justify-center text-xs text-zinc-500"><Loader2 size={16} className="mr-2 animate-spin" />{tr('正在检索日志…', 'Searching logs…')}</div>}
          {!loading && !error && records.length === 0 && (
            <div className="flex h-56 flex-col items-center justify-center text-center">
              <FileSearch size={30} className="mb-3 text-zinc-700" />
              <p className="text-sm text-zinc-300">{tr('该时间窗内没有匹配日志', 'No matching logs in this time range')}</p>
              <p className="mt-1 max-w-lg text-xs text-zinc-600">{tr('新安装的 Edge 会由 otelcol-contrib 采集 journald、文件或 Kubernetes 容器日志；也可以扩大时间窗或减少筛选条件。', 'New edges use otelcol-contrib for journald, files, and Kubernetes container logs. Try a wider window or fewer filters.')}</p>
            </div>
          )}
          <div className="space-y-1 font-mono text-[12px] leading-snug">
            {records.map((record) => <LogRow key={recordKey(record)} record={record} selected={selected != null && recordKey(selected) === recordKey(record)} onContext={() => void openContext(record)} />)}
          </div>
          {nextCursor && (
            <div className="flex justify-center py-4">
              <button type="button" onClick={() => void loadMore()} disabled={loadingMore || records.length >= MAX_EXPORT_ROWS} className="inline-flex items-center gap-1.5 rounded-md border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-xs text-zinc-300 disabled:opacity-40">
                {loadingMore ? <Loader2 size={12} className="animate-spin" /> : <ChevronDown size={12} />}{records.length >= MAX_EXPORT_ROWS ? tr('已达到 1000 条页面上限', '1,000-row page cap reached') : tr('加载更多', 'Load more')}
              </button>
            </div>
          )}
        </section>

        {selected && (
          <aside className="w-[430px] shrink-0 overflow-y-auto border-l border-zinc-800 bg-zinc-950/80">
            <div className="sticky top-0 z-10 flex items-center justify-between border-b border-zinc-800 bg-zinc-950 px-4 py-3">
              <div><p className="text-xs font-medium text-zinc-200">{tr('上下文日志', 'Log context')}</p><p className="mt-0.5 text-[10px] text-zinc-600">{new Date(selected.timestamp).toLocaleString()}</p></div>
              <button type="button" onClick={() => { setSelected(null); setContextRows([]); }} className="rounded p-1 text-zinc-500 hover:bg-zinc-900 hover:text-zinc-200"><X size={14} /></button>
            </div>
            <div className="p-3">
              {contextLoading ? <div className="flex h-24 items-center justify-center text-xs text-zinc-500"><Loader2 size={14} className="mr-2 animate-spin" />{tr('读取前后文…', 'Loading context…')}</div> : (
                <div className="space-y-1 font-mono text-[11px]">
                  {contextRows.map((record) => <ContextRow key={recordKey(record)} record={record} active={recordKey(record) === recordKey(selected)} />)}
                </div>
              )}
            </div>
          </aside>
        )}
      </div>
    </main>
  );
}

function FilterSelect({ label, value, onChange, options, empty }: { label: string; value: string; onChange: (value: string) => void; options: { value: string; label: string }[]; empty: string }) {
  return <label className="block w-48 shrink-0"><span className="mb-1 block text-[11px] text-zinc-500">{label}</span><select value={value} onChange={(event) => onChange(event.target.value)} className={INPUT}><option value="">{empty}</option>{options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></label>;
}

function FilterInput({ label, value, onChange, suggestions, wide = false }: { label: string; value: string; onChange: (value: string) => void; suggestions?: string[]; wide?: boolean }) {
  const listID = `log-filter-${label.replace(/[^a-zA-Z0-9]/g, '-').toLowerCase()}`;
  return <label className={cn('block shrink-0', wide ? 'w-56' : 'w-40')}><span className="mb-1 block text-[11px] text-zinc-500">{label}</span><input value={value} onChange={(event) => onChange(event.target.value)} list={suggestions?.length ? listID : undefined} placeholder="*" className={cn(INPUT, 'font-mono')} />{suggestions?.length ? <datalist id={listID}>{Array.from(new Set(suggestions)).slice(0, 100).map((item) => <option key={item} value={item} />)}</datalist> : null}</label>;
}

function LogRow({ record, selected, onContext }: { record: LogRecord; selected: boolean; onContext: () => void }) {
  const timestamp = new Date(record.timestamp);
  const severity = record.severity_text || scopeValue(record, 'level', 'severity');
  const device = scopeValue(record, 'device_id');
  const service = scopeValue(record, 'service.name', 'service_name');
  const pod = scopeValue(record, 'k8s.pod.name', 'pod');
  const source = scopeValue(record, 'ongrid_source');
  const level = severity.toLowerCase();
  const color = /fatal|error|critical|panic/.test(level) ? 'bg-red-500' : /warn/.test(level) ? 'bg-amber-500' : /info|notice/.test(level) ? 'bg-sky-500' : 'bg-zinc-600';
  return (
    <button type="button" onClick={onContext} className={cn('group grid w-full grid-cols-[82px_7px_minmax(0,1fr)] gap-2 rounded px-2 py-1.5 text-left hover:bg-zinc-900', selected && 'bg-zinc-900 ring-1 ring-zinc-700')}>
      <span className="tabular-nums text-zinc-600">{formatLogTime(timestamp)}</span>
      <span className={cn('mt-1 h-2 w-2 rounded-full', color)} />
      <span className="min-w-0">
        <span className="break-words text-zinc-200">{record.message}</span>
        <span className="mt-1 flex flex-wrap gap-1 text-[10px] text-zinc-600">
          <Tag value={record.backend} />{severity && <Tag value={severity} />}{device && <Tag value={`device:${device}`} />}{service && <Tag value={`service:${service}`} />}{pod && <Tag value={`pod:${pod}`} />}{source && <Tag value={`source:${source}`} />}{record.trace_id && <Tag value={`trace:${record.trace_id.slice(0, 12)}…`} />}
          <span className="ml-auto hidden items-center gap-0.5 text-zinc-500 group-hover:inline-flex"><ChevronRight size={10} />context</span>
        </span>
      </span>
    </button>
  );
}

function ContextRow({ record, active }: { record: LogRecord; active: boolean }) {
  return <div className={cn('rounded border-l-2 px-2 py-1.5', active ? 'border-sky-400 bg-sky-500/10 text-zinc-100' : 'border-zinc-800 text-zinc-400')}><span className="mr-2 text-zinc-600">{new Date(record.timestamp).toLocaleTimeString(undefined, { hour12: false })}</span><span className="break-words">{record.message}</span></div>;
}

function Tag({ value }: { value: string }) {
  return <span className="rounded bg-zinc-900 px-1 py-px text-zinc-500">{value}</span>;
}
