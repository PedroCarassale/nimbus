import { useState, useEffect, useRef } from 'react';
import { getFunction, deployFunction, invokeFunction } from '../api';
import type { FunctionDetail as FunctionDetailType, InvokeResponse } from '../types';
import { JsonEditor } from '../components/JsonEditor';
import { LogsPanel } from '../components/LogsPanel';

interface FunctionDetailProps {
  functionId: string;
  onBack: () => void;
}

export function FunctionDetail({ functionId, onBack }: FunctionDetailProps) {
  const [fn, setFn] = useState<FunctionDetailType | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  
  // Deploy state
  const [selectedFile, setSelectedFile] = useState<File | null>(null);
  const [deploying, setDeploying] = useState(false);
  const [deployMessage, setDeployMessage] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  
  // Invoke state
  const [eventJson, setEventJson] = useState('{\n  "nombre": "Nimbus"\n}');
  const [eventValid, setEventValid] = useState(true);
  const [selectedVersion, setSelectedVersion] = useState<string>('');
  const [invoking, setInvoking] = useState(false);
  const [invokeResult, setInvokeResult] = useState<InvokeResponse | null>(null);
  
  // Logs state
  const [showLogs, setShowLogs] = useState(false);
  const [logsInvocationId, setLogsInvocationId] = useState<string | null>(null);

  const loadFunction = async () => {
    try {
      setLoading(true);
      setError(null);
      const data = await getFunction(functionId);
      setFn(data);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadFunction();
  }, [functionId]);

  const handleDeploy = async () => {
    if (!selectedFile) return;

    try {
      setDeploying(true);
      setDeployMessage(null);
      setError(null);
      const result = await deployFunction(functionId, selectedFile);
      setDeployMessage(`Deploy exitoso: v${result.version.version}`);
      setSelectedFile(null);
      if (fileInputRef.current) fileInputRef.current.value = '';
      await loadFunction();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setDeploying(false);
    }
  };

  const handleInvoke = async () => {
    if (!eventValid) return;

    try {
      setInvoking(true);
      setError(null);
      setInvokeResult(null);
      
      let event = {};
      if (eventJson.trim()) {
        event = JSON.parse(eventJson);
      }

      const result = await invokeFunction(
        functionId,
        event,
        selectedVersion || undefined
      );
      
      setInvokeResult(result);
      setLogsInvocationId(result.invocationId);
      setShowLogs(true);
      await loadFunction();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setInvoking(false);
    }
  };

  if (loading) {
    return <div className="loading">Cargando función...</div>;
  }

  if (!fn) {
    return (
      <div className="function-detail">
        <button onClick={onBack} className="btn-back">← Volver</button>
        <div className="error-message">Función no encontrada</div>
      </div>
    );
  }

  return (
    <div className="function-detail">
      <button onClick={onBack} className="btn-back">← Volver a funciones</button>
      
      <div className="fn-header">
        <h2>{fn.name}</h2>
        <span className="fn-id">{fn.id}</span>
      </div>

      {error && <div className="error-message">{error}</div>}

      <div className="detail-sections">
        {/* Deploy Section */}
        <section className="section">
          <h3>Desplegar</h3>
          <p className="section-desc">Sube un archivo .zip con tu código</p>
          
          <div className="deploy-form">
            <input
              ref={fileInputRef}
              type="file"
              accept=".zip,application/zip,application/x-zip-compressed"
              onChange={(e) => setSelectedFile(e.target.files?.[0] || null)}
              disabled={deploying}
            />
            <button 
              onClick={handleDeploy} 
              className="btn-primary"
              disabled={deploying || !selectedFile}
            >
              {deploying ? 'Desplegando...' : 'Desplegar'}
            </button>
          </div>
          
          {deployMessage && <div className="success-message">{deployMessage}</div>}

          {fn.versions.length > 0 && (
            <div className="versions-list">
              <h4>Versiones desplegadas</h4>
              <ul>
                {fn.versions.map((v, i) => (
                  <li key={v.version} className={i === 0 ? 'latest' : ''}>
                    <span className="version-tag">v{v.version}</span>
                    {i === 0 && <span className="badge">Última</span>}
                    <span className="version-date">
                      {new Date(v.createdAt).toLocaleString()}
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </section>

        {/* Invoke Section */}
        <section className="section">
          <h3>Invocar</h3>
          
          {fn.versions.length === 0 ? (
            <p className="text-muted">Despliega código antes de invocar la función.</p>
          ) : (
            <>
              <div className="invoke-options">
                <label>
                  Versión:
                  <select 
                    value={selectedVersion} 
                    onChange={(e) => setSelectedVersion(e.target.value)}
                    disabled={invoking}
                  >
                    <option value="">Última ({fn.versions[0]?.version})</option>
                    {fn.versions.map((v) => (
                      <option key={v.version} value={v.version}>
                        v{v.version}
                      </option>
                    ))}
                  </select>
                </label>
              </div>

              <JsonEditor
                label="Evento (JSON)"
                value={eventJson}
                onChange={(val, valid) => {
                  setEventJson(val);
                  setEventValid(valid);
                }}
                placeholder='{"nombre": "Nimbus"}'
              />

              <button
                onClick={handleInvoke}
                className="btn-primary btn-invoke"
                disabled={invoking || !eventValid}
              >
                {invoking ? 'Invocando...' : '▶ Invocar función'}
              </button>
            </>
          )}
        </section>

        {/* Result Section */}
        {invokeResult && (
          <section className="section result-section">
            <h3>Resultado</h3>
            <div className={`result-status ${invokeResult.status.toLowerCase()}`}>
              Estado: {invokeResult.status}
              {invokeResult.durationMs && ` (${invokeResult.durationMs}ms)`}
            </div>
            
            <div className="result-ids">
              <small>Invocation ID: {invokeResult.invocationId}</small>
              <small>Execution ID: {invokeResult.executionId}</small>
            </div>

            {invokeResult.output !== undefined && invokeResult.output !== null && (
              <div className="result-output">
                <h4>Output</h4>
                <pre>{JSON.stringify(invokeResult.output, null, 2)}</pre>
              </div>
            )}

            {invokeResult.error && (
              <div className="result-error">
                <h4>Error</h4>
                <pre>{invokeResult.error}</pre>
              </div>
            )}

            <button
              onClick={() => {
                setLogsInvocationId(invokeResult.invocationId);
                setShowLogs(true);
              }}
              className="btn-secondary"
            >
              Ver logs
            </button>
          </section>
        )}
      </div>

      {/* Logs Panel */}
      {showLogs && (
        <LogsPanel
          invocationId={logsInvocationId}
          onClose={() => {
            setShowLogs(false);
            setLogsInvocationId(null);
          }}
        />
      )}
    </div>
  );
}
