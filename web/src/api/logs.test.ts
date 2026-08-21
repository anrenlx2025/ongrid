import { describe, expect, it } from 'vitest';

import { currentLogBackend, type LogBackend } from './logs';

describe('currentLogBackend', () => {
  it('uses the explicit server-side current backend even when the visible row is only saved', () => {
    expect(currentLogBackend({ current_backend: 'elasticsearch' } as LogBackend)).toBe('elasticsearch');
    expect(currentLogBackend({ current_backend: 'loki', cutover_at: '2026-08-20T00:00:00Z' } as LogBackend)).toBe('loki');
  });

  it('uses Loki when there is no configured backend', () => {
    expect(currentLogBackend(null)).toBe('loki');
  });
});
