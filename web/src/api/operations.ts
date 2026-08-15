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

export type OperationArtifact = {
  id: string;
  operation_id: string;
  kind: string;
  title: string;
  url: string;
  metadata_json?: string;
  created_at?: string;
};

export type OperationDetail = {
  operation: Operation;
  artifacts?: OperationArtifact[];
};

export function getOperation(id: string) {
  return request<OperationDetail>('GET', `/operations/${encodeURIComponent(id)}`);
}

export function executeOperationAction(id: string, action: string) {
  return request<Operation>('POST', `/operations/${encodeURIComponent(id)}/actions/${encodeURIComponent(action)}`, {});
}
