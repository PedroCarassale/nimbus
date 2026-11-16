// Flujo de la consola: crear, subir zip, invocar y ver el panel SSE.
// Usa el Chrome del sistema. No abre un navegador con ventana.
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const puppeteer = require('puppeteer-core');

const base = process.env.CONSOLE_URL || 'http://localhost:8088';
const zipPath = process.argv[2];
const shotDir = process.argv[3] || mkdtempSync(join(tmpdir(), 'nimbus-console-'));
const name = process.argv[4] || `qa-console-${Date.now()}`;
const chrome = process.env.CHROME_PATH || '/usr/local/bin/google-chrome';

if (!zipPath) {
  console.error('uso: qa-console.mjs <zip> <dir-capturas> <nombre>');
  process.exit(2);
}

async function clickText(page, selector, text) {
  const clicked = await page.$$eval(
    selector,
    (els, wanted) => {
      const el = els.find((node) => (node.textContent || '').includes(wanted));
      if (!el) return false;
      el.click();
      return true;
    },
    text,
  );
  if (!clicked) throw new Error(`no encontré ${selector} con texto "${text}"`);
}

const browser = await puppeteer.launch({
  executablePath: chrome,
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
});
const page = await browser.newPage();
page.setDefaultTimeout(30000);
const shots = [];
async function shot(label) {
  const file = join(shotDir, `${label}.png`);
  await page.screenshot({ path: file, fullPage: true });
  shots.push(file);
  console.log('screenshot', file);
}

try {
  await page.goto(base, { waitUntil: 'networkidle0' });
  await page.waitForFunction(() => document.body.innerText.includes('Funciones'));
  await shot('01-lista');

  await clickText(page, 'button', 'Nueva función');
  await page.waitForSelector('input[placeholder="Nombre de la función"]');
  await page.type('input[placeholder="Nombre de la función"]', name);
  await page.click('button[type="submit"]');
  await page.waitForFunction((fn) => document.body.innerText.includes(fn), {}, name);
  await shot('02-creada');

  const opened = await page.$$eval(
    'tr.clickable',
    (rows, fn) => {
      const row = rows.find((node) => (node.textContent || '').includes(fn));
      if (!row) return false;
      row.click();
      return true;
    },
    name,
  );
  if (!opened) throw new Error(`no encontré la fila de ${name}`);
  await page.waitForFunction(() => document.body.innerText.includes('Desplegar'));
  const input = await page.waitForSelector('input[type="file"]');
  await input.uploadFile(zipPath);
  await clickText(page, 'button', 'Desplegar');
  await page.waitForFunction(() => document.body.innerText.includes('Deploy exitoso'), { timeout: 30000 });
  await shot('03-deploy');

  await page.waitForSelector('textarea');
  // El textarea es controlado por React. Tipear encima del JSON lindo deja
  // el texto anterior y el editor marca el JSON como inválido.
  await page.$eval('textarea', (el, text) => {
    const proto = Object.getOwnPropertyDescriptor(
      window.HTMLTextAreaElement.prototype,
      'value',
    );
    proto.set.call(el, text);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  }, '{"name":"Consola"}');
  await clickText(page, 'button', 'Invocar función');
  await page.waitForFunction(() => document.body.innerText.includes('Estado: SUCCESS'), { timeout: 30000 });
  await page.waitForFunction(() => document.body.innerText.includes('¡Hola, Consola!'));
  await page.waitForFunction(() => document.body.innerText.includes('Finalizado'), { timeout: 15000 });
  const logs = await page.$eval('.logs-panel', (el) => el.innerText);
  if (logs.includes('Conexión perdida')) {
    throw new Error(`el SSE se marcó como error:\n${logs}`);
  }
  await shot('04-invoke-logs');
  console.log('CONSOLE_OK', name);
  console.log(shots.join('\n'));
} catch (err) {
  await shot('99-error').catch(() => {});
  console.error('CONSOLE_FAIL', err && err.stack ? err.stack : err);
  process.exitCode = 1;
} finally {
  await browser.close();
}
