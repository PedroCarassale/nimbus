import { useState, useEffect } from 'react';

interface JsonEditorProps {
  value: string;
  onChange: (value: string, isValid: boolean) => void;
  placeholder?: string;
  readOnly?: boolean;
  label?: string;
}

export function JsonEditor({ value, onChange, placeholder, readOnly, label }: JsonEditorProps) {
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    validateJson(value);
  }, []);

  const validateJson = (text: string) => {
    if (!text.trim()) {
      setError(null);
      onChange(text, true);
      return;
    }
    try {
      JSON.parse(text);
      setError(null);
      onChange(text, true);
    } catch (e) {
      setError((e as Error).message);
      onChange(text, false);
    }
  };

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    validateJson(e.target.value);
  };

  const formatJson = () => {
    try {
      const parsed = JSON.parse(value);
      const formatted = JSON.stringify(parsed, null, 2);
      validateJson(formatted);
    } catch {
      // ignore - already invalid
    }
  };

  return (
    <div className="json-editor">
      {label && <label className="json-label">{label}</label>}
      <div className="json-editor-wrapper">
        <textarea
          value={value}
          onChange={handleChange}
          placeholder={placeholder || '{}'}
          readOnly={readOnly}
          spellCheck={false}
          className={error ? 'has-error' : ''}
        />
        {!readOnly && value && !error && (
          <button onClick={formatJson} className="btn-format" title="Formatear JSON">
            { }
          </button>
        )}
      </div>
      {error && <div className="json-error">{error}</div>}
    </div>
  );
}
