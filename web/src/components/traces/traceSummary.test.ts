import { describe, expect, it } from 'vitest';

import {
  formatTraceSummaryDuration,
  traceSearchQuery,
  traceSummaryDurationMs,
} from './traceSummary';

describe('traceSummaryDurationMs', () => {
  it('uses Tempo durationNanos when a sub-millisecond durationMs is omitted', () => {
    expect(traceSummaryDurationMs({
      traceID: 'sub-ms',
      spanSet: { spans: [{ durationNanos: '245347' }] },
    })).toBeCloseTo(0.245347);
  });

  it('prefers the trace duration and falls back to the longest returned span', () => {
    expect(traceSummaryDurationMs({
      traceID: 'milliseconds',
      durationMs: 12,
      spanSet: { spans: [{ durationNanos: '245347' }] },
    })).toBe(12);
    expect(traceSummaryDurationMs({
      traceID: 'span-sets',
      spanSets: [{ spans: [{ durationNanos: '65337' }, { durationNanos: '376854' }] }],
    })).toBeCloseTo(0.376854);
  });
});

describe('formatTraceSummaryDuration', () => {
  it('keeps sub-millisecond values visible in milliseconds', () => {
    expect(formatTraceSummaryDuration(0.245347)).toBe('0.245ms');
    expect(formatTraceSummaryDuration(0.0004)).toBe('<0.001ms');
    expect(formatTraceSummaryDuration(0)).toBe('-');
  });
});

describe('traceSearchQuery', () => {
  it('requests the latest traces for unfiltered, facet, and explicit searches', () => {
    expect(traceSearchQuery('', '', '', '', 'all')).toBe('{} with (most_recent=true)');
    expect(traceSearchQuery('', 'api"v2', 'GET /orders', 'mysql', 'all')).toBe(
      '{ resource.service.name = "api\\"v2" && span:name = "GET /orders" && span.peer.service = "mysql" } with (most_recent=true)',
    );
    expect(traceSearchQuery('{ duration > 1s }', 'ignored', 'ignored')).toBe(
      '{ duration > 1s } with (most_recent=true)',
    );
    expect(traceSearchQuery('{} with (most_recent=true)', '', '')).toBe('{} with (most_recent=true)');
  });

  it('defaults to business traces and can select internal traffic', () => {
    expect(traceSearchQuery('', '', '')).toContain('trace:rootName !~');
    expect(traceSearchQuery('', '', '', '', 'internal')).toContain('trace:rootName =~');
  });
});
