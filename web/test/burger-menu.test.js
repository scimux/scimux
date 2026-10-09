import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { installHost, jsonResponse, routeBody } from './s2-helpers.js';
import { createApp, mountHarnessRows } from '../js/app.js';
import { openAttachmentSettings } from '../js/harness.js';

// Structural parser only: interaction tests below execute the shipped functions.
function markup() {
  const root = { tag: 'root', attrs: {}, children: [] }, stack = [root], ids = new Map();
  const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8').replace(/<!--[\s\S]*?-->/g, '');
  for (const m of html.matchAll(/<\/?([\w-]+)\b([^>]*)>|([^<]+)/g)) {
    if (m[3]) { stack.at(-1).text = (stack.at(-1).text || '') + m[3]; continue; }
    if (m[0].startsWith('</')) { stack.pop(); continue; }
    const attrs = Object.fromEntries([...m[2].matchAll(/([\w-]+)(?:="([^"]*)"|='([^']*)')?/g)].map(a => [a[1], a[2] ?? a[3] ?? '']));
    const el = { tag: m[1], attrs, children: [], parent: stack.at(-1) };
    el.parent.children.push(el);
    if (attrs.id) { assert.ok(!ids.has(attrs.id), `duplicate ID ${attrs.id}`); ids.set(attrs.id, el); }
    if (!['meta', 'link', 'input', 'br', 'img', 'hr'].includes(el.tag)) stack.push(el);
  }
  return { ids, root };
}
const owner = el => { while (el && el.parent?.attrs.id !== 'menu') el = el.parent; return el?.attrs.id; };
const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
const flush = async () => { for (let i = 0; i < 40; i++) await Promise.resolve(); await new Promise(r => setImmediate(r)); };

test('four native menu sections have exact order, defaults, unique IDs and content ownership', () => {
  const { ids } = markup(), menu = ids.get('menu');
  const sections = menu.children.filter(e => e.tag === 'details');
  assert.deepEqual(sections.map(e => e.children[0].text.trim()), ['ABOUT', 'REMOTE', 'HARNESSES', 'PREFERENCES']);
  assert.deepEqual(sections.map(e => 'open' in e.attrs), [true, false, false, false]);
  assert.ok('hidden' in ids.get('m_remote').attrs);
  assert.ok(sections.every(e => !('name' in e.attrs)), 'sections must expand independently');
  for (const id of ['m_version', 'm_check', 'm_result', 'm_reltext', 'm_relurl', 'm_apply', 'm_notes', 'm_affil', 'm_privacy', 'm_ai_engineering', 'm_lictext']) assert.equal(owner(ids.get(id)), 'm_about', id);
  for (const id of ['m_devices', 'm_pair', 'm_pair_note', 'm_unlink', 'm_unlink_note', 'm_homescreen']) assert.equal(owner(ids.get(id)), 'm_remote', id);
  for (const id of ['m_harnesses', 'm_vibe_inspect_row', 'm_vibe_inspect', 'm_hnote', 'm_hcheck']) assert.equal(owner(ids.get(id)), 'm_harness_section', id);
  for (const id of ['m_external_attachments', 'm_external_attachments_note', 'm_accent_harness_logos']) assert.equal(owner(ids.get(id)), 'm_attachment_settings', id);
  const harness = ids.get('m_harness_section');
  assert.equal(harness.children.at(-1).attrs.id, 'm_hcheck');
  assert.equal(ids.get('m_hcheck').text.trim(), 'Check for updates');
  assert.match(ids.get('m_about').children.find(e => e.attrs.id === 'm_description').text, /supervis/i);
  assert.equal(ids.get('m_privacy').tag, 'details');
  assert.equal(ids.get('m_privacy').children[0].tag, 'summary');
});

test('preference navigation opens the containing details before scrolling, without closing an open menu', () => {
  for (const alreadyOpen of [false, true]) {
    const order = [], menu = { classList: { contains: () => alreadyOpen } };
    const details = { open: false };
    const section = { closest: s => s === 'details' ? details : menu, scrollIntoView: opts => { assert.equal(details.open, true); order.push(opts); } };
    openAttachmentSettings({ click: () => order.push('open menu') }, section);
    assert.deepEqual(order, [...(alreadyOpen ? [] : ['open menu']), { block: 'center' }]);
  }
});

async function appFixture(t, { remote = { hosted: 'enrolled', can_pair: true }, harnesses = [{ agent: 'vibe', present: true, launchable: true, installed: '2.0.0' }] } = {}) {
  let lanes = null, status = remote, calls = [];
  const fetchImpl = async (path, opts = {}) => {
    calls.push({ path, method: opts.method || 'GET' });
    if (path === '/api/harnesses/latest') return jsonResponse(await lanes.harness.promise);
    if (path === '/api/update/check') return jsonResponse(await lanes.scimux.promise);
    if (path === '/api/remote/status') {
      if (status instanceof Error) return jsonResponse({ error: status.message }, { status: status.status || 503 });
      return jsonResponse(status);
    }
    if (path === '/api/state') return jsonResponse({ ...routeBody(path), version: 'v1.0.0' });
    if (path === '/api/harnesses') return jsonResponse({ harnesses });
    if (path === '/api/licenses') return jsonResponse({ scimux: 'synthetic project license', pion: 'synthetic dependency license' });
    return jsonResponse(routeBody(path));
  };
  const host = installHost({ fetchImpl });
  const get = id => host.byId.get(id);
  // Link only the menu ancestry needed by these interactions; no alternate product logic.
  for (const [child, parent] of [['m_harnesses', 'm_harness_section'], ['m_harness_section', 'menu'], ['m_attachment_settings', 'menu']]) if (get(child) && get(parent)) { get(child).parentElement = get(parent); }
  for (const id of ['m_harness_section', 'm_attachment_settings']) if (get(id)) {
    get(id).open = false;
    get(id).closest = s => s === 'details' ? get(id) : s === '.sheet' ? get('menu') : null;
  }
  get('m_harnesses').closest = s => s === 'details' ? get('m_harness_section') : s === '.sheet' ? get('menu') : null;
  get('m_version').textContent = 'v1.0.0';
  get('m_remote').open = false;
  const lic = ['scimux', 'pion'].map(key => { const el = host.document.createElement('button'); el.dataset.lic = key; return el; });
  const queryAll = host.document.querySelectorAll;
  host.document.querySelectorAll = s => s === '#menu .lic' ? lic : s === '.sheet' ? [get('menu')] : queryAll(s);
  get('menu').matches = s => s === '#menu';
  const instance = await createApp({ fetchImpl, assetURL: p => p, document: host.document, window: host.window });
  t.after(() => instance.retire());
  await flush();
  return { get, calls, lic, host, setStatus: s => { status = s; }, gates: () => (lanes = { harness: deferred(), scimux: deferred() }) };
}

test('Claude usage click executes composition navigation for closed and open menus', async t => {
  const s = await appFixture(t);
  for (const open of [false, true]) {
    s.get('menu').classList.toggle('open', open);
    const details = s.get('m_harness_section');
    if (details) details.open = false;
    let scrolled = false;
    s.get('m_harnesses').scrollIntoView = () => { assert.equal(details?.open, true); assert.equal(s.get('menu').classList.contains('open'), true); scrolled = true; };
    s.get('sys').dispatchEvent({ type: 'click', target: { closest: () => ({}) } });
    await flush();
    assert.equal(scrolled, true);
  }
  s.get('sys').dispatchEvent({ type: 'click', target: null });
  s.get('sys').dispatchEvent({ type: 'click', target: {} });
  s.get('sys').dispatchEvent({ type: 'click', target: { closest: () => null } });
});

for (const [harnessFail, scimuxFail] of [[false, false], [true, false], [false, true], [true, true]]) {
  test(`combined pending checks, independent results and retry: harness failure=${harnessFail}, scimux failure=${scimuxFail}`, async t => {
    const s = await appFixture(t);
    const gates = s.gates();
    s.get('m_vibe_inspect').checked = true;
    if (s.get('m_about')) s.get('m_about').open = false;
    await s.get('m_hcheck').click(); await flush();
    assert.equal(s.get('m_hcheck').disabled, true);
    assert.equal(s.get('m_check').disabled, true);
    assert.equal(s.get('m_vibe_inspect').disabled, true);
    assert.equal(s.get('m_vibe_inspect').checked, false);
    assert.equal(s.get('m_hcheck').getAttribute('aria-busy'), 'true');
    assert.match(s.get('m_hcheck').getAttribute('aria-label'), /checking.*harness.*scimux/i);
    assert.match(s.get('m_hcheck').innerHTML, /class="ctadots" aria-hidden="true"><i><\/i><i><\/i><i><\/i>/);
    assert.equal(s.get('m_version').textContent, 'v1.0.0');
    await s.get('m_hcheck').click(); await flush();
    assert.equal(s.calls.filter(c => c.path === '/api/harnesses/latest').length, 1);
    const release = { current: 'v1.0.0', latest: 'v2.0.0', available: true, notes: 'Synthetic notes', url: 'https://example.invalid/release' };
    if (scimuxFail) gates.scimux.reject(new Error('release unavailable')); else gates.scimux.resolve(release);
    await flush();
    assert.equal(s.get('m_hcheck').disabled, true, 'busy lasts for the remaining harness lane');
    assert.equal(s.get('m_check').disabled, true);
    assert.equal(s.get('m_about')?.open, true, 'release result/error expands ABOUT');
    if (harnessFail) gates.harness.reject(new Error('catalog unavailable')); else gates.harness.resolve({ latest: {}, harnesses: [{ agent: 'claude', present: true, launchable: true, installed: '1.0.0' }], agents: {} });
    await flush();
    assert.equal(s.get('m_hcheck').disabled, false);
    assert.equal(s.get('m_check').disabled, false);
    assert.equal(s.get('m_vibe_inspect').disabled, false);
    assert.equal(s.get('m_hcheck').getAttribute('aria-busy'), null);
    assert.doesNotMatch(s.get('m_hcheck').innerHTML, /ctadots/);
    assert.equal(s.get('m_version').textContent, 'v1.0.0');
    assert.match(s.get('m_reltext').textContent, scimuxFail ? /check failed/i : /v2.0.0 available/);
    assert.equal(s.get('m_apply').hidden, scimuxFail);
    if (!harnessFail) assert.match(s.get('m_harnesses').innerHTML, /data-agent="claude"/);
    const retry = s.gates();
    await s.get('m_hcheck').click(); await flush();
    retry.harness.resolve({}); await flush();
    assert.equal(s.get('m_hcheck').disabled, true, 'busy also lasts when harness settles first');
    retry.scimux.resolve({ ...release, available: false, notes: '', url: '' }); await flush();
    assert.equal(s.get('m_hcheck').disabled, false);
    assert.equal(s.get('m_hcheck').textContent, 'Check for updates');
    assert.equal(s.calls.filter(c => c.path === '/api/harnesses/latest').length, 2);
  });
}

test('remote visibility, refusal/unlink controls, folding and menu reopen preserve server decisions without writes', async t => {
  const absent = Object.assign(new Error('remote disabled'), { status: 404 });
  const s = await appFixture(t, { remote: absent });
  await s.get('burger').click(); await flush();
  assert.equal(s.get('m_remote').hidden, true);
  s.setStatus({ hosted: 'enrolled', can_pair: false, pair_refusal: 'Synthetic refusal' });
  await s.get('burger').click(); await flush();
  assert.equal(s.get('m_remote').hidden, false);
  assert.equal(s.get('m_remote').open, false);
  assert.equal(s.get('m_pair').hidden, true);
  assert.match(s.get('m_pair_note').innerHTML, /Synthetic refusal/);
  assert.equal(s.get('m_unlink').hidden, false);
  for (const id of ['m_about', 'm_remote', 'm_harness_section', 'm_attachment_settings']) {
    const el = s.get(id); assert.ok(el, id); el.open = false; el.open = true;
    el.dispatchEvent({ type: 'toggle' });
  }
  s.setStatus(new Error('status unavailable'));
  await s.get('burger').click(); await flush();
  assert.equal(s.get('m_remote').hidden, false);
  assert.equal(s.get('m_remote').open, true, 'live fold choice survives refresh');
  assert.equal(s.get('m_pair').hidden, true);
  s.setStatus({ hosted: 'unavailable', can_pair: true });
  await s.get('burger').click(); await flush();
  assert.equal(s.get('m_pair').hidden, false);
  s.setStatus({ hosted: '', can_pair: false });
  await s.get('burger').click(); await flush();
  assert.equal(s.get('m_unlink').hidden, true);
  assert.ok(s.calls.every(c => c.method === 'GET'));
  assert.ok(s.calls.every(c => !['/api/harnesses/latest', '/api/update/check', '/api/remote/pairing', '/api/remote/unenroll'].includes(c.path)));
});

test('license disclosure still loads and folds inside the ABOUT menu', async t => {
  const s = await appFixture(t);
  await s.lic[0].click();
  assert.equal(s.get('m_lictext').textContent, 'synthetic project license');
  assert.equal(s.get('m_lictext').hidden, false);
  await s.lic[0].click(); assert.equal(s.get('m_lictext').hidden, true);
  await s.lic[1].click(); assert.equal(s.get('m_lictext').textContent, 'synthetic dependency license');
  assert.equal(s.calls.filter(c => c.path === '/api/licenses').length, 1);
});

test('harness rerender preserves the same live Vibe checkbox and its pending state', () => {
  const row = { hidden: false, checked: true, disabled: true, remove() {} }, mounted = [];
  const host = { innerHTML: '', querySelector: () => ({ appendChild: el => mounted.push(el) }) };
  mountHarnessRows(host, '<div data-agent="vibe"></div>', row);
  mountHarnessRows(host, '<div data-agent="vibe"></div>', row);
  assert.deepEqual(mounted, [row, row]);
  assert.equal(row.checked, true); assert.equal(row.disabled, true);
});

test('release rendering preserves folds for current releases, reveals empty-note updates, and recovers from errors', async t => {
  const s = await appFixture(t);
  const { createMenuReleaseView, renderMenuCheck } = await import('../js/burger-menu.js');
  const view = createMenuReleaseView(sel => s.get(sel.slice(1)), text => `rendered:${text}`);
  const about = s.get('m_about');
  about.open = false;
  view.checking();
  assert.equal(s.get('m_version').textContent, 'v1.0.0');
  assert.equal(about.open, false);
  view.result({ current: 'v1.0.0', latest: 'v1.0.0', available: false, url: '' });
  assert.equal(about.open, false);
  assert.equal(s.get('m_notes').innerHTML, '');
  assert.equal(s.get('m_relurl').href, 'https://github.com/scimux/scimux/releases');
  view.result({ current: 'v1.0.0', latest: 'v2.0.0', available: true });
  assert.equal(about.open, true);
  assert.equal(s.get('m_notes').innerHTML, 'rendered:');
  assert.equal(s.get('m_notes').hidden, true);
  view.failed();
  assert.equal(s.get('m_version').textContent, 'v1.0.0');
  assert.equal(s.get('m_relurl').hidden, true);
  const bare = {};
  renderMenuCheck(bare, true, '<checking>');
  assert.match(bare.innerHTML, /&lt;checking&gt;/);
  renderMenuCheck(bare, false, 'retry');
  assert.doesNotMatch(bare.innerHTML, /ctadots/);
});

test('standalone scimux control remains independent, preserves installed version and reveals errors', async t => {
  const s = await appFixture(t);
  const gate = s.gates();
  s.get('m_about').open = false;
  await s.get('m_check').click(); await flush();
  assert.equal(s.get('m_check').disabled, true);
  assert.equal(s.get('m_version').textContent, 'v1.0.0');
  gate.scimux.reject(new Error('release unavailable')); await flush();
  assert.equal(s.get('m_check').disabled, false);
  assert.equal(s.get('m_about').open, true);
  assert.equal(s.get('m_version').textContent, 'v1.0.0');
  assert.equal(s.calls.filter(c => c.path === '/api/harnesses/latest').length, 0);
});
