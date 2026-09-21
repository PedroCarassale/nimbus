/**
 * Nimbus Functions — Ejemplo "Hello"
 * 
 * Función simple que responde con un saludo.
 */

exports.handler = async (event, context) => {
  const name = event.name || 'Mundo';
  
  return {
    statusCode: 200,
    body: {
      message: `¡Hola, ${name}!`,
      timestamp: new Date().toISOString(),
      functionName: context.functionName,
    },
  };
};
