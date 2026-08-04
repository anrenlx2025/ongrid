import { render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it } from 'vitest';

import SettingsUpgrade from './Upgrade';
import { server } from '@/test/msw-server';

const universalCommand = [
  'curl -fL -O https://ongrid.cloud/dl/ongrid-v0.11.1-linux.tar.xz || wget https://ongrid.cloud/dl/ongrid-v0.11.1-linux.tar.xz',
  'tar xf ongrid-v0.11.1-linux.tar.xz && cd ongrid-v0.11.1-linux',
  'sudo ./upgrade.sh',
].join('\n');

describe('SettingsUpgrade', () => {
  beforeEach(() => {
    localStorage.setItem('ongrid-locale', 'zh-CN');
    server.use(
      http.post('/api/v1/system/upgrade/check', () =>
        HttpResponse.json({
          current_version: 'v0.11.0',
          latest_version: 'v0.11.1',
          update_available: true,
          comparison_supported: true,
          checked_at: '2026-08-04T08:00:00Z',
          commands: [
            {
              id: 'linux-universal',
              label: 'Universal Linux package',
              arch: 'linux',
              command: universalCommand,
            },
          ],
        }),
      ),
    );
  });

  it('只展示一个可自动识别 AMD64 和 ARM64 的通用安装包', async () => {
    render(<SettingsUpgrade />);

    expect(await screen.findByText('通用 Linux 安装包')).toBeInTheDocument();
    expect(screen.getByText('AMD64 / ARM64 · 安装时自动识别')).toBeInTheDocument();
    expect(screen.getByText((_, element) => (
      element?.tagName === 'CODE' && element.textContent === universalCommand
    ))).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: '复制命令' })).toHaveLength(1);
    expect(screen.queryByText('Linux amd64 服务器')).not.toBeInTheDocument();
    expect(screen.queryByText('Linux arm64 服务器')).not.toBeInTheDocument();
    expect(screen.queryByText('自动识别 Linux 架构')).not.toBeInTheDocument();
  });
});
