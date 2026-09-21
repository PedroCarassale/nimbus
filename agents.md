# Agents — Nimbus Functions

Instrucciones obligatorias para cualquier asistente de IA (Cursor, Copilot, cloud agents, etc.) que trabaje en este repo.

## Proyecto universitario — AI Decision Log

Este es un trabajo universitario. El ingeniero humano es responsable del código en producción; la IA asiste bajo auditoría.

### Archivo obligatorio

Mantener en la **raíz del repo** el archivo:

`AI-DECISIONS.md`

Es el log de auditoría de decisiones técnicas cuando se genera o cambia código/diseño con IA.

### Regla en cada cambio con IA

Antes de cerrar un commit que incluya código o diseño generado/modificado con IA, **agregar una entrada nueva** al final de `AI-DECISIONS.md` con esta estructura exacta:

```markdown
## <título corto de la decisión>

**Problema abordado:** …
**Prompt / Herramienta utilizada:** …
**Código / Arquitectura generada:** …
**Validación y Corrección Humana:** …
```

### Criterios de la sección humana

En **Validación y Corrección Humana** el humano (o el agente en su nombre, dejando claro qué falta revisar) debe señalar, si aplica:

- alucinaciones o supuestos incorrectos
- riesgos de seguridad
- ineficiencias / hotspots
- cambios hechos sobre la propuesta de la IA

Si el humano aún no validó, escribir explícitamente: `PENDIENTE de validación humana por Pedro/Federico` y no inventar una auditoría falsa.

### Qué no hacer

- No commitear solo código de IA sin actualizar `AI-DECISIONS.md`
- No borrar entradas anteriores del log
- No usar el log para chat irrelevante: solo decisiones técnicas de código/arquitectura

### Commits

Mensajes de commit en español o inglés claros. Preferir que el mismo commit (o uno inmediatamente anterior en el PR) incluya el update a `AI-DECISIONS.md`.
