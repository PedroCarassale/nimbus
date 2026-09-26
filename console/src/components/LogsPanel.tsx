import { useEffect, useRef, useState } from 'react';
import type { LogEntry, SSEConnectedEvent } from '../types';
import { getLogsUrl } from '../api';

interface LogsPanelProps {
  invocationId: string | null;
  onClose: () => void;
}

export function LogsPanel({ invocationId, onClose }: LogsPanelProps) {
  const [logs, setLogs] = useState<LogEntry[]>([]);
  const [status, setStatus] = useState<'connecting' | 'connected' | 'ended' | 'error'>('connecting');
  const [error, setError] = useState<string | null>(null);
  const logsEndRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!invocationId) return;

    setLogs([]);
    setStatus('connecting');
    setError(null);

    const eventSource = new EventSource(getLogsUrl(invocationId));

    eventSource.addEventListener('connected', (e) => {
      const data: SSEConnectedEvent = JSON.parse(e.data);
      setStatus('connected');
      console.log('SSE connected:', data);
    });

    eventSource.addEventListener('log', (e) => {
      const entry: LogEntry = JSON.parse(e.data);
      setLogs((prev) => [...prev, entry]);
    });

    eventSource.addEventListener('end', () => {
      setStatus('ended');
      eventSource.close();
    });

    eventSource.addEventListener('error', (e) => {
      if (e instanceof MessageEvent) {
        const data = JSON.parse(e.data);
        setError(data.message || 'Error en el stream de logs');
      }
      setStatus('error');
      eventSource.close();
    });

    eventSource.onerror = () => {
      if (status !== 'ended') {
        setStatus('error');
        setError('Conexión perdida con el servidor');
      }
      eventSource.close();
    };

    return () => {
      eventSource.close();
    };
  }, [invocationId]);

  useEffect(() => {
    logsEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [logs]);

  if (!invocationId) return null;

  return (
    <div className="logs-panel">
      <div className="logs-header">
        <h3>Logs en vivo</h3>
        <div className="logs-status">
          <span className={`status-indicator ${status}`} />
          {status === 'connecting' && 'Conectando...'}
          {status === 'connected' && 'En vivo'}
          {status === 'ended' && 'Finalizado'}
          {status === 'error' && 'Error'}
        </div>
        <button onClick={onClose} className="btn-close" title="Cerrar">×</button>
      </div>
      
      {error && <div className="logs-error">{error}</div>}
      
      <div className="logs-container">
        {logs.length === 0 && status === 'connected' && (
          <div className="logs-empty">Esperando logs...</div>
        )}
        {logs.map((entry, i) => (
          <div key={i} className={`log-entry ${entry.stream}`}>
            <span className="log-time">
              {new Date(entry.timestamp).toLocaleTimeString()}
            </span>
            <span className="log-stream">[{entry.stream}]</span>
            <span className="log-line">{entry.line}</span>
          </div>
        ))}
        <div ref={logsEndRef} />
      </div>
    </div>
  );
}
