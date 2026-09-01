import { describe, expect, it } from 'vitest';

import type { TraceGetResponse } from '@/api/traces';
import { buildTraceWaterfallModel } from './TraceWaterfall';

describe('buildTraceWaterfallModel', () => {
  it('builds an ordered span tree and keeps orphan spans visible', () => {
    const trace: TraceGetResponse = {
      resourceSpans: [
        {
          resource: { attributes: [{ key: 'service.name', value: { stringValue: 'api' } }] },
          scopeSpans: [{
            spans: [
              { spanId: 'root', name: 'GET /orders', startTimeUnixNano: '1000000000', endTimeUnixNano: '5000000000' },
              { spanId: 'late', parentSpanId: 'root', name: 'publish', startTimeUnixNano: '3000000000', endTimeUnixNano: '4500000000', status: { code: 2 } },
              {
                spanId: 'early',
                parentSpanId: 'root',
                name: 'DB SELECT',
                startTimeUnixNano: '1200000000',
                endTimeUnixNano: '1800000000',
                attributes: [{ key: 'peer.service', value: { stringValue: 'mysql' } }],
              },
              { spanId: 'orphan', parentSpanId: 'missing', name: 'detached', startTimeUnixNano: '2000000000', endTimeUnixNano: '2100000000' },
            ],
          }],
        },
      ],
    };

    const model = buildTraceWaterfallModel(trace);

    expect(model.durationMs).toBe(4000);
    expect(model.errorCount).toBe(1);
    expect(model.services).toEqual(['api', 'mysql']);
    expect(model.roots.map((span) => span.spanId)).toEqual(['root', 'orphan']);
    expect(model.byKey.get('root')?.children.map((span) => span.spanId)).toEqual(['early', 'late']);
    expect(model.byKey.get('early')?.peerService).toBe('mysql');
  });
});
