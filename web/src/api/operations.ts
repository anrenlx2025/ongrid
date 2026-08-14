import { request } from './client';

export type Operation = {
  id: string;
  kind: string;
  state: string;
  title: string;
  summary?: string;
  detail_url?: string;
  actions_json?: string;
};

export function executeOperationAction(id: string, action: string) {
  return request<Operation>('POST', `/operations/${encodeURIComponent(id)}/actions/${encodeURIComponent(action)}`, {});
}
