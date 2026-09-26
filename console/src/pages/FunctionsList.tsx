import { useState, useEffect } from 'react';
import { listFunctions, createFunction } from '../api';
import type { FunctionSummary } from '../types';

interface FunctionsListProps {
  onSelect: (fn: FunctionSummary) => void;
}

export function FunctionsList({ onSelect }: FunctionsListProps) {
  const [functions, setFunctions] = useState<FunctionSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [newName, setNewName] = useState('');
  const [creating, setCreating] = useState(false);
  const [showCreateForm, setShowCreateForm] = useState(false);

  const loadFunctions = async () => {
    try {
      setLoading(true);
      setError(null);
      const fns = await listFunctions();
      setFunctions(fns);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadFunctions();
  }, []);

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newName.trim()) return;

    try {
      setCreating(true);
      setError(null);
      await createFunction(newName.trim());
      setNewName('');
      setShowCreateForm(false);
      await loadFunctions();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setCreating(false);
    }
  };

  return (
    <div className="functions-list">
      <div className="list-header">
        <h2>Funciones</h2>
        <div className="header-actions">
          <button onClick={loadFunctions} className="btn-secondary" disabled={loading}>
            ↻ Actualizar
          </button>
          <button 
            onClick={() => setShowCreateForm(!showCreateForm)} 
            className="btn-primary"
          >
            + Nueva función
          </button>
        </div>
      </div>

      {showCreateForm && (
        <form onSubmit={handleCreate} className="create-form">
          <input
            type="text"
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            placeholder="Nombre de la función"
            disabled={creating}
            autoFocus
          />
          <button type="submit" className="btn-primary" disabled={creating || !newName.trim()}>
            {creating ? 'Creando...' : 'Crear'}
          </button>
          <button 
            type="button" 
            className="btn-secondary" 
            onClick={() => { setShowCreateForm(false); setNewName(''); }}
          >
            Cancelar
          </button>
        </form>
      )}

      {error && <div className="error-message">{error}</div>}

      {loading ? (
        <div className="loading">Cargando funciones...</div>
      ) : functions.length === 0 ? (
        <div className="empty-state">
          <p>No hay funciones creadas.</p>
          <p>Haz clic en "+ Nueva función" para crear una.</p>
        </div>
      ) : (
        <div className="functions-table">
          <table>
            <thead>
              <tr>
                <th>Nombre</th>
                <th>Versión</th>
                <th>Versiones</th>
                <th>Invocaciones</th>
                <th>Creada</th>
              </tr>
            </thead>
            <tbody>
              {functions.map((fn) => (
                <tr key={fn.id} onClick={() => onSelect(fn)} className="clickable">
                  <td className="fn-name">{fn.name}</td>
                  <td>{fn.latestVersion || <span className="text-muted">Sin deploy</span>}</td>
                  <td>{fn.versionsCount}</td>
                  <td>{fn.invocationsCount}</td>
                  <td>{new Date(fn.createdAt).toLocaleDateString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
