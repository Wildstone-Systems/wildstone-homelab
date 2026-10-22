// Actual embedded Calendar page with deterministic API fixtures.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { chromium } from 'playwright';
const portal = new URL('../../keystone/portal/web/', import.meta.url);
const html = await readFile(new URL('calendar.html', portal), 'utf8');
const css = await readFile(new URL('assets/nocturne.css', portal), 'utf8');
const day = new Date().toISOString().slice(0, 10);
const browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined });
try {
  for (const width of [1280, 390]) {
    const page = await browser.newPage({ viewport: { width, height: 900 } });
    const faults = [], writes = [];
    const calendars = [{ id: 'household', name: 'Household', color: '#3b82f6', default: true, writable: true, visible: true },
      { id: 'school', name: 'School', color: '#f59e0b', writable: true, visible: true },
      { id: 'holidays', name: 'Holidays', color: '#22cc22', writable: false, visible: true }];
    const events = calendars.map(c => ({ calendar: c.id, calendarName: c.name, color: c.color, writable: c.writable,
      calendarEvent: `/GROUPS/${c.id}/fixture.ics`, title: c.name + ' event', day, lastDay: day, allDay: true }));
    page.on('pageerror', err => faults.push(err.message));
    await page.route('http://calendar.test/**', async route => {
      const req = route.request(), url = new URL(req.url());
      if (url.pathname === '/calendar/') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname.endsWith('nocturne.css')) return route.fulfill({ contentType: 'text/css', body: css });
      if (req.method() === 'POST') {
        const body = req.postDataJSON(); writes.push({ path: url.pathname, ...body });
        if (url.pathname.endsWith('/visibility')) calendars.find(c => c.id === body.calendar).visible = body.visible;
        return route.fulfill({ json: {} });
      }
      if (url.pathname === '/api/v1/calendar/events') return route.fulfill({ json: { today: day, timezone: 'UTC', calendars,
        events: events.filter(e => calendars.find(c => c.id === e.calendar).visible) } });
      return route.fulfill({ status: 404, json: {} });
    });
    await page.goto('http://calendar.test/calendar/');
    await page.locator('#day-list').getByRole('button', { name: /School event/ }).waitFor();
    await page.getByLabel('School', { exact: true }).uncheck();
    await page.locator('#day-list').getByRole('button', { name: /School event/ }).waitFor({ state: 'detached' });
    await page.reload();
    await page.getByLabel('Household', { exact: true }).waitFor();
    assert.equal(await page.getByLabel('School', { exact: true }).isChecked(), false);
    await page.locator('#add').click();
    await page.locator('#ev-title').fill('New lesson');
    await page.locator('#ev-calendar').selectOption('school');
    assert.equal(await page.locator('#ev-calendar option[value=holidays]').count(), 0);
    await page.locator('#save').click();
    await page.locator('#editor').waitFor({ state: 'hidden' });
    assert(writes.some(w => w.title === 'New lesson' && w.calendar === 'school'));
    await page.locator('#day-list').getByRole('button', { name: /Household event/ }).click();
    await page.locator('#ev-calendar').selectOption('school');
    await page.locator('#save').click();
    await page.locator('#editor').waitFor({ state: 'hidden' });
    assert(writes.some(w => w.path.endsWith('/update') && w.calendar === 'household' && w.targetCalendar === 'school'));
    await page.locator('#day-list').getByRole('button', { name: /Holidays event/ }).click();
    assert(await page.locator('#save').isDisabled());
    assert(await page.locator('#delete').isHidden());
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
    assert(!overflow, `horizontal overflow at ${width}`);
    assert.deepEqual(faults, []);
    await page.close();
    console.log(`Calendar selector, visibility and read-only controls passed at ${width}px`);
  }
} finally { await browser.close(); }
