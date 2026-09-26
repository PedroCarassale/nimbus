export interface FunctionSummary {
  id: string;
  name: string;
  createdAt: string;
  latestVersion: string | null;
  versionsCount: number;
  invocationsCount: number;
}

export interface FunctionVersion {
  version: string;
  artifactKey: string;
  createdAt: string;
}

export interface FunctionDetail {
  id: string;
  name: string;
  createdAt: string;
  updatedAt: string;
  versions: FunctionVersion[];
  invocationsCount: number;
}

export interface InvokeResponse {
  invocationId: string;
  requestId: string;
  executionId: string;
  status: 'SUCCESS' | 'ERROR' | 'TIMEOUT' | 'RUNNING';
  durationMs: number;
  output: unknown;
  error: string | null;
  logsUrl: string;
}

export interface LogEntry {
  timestamp: number;
  stream: 'stdout' | 'stderr';
  line: string;
  sequence: number;
}

export interface SSEConnectedEvent {
  invocationId: string;
  requestId: string;
  executionId: string;
  status: string;
}
