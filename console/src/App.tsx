import { useState } from 'react';
import { FunctionsList } from './pages/FunctionsList';
import { FunctionDetail } from './pages/FunctionDetail';
import type { FunctionSummary } from './types';
import './App.css';

type View = 'list' | 'detail';

function App() {
  const [view, setView] = useState<View>('list');
  const [selectedFunctionId, setSelectedFunctionId] = useState<string | null>(null);

  const handleSelectFunction = (fn: FunctionSummary) => {
    setSelectedFunctionId(fn.id);
    setView('detail');
  };

  const handleBack = () => {
    setSelectedFunctionId(null);
    setView('list');
  };

  return (
    <div className="app">
      <header className="app-header">
        <h1 onClick={() => handleBack()} style={{ cursor: 'pointer' }}>
          ☁️ Nimbus Functions
        </h1>
        <span className="subtitle">Consola MVP</span>
      </header>
      
      <main className="app-main">
        {view === 'list' && (
          <FunctionsList onSelect={handleSelectFunction} />
        )}
        {view === 'detail' && selectedFunctionId && (
          <FunctionDetail 
            functionId={selectedFunctionId} 
            onBack={handleBack} 
          />
        )}
      </main>

      <footer className="app-footer">
        <small>
          Control Plane: {import.meta.env.VITE_API_URL || 'http://localhost:3000'} (via proxy)
        </small>
      </footer>
    </div>
  );
}

export default App;
