import { describe, expect, it } from 'vitest';

import { currentLogBackend, type LogBackend } from './logs';

describe('currentLogBackend', () => {
  it('uses the explicit server-side current backend even when the visible row is a draft', () => {
    expect(currentLogBackend({ current_backend: 'elasticsearch' } as LogBackend)).toBe('elasticsearch');
    expect(currentLogBackend({ current_backend: 'loki', cutover_at: '2026-08-20T00:00:00Z' } as LogBackend)).toBe('loki');
  });

  it('keeps compatibility with servers that only expose cutover timestamps', () => {
    expect(currentLogBackend(null)).toBe('loki');
    expect(currentLogBackend({ cutover_at: '2026-08-20T00:00:00Z' } as LogBackend)).toBe('elasticsearch');
    expect(currentLogBackend({
      cutover_at: '2026-08-20T00:00:00Z',
      ended_at: '2026-08-20T01:00:00Z',
    } as LogBackend)).toBe('loki');
  });
});
