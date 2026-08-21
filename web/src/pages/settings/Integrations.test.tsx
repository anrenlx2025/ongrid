import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import SettingsIntegrations from './Integrations';
import { applyLogBackend, getLogBackend, rollbackLogBackend, saveLogBackend, type LogBackend } from '@/api/logs';

vi.mock('@/api/settings', () => ({
  listSettings: vi.fn(async () => ({ items: [], total: 0 })),
  setSetting: vi.fn(async () => undefined),
  revealSetting: vi.fn(async () => ({ value: '' })),
  testGrafanaConnection: vi.fn(async () => ({})),
  syncGrafana: vi.fn(async () => ({})),
  testPromConnection: vi.fn(async () => ({})),
  testLokiConnection: vi.fn(async () => ({})),
  testTempoConnection: vi.fn(async () => ({})),
  testWebSearchConnection: vi.fn(async () => ({})),
}));

vi.mock('@/api/logs', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/logs')>();
  return {
    ...actual,
    applyLogBackend: vi.fn(async () => ({})),
    getLogBackend: vi.fn(),
    rollbackLogBackend: vi.fn(),
    saveLogBackend: vi.fn(async () => ({})),
  };
});

vi.mock('@/api/edges', () => ({
  listEdges: vi.fn(async () => ({ items: [], total: 0 })),
}));

vi.mock('@/api/integrations', () => ({
  getPluginCounts: vi.fn(async () => ({ counts: {} })),
}));

function backend(status: LogBackend['status'], lastError = '', currentBackend: LogBackend['current_backend'] = 'elasticsearch'): LogBackend {
  return {
    id: 7,
    name: 'external-elasticsearch',
    type: 'elasticsearch',
    status,
    generation: 1,
    current_backend: currentBackend,
    current_backend_id: 7,
    write_endpoints: ['https://es.example.com:9200'],
    query_endpoint: 'https://es.example.com:9200',
    dataset: 'ongrid.system',
    namespace: 'prod',
    index_pattern: 'logs-ongrid.*.otel-prod',
    write_credential_ref: 'write-key',
    query_credential_ref: 'query-key',
    has_custom_ca: false,
    tls_insecure: false,
    last_error: lastError,
    assignments: status === 'rolling_back' ? [{ id: 1, backend_id: 7, edge_id: 64, desired_generation: 1, applied_generation: 1, status: 'failed', last_error: lastError }] : [],
    created_at: '2026-08-20T00:00:00Z',
    updated_at: '2026-08-20T00:00:00Z',
  };
}

