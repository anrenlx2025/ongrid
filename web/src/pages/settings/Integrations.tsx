import { useCallback, useEffect, useState, type ReactNode } from 'react';
import {
  Activity,
  Check,
  ChevronDown,
  ChevronRight,
  ExternalLink,
  Eye,
  EyeOff,
  Save,
  Database,
  Loader2,
  PlugZap,
  Cloud,
  FileText,
  GitBranch,
  Search,
  RefreshCw,
} from 'lucide-react';
import {
  openMetricDrilldown,
  openObservabilityUrl,
  invalidateGrafanaRootCache,
  buildExploreUrl,
} from '@/lib/drilldown';
import { useObservability } from '@/store/observability';
import {
  listSettings,
  setSetting,
  revealSetting,
  testGrafanaConnection,
  syncGrafana,
  syncLokiDatasource,
  testPromConnection,
  testLokiConnection,
  testTempoConnection,
  testWebSearchConnection,
  type SystemSetting,
  type GrafanaSyncResult,
} from '@/api/settings';
import {
  activateLogBackend,
  getLogBackend,
  rollbackLogBackend,
  saveLogBackend,
  testLogBackend,
  type LogBackend,
  type SaveLogBackendInput,
} from '@/api/logs';
import { listSecrets, type SecretView } from '@/api/secrets';
import { listEdges, type Edge } from '@/api/edges';
import { getPluginCounts } from '@/api/integrations';
import { ApiError } from '@/api/client';
import { Button, Card } from '@/components/ui';
import { cn } from '@/lib/cn';
import { useI18n } from '@/i18n/locale';

// Settings → 集成. Four cards, all backend-driven, parallel naming:
//   - Prometheus 集成   → system_settings.prom; manager reads on every
//     remote_write / PromQL call (auth ~5s TTL, URLs at restart)
//   - Grafana 集成      → system_settings.grafana; "测试" + "同步"
//   - Loki 集成 (日志)  → system_settings.loki; the URL feeds both
//     edge-side push (logs plugin) and Grafana datasource
//   - Tempo 集成 (链路) → system_settings.tempo; same pattern as Loki
//     for the trace signal
//
// Empty Loki/Tempo URL falls back to the docker-internal seed (set on
// first boot from ONGRID_LOG_URL / ONGRID_TRACE_QUERY_URL); a
// hostname with a dot or IP is treated as customer-supplied and
// edges push directly there.
export default function SettingsIntegrations() {
  return (
    <div className="space-y-5">
      <PrometheusCard />
      <GrafanaCard />
      <ElasticsearchLogsCard />
      <LokiCard />
      <TempoCard />
      <WebSearchCard />
    </div>
  );
}

// ---------- Prometheus card (backend-driven) ----------

type PromForm = {
  query_url: string;
  remote_write_url: string;
  bearer_token: string;
  basic_user: string;
  basic_password: string;
};

const PROM_KEYS: (keyof PromForm)[] = [
  'query_url',
  'remote_write_url',
  'bearer_token',
  'basic_user',
  'basic_password',
];

const PROM_SENSITIVE: Set<keyof PromForm> = new Set(['bearer_token', 'basic_password']);

const emptyPromForm: PromForm = {
  query_url: '',
  remote_write_url: '',
  bearer_token: '',
  basic_user: '',
  basic_password: '',
};

