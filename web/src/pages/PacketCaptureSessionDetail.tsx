import { useCallback, useEffect, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { AlertTriangle, ChevronLeft, RefreshCw, Sparkles } from 'lucide-react';
import { getPacketCaptureSession, refreshPacketCaptureSession, packetCaptureArtifactID, type PacketCapture, type PacketCaptureSession } from '@/api/packetCaptures';
import { createSession } from '@/api/chat';
import { useI18n } from '@/i18n/locale';
import { Button, EmptyState, PageHeader } from '@/components/ui';
import { relativeTime } from '@/lib/format';

export default function PacketCaptureSessionDetailPage() {
  const { tr } = useI18n(); const navigate = useNavigate(); const { sessionID = '' } = useParams<{sessionID:string}>();
  const [session, setSession] = useState<PacketCaptureSession | null>(null); const [captures, setCaptures] = useState<PacketCapture[]>([]); const [busy,setBusy]=useState(false); const [error,setError]=useState('');
  const load=useCallback(async(refresh=false)=>{ setBusy(refresh); try { const result=refresh?await refreshPacketCaptureSession(sessionID):await getPacketCaptureSession(sessionID); setSession(result.session);setCaptures(result.captures);setError(''); } catch(e){setError(e instanceof Error?e.message:String(e));} finally{setBusy(false);} },[sessionID]);
  useEffect(()=>{void load();},[load]);
  const analysis=session?.analysis;
  const analyzeWithAI = async () => {
    if (!session || busy) return;
    setBusy(true);
    try {
      const title = tr(`分析抓包会话 ${session.id.slice(-8)}`, `Analyze capture session ${session.id.slice(-8)}`);
      const prompt = tr(
        `请分析抓包会话 session_id=${session.id}。先调用 get_packet_capture_session 获取成员抓包、跨 Edge 流和合并时间线；基于证据说明观察到的通信路径、未在某 Edge 观测到的流，以及下一步验证建议。不要把未观测到直接断言为丢包，也不要将未校时的跨 Edge 时间差解释为网络时延。`,
        `Analyze packet capture session session_id=${session.id}. First call get_packet_capture_session for member captures, cross-edge flows, and the merged timeline. Explain observed paths, flows not observed on an edge, and next verification steps. Do not call absence a packet loss or use uncalibrated cross-edge deltas as network latency.`,
      );
      const chat = await createSession({ title, agent_id: 'default' });
      navigate(`/chat/${chat.id}`, { state: { initialPrompt: prompt } });
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); setBusy(false); }
  };
  return <main className="anim-fade flex flex-1 flex-col overflow-hidden"><PageHeader title={session?.title || tr('抓包会话','Capture session')} subtitle={session ? `${session.id} · ${session.canonical_filter || tr('全部流量','all traffic')}` : tr('加载会话数据','Loading session')}/>
    <div className="flex flex-1 flex-col gap-4 overflow-y-auto px-6 py-4">
      {error && <div className="rounded-md border border-red-900/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">{error}</div>}
      {session && <><Link to="/pages?tab=packets" className="inline-flex w-fit items-center gap-1 text-xs text-zinc-400 hover:text-zinc-200"><ChevronLeft size={13}/>{tr('返回数据包','Back to packets')}</Link><section className="grid gap-3 border-b border-zinc-800 pb-4 md:grid-cols-5"><Metric label={tr('成员数据包','Member artifacts')} value={`${analysis?.summary.ready_count ?? 0}/${analysis?.summary.capture_count ?? captures.length}`}/><Metric label={tr('关联流','Correlated flows')} value={String(analysis?.summary.flow_count ?? 0)}/><Metric label={tr('时间线事件','Timeline events')} value={String(analysis?.summary.event_count ?? 0)}/><div className="flex items-end"><Button onClick={()=>void load(true)} disabled={busy} className="h-8"><RefreshCw size={13} className={busy?'animate-spin':''}/>{tr('刷新','Refresh')}</Button></div><div className="flex items-end"><Button onClick={()=>void analyzeWithAI()} disabled={busy} className="h-8"><Sparkles size={13}/>{tr('AI 分析','AI analysis')}</Button></div></section>
      <div className="flex items-start gap-2 border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-100"><AlertTriangle size={14} className="mt-0.5 shrink-0"/>{tr('Edge 时钟未校准：可比较包出现顺序和同一流是否被观测到，不能将跨 edge 时间差直接当作网络时延。','Edge clocks are not calibrated: compare ordering and observation presence; do not treat cross-edge time deltas as network latency.')}</div>
      <section className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900/40"><div className="border-b border-zinc-800 px-4 py-3 text-sm font-medium text-zinc-100">{tr('成员采集','Member captures')}</div><div className="divide-y divide-zinc-800">{captures.map(c=><div key={c.id} className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-3 text-xs"><span className="font-mono text-zinc-400">edge {c.edge_id}</span><span className="text-zinc-300">device {c.device_id} · {c.interface_name}</span><span className="text-zinc-500">{c.state}</span>{c.artifact_id&&<Link className="ml-auto text-indigo-300 hover:text-indigo-200" to={`/artifacts/packets/${encodeURIComponent(packetCaptureArtifactID(c))}`}>{tr('查看单机包','Open packets')}</Link>}</div>)}</div></section>
      <section className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900/40"><div className="border-b border-zinc-800 px-4 py-3 text-sm font-medium text-zinc-100">{tr('跨 Edge 流','Cross-edge flows')}</div>{!analysis?.flows?.length?<EmptyState title={tr('等待解析结果','Waiting for parsed captures')} className="py-10"/>:<div className="overflow-auto"><table className="w-full min-w-[760px] text-left text-xs"><thead className="border-b border-zinc-800 text-zinc-500"><tr><th className="px-4 py-2">{tr('端点','Endpoints')}</th><th className="px-4 py-2">{tr('观测 Edge','Observed edges')}</th><th className="px-4 py-2">{tr('未观测 Edge','Not observed')}</th><th className="px-4 py-2">{tr('包','Packets')}</th><th className="px-4 py-2">{tr('首次出现','First seen')}</th></tr></thead><tbody className="divide-y divide-zinc-800">{analysis.flows.map(flow=><tr key={flow.id}><td className="px-4 py-3 font-mono text-zinc-300">{flow.endpoints.join(' ↔ ')}</td><td className="px-4 py-3 text-zinc-400">{flow.edge_ids.join(', ')}</td><td className="px-4 py-3 text-amber-300">{flow.missing_edge_ids?.join(', ') || '—'}</td><td className="px-4 py-3 text-zinc-400">{flow.packets}</td><td className="px-4 py-3 text-zinc-500">{relativeTime(flow.first_seen_at)}</td></tr>)}</tbody></table></div>}</section>
      <section className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900/40"><div className="border-b border-zinc-800 px-4 py-3 text-sm font-medium text-zinc-100">{tr('合并时间线','Merged timeline')}</div>{!analysis?.timeline?.length?<EmptyState title={tr('暂无可展示事件','No events to show')} className="py-10"/>:<div className="max-h-[400px] overflow-auto"><table className="w-full min-w-[840px] text-left text-xs"><tbody className="divide-y divide-zinc-800">{analysis.timeline.map((event,index)=><tr key={`${event.capture_id}-${index}`}><td className="px-4 py-2 font-mono text-zinc-500">{event.timestamp}</td><td className="px-4 py-2 text-sky-300">edge {event.edge_id}</td><td className="px-4 py-2 font-mono text-zinc-300">{event.source} → {event.destination}</td><td className="px-4 py-2 text-zinc-400">{event.protocol} · {event.length} B</td><td className="px-4 py-2 text-zinc-500">{event.info}</td></tr>)}</tbody></table></div>}</section></>}
    </div></main>;
}
function Metric({label,value}:{label:string;value:string}) { return <div><div className="text-[11px] text-zinc-500">{label}</div><div className="mt-1 text-lg font-medium text-zinc-100">{value}</div></div>; }
