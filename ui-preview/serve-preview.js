'use strict';

// Local-only static server for dollar-preview.html. It never proxies CPA,
// never reads credentials, and serves only files inside this repository.
const http = require('http');
const fs = require('fs');
const path = require('path');

const root = path.resolve(__dirname, '..');
const port = 4173;
const contentTypes = {
  '.css': 'text/css; charset=utf-8',
  '.html': 'text/html; charset=utf-8',
  '.jpeg': 'image/jpeg',
  '.jpg': 'image/jpeg',
  '.js': 'text/javascript; charset=utf-8',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
};

http.createServer((request, response) => {
  const requestPath = decodeURIComponent((request.url || '/').split('?')[0]);
  const relativePath = requestPath === '/' ? 'ui-preview/dollar-preview.html' : requestPath.replace(/^\/+/, '');
  const filePath = path.resolve(root, relativePath);
  if (!filePath.startsWith(root + path.sep)) {
    response.writeHead(403);
    response.end('Forbidden');
    return;
  }
  fs.readFile(filePath, (error, content) => {
    if (error) {
      response.writeHead(404);
      response.end('Not found');
      return;
    }
    response.writeHead(200, {'Content-Type': contentTypes[path.extname(filePath).toLowerCase()] || 'application/octet-stream'});
    response.end(content);
  });
}).listen(port, '127.0.0.1', () => {
  console.log(`codex-carpool preview: http://127.0.0.1:${port}/ui-preview/dollar-preview.html`);
});