describe('SettingsIntegrations log backend presentation', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.setItem('ongrid-locale', 'zh-CN');
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: vi.fn(),
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('keeps a failed Loki switch in the Loki panel and collapses Elasticsearch routing fields', async () => {
    let currentBackend = backend('active');
    const rollbackFailure = 'built-in Loki write probe was not found';
    vi.mocked(getLogBackend).mockImplementation(async () => currentBackend);
    vi.mocked(rollbackLogBackend).mockImplementation(async () => {
      currentBackend = backend('rolling_back', rollbackFailure);
      return currentBackend;
    });

    render(
      <MemoryRouter initialEntries={['/settings/integrations?focus=logs']}>
        <SettingsIntegrations />
      </MemoryRouter>,
    );

    const user = userEvent.setup();
    const lokiTab = await screen.findByRole('tab', { name: /Loki/ });
    await act(async () => user.click(lokiTab));
    await act(async () => user.click(screen.getByRole('button', { name: '验证并设为当前后端' })));

    expect(await screen.findByText('Loki 实写验证失败，Elasticsearch 仍是当前后端。请重试切换验证。')).toBeVisible();
    expect(screen.getByText(rollbackFailure)).toBeVisible();
    expect(screen.getByRole('tab', { name: /Loki/ })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('button', { name: '重试 Loki 切换验证' })).toBeEnabled();

    await act(async () => user.click(screen.getByRole('tab', { name: /Elasticsearch/ })));
    await screen.findByRole('heading', { name: 'Elasticsearch 配置' });
    await waitFor(() => expect(screen.queryByText(rollbackFailure)).not.toBeInTheDocument());

    const advancedSummary = screen.getByText('高级配置 · Elasticsearch Data Stream');
    const advanced = advancedSummary.closest('details');
    expect(advanced).not.toBeNull();
    expect(advanced).not.toHaveAttribute('open');

    fireEvent.click(advancedSummary);
    expect(advanced).toHaveAttribute('open');
    expect(screen.getByRole('textbox', { name: /日志数据集/ })).toHaveValue('ongrid.system');
    expect(screen.getByRole('textbox', { name: /环境标识/ })).toHaveValue('prod');
  });

  it('requires saving a rolled-back Elasticsearch configuration before applying it again', async () => {
    const rolledBack = backend('rolled_back', '', 'loki');
    const saved = { ...backend('saved', '', 'loki'), id: 8, generation: 2 };
    const applying = { ...saved, status: 'distributing' as const };
    vi.mocked(getLogBackend).mockResolvedValue(rolledBack);
    vi.mocked(saveLogBackend).mockResolvedValue(saved);
    vi.mocked(applyLogBackend).mockResolvedValue(applying);

    render(
      <MemoryRouter initialEntries={['/settings/integrations?focus=logs']}>
        <SettingsIntegrations />
      </MemoryRouter>,
    );

    const user = userEvent.setup();
    await act(async () => user.click(await screen.findByRole('tab', { name: /Elasticsearch/ })));
    await screen.findByRole('heading', { name: 'Elasticsearch 配置' });
    const elasticsearch = screen.getByRole('region', { name: 'Elasticsearch 日志后端配置' });
    expect(screen.queryByRole('combobox', { name: '真实写探针 Edge' })).not.toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: '仅灰度，不自动全量' })).not.toBeInTheDocument();
    expect(within(elasticsearch).getByRole('button', { name: '应用' })).toBeDisabled();
    await act(async () => user.click(within(elasticsearch).getByRole('button', { name: '保存' })));

    await waitFor(() => expect(saveLogBackend).toHaveBeenCalledOnce());
    expect(saveLogBackend).toHaveBeenCalledWith(expect.objectContaining({
      write_endpoints: ['https://es.example.com:9200'],
      query_endpoint: 'https://es.example.com:9200',
      dataset: 'ongrid.system',
      namespace: 'prod',
      write_credential_ref: 'write-key',
      query_credential_ref: 'query-key',
      tls_insecure: false,
    }));
    await act(async () => user.click(within(elasticsearch).getByRole('button', { name: '应用' })));
    await waitFor(() => expect(applyLogBackend).toHaveBeenCalledWith(8));
    expect(applyLogBackend).not.toHaveBeenCalledWith(7);
  });

  it('replaces the switching message after polling observes Elasticsearch is active', async () => {
    const pollCallbacks: Array<() => void> = [];
    vi.spyOn(window, 'setInterval').mockImplementation(((handler: TimerHandler) => {
      if (typeof handler === 'function') pollCallbacks.push(() => handler());
      return pollCallbacks.length;
    }) as typeof window.setInterval);
    const rolledBack = backend('rolled_back', '', 'loki');
    const saved = { ...backend('saved', '', 'loki'), id: 8, generation: 2 };
    const applying = { ...saved, status: 'distributing' as const };
    const active = {
      ...saved,
      status: 'active' as const,
      current_backend: 'elasticsearch' as const,
      current_backend_id: 8,
      cutover_at: '2026-08-20T01:00:00Z',
    };
    let currentBackend = rolledBack;
    vi.mocked(getLogBackend).mockImplementation(async () => currentBackend);
    vi.mocked(saveLogBackend).mockResolvedValue(saved);
    vi.mocked(applyLogBackend).mockImplementation(async () => {
      currentBackend = active;
      return applying;
    });

    render(
      <MemoryRouter initialEntries={['/settings/integrations?focus=logs']}>
        <SettingsIntegrations />
      </MemoryRouter>,
    );

    const user = userEvent.setup();
    await act(async () => user.click(await screen.findByRole('tab', { name: /Elasticsearch/ })));
    const elasticsearch = await screen.findByRole('region', { name: 'Elasticsearch 日志后端配置' });
    await act(async () => user.click(within(elasticsearch).getByRole('button', { name: '保存' })));
    await act(async () => user.click(within(elasticsearch).getByRole('button', { name: '应用' })));
    expect(screen.getByText(/正在验证 Elasticsearch 和全部日志采集 Edge/)).toBeVisible();
    await waitFor(() => expect(pollCallbacks.length).toBeGreaterThan(0));

    await act(async () => {
      pollCallbacks.forEach((poll) => poll());
      await Promise.resolve();
    });

    await waitFor(() => expect(screen.getByRole('tabpanel')).toHaveTextContent('已成功切换到 Elasticsearch；Edge 写入与日志中心查询已同步生效。'));
    expect(screen.queryByText(/正在验证 Elasticsearch 和全部日志采集 Edge/)).not.toBeInTheDocument();
  });

  it('exposes only save and apply for Elasticsearch configuration changes', async () => {
    vi.mocked(getLogBackend).mockResolvedValue(backend('saved', '', 'loki'));

    render(
      <MemoryRouter initialEntries={['/settings/integrations?focus=logs']}>
        <SettingsIntegrations />
      </MemoryRouter>,
    );

    const user = userEvent.setup();
    await act(async () => user.click(await screen.findByRole('tab', { name: /Elasticsearch/ })));
    const elasticsearch = await screen.findByRole('region', { name: 'Elasticsearch 日志后端配置' });
    expect(within(elasticsearch).getByRole('button', { name: '保存' })).toBeInTheDocument();
    expect(within(elasticsearch).getByRole('button', { name: '应用' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '测试端点与认证' })).not.toBeInTheDocument();
    expect(screen.queryByText(/草稿/)).not.toBeInTheDocument();
  });
});
