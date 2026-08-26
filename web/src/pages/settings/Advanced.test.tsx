import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import BuiltInStorageAdvanced from './Advanced';
import { applyObservabilityLimits, listSettings } from '@/api/settings';

vi.mock('@/api/settings', () => ({
  listSettings: vi.fn(),
  applyObservabilityLimits: vi.fn(async () => ({ status: 'applied' })),
}));

describe('BuiltInStorageAdvanced', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.setItem('ongrid-locale', 'zh-CN');
    vi.mocked(listSettings).mockResolvedValue({
      items: [
        { category: 'observability', key: 'prometheus_retention_time', value: '168h', sensitive: false, updated_at: '' },
        { category: 'observability', key: 'prometheus_retention_size', value: '2GB', sensitive: false, updated_at: '' },
      ],
      total: 2,
    });
  });

  it('keeps Prometheus limits collapsed, scoped to built-in, and saves its fields only', async () => {
    render(<BuiltInStorageAdvanced service="prometheus" />);
    expect(screen.queryByLabelText('Prometheus 保留天数')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '高级配置：内置存储限制' }));
    const retention = await screen.findByLabelText('Prometheus 保留天数');
    expect(retention).toHaveValue(7);
    expect(screen.getByText('仅对 Ongrid 内置 Prometheus 生效。')).toBeVisible();

    fireEvent.change(retention, { target: { value: '5' } });
    fireEvent.click(screen.getByRole('button', { name: '保存并应用' }));

    await waitFor(() => expect(applyObservabilityLimits).toHaveBeenCalledTimes(1));
    expect(applyObservabilityLimits).toHaveBeenCalledWith('prometheus', {
      prometheus_retention_time: '120h',
      prometheus_retention_size: '2GB',
    });
    expect(screen.getByText('已保存并应用到内置 Prometheus。')).toBeVisible();
  });
});