function PrometheusCard() {
  const { tr } = useI18n();
  // Both sensitive and non-sensitive fields land in draft as cleartext.
  // For sensitive rows we hit the /reveal endpoint after the masked list
  // returns, then populate draft with the real value. The input is
  // type=password by default (renders as ●●●●●●) and an eye-icon flips
  // to type=text to expose the chars. Diff/save logic is then trivial:
  // draft[k] !== server[k] regardless of sensitivity.
  const [server, setServer] = useState<PromForm>(emptyPromForm);
  const [draft, setDraft] = useState<PromForm>(emptyPromForm);
  const [revealed, setRevealed] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [savedOk, setSavedOk] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // Probe state — separate from form state so saving doesn't reset the
  // last known test outcome and probe failures don't make the form look
  // dirty.
  const [probe, setProbe] = useState<
    | { kind: 'idle' }
    | { kind: 'testing' }
    | { kind: 'ok' }
    | { kind: 'error'; msg: string }
  >({ kind: 'idle' });

  const refresh = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const r = await listSettings('prom');
      const next = { ...emptyPromForm };
      for (const it of r.items as SystemSetting[]) {
        if (!(PROM_KEYS as string[]).includes(it.key)) continue;
        if (!PROM_SENSITIVE.has(it.key as keyof PromForm)) {
          (next as Record<string, string>)[it.key] = it.value ?? '';
        }
      }
      // Fetch plaintext for any sensitive row that has a stored value.
      // Run in parallel — a single failure shouldn't block other rows.
      await Promise.all(
        (r.items as SystemSetting[])
          .filter((it) => PROM_SENSITIVE.has(it.key as keyof PromForm) && (it.value ?? '') !== '')
          .map(async (it) => {
            try {
              const real = await revealSetting('prom', it.key);
              (next as Record<string, string>)[it.key] = real.value ?? '';
            } catch {
              /* leave the field empty so the user can paste a fresh value */
            }
          })
      );
      setServer(next);
      setDraft(next);
      setRevealed({});
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Dirty iff any field differs from what we loaded.
  const dirty = PROM_KEYS.some((k) => draft[k] !== server[k]);

  const update = (k: keyof PromForm, v: string) => {
    setSavedOk(false);
    setDraft((cur) => ({ ...cur, [k]: v }));
  };

  const submit = async () => {
    setSaving(true);
    setErr(null);
    try {
      for (const k of PROM_KEYS) {
        if (draft[k] === server[k]) continue;
        await setSetting('prom', k, draft[k], PROM_SENSITIVE.has(k));
      }
      await refresh();
      setSavedOk(true);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const probeProm = async () => {
    setProbe({ kind: 'testing' });
    try {
      await testPromConnection();
      setProbe({ kind: 'ok' });
    } catch (e) {
      setProbe({ kind: 'error', msg: e instanceof ApiError ? e.message : (e as Error).message });
    }
  };

  return (
    <Card className="p-5">
      <div className="mb-3 flex items-center gap-2">
        <Database size={14} className="text-zinc-400" />
        <h2 className="text-sm font-medium text-zinc-100">{tr('Prometheus 集成', 'Prometheus integration')}</h2>
      </div>
      <p className="mb-4 text-[11px] text-zinc-500">
        {tr(
          'Manager 用这里的 URL + 凭证写入 / 查询 TSDB。支持原版 Prometheus、VictoriaMetrics、Mimir、Cortex、Thanos receive。保存后 ~5 秒内对所有新请求生效，',
          'Manager uses this URL + credentials to write / query the TSDB. Works with vanilla Prometheus, VictoriaMetrics, Mimir, Cortex, Thanos receive. New requests pick up changes within ~5 s, ',
        )}<b>{tr('无需重启', 'no restart needed')}</b>{tr('。', '.')}
        <br />
        <span className="text-zinc-600">
          {tr(
            '内建 Prometheus 在 docker 内网，',
            'The built-in Prometheus runs on the docker internal network and ',
          )}<b>{tr('无需鉴权', 'requires no auth')}</b>{tr(
            '——下面 Bearer / Basic 留空。仅在对接外部带认证的 TSDB 时填写。',
            ' — leave Bearer / Basic blank below. Only fill them when pointing at an external auth-protected TSDB.',
          )}
        </span>
        <br />
        <span className="text-zinc-600">{tr('注：切换数据源后老数据留在原 TSDB，不会自动搬过来。', 'Note: switching data sources leaves old data in the original TSDB — it does not migrate automatically.')}</span>
      </p>

      {loading ? (
        <div className="flex h-32 items-center justify-center text-sm text-zinc-500">
          <Loader2 size={14} className="mr-2 animate-spin" /> {tr('加载中…', 'Loading…')}
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
          <PromField
            label="Query URL"
            hint={tr('PromQL 查询根，例 http://prom:9090 或 https://vm.example.com', 'PromQL query root, e.g. http://prom:9090 or https://vm.example.com')}
            value={draft.query_url}
            onChange={(v) => update('query_url', v)}
            placeholder="http://prometheus:9090/prometheus"
          />
          <PromField
            label="Remote Write URL"
            hint={tr('留空则取 Query URL + /api/v1/write', 'Empty = Query URL + /api/v1/write')}
            value={draft.remote_write_url}
            onChange={(v) => update('remote_write_url', v)}
            placeholder="https://vm.example.com/api/v1/write"
          />
          <PromField
            label="Bearer Token"
            hint={tr('Authorization: Bearer ... 优先于 Basic', 'Authorization: Bearer ... takes precedence over Basic')}
            sensitive
            revealed={!!revealed.bearer_token}
            onToggleReveal={() => setRevealed((r) => ({ ...r, bearer_token: !r.bearer_token }))}
            value={draft.bearer_token}
            onChange={(v) => update('bearer_token', v)}
            placeholder={tr('（留空 = 不用 Bearer）', '(empty = no Bearer)')}
          />
          <PromField
            label="Basic User"
            value={draft.basic_user}
            onChange={(v) => update('basic_user', v)}
            placeholder={tr('（留空 = 不用 Basic）', '(empty = no Basic)')}
          />
          <PromField
            label="Basic Password"
            sensitive
            revealed={!!revealed.basic_password}
            onToggleReveal={() => setRevealed((r) => ({ ...r, basic_password: !r.basic_password }))}
            value={draft.basic_password}
            onChange={(v) => update('basic_password', v)}
            placeholder={tr('（留空 = 不用 Basic）', '(empty = no Basic)')}
          />
        </div>
      )}

      <div className="mt-5 flex flex-wrap items-center gap-3">
        <Button onClick={submit} disabled={!dirty || saving} variant="primary">
          {savedOk && !dirty ? <Check size={14} /> : <Save size={14} />}
          <span>{saving ? tr('保存中…', 'Saving…') : savedOk && !dirty ? tr('已保存', 'Saved') : tr('保存', 'Save')}</span>
        </Button>
        <Button
          onClick={probeProm}
          disabled={probe.kind === 'testing' || dirty || server.query_url.trim() === ''}
          variant="ghost"
        >
          {probe.kind === 'testing' ? <Loader2 size={14} className="animate-spin" /> : <PlugZap size={14} />}
          <span>{tr('测试连接', 'Test connection')}</span>
        </Button>
        <span className="text-xs text-zinc-500">
          {dirty ? tr('有未保存修改', 'Unsaved changes') : tr('保存后 ~5 秒内自动生效（无需重启）', 'Takes effect within ~5 s of saving (no restart needed)')}
        </span>
        {err && <span className="text-xs text-red-400">{err}</span>}
      </div>
      <PromProbeLine probe={probe} />
    </Card>
  );
}

// PromProbeLine renders the result of the most recent test-connection
// click. Lives next to the form so a passing probe doesn't clutter the
// rest of the page.
function PromProbeLine({
  probe,
}: {
  probe: { kind: 'idle' } | { kind: 'testing' } | { kind: 'ok' } | { kind: 'error'; msg: string };
}) {
  const { tr } = useI18n();
  switch (probe.kind) {
    case 'ok':
      return <p className="mt-3 text-xs text-emerald-400">{tr('✓ Prom 可达，PromQL 探针 (up) 返回成功', '✓ Prom reachable, PromQL probe (up) returned success')}</p>;
    case 'error':
      return <p className="mt-3 break-all text-xs text-red-400">✗ {probe.msg}</p>;
    default:
      return null;
  }
}

function PromField({
  label,
  hint,
  value,
  onChange,
  placeholder,
  sensitive,
  revealed,
  onToggleReveal,
}: {
  label: string;
  hint?: string;
  value: string;
  onChange(v: string): void;
  placeholder?: string;
  sensitive?: boolean;
  // When sensitive, parent owns the revealed flag (so eye state persists
  // across re-renders) and provides a toggle. Default = hidden (●●●●).
  revealed?: boolean;
  onToggleReveal?: () => void;
}) {
  const inputType = sensitive ? (revealed ? 'text' : 'password') : 'text';
  return (
    <label className="block">
      <span className="mb-1 flex items-center gap-1.5 text-xs text-zinc-400">
        {label}
        {sensitive && (
          <span className="rounded border border-amber-700/50 bg-amber-900/20 px-1 text-[10px] text-amber-300">
            sensitive
          </span>
        )}
      </span>
      <div className="relative">
        <input
          type={inputType}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={placeholder}
          className={cn(
            'w-full rounded-lg border border-zinc-800 bg-zinc-950/40 px-3 py-2 text-sm text-zinc-100 placeholder:text-zinc-600 focus:border-zinc-600 focus:outline-none',
            sensitive && 'pr-9'
          )}
          autoComplete="off"
        />
        {sensitive && onToggleReveal && (
          <button
            type="button"
            onClick={onToggleReveal}
            tabIndex={-1}
            aria-label={revealed ? 'Hide' : 'Show'}
            className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-1 text-zinc-500 hover:bg-zinc-800 hover:text-zinc-200"
          >
            {revealed ? <EyeOff size={14} /> : <Eye size={14} />}
          </button>
        )}
      </div>
      {hint && <span className="mt-1 block text-[11px] text-zinc-500">{hint}</span>}
    </label>
  );
}

// ---------- Grafana card (backend-driven) ----------

type GrafanaForm = {
  root_url: string;
  sa_token: string;
  api_key: string;
  org_id: string;
};
const GRAFANA_KEYS: (keyof GrafanaForm)[] = ['root_url', 'sa_token', 'api_key', 'org_id'];
const GRAFANA_SENSITIVE: Set<keyof GrafanaForm> = new Set(['sa_token', 'api_key']);
const emptyGrafanaForm: GrafanaForm = { root_url: '', sa_token: '', api_key: '', org_id: '' };

type SyncStatus =
  | { kind: 'idle' }
  | { kind: 'testing' }
  | { kind: 'tested-ok' }
  | { kind: 'syncing' }
  | { kind: 'synced'; res: GrafanaSyncResult }
  | { kind: 'error'; msg: string };

function GrafanaCard() {
  const { tr } = useI18n();
  // Same eager-reveal discipline as PrometheusCard. Sensitive rows land
  // in draft with cleartext via /reveal so the eye toggle has something
  // to expose, but the input defaults to type=password so dots show.
  const [server, setServer] = useState<GrafanaForm>(emptyGrafanaForm);
  const [draft, setDraft] = useState<GrafanaForm>(emptyGrafanaForm);
  const [revealed, setRevealed] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [savedOk, setSavedOk] = useState(false);
  const [status, setStatus] = useState<SyncStatus>({ kind: 'idle' });

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const r = await listSettings('grafana');
      const next = { ...emptyGrafanaForm };
      for (const it of r.items as SystemSetting[]) {
        if (!(GRAFANA_KEYS as string[]).includes(it.key)) continue;
        if (!GRAFANA_SENSITIVE.has(it.key as keyof GrafanaForm)) {
          (next as Record<string, string>)[it.key] = it.value ?? '';
        }
      }
      await Promise.all(
        (r.items as SystemSetting[])
          .filter((it) => GRAFANA_SENSITIVE.has(it.key as keyof GrafanaForm) && (it.value ?? '') !== '')
          .map(async (it) => {
            try {
              const real = await revealSetting('grafana', it.key);
              (next as Record<string, string>)[it.key] = real.value ?? '';
            } catch {
              /* leave the field empty so the user can paste a fresh token */
            }
          })
      );
      setServer(next);
      setDraft(next);
      setRevealed({});
    } catch (e) {
      setStatus({ kind: 'error', msg: e instanceof ApiError ? e.message : (e as Error).message });
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const dirty = GRAFANA_KEYS.some((k) => draft[k] !== server[k]);

  const update = (k: keyof GrafanaForm, v: string) => {
    setSavedOk(false);
    setDraft((cur) => ({ ...cur, [k]: v }));
  };

  const save = async () => {
    setSaving(true);
    setStatus({ kind: 'idle' });
    try {
      for (const k of GRAFANA_KEYS) {
        if (draft[k] === server[k]) continue;
        await setSetting('grafana', k, draft[k], GRAFANA_SENSITIVE.has(k));
      }
      // Invalidate the drilldown helper's root_url cache so the next
      // chart-page 「打开 Grafana」 click picks up the new URL instantly,
      // not after the 60s TTL.
      invalidateGrafanaRootCache();
      await refresh();
      setSavedOk(true);
    } catch (e) {
      setStatus({ kind: 'error', msg: e instanceof ApiError ? e.message : (e as Error).message });
    } finally {
      setSaving(false);
    }
  };

  const test = async () => {
    setStatus({ kind: 'testing' });
    try {
      await testGrafanaConnection();
      setStatus({ kind: 'tested-ok' });
    } catch (e) {
      setStatus({ kind: 'error', msg: e instanceof ApiError ? e.message : (e as Error).message });
    }
  };

  const sync = async () => {
    setStatus({ kind: 'syncing' });
    try {
      const res = await syncGrafana();
      setStatus({ kind: 'synced', res });
    } catch (e) {
      setStatus({ kind: 'error', msg: e instanceof ApiError ? e.message : (e as Error).message });
    }
  };

  const testJump = () => {
    void openMetricDrilldown({ expr: 'up', rangeInput: '1h', stepInput: '30s', title: 'up' });
  };

  return (
    <Card className="p-5">
      <div className="mb-3 flex items-center gap-2">
        <Activity size={14} className="text-zinc-400" />
        <h2 className="text-sm font-medium text-zinc-100">{tr('Grafana 集成', 'Grafana integration')}</h2>
      </div>
      <p className="mb-4 text-[11px] text-zinc-500">
        {tr(
          '填 Grafana 根地址 + Service Account Token，「测试」验通，「同步」自动把 ',
          'Fill in the Grafana root URL + Service Account Token. "Test" verifies the connection; "Sync" pushes ',
        )}
        <code className="mx-1 font-mono text-zinc-400">ongrid-prometheus</code>
        {tr(' 数据源和默认 dashboard 推到 Grafana 的 ', ' datasource and default dashboards into the Grafana ')}<code className="mx-1 font-mono text-zinc-400">ongrid</code>
        {tr(' 文件夹。跳转过去仍然由用户在 Grafana 那边登录（Ongrid 不代登录）。', ' folder. Jumping to Grafana still requires the user to sign in there (Ongrid does not impersonate).')}
      </p>

      {loading ? (
        <div className="flex h-32 items-center justify-center text-sm text-zinc-500">
          <Loader2 size={14} className="mr-2 animate-spin" /> {tr('加载中…', 'Loading…')}
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
          <PromField
            label="Grafana Root URL"
            hint={tr('例 https://grafana.example.com（不含路径）', 'e.g. https://grafana.example.com (no path)')}
            value={draft.root_url}
            onChange={(v) => update('root_url', v)}
            placeholder="https://grafana.example.com"
          />
          <PromField
            label="Service Account Token"
            hint={
              server.sa_token !== ''
                ? tr('已有 token（首次启动 manager 会为内建 Grafana 自动生成）。点眼睛查看，要轮换直接粘新值', 'Token already set (auto-generated for the built-in Grafana on first manager boot). Click the eye to reveal; paste a new one to rotate.')
                : tr('Grafana → Administration → Service accounts 里建 admin 账号生成', 'Create an admin Service Account at Grafana → Administration → Service accounts')
            }
            sensitive
            revealed={!!revealed.sa_token}
            onToggleReveal={() => setRevealed((r) => ({ ...r, sa_token: !r.sa_token }))}
            value={draft.sa_token}
            onChange={(v) => update('sa_token', v)}
            placeholder="glsa_..."
          />
          <PromField
            label={tr('API Key（外接 Grafana 备用）', 'API Key (fallback for external Grafana)')}
            hint={tr('对接客户自有 Grafana 但不便建 SA 时填这里；与 SA Token 选其一即可，SA 优先', "Use this when you can't create a Service Account on a customer Grafana. Choose either SA Token or API Key; SA wins if both are set.")}
            sensitive
            revealed={!!revealed.api_key}
            onToggleReveal={() => setRevealed((r) => ({ ...r, api_key: !r.api_key }))}
            value={draft.api_key}
            onChange={(v) => update('api_key', v)}
            placeholder="eyJrIj..."
          />
          <PromField
            label="Org ID"
            hint={tr('多组织 Grafana 用，单组织默认 1 即可', 'For multi-org Grafana; single-org installs always use 1')}
            value={draft.org_id}
            onChange={(v) => update('org_id', v)}
            placeholder="1"
          />
        </div>
      )}

      <div className="mt-5 flex flex-wrap items-center gap-3">
        <Button onClick={save} disabled={!dirty || saving} variant="primary">
          {savedOk && !dirty ? <Check size={14} /> : <Save size={14} />}
          <span>{saving ? tr('保存中…', 'Saving…') : savedOk && !dirty ? tr('已保存', 'Saved') : tr('保存', 'Save')}</span>
        </Button>
        <Button
          onClick={test}
          disabled={status.kind === 'testing' || dirty || !canTestSync(server)}
          variant="ghost"
        >
          {status.kind === 'testing' ? <Loader2 size={14} className="animate-spin" /> : <PlugZap size={14} />}
          <span>{tr('测试连接', 'Test connection')}</span>
        </Button>
        <button
          type="button"
          onClick={sync}
          disabled={status.kind === 'syncing' || dirty || !canTestSync(server)}
          className="inline-flex items-center gap-1.5 rounded-lg border border-emerald-700/60 bg-emerald-900/20 px-3 py-1.5 text-sm text-emerald-300 transition-colors hover:bg-emerald-900/40 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {status.kind === 'syncing' ? <Loader2 size={14} className="animate-spin" /> : <Cloud size={14} />}
          <span>{tr('同步 dashboard', 'Sync dashboard')}</span>
        </button>
        <button
          type="button"
          onClick={testJump}
          className="inline-flex items-center gap-1.5 rounded-lg border border-zinc-700 px-3 py-1.5 text-sm text-zinc-200 transition-colors hover:border-zinc-500 hover:bg-zinc-800"
        >
          <ExternalLink size={14} />
          <span>{tr('测试跳转', 'Test jump')}</span>
        </button>
      </div>

      <StatusLine status={status} dirty={dirty} />

      <GrafanaDrilldownAdvanced />
    </Card>
  );
}

// GrafanaDrilldownAdvanced is the optional bit that used to live on
// Settings → 通用 (now removed). Long-tail config: "what dashboard UID
// does the chart-page 「打开 Grafana」 button deep-link into" + "which
// Grafana org id". 99% of users never touch this — collapsed by default
// and clearly marked per-browser only.
function GrafanaDrilldownAdvanced() {
  const { tr } = useI18n();
  const { grafanaDashboardUid, grafanaOrgId, setConfig } = useObservability();
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState({ grafanaDashboardUid, grafanaOrgId });
  const [savedFlag, setSavedFlag] = useState(false);

  useEffect(() => {
    setDraft({ grafanaDashboardUid, grafanaOrgId });
  }, [grafanaDashboardUid, grafanaOrgId]);

  const dirty =
    draft.grafanaDashboardUid !== grafanaDashboardUid || draft.grafanaOrgId !== grafanaOrgId;

  function save() {
    setConfig({
      grafanaDashboardUid: draft.grafanaDashboardUid,
      grafanaOrgId: draft.grafanaOrgId,
    });
    setSavedFlag(true);
  }

  return (
    <div className="mt-6 border-t border-zinc-800 pt-4">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1 text-[11px] text-zinc-500 hover:text-zinc-300"
      >
        {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
        <span>{tr('高级：图表「打开 Grafana」深链参数（仅当前浏览器）', 'Advanced: chart "Open in Grafana" deep-link params (this browser only)')}</span>
      </button>
      {open && (
        <div className="mt-3 grid grid-cols-1 gap-4 md:grid-cols-2">
          <PromField
            label={tr('设备详情 Dashboard UID', 'Device detail Dashboard UID')}
            hint={tr('安装包默认 provision 为 ongrid-server-detail；只有把面板复制到自己文件夹换了 UID 时才需要改', 'The installer provisions this as ongrid-server-detail by default; only change it if you copied the dashboard to your own folder with a new UID')}
            value={draft.grafanaDashboardUid}
            onChange={(v) => {
              setSavedFlag(false);
              setDraft((d) => ({ ...d, grafanaDashboardUid: v }));
            }}
            placeholder="ongrid-server-detail"
          />
          <PromField
            label="Grafana orgId"
            hint={tr('单 org 安装永远是 1；多 org 隔离时填对应 id', 'Single-org installs are always 1; use the matching id for multi-org isolation')}
            value={draft.grafanaOrgId}
            onChange={(v) => {
              setSavedFlag(false);
              setDraft((d) => ({ ...d, grafanaOrgId: v }));
            }}
            placeholder="1"
          />
          <div className="md:col-span-2 flex items-center gap-3">
            <button
              type="button"
              onClick={save}
              disabled={!dirty}
              className="inline-flex items-center gap-1.5 rounded-md border border-zinc-700 bg-zinc-900 px-2.5 py-1.5 text-xs text-zinc-200 hover:bg-zinc-800 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <Save size={12} />
              <span>{savedFlag && !dirty ? tr('已保存', 'Saved') : tr('保存', 'Save')}</span>
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

// canTestSync gates the Test/Sync buttons. root_url is required; the
// bearer can come from EITHER sa_token (preferred — the embedded
// bootstrap path mints it) OR api_key (operator-pasted, used for
// external Grafana where they don't have admin to mint a fresh SA).
// sa_token / api_key come back via /reveal so they're actually
// populated when the row exists (the older `!server.sa_token` check
// was always-disabled because we used to strip sensitive values).
function canTestSync(form: GrafanaForm): boolean {
  if (form.root_url.trim() === '') return false;
  return form.sa_token.trim() !== '' || form.api_key.trim() !== '';
}

function StatusLine({ status, dirty }: { status: SyncStatus; dirty: boolean }) {
  const { tr } = useI18n();
  if (dirty) {
    return <p className="mt-3 text-xs text-zinc-500">{tr('有未保存修改，先保存才能测试 / 同步', 'Unsaved changes — save first to test / sync')}</p>;
  }
  switch (status.kind) {
    case 'tested-ok':
      return <p className="mt-3 text-xs text-emerald-400">{tr('✓ Grafana 可达，认证通过', '✓ Grafana reachable, auth passed')}</p>;
    case 'synced':
      return (
        <p className="mt-3 text-xs text-emerald-400">
          {tr('✓ 已同步到文件夹 ', '✓ Synced to folder ')}
          <code className="font-mono">{status.res.folder}</code>{tr(' · 数据源 ', ' · datasource ')}
          <code className="font-mono">{status.res.datasource}</code>{tr(` · ${status.res.dashboards.length} 个 dashboard`, ` · ${status.res.dashboards.length} dashboard(s)`)}
          {status.res.dashboards.length > 0 && (
            <span className="text-zinc-500">{tr('：', ': ')}{status.res.dashboards.join(tr('、', ', '))}</span>
          )}
        </p>
      );
    case 'error':
      return <p className="mt-3 break-all text-xs text-red-400">✗ {status.msg}</p>;
    default:
      return null;
  }
}

// ---------- Loki / Tempo cards (read-only status, jump to Grafana) ----

// useGrafanaExploreLink builds a Grafana Explore deep-link for one of
// the built-in datasources. It mirrors lib/drilldown.ts's behaviour:
//   1. Pull root_url from system_settings.grafana
//   2. Reject docker-internal hosts (loki:3100 / grafana:3000) the
//      browser can't reach — fall back to same-origin /grafana.
//   3. Build /explore?left={"datasource":...,"queries":[{"expr":...}]}
// Returns null while the root URL is still being fetched so the button
// renders disabled rather than pointing at the wrong place.
function useGrafanaExploreLink(datasource: string, expr: string): string | null {
  const [root, setRoot] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const sameOrigin = `${window.location.origin}/grafana`;
      try {
        const r = await listSettings('grafana');
        for (const it of r.items) {
          if (it.key === 'root_url' && (it.value ?? '').trim() !== '') {
            const stored = it.value.replace(/\/+$/, '');
            if (cancelled) return;
            setRoot(isBrowserReachableURL(stored) ? stored : sameOrigin);
            return;
          }
        }
      } catch {
        /* fall through */
      }
      if (!cancelled) setRoot(sameOrigin);
    })();
    return () => {
      cancelled = true;
    };
  }, []);
  if (!root) return null;
  // datasource string is the provisioned uid (ongrid-loki / ongrid-tempo
  // / ongrid-prometheus); derive the engine type from it for the v11
  // panes schema.
  const dsType = datasource.includes('tempo')
    ? 'tempo'
    : datasource.includes('loki')
      ? 'loki'
      : 'prometheus';
  const query =
    dsType === 'tempo' ? { query: expr, queryType: 'traceql' } : { expr };
  return buildExploreUrl({
    base: root,
    dsType,
    dsUid: datasource,
    query,
    fromMs: 'now-1h',
    toMs: 'now',
  });
}

function isBrowserReachableURL(rawUrl: string): boolean {
  try {
    const u = new URL(rawUrl);
    const host = u.hostname;
    if (host === 'localhost' || host === '127.0.0.1' || host === '::1') return false;
    if (!host.includes('.') && !host.includes(':')) return false;
    return true;
  } catch {
    return false;
  }
}

// usePluginCount fetches "已在 N 台 edge 启用 <plugin>" once per mount.
// Returns null while loading, number on success, "err" on failure so the
// card can show inline error text instead of blowing up.
function usePluginCount(name: string): number | null | 'err' {
  const [count, setCount] = useState<number | null | 'err'>(null);
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const r = await getPluginCounts();
        if (cancelled) return;
        setCount(Number(r.counts?.[name] ?? 0));
      } catch {
        if (!cancelled) setCount('err');
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [name]);
  return count;
}

type ElasticsearchLogsForm = {
  name: string;
  writeEndpoints: string;
  queryEndpoint: string;
  dataset: string;
  namespace: string;
  writeCredentialRef: string;
  queryCredentialRef: string;
  caPEM: string;
  kibanaURL: string;
  tlsInsecure: boolean;
};

const emptyElasticsearchLogsForm: ElasticsearchLogsForm = {
  name: 'external-elasticsearch',
  writeEndpoints: '',
  queryEndpoint: '',
  dataset: 'ongrid.system',
  namespace: 'default',
  writeCredentialRef: '',
  queryCredentialRef: '',
  caPEM: '',
  kibanaURL: '',
  tlsInsecure: false,
};

function integrationError(error: unknown): string {
  return error instanceof ApiError ? error.message : (error as Error).message;
}

function backendToForm(backend: LogBackend): ElasticsearchLogsForm {
  return {
    name: backend.name,
    writeEndpoints: backend.write_endpoints.join('\n'),
    queryEndpoint: backend.query_endpoint,
    dataset: backend.dataset,
    namespace: backend.namespace,
    writeCredentialRef: backend.write_credential_ref,
    queryCredentialRef: backend.query_credential_ref,
    caPEM: '',
    kibanaURL: backend.kibana_url ?? '',
    tlsInsecure: backend.tls_insecure,
  };
}

function logBackendStatus(status: LogBackend['status'], tr: (zh: string, en: string) => string): string {
  switch (status) {
    case 'draft': return tr('草稿', 'Draft');
    case 'distributing': return tr('分发中', 'Distributing');
    case 'verifying': return tr('真实写入验证中', 'Write-path verification');
    case 'active': return tr('已激活', 'Active');
    case 'rolling_back': return tr('回滚预热验证中', 'Rollback verification');
    case 'degraded': return tr('已降级', 'Degraded');
    case 'rolled_back': return tr('已回滚', 'Rolled back');
  }
}

function ElasticsearchLogsCard() {
  const { tr } = useI18n();
  const count = usePluginCount('logs');
  const [backend, setBackend] = useState<LogBackend | null>(null);
  const [form, setForm] = useState<ElasticsearchLogsForm>(emptyElasticsearchLogsForm);
  const [secrets, setSecrets] = useState<SecretView[]>([]);
  const [edges, setEdges] = useState<Edge[]>([]);
  const [probeEdgeID, setProbeEdgeID] = useState('');
  const [canary, setCanary] = useState(true);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null);

  const refresh = useCallback(async (showLoading = true) => {
    if (showLoading) setLoading(true);
    try {
      const [credentialResult, edgeResult] = await Promise.all([listSecrets(), listEdges()]);
      setSecrets((credentialResult.items ?? []).filter((secret) => secret.type === 'elasticsearch' && secret.field_keys.includes('api_key')));
      setEdges((edgeResult.items ?? []).filter((edge) => edge.status === 'online' && edge.device_id != null));
      try {
        const value = await getLogBackend();
        setBackend(value);
        setForm(backendToForm(value));
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) {
          setBackend(null);
          setForm(emptyElasticsearchLogsForm);
        } else {
          throw error;
        }
      }
    } catch (error) {
      setMessage({ ok: false, text: error instanceof ApiError ? error.message : (error as Error).message });
    } finally {
      if (showLoading) setLoading(false);
    }
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);

  useEffect(() => {
    if (!backend || !['distributing', 'verifying', 'rolling_back'].includes(backend.status)) return;
    const timer = window.setInterval(() => void refresh(false), 3000);
    return () => window.clearInterval(timer);
  }, [backend, refresh]);

  const update = <K extends keyof ElasticsearchLogsForm>(key: K, value: ElasticsearchLogsForm[K]) => {
    setMessage(null);
    setForm((current) => ({ ...current, [key]: value }));
  };

  const save = async () => {
    const endpoints = form.writeEndpoints.split(/[\n,]+/).map((value) => value.trim()).filter(Boolean);
    if (form.writeCredentialRef === form.queryCredentialRef) {
      setMessage({ ok: false, text: tr('写入 API Key 和只读查询 API Key 必须是两个不同凭证。', 'Write and read-only query API keys must use different credentials.') });
      return;
    }
    const input: SaveLogBackendInput = {
      name: form.name.trim(),
      write_endpoints: endpoints,
      query_endpoint: form.queryEndpoint.trim(),
      dataset: form.dataset.trim(),
      namespace: form.namespace.trim(),
      write_credential_ref: form.writeCredentialRef,
      query_credential_ref: form.queryCredentialRef,
      ca_pem: form.caPEM.trim() || undefined,
      preserve_ca: Boolean(backend?.has_custom_ca && !form.caPEM.trim()),
      kibana_url: form.kibanaURL.trim() || undefined,
      tls_insecure: form.tlsInsecure,
    };
    setBusy('save');
    setMessage(null);
    try {
      const value = await saveLogBackend(input);
      setBackend(value);
      setForm(backendToForm(value));
      setMessage({ ok: true, text: tr('已保存为草稿；日志链路尚未切换。', 'Saved as a draft; log traffic has not switched.') });
    } catch (error) {
      setMessage({ ok: false, text: integrationError(error) });
    } finally {
      setBusy(null);
    }
  };

  const test = async () => {
    if (!backend) return;
    setBusy('test');
    setMessage(null);
    try {
      const value = await testLogBackend(backend.id);
      setBackend(value);
      setMessage({ ok: true, text: tr(`端点、认证和版本探测通过（Elasticsearch ${value.detected_version ?? ''}）；读写权限将在 Edge 真实探针阶段验证。`, `Endpoint, authentication, and version probe passed (Elasticsearch ${value.detected_version ?? ''}); read/write permissions are verified by the real Edge probe.`) });
    } catch (error) {
      setMessage({ ok: false, text: integrationError(error) });
    } finally {
      setBusy(null);
    }
  };

  const activate = async (promote = false) => {
    if (!backend) return;
    const edgeID = Number(probeEdgeID);
    if (!promote && canary && (!Number.isInteger(edgeID) || edgeID <= 0)) {
      setMessage({ ok: false, text: tr('请选择一台在线 Edge 执行真实写入探针。', 'Select an online edge for the real write probe.') });
      return;
    }
    setBusy(promote ? 'promote' : 'activate');
    setMessage(null);
    try {
      const value = await activateLogBackend(backend.id, { edge_ids: promote || !canary ? [] : [edgeID], canary: promote ? false : canary });
      setBackend(value);
      setMessage({ ok: true, text: promote
        ? value.status === 'active'
          ? tr('已全量切换到外部 Elasticsearch。', 'Fleet cutover to external Elasticsearch is active.')
          : tr('已开始全量预热；所有启用 logs 的 Edge 均在线且真实探针成功后将自动切换。', 'Fleet pre-warming started; cutover occurs only after every log-enabled edge is online and its real probe succeeds.')
        : tr('配置已下发；等待 Edge 启动 Collector 并由 Manager 读回唯一探针日志。', 'Configuration distributed; waiting for the edge Collector and Manager read-back probe.') });
    } catch (error) {
      setMessage({ ok: false, text: integrationError(error) });
    } finally {
      setBusy(null);
    }
  };

  const rollback = async () => {
    if (!backend) return;
    const cancelsCandidate = backend.status !== 'active' && !backend.cutover_at;
    setBusy('rollback');
    setMessage(null);
    try {
      const value = await rollbackLogBackend(backend.id);
      setBackend(value);
      setMessage({ ok: true, text: cancelsCandidate
        ? tr('已取消候选后端；Edge 已恢复当前权威后端。', 'Candidate rollout cancelled; edges are back on the current authoritative backend.')
        : value.status === 'rolling_back'
          ? tr('已开始回滚预热；所有启用 logs 的 Edge 均在线且 Loki 实写探针成功后，才会结束 ES 时间线并切换查询。', 'Rollback pre-warming started; the ES timeline closes only after every log-enabled edge is online and has a verified real write in Loki.')
          : tr('已回滚到内置 Loki；Edge 仍使用同一套 OTel Collector 与 checkpoint。', 'Rolled back to built-in Loki with the same OTel Collector and checkpoints.') });
    } catch (error) {
      setMessage({ ok: false, text: integrationError(error) });
    } finally {
      setBusy(null);
    }
  };

  const assignments = backend?.assignments ?? [];
  const canEdit = !backend || ['draft', 'degraded', 'rolled_back'].includes(backend.status);
  const allAssignmentsVerified = assignments.length > 0 && assignments.every((assignment) => assignment.status === 'verified' && assignment.last_write_success_at);
  const canPromoteCanary = backend?.status === 'verifying' && !backend.rollout_auto_activate && allAssignmentsVerified;
  const fleetConverging = backend != null && backend.rollout_auto_activate && (backend.status === 'distributing' || backend.status === 'verifying');
  const rollbackConverging = backend?.status === 'rolling_back';
  const rollbackHasFailures = rollbackConverging && assignments.some((assignment) => assignment.status === 'failed');

  return (
    <Card className="p-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="flex items-center gap-2">
            <Database size={14} className="text-sky-400" />
            <h2 className="text-sm font-medium text-zinc-100">{tr('外部 Elasticsearch 日志后端', 'External Elasticsearch log backend')}</h2>
            {backend && <span className={cn('rounded-full border px-2 py-0.5 text-[10px]', backend.status === 'active' ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300' : backend.status === 'degraded' ? 'border-red-500/40 bg-red-500/10 text-red-300' : 'border-zinc-700 bg-zinc-900 text-zinc-400')}>{logBackendStatus(backend.status, tr)} · gen {backend.generation}</span>}
          </div>
          <p className="mt-2 max-w-4xl text-[11px] leading-5 text-zinc-500">
            {tr('Edge 上的 otelcol-contrib 直接写入这些 endpoint，日志正文不经过 Manager。Manager 只下发非敏感配置、通过专用通道投递写 Key，并使用独立只读 Key 查询与验证。内置 Loki 始终保留为默认和回滚出口。', 'otelcol-contrib on each edge writes directly to these endpoints; log bytes never pass through Manager. Manager distributes non-sensitive configuration, delivers the write key through a dedicated channel, and queries with a separate read-only key. Built-in Loki remains the default and rollback target.')}
          </p>
        </div>
        <PluginCountLine label="logs / otelcol-contrib" count={count} />
      </div>

      {loading ? <div className="flex h-36 items-center justify-center text-sm text-zinc-500"><Loader2 size={14} className="mr-2 animate-spin" />{tr('加载中…', 'Loading…')}</div> : (
        <>
          <fieldset disabled={!canEdit || busy !== null} className="mt-5 grid grid-cols-1 gap-4 disabled:opacity-65 md:grid-cols-2">
            <PromField label={tr('写入 endpoints（每行一个）', 'Write endpoints (one per line)')} hint={tr('仅允许根路径；生产环境要求 HTTPS。', 'Root URLs only; HTTPS is required in production.')} value={form.writeEndpoints} onChange={(value) => update('writeEndpoints', value)} placeholder="https://es-data-1.example.com:9200" />
            <PromField label={tr('Manager 查询 endpoint', 'Manager query endpoint')} hint={tr('必须可从 Manager 网络访问。', 'Must be reachable from the Manager network.')} value={form.queryEndpoint} onChange={(value) => update('queryEndpoint', value)} placeholder="https://es-query.example.com:9200" />
            <PromField label="OTel dataset" value={form.dataset} onChange={(value) => update('dataset', value)} placeholder="ongrid.system" />
            <PromField label="Data stream namespace" value={form.namespace} onChange={(value) => update('namespace', value)} placeholder="prod" />
            <CredentialSelect label={tr('Edge 写入凭证', 'Edge write credential')} value={form.writeCredentialRef} onChange={(value) => update('writeCredentialRef', value)} secrets={secrets} />
            <CredentialSelect label={tr('Manager 只读查询凭证', 'Manager read-only credential')} value={form.queryCredentialRef} onChange={(value) => update('queryCredentialRef', value)} secrets={secrets} />
            <label className="block md:col-span-2"><span className="mb-1 block text-xs text-zinc-400">{tr('自定义 CA PEM（可选）', 'Custom CA PEM (optional)')}</span><textarea value={form.caPEM} onChange={(event) => update('caPEM', event.target.value)} rows={3} placeholder={backend?.has_custom_ca ? tr('已保存自定义 CA；留空会保持不变', 'A custom CA is stored; leave blank to preserve it') : '-----BEGIN CERTIFICATE-----'} className="w-full rounded-lg border border-zinc-800 bg-zinc-950/40 px-3 py-2 font-mono text-xs text-zinc-100 placeholder:text-zinc-600 focus:border-zinc-600 focus:outline-none" /></label>
            <PromField label="Kibana URL" hint={tr('可选；仅用于外部 Discover 跳转。', 'Optional; used only for an external Discover link.')} value={form.kibanaURL} onChange={(value) => update('kibanaURL', value)} placeholder="https://kibana.example.com" />
            <label className="flex items-center gap-2 self-end pb-2 text-xs text-zinc-300"><input type="checkbox" checked={form.tlsInsecure} onChange={(event) => update('tlsInsecure', event.target.checked)} className="accent-amber-500" />{tr('兼容测试：允许 HTTP / 跳过 TLS 校验', 'Compatibility testing: allow HTTP / skip TLS verification')}</label>
          </fieldset>

          <div className="mt-2 text-[11px] text-zinc-500">{tr('凭证值只写并加密保存。请先在', 'Credential values are write-only and encrypted. Create two Elasticsearch credentials in ')} <a href="/settings/secrets" className="text-sky-400 hover:text-sky-300">{tr('设置 → 凭证', 'Settings → Credentials')}</a>{tr('中分别创建最小权限写 Key 和只读 Key。', ' with least-privilege write and read-only keys.')}</div>

          <div className="mt-5 flex flex-wrap items-end gap-3 border-t border-zinc-800/70 pt-4">
            <Button onClick={() => void save()} disabled={!canEdit || busy !== null} variant="subtle">{busy === 'save' ? <Loader2 size={14} className="animate-spin" /> : <Save size={14} />}<span>{tr('保存草稿', 'Save draft')}</span></Button>
            <Button onClick={() => void test()} disabled={!backend || busy !== null || backend.status === 'active' || rollbackConverging} variant="ghost">{busy === 'test' ? <Loader2 size={14} className="animate-spin" /> : <PlugZap size={14} />}<span>{tr('测试端点与认证', 'Test endpoints & auth')}</span></Button>
            {canPromoteCanary ? (
              <Button onClick={() => void activate(true)} disabled={busy !== null} variant="subtle">{busy === 'promote' ? <Loader2 size={14} className="animate-spin" /> : <Check size={14} />}<span>{tr('灰度通过，切换全量', 'Promote canary to fleet')}</span></Button>
            ) : rollbackConverging ? (
              <>
                <span className="inline-flex h-8 items-center gap-2 rounded-md border border-amber-500/20 bg-amber-500/5 px-3 text-xs text-amber-300"><Loader2 size={13} className={rollbackHasFailures ? '' : 'animate-spin'} />{rollbackHasFailures ? tr('部分 Loki 实写探针失败，ES 仍保持权威', 'Some Loki write probes failed; Elasticsearch remains authoritative') : tr('全量 Edge 正在双写 ES 与 Loki，并验证 Loki 实写', 'The fleet is dual-writing ES and Loki while the Loki path is verified')}</span>
                {rollbackHasFailures && <Button onClick={() => void rollback()} disabled={busy !== null} variant="ghost"><RefreshCw size={14} /><span>{tr('重试失败 Edge', 'Retry failed edges')}</span></Button>}
              </>
            ) : fleetConverging ? (
              <span className="inline-flex h-8 items-center gap-2 rounded-md border border-sky-500/20 bg-sky-500/5 px-3 text-xs text-sky-300"><Loader2 size={13} className="animate-spin" />{tr('所有启用 logs 的 Edge 全量预热与验证中', 'Pre-warming and verifying every log-enabled edge')}</span>
            ) : backend?.status !== 'active' && (
              <>
                <label className="block min-w-52"><span className="mb-1 block text-[10px] text-zinc-500">{tr('真实写探针 Edge', 'Real write-probe edge')}</span><select value={probeEdgeID} disabled={!canary} onChange={(event) => setProbeEdgeID(event.target.value)} className="h-8 w-full rounded-md border border-zinc-800 bg-zinc-950 px-2 text-xs text-zinc-200 disabled:opacity-40"><option value="">{canary ? tr('选择 Edge', 'Select edge') : tr('自动检查全部启用 logs 的 Edge', 'All log-enabled edges checked automatically')}</option>{edges.map((edge) => <option key={edge.id} value={String(edge.id)}>{edge.name} (edge #{edge.id}{edge.device_id ? ` / device #${edge.device_id}` : ''})</option>)}</select></label>
                <label className="flex h-8 items-center gap-2 text-xs text-zinc-400"><input type="checkbox" checked={canary} onChange={(event) => setCanary(event.target.checked)} className="accent-sky-500" />{tr('仅灰度，不自动全量', 'Canary only')}</label>
                <Button onClick={() => void activate(false)} disabled={!backend || busy !== null || backend.status === 'distributing'} variant="subtle">{busy === 'activate' ? <Loader2 size={14} className="animate-spin" /> : <GitBranch size={14} />}<span>{canary ? tr('下发灰度并验证', 'Distribute canary') : tr('全量预热并验证', 'Pre-warm fleet')}</span></Button>
              </>
            )}
            {backend && ['active', 'distributing', 'verifying', 'degraded'].includes(backend.status) && <Button onClick={() => void rollback()} disabled={busy !== null} variant="ghost"><RefreshCw size={14} /><span>{backend.status !== 'active' && !backend.cutover_at ? tr('取消候选切换', 'Cancel candidate rollout') : tr('预热并回滚到内置 Loki', 'Pre-warm and roll back to built-in Loki')}</span></Button>}
          </div>

          {message && <p className={cn('mt-3 break-all text-xs', message.ok ? 'text-emerald-400' : 'text-red-400')}>{message.ok ? '✓ ' : '✗ '}{message.text}</p>}
          {backend?.last_error && <p className="mt-2 rounded border border-red-500/20 bg-red-500/5 px-2 py-1 text-[11px] text-red-300">{backend.last_error}</p>}

          {assignments.length > 0 && (
            <div className="mt-4 overflow-hidden rounded-lg border border-zinc-800">
              <div className="grid grid-cols-[100px_110px_110px_1fr] bg-zinc-900/70 px-3 py-2 text-[10px] uppercase tracking-wide text-zinc-500"><span>Edge</span><span>{tr('期望 / 应用', 'Desired / applied')}</span><span>{tr('状态', 'Status')}</span><span>{tr('最后写入验证', 'Last write verification')}</span></div>
              {assignments.map((assignment) => <div key={assignment.id} className="grid grid-cols-[100px_110px_110px_1fr] border-t border-zinc-800 px-3 py-2 text-[11px] text-zinc-400"><span>#{assignment.edge_id}</span><span>{assignment.desired_generation} / {assignment.applied_generation}</span><span className={assignment.status === 'verified' ? 'text-emerald-400' : assignment.status === 'failed' ? 'text-red-400' : 'text-amber-300'}>{assignment.status}</span><span>{assignment.last_write_success_at ? new Date(assignment.last_write_success_at).toLocaleString() : assignment.last_error || '—'}</span></div>)}
            </div>
          )}
        </>
      )}
    </Card>
  );
}

function CredentialSelect({ label, value, onChange, secrets }: { label: string; value: string; onChange: (value: string) => void; secrets: SecretView[] }) {
  const { tr } = useI18n();
  return <label className="block"><span className="mb-1 block text-xs text-zinc-400">{label}</span><select value={value} onChange={(event) => onChange(event.target.value)} className="w-full rounded-lg border border-zinc-800 bg-zinc-950/40 px-3 py-2 text-sm text-zinc-100 focus:border-zinc-600 focus:outline-none"><option value="">{tr('选择 Elasticsearch API Key', 'Select Elasticsearch API key')}</option>{secrets.map((secret) => <option key={secret.id} value={secret.name}>{secret.name}</option>)}</select></label>;
}

// LokiCard mirrors PrometheusCard. Admin fills in URL + optional basic
// auth + TLS-skip; the manager seeds the URL on first boot from the
// ONGRID_LOG_URL env var (default http://loki:3100, the docker-
// internal service). A URL whose hostname has no dot is treated as
// the docker-internal default — edges fall through to manager nginx
// /loki/api/v1/push instead. Override with a customer URL (e.g.
// https://loki.customer.com) and edges push there directly.
type LokiForm = {
  url: string;
  basic_user: string;
  basic_password: string;
  tls_insecure: string; // "true" / "false" / ""
};

const LOKI_KEYS: (keyof LokiForm)[] = ['url', 'basic_user', 'basic_password', 'tls_insecure'];
const LOKI_SENSITIVE: Set<keyof LokiForm> = new Set(['basic_password']);
const emptyLokiForm: LokiForm = { url: '', basic_user: '', basic_password: '', tls_insecure: '' };

function LokiCard() {
  const { tr } = useI18n();
  const count = usePluginCount('logs');
  const exploreUrl = useGrafanaExploreLink(
    'ongrid-loki',
    '{ongrid_source=~"journald:.+|file:.+"}'
  );
  const [server, setServer] = useState<LokiForm>(emptyLokiForm);
  const [draft, setDraft] = useState<LokiForm>(emptyLokiForm);
  const [revealed, setRevealed] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [savedOk, setSavedOk] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [grafanaSyncWarning, setGrafanaSyncWarning] = useState<string | null>(null);
  const [probe, setProbe] = useState<
    | { kind: 'idle' }
    | { kind: 'testing' }
    | { kind: 'ok' }
    | { kind: 'error'; msg: string }
  >({ kind: 'idle' });

  const refresh = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const r = await listSettings('loki');
      const next = { ...emptyLokiForm };
      for (const it of r.items as SystemSetting[]) {
        if (!(LOKI_KEYS as string[]).includes(it.key)) continue;
        if (!LOKI_SENSITIVE.has(it.key as keyof LokiForm)) {
          (next as Record<string, string>)[it.key] = it.value ?? '';
        }
      }
      await Promise.all(
        (r.items as SystemSetting[])
          .filter(
            (it) => LOKI_SENSITIVE.has(it.key as keyof LokiForm) && (it.value ?? '') !== ''
          )
          .map(async (it) => {
            try {
              const real = await revealSetting('loki', it.key);
              (next as Record<string, string>)[it.key] = real.value ?? '';
            } catch {
              /* leave empty so user can paste fresh */
            }
          })
      );
      setServer(next);
      setDraft(next);
      setRevealed({});
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const dirty = LOKI_KEYS.some((k) => draft[k] !== server[k]);
  const update = (k: keyof LokiForm, v: string) => {
    setSavedOk(false);
    setGrafanaSyncWarning(null);
    setDraft((cur) => ({ ...cur, [k]: v }));
  };
  const submit = async () => {
    setSaving(true);
    setErr(null);
    setGrafanaSyncWarning(null);
    try {
      for (const k of LOKI_KEYS) {
        if (draft[k] === server[k]) continue;
        await setSetting('loki', k, draft[k], LOKI_SENSITIVE.has(k));
      }
      await refresh();
      setSavedOk(true);
      try {
        await syncLokiDatasource();
      } catch (e) {
        const msg = e instanceof ApiError ? e.message : (e as Error).message;
        setGrafanaSyncWarning(
          tr(
            `Loki 已保存，但 Grafana 数据源同步失败：${msg}`,
            `Loki was saved, but the Grafana datasource sync failed: ${msg}`
          )
        );
      }
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setSaving(false);
    }
  };
  const probeLoki = async () => {
    setProbe({ kind: 'testing' });
    try {
      await testLokiConnection();
      setProbe({ kind: 'ok' });
    } catch (e) {
      setProbe({ kind: 'error', msg: e instanceof ApiError ? e.message : (e as Error).message });
    }
  };

  return (
    <Card className="p-5">
      <div className="mb-3 flex items-center gap-2">
        <FileText size={14} className="text-zinc-400" />
        <h2 className="text-sm font-medium text-zinc-100">{tr('Loki 集成（日志）', 'Loki integration (logs)')}</h2>
      </div>
      <p className="mb-4 text-[11px] text-zinc-500">
        {tr('填外部 Loki / VictoriaLogs URL 后，边端 ', 'Set an external Loki / VictoriaLogs URL and the edge ')}<code className="font-mono text-zinc-400">logs</code>{tr(' plugin 会直接推到这里，Grafana ', ' plugin pushes there directly; the Grafana ')}<code className="font-mono text-zinc-400">ongrid-loki</code>{tr(' 数据源也走这里。留空 / 留默认 = 走内置 docker-compose 的 ', ' datasource also points here. Empty / default = use the bundled docker-compose ')}<code className="font-mono text-zinc-400">loki</code>{tr(' 容器，边端通过 manager nginx ', ' container; edges write through manager nginx ')}<code className="font-mono text-zinc-400">/loki/api/v1/push</code>{tr(' 反向写入。', ' as a reverse-write path.')}
      </p>

      <div className="mb-4 space-y-2 text-[12px] text-zinc-300">
        <PluginCountLine label="logs plugin" count={count} />
      </div>

      {loading ? (
        <div className="flex h-32 items-center justify-center text-sm text-zinc-500">
          <Loader2 size={14} className="mr-2 animate-spin" /> {tr('加载中…', 'Loading…')}
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
          <PromField
            label="Loki URL"
            hint={tr('例 https://loki.customer.com（外部）；http://loki:3100（内置默认）', 'e.g. https://loki.customer.com (external); http://loki:3100 (built-in default)')}
            value={draft.url}
            onChange={(v) => update('url', v)}
            placeholder="http://loki:3100"
          />
          <PromField
            label="Basic User"
            value={draft.basic_user}
            onChange={(v) => update('basic_user', v)}
            placeholder={tr('（留空 = 不用 Basic）', '(empty = no Basic)')}
          />
          <PromField
            label="Basic Password"
            sensitive
            revealed={!!revealed.basic_password}
            onToggleReveal={() =>
              setRevealed((r) => ({ ...r, basic_password: !r.basic_password }))
            }
            value={draft.basic_password}
            onChange={(v) => update('basic_password', v)}
            placeholder={tr('（留空 = 不用 Basic）', '(empty = no Basic)')}
          />
          <label className="flex items-center gap-2 text-xs text-zinc-300">
            <input
              type="checkbox"
              checked={draft.tls_insecure === 'true'}
              onChange={(e) => update('tls_insecure', e.target.checked ? 'true' : 'false')}
              className="h-3.5 w-3.5 rounded border-zinc-600 bg-zinc-900 accent-emerald-500"
            />
            {tr('跳过 TLS 校验（自签证书时勾选）', 'Skip TLS verification (check this for self-signed certs)')}
          </label>
        </div>
      )}

      <div className="mt-5 flex flex-wrap items-center gap-3">
        <Button onClick={submit} disabled={!dirty || saving} variant="primary">
          {savedOk && !dirty ? <Check size={14} /> : <Save size={14} />}
          <span>{saving ? tr('保存中…', 'Saving…') : savedOk && !dirty ? tr('已保存', 'Saved') : tr('保存', 'Save')}</span>
        </Button>
        <Button
          onClick={probeLoki}
          disabled={probe.kind === 'testing' || dirty}
          variant="ghost"
        >
          {probe.kind === 'testing' ? <Loader2 size={14} className="animate-spin" /> : <PlugZap size={14} />}
          <span>{tr('测试连接', 'Test connection')}</span>
        </Button>
        <button
          type="button"
          disabled={!exploreUrl}
          onClick={() => exploreUrl && void openObservabilityUrl(exploreUrl)}
          className={cn(
            'inline-flex items-center gap-1.5 rounded-lg border px-3 py-1.5 text-sm transition-colors',
            exploreUrl
              ? 'border-zinc-700 text-zinc-200 hover:border-zinc-500 hover:bg-zinc-800'
              : 'cursor-not-allowed border-zinc-800 text-zinc-600'
          )}
        >
          <ExternalLink size={14} />
          <span>{tr('在 Grafana 中查看日志', 'Open logs in Grafana')}</span>
        </button>
        {err && <span className="break-all text-xs text-red-400">{err}</span>}
        {grafanaSyncWarning && <span className="break-all text-xs text-amber-400">{grafanaSyncWarning}</span>}
      </div>
      <ProbeLine probe={probe} okLabel={tr('✓ Loki 可达，/ready 返回成功', '✓ Loki reachable, /ready returned success')} />
    </Card>
  );
}

// TempoCard mirrors LokiCard — same shape, different category. The
// URL points at the OTLP HTTP push endpoint (e.g. /v1/traces). Empty
// / default = internal tempo:3200, edges push to manager OTLP write
// path.
type TempoForm = {
  url: string;
  basic_user: string;
  basic_password: string;
  tls_insecure: string;
};

const TEMPO_KEYS: (keyof TempoForm)[] = ['url', 'basic_user', 'basic_password', 'tls_insecure'];
const TEMPO_SENSITIVE: Set<keyof TempoForm> = new Set(['basic_password']);
const emptyTempoForm: TempoForm = { url: '', basic_user: '', basic_password: '', tls_insecure: '' };

function TempoCard() {
  const { tr } = useI18n();
  const count = usePluginCount('traces');
  const exploreUrl = useGrafanaExploreLink('ongrid-tempo', '{}');
  const [server, setServer] = useState<TempoForm>(emptyTempoForm);
  const [draft, setDraft] = useState<TempoForm>(emptyTempoForm);
  const [revealed, setRevealed] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [savedOk, setSavedOk] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [probe, setProbe] = useState<
    | { kind: 'idle' }
    | { kind: 'testing' }
    | { kind: 'ok' }
    | { kind: 'error'; msg: string }
  >({ kind: 'idle' });

  const refresh = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const r = await listSettings('tempo');
      const next = { ...emptyTempoForm };
      for (const it of r.items as SystemSetting[]) {
        if (!(TEMPO_KEYS as string[]).includes(it.key)) continue;
        if (!TEMPO_SENSITIVE.has(it.key as keyof TempoForm)) {
          (next as Record<string, string>)[it.key] = it.value ?? '';
        }
      }
      await Promise.all(
        (r.items as SystemSetting[])
          .filter(
            (it) => TEMPO_SENSITIVE.has(it.key as keyof TempoForm) && (it.value ?? '') !== ''
          )
          .map(async (it) => {
            try {
              const real = await revealSetting('tempo', it.key);
              (next as Record<string, string>)[it.key] = real.value ?? '';
            } catch {
              /* leave empty */
            }
          })
      );
      setServer(next);
      setDraft(next);
      setRevealed({});
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const dirty = TEMPO_KEYS.some((k) => draft[k] !== server[k]);
  const update = (k: keyof TempoForm, v: string) => {
    setSavedOk(false);
    setDraft((cur) => ({ ...cur, [k]: v }));
  };
  const submit = async () => {
    setSaving(true);
    setErr(null);
    try {
      for (const k of TEMPO_KEYS) {
        if (draft[k] === server[k]) continue;
        await setSetting('tempo', k, draft[k], TEMPO_SENSITIVE.has(k));
      }
      await refresh();
      setSavedOk(true);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setSaving(false);
    }
  };
  const probeTempo = async () => {
    setProbe({ kind: 'testing' });
    try {
      await testTempoConnection();
      setProbe({ kind: 'ok' });
    } catch (e) {
      setProbe({ kind: 'error', msg: e instanceof ApiError ? e.message : (e as Error).message });
    }
  };

  return (
    <Card className="p-5">
      <div className="mb-3 flex items-center gap-2">
        <GitBranch size={14} className="text-zinc-400" />
        <h2 className="text-sm font-medium text-zinc-100">{tr('Tempo 集成（链路）', 'Tempo integration (traces)')}</h2>
      </div>
      <p className="mb-4 text-[11px] text-zinc-500">
        {tr('填外部 Tempo / VictoriaTraces 的 OTLP HTTP 端点（含 ', 'Set an external Tempo / VictoriaTraces OTLP HTTP endpoint (including the ')}<code className="font-mono text-zinc-400">/v1/traces</code>{tr(' 路径）后，边端 ', ' path), and the edge ')}<code className="font-mono text-zinc-400">traces</code>{tr(' plugin 直接推到这里，Grafana 数据源也走这里。留空 / 留默认 = 走内置 docker-compose 的 ', ' plugin pushes there directly; the Grafana datasource also points here. Empty / default = use the bundled docker-compose ')}<code className="font-mono text-zinc-400">tempo</code>{tr(' 容器。', ' container.')}
      </p>

      <div className="mb-4 space-y-2 text-[12px] text-zinc-300">
        <PluginCountLine label="traces plugin" count={count} />
      </div>

      {loading ? (
        <div className="flex h-32 items-center justify-center text-sm text-zinc-500">
          <Loader2 size={14} className="mr-2 animate-spin" /> {tr('加载中…', 'Loading…')}
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
          <PromField
            label="Tempo URL"
            hint={tr('例 https://tempo.customer.com/v1/traces；http://tempo:3200（内置默认）', 'e.g. https://tempo.customer.com/v1/traces; http://tempo:3200 (built-in default)')}
            value={draft.url}
            onChange={(v) => update('url', v)}
            placeholder="http://tempo:3200"
          />
          <PromField
            label="Basic User"
            value={draft.basic_user}
            onChange={(v) => update('basic_user', v)}
            placeholder={tr('（留空 = 不用 Basic）', '(empty = no Basic)')}
          />
          <PromField
            label="Basic Password"
            sensitive
            revealed={!!revealed.basic_password}
            onToggleReveal={() =>
              setRevealed((r) => ({ ...r, basic_password: !r.basic_password }))
            }
            value={draft.basic_password}
            onChange={(v) => update('basic_password', v)}
            placeholder={tr('（留空 = 不用 Basic）', '(empty = no Basic)')}
          />
          <label className="flex items-center gap-2 text-xs text-zinc-300">
            <input
              type="checkbox"
              checked={draft.tls_insecure === 'true'}
              onChange={(e) => update('tls_insecure', e.target.checked ? 'true' : 'false')}
              className="h-3.5 w-3.5 rounded border-zinc-600 bg-zinc-900 accent-emerald-500"
            />
            {tr('跳过 TLS 校验（自签证书时勾选）', 'Skip TLS verification (check this for self-signed certs)')}
          </label>
        </div>
      )}

      <div className="mt-5 flex flex-wrap items-center gap-3">
        <Button onClick={submit} disabled={!dirty || saving} variant="primary">
          {savedOk && !dirty ? <Check size={14} /> : <Save size={14} />}
          <span>{saving ? tr('保存中…', 'Saving…') : savedOk && !dirty ? tr('已保存', 'Saved') : tr('保存', 'Save')}</span>
        </Button>
        <Button
          onClick={probeTempo}
          disabled={probe.kind === 'testing' || dirty}
          variant="ghost"
        >
          {probe.kind === 'testing' ? <Loader2 size={14} className="animate-spin" /> : <PlugZap size={14} />}
          <span>{tr('测试连接', 'Test connection')}</span>
        </Button>
        <button
          type="button"
          disabled={!exploreUrl}
          onClick={() => exploreUrl && void openObservabilityUrl(exploreUrl)}
          className={cn(
            'inline-flex items-center gap-1.5 rounded-lg border px-3 py-1.5 text-sm transition-colors',
            exploreUrl
              ? 'border-zinc-700 text-zinc-200 hover:border-zinc-500 hover:bg-zinc-800'
              : 'cursor-not-allowed border-zinc-800 text-zinc-600'
          )}
        >
          <ExternalLink size={14} />
          <span>{tr('在 Grafana 中查看链路', 'Open traces in Grafana')}</span>
        </button>
        {err && <span className="break-all text-xs text-red-400">{err}</span>}
      </div>
      <ProbeLine probe={probe} okLabel={tr('✓ Tempo 连接测试成功', '✓ Tempo connection test succeeded')} />
    </Card>
  );
}

// ProbeLine renders a generic probe outcome below the action row.
function ProbeLine({
  probe,
  okLabel,
}: {
  probe: { kind: 'idle' } | { kind: 'testing' } | { kind: 'ok' } | { kind: 'error'; msg: string };
  okLabel: string;
}) {
  switch (probe.kind) {
    case 'ok':
      return <p className="mt-3 text-xs text-emerald-400">{okLabel}</p>;
    case 'error':
      return <p className="mt-3 break-all text-xs text-red-400">✗ {probe.msg}</p>;
    default:
      return null;
  }
}

function PluginCountLine({ label, count }: { label: string; count: number | null | 'err' }) {
  const { tr } = useI18n();
  if (count === 'err') {
    return (
      <div className="text-[11px] text-amber-300">
        {tr('无法获取 plugin 启用统计（plugin runtime 后端可能未就绪）', 'Cannot fetch plugin enablement (the plugin runtime backend may not be ready)')}
      </div>
    );
  }
  return (
    <div className="text-zinc-400">
      {tr('已在 ', 'Enabled on ')}<span className="font-mono text-zinc-200">{count ?? '…'}</span>{tr(' 台 edge 启用 ', ' edge(s) — ')}{label}
    </div>
  );
}

// ---------- WebSearch (multi-provider) card ----------------------
//
// Backs the manager-scoped `web_search` skill. The skill dispatches to
// one of three providers; the operator picks via radio:
//
//   - SearXNG (default, self-hosted, zero-key, runs in docker-compose)
//   - Tavily  (commercial, 1k/月 free tier, returns auto-answer too)
//   - Brave Search (commercial, 2k/月 free tier, links only)
//
// Per-provider fields are shown contextually so the form doesn't ask
// the operator for irrelevant credentials. The 测试连接 button issues
// a 1-result probe to whatever's currently selected & saved server-side
// (so save first, then test — same discipline as Loki/Tempo cards).

type WebSearchForm = {
  provider: string; // "searxng" | "tavily" | "brave"
  searxng_url: string;
  tavily_api_key: string;
  brave_api_key: string;
};

const WEBSEARCH_KEYS: (keyof WebSearchForm)[] = [
  'provider',
  'searxng_url',
  'tavily_api_key',
  'brave_api_key',
];
const WEBSEARCH_SENSITIVE: Set<keyof WebSearchForm> = new Set([
  'tavily_api_key',
  'brave_api_key',
]);
const emptyWebSearchForm: WebSearchForm = {
  provider: 'searxng',
  searxng_url: 'http://searxng:8080',
  tavily_api_key: '',
  brave_api_key: '',
};

function WebSearchCard() {
  const { tr } = useI18n();
  const [server, setServer] = useState<WebSearchForm>(emptyWebSearchForm);
  const [draft, setDraft] = useState<WebSearchForm>(emptyWebSearchForm);
  const [revealed, setRevealed] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [savedOk, setSavedOk] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [probe, setProbe] = useState<
    | { kind: 'idle' }
    | { kind: 'testing' }
    | { kind: 'ok'; provider: string; sample: string }
    | { kind: 'error'; msg: string }
  >({ kind: 'idle' });

  const refresh = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const r = await listSettings('websearch');
      const next = { ...emptyWebSearchForm };
      for (const it of r.items as SystemSetting[]) {
        if (!(WEBSEARCH_KEYS as string[]).includes(it.key)) continue;
        if (!WEBSEARCH_SENSITIVE.has(it.key as keyof WebSearchForm)) {
          (next as Record<string, string>)[it.key] = it.value ?? '';
        }
      }
      await Promise.all(
        (r.items as SystemSetting[])
          .filter(
            (it) => WEBSEARCH_SENSITIVE.has(it.key as keyof WebSearchForm) && (it.value ?? '') !== ''
          )
          .map(async (it) => {
            try {
              const real = await revealSetting('websearch', it.key);
              (next as Record<string, string>)[it.key] = real.value ?? '';
            } catch {
              /* leave empty so the user can paste a fresh value */
            }
          })
      );
      // Normalise: a fresh DB without seeds returns provider="" — fall
      // back to the default so the radio renders something selected.
      if (!next.provider) next.provider = 'searxng';
      if (!next.searxng_url) next.searxng_url = 'http://searxng:8080';
      setServer(next);
      setDraft(next);
      setRevealed({});
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const dirty = WEBSEARCH_KEYS.some((k) => draft[k] !== server[k]);
  const update = (k: keyof WebSearchForm, v: string) => {
    setSavedOk(false);
    setDraft((cur) => ({ ...cur, [k]: v }));
  };

  const submit = async () => {
    setSaving(true);
    setErr(null);
    try {
      for (const k of WEBSEARCH_KEYS) {
        if (draft[k] === server[k]) continue;
        await setSetting('websearch', k, draft[k], WEBSEARCH_SENSITIVE.has(k));
      }
      await refresh();
      setSavedOk(true);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const probeWebSearch = async () => {
    setProbe({ kind: 'testing' });
    try {
      const r = await testWebSearchConnection();
      setProbe({ kind: 'ok', provider: r.provider, sample: r.sample });
    } catch (e) {
      setProbe({ kind: 'error', msg: e instanceof ApiError ? e.message : (e as Error).message });
    }
  };

  // Status hint at the bottom of the card. Encodes the provider-specific
  // "ok / missing key" verdict the same way the skill itself decides.
  const statusHint = (() => {
    if (dirty) return tr('有未保存修改', 'Unsaved changes');
    switch (server.provider) {
      case 'searxng':
        return tr(`已选 SearXNG · ${server.searxng_url || 'http://searxng:8080'}`, `Using SearXNG · ${server.searxng_url || 'http://searxng:8080'}`);
      case 'tavily':
        return server.tavily_api_key
          ? tr('已选 Tavily · key 已配置', 'Using Tavily · key configured')
          : tr('已选 Tavily · 缺 key — AI 调用会返回 skipped_reason', 'Using Tavily · key missing — AI calls will return skipped_reason');
      case 'brave':
        return server.brave_api_key
          ? tr('已选 Brave · key 已配置', 'Using Brave · key configured')
          : tr('已选 Brave · 缺 key — AI 调用会返回 skipped_reason', 'Using Brave · key missing — AI calls will return skipped_reason');
      default:
        return '';
    }
  })();

  return (
    <Card className="p-5">
      <div className="mb-3 flex items-center gap-2">
        <Search size={14} className="text-zinc-400" />
        <h2 className="text-sm font-medium text-zinc-100">{tr('联网搜索（web_search skill）', 'Web search (web_search skill)')}</h2>
      </div>
      <p className="mb-4 text-[11px] text-zinc-500">
        {tr('AI agent 的 ', 'The AI agent\'s ')}<code className="font-mono text-zinc-400">web_search</code>
        {tr(
          ' 技能可走三种 provider，默认走 SearXNG（自托管聚合搜索，零 key 零额度）。切换到 Tavily / Brave 需要在对应平台注册 API key。无论选哪个，技能调用方式一致；只有响应里 ',
          ' skill supports three providers; defaults to SearXNG (self-hosted aggregator, no key, no quota). Switching to Tavily / Brave needs an API key from the corresponding platform. The skill call shape is identical — only the response\'s ',
        )}<code className="font-mono text-zinc-400">provider</code>{tr(' 字段不同。', ' field differs.')}
      </p>

      {loading ? (
        <div className="flex h-32 items-center justify-center text-sm text-zinc-500">
          <Loader2 size={14} className="mr-2 animate-spin" /> {tr('加载中…', 'Loading…')}
        </div>
      ) : (
        <div className="space-y-4">
          <ProviderBlock
            id="searxng"
            label="SearXNG"
            badge={tr('自托管，零成本默认', 'Self-hosted, zero-cost default')}
            checked={draft.provider === 'searxng'}
            onSelect={() => update('provider', 'searxng')}
            description={tr(
              'docker-compose 里跟 Loki/Tempo/Prom 一起跑的元搜索聚合器，把查询发到 Bing/DuckDuckGo/Brave/百度/搜狗 等，再合并结果。无 API key、无额度限制。',
              'Meta-search aggregator that runs alongside Loki/Tempo/Prom in docker-compose; fans a query out to Bing/DuckDuckGo/Brave/Baidu/Sogou and merges the results. No API key, no quota.',
            )}
          >
            <PromField
              label="SearXNG URL"
              hint={tr('默认 http://searxng:8080（docker 内网）；外部部署填完整 URL', 'Default http://searxng:8080 (docker internal); use the full URL for external deployments')}
              value={draft.searxng_url}
              onChange={(v) => update('searxng_url', v)}
              placeholder="http://searxng:8080"
            />
          </ProviderBlock>

          <ProviderBlock
            id="tavily"
            label="Tavily"
            badge={tr('需 API key · 1000 次/月免费', 'API key required · 1000 calls/month free')}
            checked={draft.provider === 'tavily'}
            onSelect={() => update('provider', 'tavily')}
            description={
              <>
                {tr('注册 ', 'Register at ')}
                <a
                  href="https://tavily.com"
                  target="_blank"
                  rel="noreferrer"
                  className="text-emerald-400 hover:text-emerald-300"
                >
                  tavily.com
                </a>{' '}
                {tr('拿 key。返回标题 + 链接 + 摘要 + auto-generated answer，质量比 SearXNG 高一档。', 'to get a key. Returns title + link + snippet + auto-generated answer; higher quality than SearXNG.')}
              </>
            }
          >
            <PromField
              label="Tavily API Key"
              hint={tr('tvly-... 形式', 'Format: tvly-...')}
              sensitive
              revealed={!!revealed.tavily_api_key}
              onToggleReveal={() =>
                setRevealed((r) => ({ ...r, tavily_api_key: !r.tavily_api_key }))
              }
              value={draft.tavily_api_key}
              onChange={(v) => update('tavily_api_key', v)}
              placeholder="tvly-..."
            />
          </ProviderBlock>

          <ProviderBlock
            id="brave"
            label="Brave Search"
            badge={tr('需 API key · 2000 次/月免费', 'API key required · 2000 calls/month free')}
            checked={draft.provider === 'brave'}
            onSelect={() => update('provider', 'brave')}
            description={
              <>
                {tr('在 ', 'Apply at ')}
                <a
                  href="https://api.search.brave.com"
                  target="_blank"
                  rel="noreferrer"
                  className="text-emerald-400 hover:text-emerald-300"
                >
                  api.search.brave.com
                </a>{' '}
                {tr('申请 key。隐私更好，但只返回链接 + 描述，没有 answer 字段。', 'for a key. Better privacy; returns links + descriptions only (no answer field).')}
              </>
            }
          >
            <PromField
              label="Brave API Key"
              hint={tr('X-Subscription-Token 形式', 'X-Subscription-Token format')}
              sensitive
              revealed={!!revealed.brave_api_key}
              onToggleReveal={() =>
                setRevealed((r) => ({ ...r, brave_api_key: !r.brave_api_key }))
              }
              value={draft.brave_api_key}
              onChange={(v) => update('brave_api_key', v)}
              placeholder="BSA..."
            />
          </ProviderBlock>
        </div>
      )}

      <div className="mt-5 flex flex-wrap items-center gap-3">
        <Button onClick={submit} disabled={!dirty || saving} variant="primary">
          {savedOk && !dirty ? <Check size={14} /> : <Save size={14} />}
          <span>{saving ? tr('保存中…', 'Saving…') : savedOk && !dirty ? tr('已保存', 'Saved') : tr('保存', 'Save')}</span>
        </Button>
        <Button
          onClick={probeWebSearch}
          disabled={probe.kind === 'testing' || dirty}
          variant="ghost"
        >
          {probe.kind === 'testing' ? <Loader2 size={14} className="animate-spin" /> : <PlugZap size={14} />}
          <span>{tr('测试连接', 'Test connection')}</span>
        </Button>
        <span className="text-xs text-zinc-500">{statusHint}</span>
        {err && <span className="break-all text-xs text-red-400">{err}</span>}
      </div>
      <WebSearchProbeLine probe={probe} />
    </Card>
  );
}

// ProviderBlock is one row in the provider radio. The header (radio +
// label + badge) is always visible; the per-provider input fields are
// only rendered when this block is the active selection — keeps the
// card compact and avoids confusing operators with two grayed-out
// API-key inputs they don't need to fill.
function ProviderBlock({
  id,
  label,
  badge,
  checked,
  onSelect,
  description,
  children,
}: {
  id: string;
  label: string;
  badge: string;
  checked: boolean;
  onSelect: () => void;
  description: ReactNode;
  children: ReactNode;
}) {
  return (
    <div
      className={cn(
        'rounded-lg border p-4 transition-colors',
        checked ? 'border-emerald-700/60 bg-emerald-900/10' : 'border-zinc-800 bg-zinc-950/40'
      )}
    >
      <label className="flex cursor-pointer items-start gap-3">
        <input
          type="radio"
          name="websearch_provider"
          value={id}
          checked={checked}
          onChange={onSelect}
          className="mt-1 h-3.5 w-3.5 accent-emerald-500"
        />
        <div className="flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium text-zinc-100">{label}</span>
            <span className="rounded border border-zinc-700 bg-zinc-900 px-1.5 text-[10px] text-zinc-400">
              {badge}
            </span>
          </div>
          <p className="mt-1 text-[11px] text-zinc-500">{description}</p>
        </div>
      </label>
      {checked && <div className="mt-3 grid grid-cols-1 gap-4 md:grid-cols-2">{children}</div>}
    </div>
  );
}

// WebSearchProbeLine renders the result of the most recent test-connection
// click. Shows the actual upstream-confirmed provider + a sample title
// (when the probe query returned a hit) so the operator sees tangible
// proof the wiring works.
function WebSearchProbeLine({
  probe,
}: {
  probe:
    | { kind: 'idle' }
    | { kind: 'testing' }
    | { kind: 'ok'; provider: string; sample: string }
    | { kind: 'error'; msg: string };
}) {
  const { tr } = useI18n();
  switch (probe.kind) {
    case 'ok':
      return (
        <p className="mt-3 text-xs text-emerald-400">
          ✓ {probe.provider} {tr('可达', 'reachable')}
          {probe.sample ? (
            <>
              {' '}
              {tr('· 示例结果：', '· Sample result: ')}<span className="text-zinc-300">{probe.sample}</span>
            </>
          ) : (
            tr(' · 上游返回 0 结果（key 工作正常，但探针 query 没匹配到）', ' · Upstream returned 0 results (key works, but probe query found no matches)')
          )}
        </p>
      );
    case 'error':
      return <p className="mt-3 break-all text-xs text-red-400">✗ {probe.msg}</p>;
    default:
      return null;
  }
}
