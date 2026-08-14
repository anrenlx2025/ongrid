import { useCallback, useEffect, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { AlertTriangle, ChevronLeft, Sparkles } from 'lucide-react';
import { getPacketCaptureSession, refreshPacketCaptureSession, packetCaptureArtifactID, type PacketCapture, type PacketCaptureSession } from '@/api/packetCaptures';
import { createSession } from '@/api/chat';
import { useI18n } from '@/i18n/locale';
import { Button, EmptyState, PageHeader } from '@/components/ui';

export default function PacketCaptureSessionDetailPage() {
  const { tr } = useI18n(); const navigate = useNavigate(); const { sessionID = '' } = useParams<{sessionID:string}>();
  const [session, setSession] = useState<PacketCaptureSession | null>(null); const [captures, setCaptures] = useState<PacketCapture[]>([]); const [analyzing,setAnalyzing]=useState(false); const [error,setError]=useState('');
  const load=useCallback(async(refresh=false)=>{ try { const result=refresh?await refreshPacketCaptureSession(sessionID):await getPacketCaptureSession(sessionID); setSession(result.session);setCaptures(result.captures);setError(''); } catch(e){setError(e instanceof Error?e.message:String(e));} },[sessionID]);
  useEffect(()=>{void load();},[load]);
  useEffect(()=>{ if(!session || ['ready','failed'].includes(session.state)){ return; } const timer=window.setTimeout(()=>void load(true), 2000); return ()=>window.clearTimeout(timer); },[load,session]);
  const analysis=session?.analysis;
  const analyzeWithAI = async () => {
    if (!session || analyzing) return;
    setAnalyzing(true);
    try {
      const title = tr(`分析抓包会话 ${session.id.slice(-8)}`, `Analyze capture session ${session.id.slice(-8)}`);
      const prompt = tr(
        `请分析抓包会话 session_id=${session.id}。先调用 get_packet_capture_session 获取成员抓包、跨 Edge 流和合并时间线；基于证据说明观察到的通信路径、未在某 Edge 观测到的流，以及下一步验证建议。不要把未观测到直接断言为丢包，也不要将未校时的跨 Edge 时间差解释为网络时延。`,
        `Analyze packet capture session session_id=${session.id}. First call get_packet_capture_session for member captures, cross-edge flows, and the merged timeline. Explain observed paths, flows not observed on an edge, and next verification steps. Do not call absence a packet loss or use uncalibrated cross-edge deltas as network latency.`,
      );
      const chat = await createSession({ title, agent_id: 'default' });
      navigate(`/chat/${chat.id}`, { state: { initialPrompt: prompt } });
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); setAnalyzing(false); }
  };
  const timelineGroups = groupTimeline(analysis?.timeline ?? []);
  const multipleEdges = new Set(captures.map((capture) => capture.edge_id)).size > 1;
  return <main className="anim-fade flex flex-1 flex-col overflow-hidden"><PageHeader title={session?.title || tr('抓包会话','Capture session')} subtitle={session ? `${session.id} · ${session.canonical_filter || tr('全部流量','all traffic')}` : tr('加载会话数据','Loading session')}/>
    <div className="flex flex-1 flex-col gap-4 overflow-y-auto px-6 py-4">
      {error && <div className="rounded-md border border-red-900/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">{error}</div>}
      {session && <><Link to="/pages?tab=packets" className="inline-flex w-fit items-center gap-1 text-xs text-zinc-400 hover:text-zinc-200"><ChevronLeft size={13}/>{tr('返回数据包','Back to packets')}</Link><section className="grid gap-3 border-b border-zinc-800 pb-4 md:grid-cols-4"><Metric label={tr('成员数据包','Member artifacts')} value={`${analysis?.summary.ready_count ?? 0}/${analysis?.summary.capture_count ?? captures.length}`}/><Metric label={tr('关联流','Correlated flows')} value={String(analysis?.summary.flow_count ?? 0)}/><Metric label={tr('已解析事件','Parsed events')} value={String(analysis?.summary.event_count ?? 0)}/><div className="flex items-end"><Button onClick={()=>void analyzeWithAI()} disabled={analyzing} className="h-8"><Sparkles size={13}/>{tr('AI 分析','AI analysis')}</Button></div></section>
      {multipleEdges&&<div className="flex items-start gap-2 border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-100"><AlertTriangle size={14} className="mt-0.5 shrink-0"/>{tr('各 Edge 时间线独立展示：可比较同一采集点内的先后顺序和流是否被观测到，不能将跨 Edge 时间差当作网络时延。','Each edge timeline is shown separately: compare ordering within one capture point and whether a flow was observed, not cross-edge time differences as network latency.')}</div>}
      <section className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900/40"><div className="border-b border-zinc-800 px-4 py-3 text-sm font-medium text-zinc-100">{tr('成员采集','Member captures')}</div><div className="divide-y divide-zinc-800">{captures.map(c=><div key={c.id} className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-3 text-xs"><span className="font-mono text-zinc-400">edge {c.edge_id}</span><span className="text-zinc-300">device {c.device_id} · {c.interface_name}</span><span className="text-zinc-500">{c.state}</span>{c.artifact_id&&<Link className="ml-auto text-indigo-300 hover:text-indigo-200" to={`/artifacts/packets/${encodeURIComponent(packetCaptureArtifactID(c))}`}>{tr('查看单机包','Open packets')}</Link>}</div>)}</div></section>
      <section className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900/40"><div className="border-b border-zinc-800 px-4 py-3 text-sm font-medium text-zinc-100">{tr('关联流','Correlated flows')}</div>{!analysis?.flows?.length?<EmptyState title={tr('等待解析结果','Waiting for parsed captures')} className="py-10"/>:<div className="overflow-auto"><table className="w-full min-w-[620px] text-left text-xs"><thead className="border-b border-zinc-800 text-zinc-500"><tr><th className="px-4 py-2">{tr('端点','Endpoints')}</th><th className="px-4 py-2">{tr('观测位置','Observed at')}</th>{multipleEdges&&<th className="px-4 py-2">{tr('未观测','Not observed')}</th>}<th className="px-4 py-2">{tr('包','Packets')}</th></tr></thead><tbody className="divide-y divide-zinc-800">{analysis.flows.map(flow=><tr key={flow.id}><td className="px-4 py-3 font-mono text-zinc-300">{flow.endpoints.join(' ↔ ')}</td><td className="px-4 py-3 text-zinc-400">{flow.edge_ids.map(id=>`edge ${id}`).join(', ')}</td>{multipleEdges&&<td className="px-4 py-3 text-amber-300">{flow.missing_edge_ids?.map(id=>`edge ${id}`).join(', ') || '—'}</td>}<td className="px-4 py-3 text-zinc-400">{flow.packets}</td></tr>)}</tbody></table></div>}</section>
      <section className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900/40"><div className="border-b border-zinc-800 px-4 py-3 text-sm font-medium text-zinc-100">{tr('采集点时间线','Capture-point timelines')}</div>{timelineGroups.length===0?<EmptyState title={tr('暂无可展示事件','No events to show')} className="py-10"/>:<div className="divide-y divide-zinc-800">{timelineGroups.map(group=><div key={group.captureID}><div className="flex items-center gap-2 bg-zinc-950/40 px-4 py-2 text-xs text-zinc-400"><span className="font-medium text-zinc-200">edge {group.edgeID}</span><span>PCAP {group.captureID}</span><span className="text-zinc-600">{group.events.length} {tr('个事件','events')}</span></div><div className="max-h-[320px] overflow-auto"><table className="w-full min-w-[720px] text-left text-xs"><tbody className="divide-y divide-zinc-800">{group.events.map((event,index)=><tr key={`${event.capture_id}-${index}`}><td className="w-24 px-4 py-2 font-mono text-zinc-500">+{formatElapsed(event.elapsed)}</td><td className="px-4 py-2 font-mono text-zinc-300">{event.source} → {event.destination}</td><td className="px-4 py-2 text-zinc-400">{event.protocol} · {event.length} B</td><td className="px-4 py-2 text-zinc-500">{event.info}</td></tr>)}</tbody></table></div></div>)}</div>}</section></>}
    </div></main>;
}
function Metric({label,value}:{label:string;value:string}) { return <div><div className="text-[11px] text-zinc-500">{label}</div><div className="mt-1 text-lg font-medium text-zinc-100">{value}</div></div>; }

function groupTimeline(events: NonNullable<PacketCaptureSession['analysis']>['timeline']) {
  const groups = new Map<number, typeof events>();
  for (const event of events) groups.set(event.capture_id, [...(groups.get(event.capture_id) ?? []), event]);
  return [...groups.entries()].map(([captureID, group]) => {
    const start = Date.parse(group[0]?.timestamp ?? '');
    return { captureID, edgeID: group[0]?.edge_id ?? 0, events: group.map((event) => ({ ...event, elapsed: Math.max(0, (Date.parse(event.timestamp) - start) / 1000) })) };
  });
}

function formatElapsed(seconds: number) { return seconds < 1 ? `${Math.round(seconds * 1000)} ms` : `${seconds.toFixed(3)} s`; }
