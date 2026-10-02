// Minimal Chrome DevTools Protocol client, dependency-free.
//
// Usage: node tools/cdp.js "<js expression>"
//   - evaluates the expression in the first page target of a browser started
//     with --remote-debugging-port=<port> (default 9222, override CDP_PORT).
//   - prints the result (strings verbatim, everything else as JSON).
//
// Requires Node 21+ (built-in global WebSocket). See the "Browser testing"
// section of README.md for the full recipe.
const http = require('http');

const PORT = Number(process.env.CDP_PORT || 9222);

function getJSON(path) {
  return new Promise((resolve, reject) => {
    http
      .get({ host: '127.0.0.1', port: PORT, path }, (res) => {
        let d = '';
        res.on('data', (c) => (d += c));
        res.on('end', () => {
          try {
            resolve(JSON.parse(d));
          } catch (e) {
            reject(e);
          }
        });
      })
      .on('error', reject);
  });
}

(async () => {
  const expression = process.argv.slice(2).join(' ') || 'document.title';
  const targets = await getJSON('/json');
  const page = targets.find((t) => t.type === 'page' && t.webSocketDebuggerUrl);
  if (!page) {
    console.error('cdp: no page target found');
    process.exit(2);
  }

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  const pending = new Map();
  let nextId = 0;

  ws.addEventListener('message', (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) {
      pending.get(msg.id)(msg);
      pending.delete(msg.id);
    }
  });

  function send(method, params) {
    return new Promise((resolve) => {
      const id = ++nextId;
      pending.set(id, resolve);
      ws.send(JSON.stringify({ id, method, params: params || {} }));
    });
  }

  await new Promise((resolve, reject) => {
    ws.addEventListener('open', resolve);
    ws.addEventListener('error', reject);
  });

  const res = await send('Runtime.evaluate', {
    expression,
    returnByValue: true,
    awaitPromise: true,
  });

  if (res.result && res.result.exceptionDetails) {
    console.error('cdp: evaluation error:', JSON.stringify(res.result.exceptionDetails));
    process.exit(3);
  }
  const value = res.result && res.result.result ? res.result.result.value : undefined;
  console.log(typeof value === 'string' ? value : JSON.stringify(value, null, 2));

  ws.close();
  process.exit(0);
})();
