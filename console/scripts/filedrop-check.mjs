// Browser state/request/mobile checks. Go tests separately exercise real storage
// and portal auth. Run: npx playwright install chromium; node scripts/filedrop-check.mjs
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { chromium } from 'playwright';
const portal = new URL('../../keystone/portal/web/', import.meta.url);
const html = await readFile(new URL('files.html', portal), 'utf8');
const css = await readFile(new URL('assets/nocturne.css', portal), 'utf8');
const fonts = new Map(await Promise.all(['inter-latin.woff2', 'phosphor.woff2', 'phosphor-fill.woff2'].map(async name => [name, await readFile(new URL('assets/' + name, portal))])));
const browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined });
try {
  for (const width of [1280, 390]) {
   for (const namespace of ["shared", "me"]) {
    const page = await browser.newPage({ viewport: { width, height: 780 } });
    const faults = [], writes = [];
    page.on('pageerror', e => faults.push(e.message));
    const entries = new Map([['folder', 'directory'], ['folder/photo.txt', 'file'], ['archive', 'directory'],
      ['__proto__', 'file'], ['<img src=x onerror=alert(1)>.txt', 'file']]);
    await page.route('http://files.test/**', async route => {
      const req = route.request(), url = new URL(req.url());
      if (url.pathname === '/filedrop/') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname.endsWith('nocturne.css')) return route.fulfill({ contentType: 'text/css', body: css });
      const font = fonts.get(url.pathname.split('/').pop());
      if (font) return route.fulfill({ contentType: 'font/woff2', body: font });
      if (!url.pathname.startsWith('/api/v1/filedrop/')) return route.fulfill({ status: 404, body: '' });
      const op = url.pathname.split('/').pop();
      const reply = (body, status = 200) => route.fulfill({ status, json: body });
      if (op === 'capabilities') return reply({ member: 'Sam', ready: true, maxUploadBytes: 1048576,
        namespaces: [{ key: 'shared', title: 'Shared', note: 'Everyone with access can add, rename, move and delete shared content.' },
          { key: 'photos', title: 'Photos' }, { key: 'me', title: 'Mine' }] });
      if (op === 'list') {
        const dir = url.searchParams.get('path') || '', prefix = dir ? dir + '/' : '';
        return reply({ items: [...entries].filter(([name]) => name.startsWith(prefix) && !name.slice(prefix.length).includes('/'))
          .map(([name, type]) => ({ name: name.slice(prefix.length), path: name, type, size: 5, modTime: new Date().toISOString() })) });
      }
      if (op === 'file') return route.fulfill({ contentType: 'application/octet-stream', body: 'hello' });
      const body = op === 'upload' ? Object.fromEntries(url.searchParams) : req.postDataJSON();
      writes.push({ op, ...body });
      if (op === 'upload') {
        const name = body.path ? body.path + '/' + body.name : body.name;
        if (entries.has(name)) return reply({ error: 'That name already exists.' }, 409);
        entries.set(name, 'file'); return reply({ name: body.name }, 201);
      }
      if (op === 'mkdir') { entries.set(body.path, 'directory'); return reply({}, 201); }
      if (op === 'delete') {
        if ([...entries.keys()].some(key => key.startsWith(body.path + '/'))) return reply({ error: 'The folder is not empty.' }, 409);
        entries.delete(body.path); return reply({});
      }
      if (op === 'rename' || op === 'move') {
        const prefix = body.path.includes('/') ? body.path.slice(0, body.path.lastIndexOf('/') + 1) : '';
        const to = op === 'rename' ? prefix + body.name : body.to;
        if (entries.has(to)) return reply({ error: 'That name already exists.' }, 409);
        const kind = entries.get(body.path); entries.delete(body.path); entries.set(to, kind); return reply({ name: body.name });
      }
      return reply({}, 404);
    });
    const link = name => page.getByRole('link', { name, exact: true });
    const select = name => page.getByRole('checkbox', { name: 'Select ' + name, exact: true }).check();
    await page.goto(`http://files.test/filedrop/?namespace=${namespace}&path=folder`);
    await link('photo.txt').waitFor();
    assert.equal(await page.locator('#breadcrumbs [aria-current=page]').textContent(), 'folder');
    await page.reload(); await link('photo.txt').waitFor();
    await page.getByRole('button', { name: 'New folder', exact: true }).click();
    await page.locator('#folder-value').fill('nested'); await page.locator('#folder-yes').click();
    await link('nested').waitFor();
    assert(writes.some(w => w.op === 'mkdir' && w.path === 'folder/nested'));
    await link('nested').click(); await page.waitForURL('**path=folder%2Fnested');
    await page.locator('#file-input').setInputFiles({ name: 'upload.txt', mimeType: 'text/plain', buffer: Buffer.from('hello') });
    await link('upload.txt').waitFor();
    assert(writes.some(w => w.op === 'upload' && w.path === 'folder/nested'));
    await page.goBack(); await link('photo.txt').waitFor();
    await page.goForward(); await link('upload.txt').waitFor();
    await page.locator('#drop').evaluate(node => {
      const dt = new DataTransfer(); dt.items.add(new File(['dropped'], 'drop.txt', { type: 'text/plain' }));
      node.dispatchEvent(new DragEvent('drop', { bubbles: true, dataTransfer: dt }));
    });
    await link('drop.txt').waitFor();
    assert(writes.some(w => w.op === 'upload' && w.name === 'drop.txt' && w.path === 'folder/nested'));
    await select('upload.txt'); await page.locator('#sel-rename').click();
    await page.locator('#newname').fill('renamed.txt'); await page.locator('#rename-yes').click();
    await link('renamed.txt').waitFor();
    assert(writes.some(w => w.op === 'rename' && w.path === 'folder/nested/upload.txt' && w.name === 'renamed.txt'));
    await select('renamed.txt'); await page.locator('#sel-move').click();
    await page.locator('#folder-value').fill('archive'); await page.locator('#folder-yes').click();
    await link('renamed.txt').waitFor({ state: 'detached' });
    assert(writes.some(w => w.op === 'move' && w.to === 'archive/renamed.txt'));
    await page.locator('#breadcrumbs').getByRole('link', { name: namespace === 'me' ? 'Mine' : 'Shared', exact: true }).click();
    await link('__proto__').waitFor();
    assert.equal(await page.locator('#listing img').count(), 0, 'filename rendered as HTML');
    await select('__proto__'); assert.equal(await page.locator('#selcount').textContent(), '1 selected');
    await page.locator('#sel-clear').click(); await select('folder');
    assert(await page.locator('#sel-download').isDisabled(), 'directory offered as file download');
    await page.locator('#sel-delete').click(); await page.locator('#confirm-yes').click();
    await page.getByText('folder: The folder is not empty.', { exact: true }).waitFor();
    const widths = await page.evaluate(() => ({ body: document.documentElement.scrollWidth, viewport: innerWidth }));
    assert(widths.body <= widths.viewport, `horizontal overflow at ${width}: ${JSON.stringify(widths)}`);
    if (process.env.FILEDROP_SCREENSHOT_DIR) await page.screenshot({ path: `${process.env.FILEDROP_SCREENSHOT_DIR}/files-${namespace}-${width}.png`, fullPage: true });
    await page.getByRole('tab', { name: 'Photos', exact: true }).click();
    assert(await page.locator('#new-folder').isHidden()); assert(await page.locator('#sel-move').isHidden());
    assert(writes.every(w => w.namespace === namespace), "operation crossed namespaces");
    assert.deepEqual(faults, []);
    await page.close(); console.log(`Files ${namespace} browser checks passed at ${width}px`);
  }
}
} finally { await browser.close(); }
