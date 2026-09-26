import type { FunctionSummary, FunctionDetail, InvokeResponse } from './types';

const API_URL = import.meta.env.VITE_API_URL || '/api';

export async function listFunctions(): Promise<FunctionSummary[]> {
  const res = await fetch(`${API_URL}/functions`);
  if (!res.ok) throw new Error(`Error ${res.status}: ${res.statusText}`);
  return res.json();
}

export async function getFunction(id: string): Promise<FunctionDetail> {
  const res = await fetch(`${API_URL}/functions/${id}`);
  if (!res.ok) throw new Error(`Error ${res.status}: ${res.statusText}`);
  return res.json();
}

export async function createFunction(name: string): Promise<{ id: string; name: string; createdAt: string }> {
  const res = await fetch(`${API_URL}/functions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.message || `Error ${res.status}: ${res.statusText}`);
  }
  return res.json();
}

export async function deployFunction(id: string, file: File): Promise<{
  message: string;
  function: { id: string; name: string };
  version: { version: string; artifactKey: string; createdAt: string };
}> {
  const formData = new FormData();
  formData.append('file', file);
  
  const res = await fetch(`${API_URL}/functions/${id}/deploy`, {
    method: 'POST',
    body: formData,
  });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.message || `Error ${res.status}: ${res.statusText}`);
  }
  return res.json();
}

export async function invokeFunction(
  id: string,
  event: unknown,
  version?: string
): Promise<InvokeResponse> {
  const body: { event: unknown; version?: string } = { event };
  if (version) body.version = version;

  const res = await fetch(`${API_URL}/functions/${id}/invoke`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.message || `Error ${res.status}: ${res.statusText}`);
  }
  return res.json();
}

export function getLogsUrl(invocationId: string): string {
  return `${API_URL}/invocations/${invocationId}/logs`;
}
