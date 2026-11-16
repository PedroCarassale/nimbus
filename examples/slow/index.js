/**
 * Handler lento para probar la cola.
 * event.sleepMs define cuánto espera (default 2000).
 * startedAt se toma al entrar, antes de dormir: dos invocaciones
 * con el mismo startedAt aproximado corrieron a la vez.
 */
exports.handler = async (event) => {
  const startedAt = new Date().toISOString();
  const sleepMs = Number(event && event.sleepMs) || 2000;
  await new Promise((resolve) => setTimeout(resolve, sleepMs));
  return {
    startedAt,
    finishedAt: new Date().toISOString(),
    sleepMs,
  };
};
