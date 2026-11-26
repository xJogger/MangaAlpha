const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { test } = require('node:test');
const vm = require('node:vm');

const source = readFileSync('internal/httpserver/static/ui/app.js', 'utf8');

function extractSlice(startMarker, endMarker) {
  const start = source.indexOf(startMarker);
  const end = source.indexOf(endMarker, start);
  assert.ok(start >= 0 && end > start, `missing slice ${startMarker} -> ${endMarker}`);
  return source.slice(start, end);
}

const helpersSource = [
  extractSlice('  function posterSkeletonHTML(', '  // --- Setup & Login Views ---'),
  extractSlice('  function renderHome(', '  // --- Series Detail View (/series/:id) ---'),
].join('\n');

const apiSource = extractSlice('  async function api(url, opts = {}) {', '  let modalScroll = null;');

function extractTagInnerHTML(html, openEndIndex, tagName) {
  const openTag = new RegExp(`<${tagName}\\b[^>]*>`, 'gi');
  const closeTag = new RegExp(`</${tagName}>`, 'gi');
  let depth = 1;
  let idx = openEndIndex;
  while (depth > 0 && idx < html.length) {
    openTag.lastIndex = idx;
    closeTag.lastIndex = idx;
    const nextOpen = openTag.exec(html);
    const nextClose = closeTag.exec(html);
    if (!nextClose) return html.slice(openEndIndex);
    if (nextOpen && nextOpen.index < nextClose.index) {
      depth += 1;
      idx = nextOpen.index + nextOpen[0].length;
    } else {
      depth -= 1;
      if (depth === 0) return html.slice(openEndIndex, nextClose.index);
      idx = nextClose.index + nextClose[0].length;
    }
  }
  return '';
}

function createFakeElement(id = '', ownerDoc = null) {
  const el = {
    id,
    className: '',
    disabled: false,
    checked: false,
    value: '',
    isConnected: true,
    _ownerDoc: ownerDoc,
    _attrs: new Map(),
    _ownedIds: new Set(),
    _buttons: [],
    _rectTop: 1200,
    _html: '',
    setAttribute(k, v) { this._attrs.set(k, String(v)); },
    getAttribute(k) { return this._attrs.has(k) ? this._attrs.get(k) : null; },
    remove() {
      this.isConnected = false;
      if (this._ownerDoc) {
        this._ownerDoc._elements.delete(this.id);
        for (const cid of this._ownedIds) this._ownerDoc._elements.delete(cid);
      }
      this._ownedIds.clear();
    },
    insertAdjacentHTML(_pos, html) {
      this._html += html;
    },
    getBoundingClientRect() {
      return { top: this._rectTop };
    },
    querySelector(sel) {
      if (sel === 'button') return this._buttons[0] || null;
      return null;
    },
    querySelectorAll() { return []; },
  };
  Object.defineProperty(el, 'innerHTML', {
    get() { return this._html; },
    set(val) {
      const next = String(val);
      if (this._ownerDoc) {
        for (const cid of this._ownedIds) {
          const oldChild = this._ownerDoc._elements.get(cid);
          if (oldChild) oldChild.isConnected = false;
          this._ownerDoc._elements.delete(cid);
        }
      }
      this._ownedIds.clear();
      this._html = next;
      this._buttons = [];

      const btnRe = /<button\b([^>]*)>([\s\S]*?)<\/button>/gi;
      for (const m of next.matchAll(btnRe)) {
        const attrs = m[1] || '';
        const idMatch = attrs.match(/(?:^|\s)id="([^"]+)"/i);
        const btn = createFakeElement(idMatch ? idMatch[1] : '', this._ownerDoc);
        btn.textContent = m[2];
        this._buttons.push(btn);
        if (btn.id && this._ownerDoc) {
          this._ownerDoc._elements.set(btn.id, btn);
          this._ownedIds.add(btn.id);
        }
      }

      if (this._ownerDoc) {
        const tagWithIdRe = /<([a-z0-9-]+)\b([^>]*?\sid="([^"]+)"[^>]*)>/gi;
        for (const m of next.matchAll(tagWithIdRe)) {
          const tagName = m[1];
          const cid = m[3];
          if (tagName.toLowerCase() === 'button') continue;
          const child = createFakeElement(cid, this._ownerDoc);
          const clsMatch = m[2].match(/\bclass="([^"]*)"/i);
          if (clsMatch) child.className = clsMatch[1];
          for (const am of m[2].matchAll(/\b(data-[a-z0-9-]+)="([^"]*)"/gi)) {
            child._attrs.set(am[1], am[2]);
          }
          child._html = extractTagInnerHTML(next, m.index + m[0].length, tagName);
          this._ownerDoc._elements.set(cid, child);
          this._ownedIds.add(cid);
        }
      }
    },
  });
  return el;
}

function createShelfHarness({
  search = '',
  infinite = true,
  pageSize = 2,
  withObserver = true,
  userId = 'u1',
  apiHandler,
}) {
  const elements = new Map();
  const doc = {
    title: '',
    _elements: elements,
    documentElement: { clientHeight: 900 },
    getElementById(id) { return elements.get(id) || null; },
  };
  const rootEl = createFakeElement('app-root', doc);
  elements.set('page-content', createFakeElement('page-content', doc));

  const observers = [];
  const replacedURLs = [];
  const navigated = [];

  class FakeIntersectionObserver {
    constructor(cb, opts) {
      this.cb = cb;
      this.opts = opts;
      this.observed = [];
      this.disconnected = false;
      observers.push(this);
    }
    observe(target) { this.observed.push(target); }
    unobserve(target) {
      this.observed = this.observed.filter((t) => t !== target);
      this.unobserved = (this.unobserved || []).concat([target]);
    }
    disconnect() { this.disconnected = true; }
    trigger(isIntersecting = true) {
      if (!this.disconnected) this.cb([{ isIntersecting }]);
    }
  }

  const localStore = new Map();
  const win = {
    innerHeight: 900,
    scrollToCalls: [],
    scrollTo(opts) { this.scrollToCalls.push(opts); },
    requestAnimationFrame(fn) { return setTimeout(fn, 0); },
    matchMedia() { return { matches: false }; },
  };
  if (withObserver) win.IntersectionObserver = FakeIntersectionObserver;

  const ctx = vm.createContext({
    URLSearchParams,
    Set,
    Promise,
    Number,
    String,
    Boolean,
    Math,
    setTimeout,
    document: doc,
    window: win,
    location: { search },
    history: {
      replaceState(_s, _t, url) {
        replacedURLs.push(url);
        const qIdx = url.indexOf('?');
        ctx.location.search = qIdx >= 0 ? url.slice(qIdx) : '';
      },
    },
    localStorage: {
      getItem: (k) => (localStore.has(k) ? localStore.get(k) : null),
      setItem: (k, v) => localStore.set(k, String(v)),
    },
    AbortController,
    isAbortError(err) {
      return Boolean(err && (err.name === 'AbortError' || err.code === 20));
    },
    state: {
      libraries: [],
      coverVer: 1,
      auth: { authenticated: true, userId },
      shelfObserver: null,
      shelfAbort: null,
    },
    routeSeq: 1,
    teardownShelfObserver() {
      if (ctx.state.shelfObserver) {
        ctx.state.shelfObserver.disconnect();
        ctx.state.shelfObserver = null;
      }
      if (ctx.state.shelfAbort) {
        ctx.state.shelfAbort.abort();
        ctx.state.shelfAbort = null;
      }
    },
    displayPref(name) {
      if (name === 'shelfInfiniteScroll') return infinite;
      if (name === 'shelfPageSize') return pageSize;
      return 8;
    },
    mergedWorks: () => false,
    isAdminUser: () => true,
    providerLabel: (p) => (p === 'local' ? '本地 / SMB' : '115 网盘'),
    librarySourceText: (id) => {
      const lib = ctx.state.libraries.find((l) => l.id === id);
      return lib ? `${lib.name} · 115 网盘` : '';
    },
    seriesCardHTML(s, libraryID) {
      const src = !libraryID ? ctx.librarySourceText(s.libraryId) : '';
      return `<a class="poster-card" data-id="${s.id}">${s.name}${src ? `<div class="poster-source">${src}</div>` : ''}</a>`;
    },
    renderShell(_nav, html) {
      rootEl.innerHTML = `<main id="page-content">${html}</main>`;
      const pc = doc.getElementById('page-content');
      assert.ok(pc);
      for (const id of ['f-q', 'f-sort', 'f-read', 'f-scrape', 'f-oneshot', 'f-status', 'f-genre', 'f-tag', 'f-pub', 'f-author', 'f-merge-bgm', 'f-reset', 'filter-form']) {
        if (!elements.has(id)) elements.set(id, createFakeElement(id, doc));
      }
    },
    navigate(url) { navigated.push(url); },
    esc: (v) => (v == null ? '' : String(v)),
    api: apiHandler,
  });
  if (withObserver) ctx.IntersectionObserver = FakeIntersectionObserver;

  vm.runInContext(helpersSource, ctx);
  return { ctx, doc, win, observers, replacedURLs, navigated };
}

function createApiHarness({ status, body, onJson }) {
  const navigated = [];
  const routed = [];
  const ctx = vm.createContext({
    state: { auth: { authenticated: true, userId: 'u1' } },
    location: { pathname: '/libraries', search: '' },
    navigate: (url) => navigated.push(url),
    route: () => routed.push(true),
    fetch: async () => ({
      status,
      ok: status >= 200 && status < 300,
      headers: { get: () => 'application/json' },
      clone() { return this; },
      async json() {
        if (onJson) onJson();
        return body;
      },
      async text() { return ''; },
    }),
  });
  vm.runInContext(apiSource, ctx);
  return { ctx, navigated, routed };
}

test('initial library load error removes skeleton and reveals retry UI', async () => {
  let attempt = 0;
  const h = createShelfHarness({
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [{ id: 'lib1', name: '主库', providerType: '115' }];
      if (path.startsWith('/api/v1/series')) {
        attempt += 1;
        if (attempt === 1) throw new Error('network down');
        return { content: [{ id: 's1', libraryId: 'lib1', name: 'Work 1' }], totalElements: 1, totalPages: 1 };
      }
      return [];
    },
  });

  await h.ctx.renderLibraries('');
  assert.equal(h.doc.getElementById('library-skeleton'), null);
  const status = h.doc.getElementById('library-list-status');
  assert.ok(status);
  assert.ok(!status.className.includes('visually-hidden'));
  assert.match(status.innerHTML, /列表加载失败：network down/);
  const retryBtn = status.querySelector('button');
  assert.ok(retryBtn);

  await retryBtn.onclick();
  assert.match(h.doc.getElementById('library-cards').innerHTML, /Work 1/);
});

test('infinite scroll resets stale ?page=N to page 0 and strips URL param', async () => {
  const requestedPages = [];
  const h = createShelfHarness({
    search: '?page=3&sort=createdDate,desc',
    infinite: true,
    pageSize: 2,
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [{ id: 'lib1', name: '主库', providerType: '115' }];
      if (path.startsWith('/api/v1/series')) {
        const u = new URL(path, 'http://test.invalid');
        const p = Number(u.searchParams.get('page'));
        requestedPages.push(p);
        return {
          content: [{ id: `s_${p}_1`, libraryId: 'lib1', name: `Work ${p}-1` }],
          totalElements: 2,
          totalPages: 2,
        };
      }
      return [];
    },
  });

  await h.ctx.renderLibraries('');
  assert.equal(requestedPages[0], 0);
  assert.deepEqual(h.replacedURLs, ['/libraries?sort=createdDate%2Cdesc']);
});

test('infinite scroll continues when sentinel remains near viewport and stops on error', async () => {
  let failPage2 = true;
  const requestedPages = [];
  const h = createShelfHarness({
    infinite: true,
    pageSize: 2,
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [];
      if (path.startsWith('/api/v1/series')) {
        const u = new URL(path, 'http://test.invalid');
        const p = Number(u.searchParams.get('page'));
        requestedPages.push(p);
        if (p === 0) return { content: [{ id: 's1', name: 'S1' }, { id: 's2', name: 'S2' }], totalElements: 5, totalPages: 3 };
        if (p === 1) return { content: [{ id: 's2', name: 'S2' }, { id: 's3', name: 'S3' }], totalElements: 5, totalPages: 3 };
        if (p === 2 && failPage2) throw new Error('timeout');
        return { content: [{ id: 's4', name: 'S4' }, { id: 's5', name: 'S5' }], totalElements: 5, totalPages: 3 };
      }
      return [];
    },
  });

  await h.ctx.renderLibraries('lib1');
  const sentinel = h.doc.getElementById('shelf-sentinel');
  sentinel._rectTop = 400; // still inside viewport + 800px even after page 1 appends
  assert.equal(h.observers.length, 1);
  h.observers[0].trigger(true);

  await new Promise((r) => setTimeout(r, 40));
  assert.deepEqual(requestedPages, [0, 1, 2]);
  // Deduplicated s2 -> cards in grid are s1, s2, s3
  const cardsHTML = h.doc.getElementById('library-cards').innerHTML;
  assert.equal(cardsHTML.match(/class="poster-card"/g).length, 3);
  const status = h.doc.getElementById('shelf-load-status');
  assert.match(status.innerHTML, /加载失败：timeout/);

  // While the error banner is up the sentinel must stay paused: a stray
  // intersection must not silently re-fire the failing request.
  assert.deepEqual(h.observers[0].unobserved, [sentinel]);
  h.observers[0].trigger(true);
  await new Promise((r) => setTimeout(r, 20));
  assert.deepEqual(requestedPages, [0, 1, 2]);

  // Retry succeeds and reaches completion
  failPage2 = false;
  status.querySelector('button').onclick();
  await new Promise((r) => setTimeout(r, 20));
  assert.deepEqual(requestedPages, [0, 1, 2, 2]);
  assert.match(status.innerHTML, /已加载全部 <strong>5<\/strong> 部作品/);
  assert.equal(h.observers[0].disconnected, true);
});

test('fallback load-more button stays interactive across multiple pages without IntersectionObserver', async () => {
  const h = createShelfHarness({
    infinite: true,
    pageSize: 1,
    withObserver: false,
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [];
      if (path.startsWith('/api/v1/series')) {
        const p = Number(new URL(path, 'http://test.invalid').searchParams.get('page'));
        return { content: [{ id: `s${p}`, name: `S${p}` }], totalElements: 3, totalPages: 3 };
      }
      return [];
    },
  });

  await h.ctx.renderLibraries('lib1');
  assert.equal(h.observers.length, 0);
  const btn1 = h.doc.getElementById('shelf-load-more');
  assert.ok(btn1);
  await btn1.onclick();

  // Button must still be present for page 2
  const btn2 = h.doc.getElementById('shelf-load-more');
  assert.ok(btn2);
  await btn2.onclick();

  assert.equal(h.doc.getElementById('shelf-load-more'), null);
  assert.match(h.doc.getElementById('shelf-load-status').innerHTML, /已加载全部 <strong>3<\/strong> 部作品/);
});

test('renderHome waits for libraries before rendering card sources', async () => {
  let resolveLibs;
  const h = createShelfHarness({
    apiHandler: (path) => {
      if (path === '/api/v1/libraries') {
        return new Promise((r) => { resolveLibs = r; });
      }
      if (path.startsWith('/api/v1/series')) {
        return Promise.resolve({ content: [{ id: 's1', libraryId: 'lib1', name: 'Series A' }] });
      }
      return Promise.resolve({ content: [] });
    },
  });

  h.ctx.renderHome();
  await new Promise((r) => setTimeout(r, 15));
  // home-updated has not overwritten skeleton yet because libraries are still resolving
  assert.match(h.doc.getElementById('home-updated').innerHTML, /poster-card-skeleton/);

  resolveLibs([{ id: 'lib1', name: '主库', providerType: '115' }]);
  await new Promise((r) => setTimeout(r, 15));
  assert.match(h.doc.getElementById('home-updated').innerHTML, /主库 · 115 网盘/);
  assert.match(h.doc.getElementById('home-added').innerHTML, /主库 · 115 网盘/);
});

test('random roaming falls back to pagination so no work is silently dropped', async () => {
  const requestedPages = [];
  const h = createShelfHarness({
    search: '?sort=random&page=2',
    infinite: true,
    pageSize: 2,
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [];
      if (path.startsWith('/api/v1/series')) {
        const p = Number(new URL(path, 'http://test.invalid').searchParams.get('page'));
        requestedPages.push(p);
        return { content: [{ id: `s${p}`, name: `S${p}` }], totalElements: 10, totalPages: 5 };
      }
      return [];
    },
  });

  await h.ctx.renderLibraries('lib1');
  assert.deepEqual(requestedPages, [2]);
  // No sentinel, no appended pages: plain pagination and no URL rewriting.
  assert.equal(h.doc.getElementById('shelf-sentinel'), null);
  assert.equal(h.doc.getElementById('shelf-load-status'), null);
  assert.equal(h.doc.getElementById('library-cards').getAttribute('data-user-id'), 'u1');
  assert.ok(h.doc.getElementById('pg-prev'));
  assert.ok(h.doc.getElementById('pg-next'));
  assert.deepEqual(h.replacedURLs, []);
  h.doc.getElementById('pg-next').onclick();
  assert.equal(h.navigated.length, 1);
  assert.match(h.navigated[0], /sort=random/);
  assert.match(h.navigated[0], /page=3/);
});

test('cards rendered for another account are never reused after a cross-tab switch', async () => {
  const h = createShelfHarness({
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [{ id: 'lib1', name: '主库', providerType: '115' }];
      if (path.startsWith('/api/v1/series')) {
        return { content: [{ id: 's1', libraryId: 'lib1', name: 'Work 1' }], totalElements: 1, totalPages: 1 };
      }
      return [];
    },
  });
  await h.ctx.renderLibraries('lib1');
  const cards = h.doc.getElementById('library-cards');
  assert.equal(cards.getAttribute('data-user-id'), 'u1');
  assert.match(cards.innerHTML, /Work 1/);

  // A sibling tab switched to another account: refresh must not clone u1 cards.
  h.ctx.state.auth.userId = 'u2';
  let resolveApi;
  h.ctx.api = () => new Promise((r) => { resolveApi = r; });
  h.ctx.renderLibraries('lib1');
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(h.doc.getElementById('library-updating'), null);
  assert.ok(h.doc.getElementById('library-skeleton'));
  resolveApi([]);
});

test('same-account refresh keeps the previous cards as a stale preview', async () => {
  const h = createShelfHarness({
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [{ id: 'lib1', name: '主库', providerType: '115' }];
      if (path.startsWith('/api/v1/series')) {
        return { content: [{ id: 's1', libraryId: 'lib1', name: 'Work 1' }], totalElements: 1, totalPages: 1 };
      }
      return [];
    },
  });
  await h.ctx.renderLibraries('lib1');
  let resolveApi;
  h.ctx.api = () => new Promise((r) => { resolveApi = r; });
  h.ctx.renderLibraries('lib1');
  await new Promise((r) => setTimeout(r, 10));
  const stale = h.doc.getElementById('library-updating');
  assert.ok(stale);
  assert.equal(stale.getAttribute('data-user-id'), 'u1');
  assert.match(stale.innerHTML, /Work 1/);
  assert.equal(h.doc.getElementById('library-skeleton'), null);
  resolveApi([]);
});

test('route teardown aborts the in-flight shelf request', async () => {
  const h = createShelfHarness({ apiHandler: () => new Promise(() => {}) });
  h.ctx.renderLibraries('lib1');
  await new Promise((r) => setTimeout(r, 5));
  const controller = h.ctx.state.shelfAbort;
  assert.ok(controller);
  assert.equal(controller.signal.aborted, false);
  h.ctx.teardownShelfObserver();
  assert.equal(controller.signal.aborted, true);
  assert.equal(h.ctx.state.shelfAbort, null);
});

test('out-of-range page renders the empty state without dangling pagination bindings', async () => {
  const h = createShelfHarness({
    search: '?page=9',
    infinite: false,
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [];
      if (path.startsWith('/api/v1/series')) return { content: [], totalElements: 5, totalPages: 2 };
      return [];
    },
  });
  await h.ctx.renderLibraries('lib1');
  assert.equal(h.doc.getElementById('pg-prev'), null);
  assert.equal(h.doc.getElementById('pg-next'), null);
  const reset = h.doc.getElementById('empty-reset');
  assert.ok(reset);
  reset.onclick();
  assert.deepEqual(h.navigated, ['/libraries/lib1']);
});

test('applied filter chips use the multi-source match wording', () => {
  const h = createShelfHarness({ apiHandler: async () => [] });
  const html = h.ctx.filterChipsHTML(new URLSearchParams('scrape_status=unmatched'));
  assert.match(html, /匹配<strong>未匹配<\/strong>/);
  assert.doesNotMatch(html, /自动未命中/);
});

test('shelf merge toggle keeps the mainline hint and tooltip', () => {
  assert.match(source, /title="仅网页书架生效，第三方客户端仍独立展示，不改卷列表与阅读进度"/);
  assert.match(source, /（仅网页书架生效，第三方客户端仍独立展示）/);
  assert.match(source, /<option value="unmatched"[^>]*>未匹配<\/option>/);
});

test('mobile filter search input stays at 16px so iOS does not zoom on focus', () => {
  const css = readFileSync('internal/httpserver/static/ui/app.css', 'utf8');
  const blocks = [...css.matchAll(/\.filter-search-box \.filter-search-input \{[^}]*\}/g)].map((m) => m[0]);
  assert.ok(
    blocks.some((block) => /font-size:\s*16px/.test(block) && /padding-left:\s*34px/.test(block)),
    'expected the 780px media block to pin the search input at 16px',
  );
});

test('shared shelf styling still fits a 360px viewport', () => {
  const css = readFileSync('internal/httpserver/static/ui/app.css', 'utf8');
  const mobileStart = css.indexOf('@media (max-width: 780px)');
  assert.ok(mobileStart >= 0, 'missing mobile breakpoint');
  const mobile = css.slice(mobileStart);
  assert.match(mobile, /\.poster-grid \{\s*grid-template-columns: repeat\(auto-fill, minmax\(132px, 1fr\)\)/);
  assert.match(mobile, /\.filter-grid \{\s*grid-template-columns: repeat\(2, minmax\(0, 1fr\)\)/);
  // Shared components touched by the redesign keep their wrapping behaviour.
  assert.match(css, /\.section-head \{\s*display: flex;[\s\S]*?flex-wrap: wrap;/);
  assert.match(css, /\.pagination \{[\s\S]*?flex-wrap: wrap;/);
  assert.match(css, /\.chip \{[\s\S]*?max-width: 100%;/);
});

test('every shelf read carries X-FK115-User-Id, including load-more pages', async () => {
  const calls = [];
  const h = createShelfHarness({
    userId: 'u1',
    infinite: true,
    pageSize: 1,
    apiHandler: async (path, opts) => {
      calls.push({ path, opts });
      if (path === '/api/v1/libraries') return [];
      if (path.startsWith('/api/v1/series')) {
        const p = Number(new URL(path, 'http://test.invalid').searchParams.get('page'));
        return { content: [{ id: `s${p}`, name: `S${p}` }], totalElements: 2, totalPages: 2 };
      }
      return [];
    },
  });

  await h.ctx.renderLibraries('lib1');
  h.observers[0].trigger(true);
  await new Promise((r) => setTimeout(r, 40));

  const seriesCalls = calls.filter((c) => c.path.startsWith('/api/v1/series'));
  assert.equal(seriesCalls.length, 2, 'expected both the initial page and the appended page');
  assert.deepEqual(seriesCalls.map((c) => Number(new URL(c.path, 'http://test.invalid').searchParams.get('page'))), [0, 1]);
  assert.ok(calls.some((c) => c.path === '/api/v1/libraries'));
  for (const call of calls) {
    assert.equal(
      call.opts && call.opts.headers && call.opts.headers['X-FK115-User-Id'],
      'u1',
      `${call.path} must identify the active account`,
    );
  }
});

for (const search of ['?sort=Random', '?sort=random,desc', '?sort=%20random%20']) {
  test(`sort variant ${search} uses plain pagination and selects random`, async () => {
    const requestedPages = [];
    const h = createShelfHarness({
      search,
      infinite: true,
      pageSize: 2,
      apiHandler: async (path) => {
        if (path === '/api/v1/libraries') return [];
        if (path.startsWith('/api/v1/series')) {
          const p = Number(new URL(path, 'http://test.invalid').searchParams.get('page'));
          requestedPages.push(p);
          return { content: [{ id: `s${p}`, name: `S${p}` }], totalElements: 10, totalPages: 5 };
        }
        return [];
      },
    });

    await h.ctx.renderLibraries('lib1');
    // One request only — no infinite-scroll append can silently drop sampled works.
    assert.deepEqual(requestedPages, [0]);
    assert.equal(h.doc.getElementById('shelf-sentinel'), null);
    assert.equal(h.doc.getElementById('shelf-load-status'), null);
    const sortSelect = h.doc.getElementById('f-sort');
    assert.ok(sortSelect);
    assert.match(sortSelect.innerHTML, /<option value="random" selected>/);
    assert.match(h.doc.getElementById('page-content').innerHTML, /随机漫游 · 分页浏览/);
  });
}

test('empty userId never reuses a stale data-user-id="" card grid', async () => {
  const h = createShelfHarness({
    userId: '',
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [];
      if (path.startsWith('/api/v1/series')) {
        return { content: [{ id: 's1', name: 'Work 1' }], totalElements: 1, totalPages: 1 };
      }
      return [];
    },
  });

  await h.ctx.renderLibraries('lib1');
  const cards = h.doc.getElementById('library-cards');
  assert.ok(cards);
  assert.equal(cards.getAttribute('data-user-id'), '');

  const resolvers = [];
  h.ctx.api = () => new Promise((r) => { resolvers.push(r); });
  const pending = h.ctx.renderLibraries('lib1');
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(h.doc.getElementById('library-updating'), null);
  assert.ok(h.doc.getElementById('library-skeleton'));
  resolvers.forEach((resolve) => resolve([]));
  await pending;
});

test('a stale route observer cannot clear the new route shelf observer', async () => {
  const h = createShelfHarness({
    infinite: true,
    pageSize: 1,
    apiHandler: async (path) => {
      if (path === '/api/v1/libraries') return [];
      if (path.startsWith('/api/v1/series')) {
        return { content: [{ id: 's1', name: 'S1' }], totalElements: 3, totalPages: 3 };
      }
      return [];
    },
  });

  await h.ctx.renderLibraries('lib1');
  const oldObserver = h.observers[0];
  assert.ok(oldObserver);
  assert.equal(h.ctx.state.shelfObserver, oldObserver);

  // A newer route has already installed its own observer before the old
  // IntersectionObserver callback is delivered.
  const newObserver = { disconnect() {} };
  h.ctx.state.shelfObserver = newObserver;
  h.ctx.routeSeq = 2;
  oldObserver.cb([{ isIntersecting: true }]);

  assert.equal(h.ctx.state.shelfObserver, newObserver);
  assert.equal(oldObserver.disconnected, true);
});

for (const [status, body] of [[401, { error: 'UNAUTHORIZED' }], [409, { error: 'USER_CONTEXT_MISMATCH' }]]) {
  test(`api() rejects an already-aborted signal before handling ${status}`, async () => {
    const controller = new AbortController();
    controller.abort();
    const h = createApiHarness({ status, body });
    await assert.rejects(h.ctx.api('/x', { signal: controller.signal }), (err) => err.name === 'AbortError');
    assert.deepEqual(h.navigated, []);
    assert.deepEqual(h.routed, []);
  });
}

test('api() does not navigate on a 401 whose body read races an abort', async () => {
  const controller = new AbortController();
  const h = createApiHarness({
    status: 401,
    body: { error: 'UNAUTHORIZED' },
    onJson: () => controller.abort(),
  });
  await assert.rejects(h.ctx.api('/x', { signal: controller.signal }), (err) => err.name === 'AbortError');
  assert.deepEqual(h.navigated, []);
  assert.deepEqual(h.routed, []);
});

test('api() does not route on a 409 whose body read races an abort', async () => {
  const controller = new AbortController();
  const h = createApiHarness({
    status: 409,
    body: { error: 'USER_CONTEXT_MISMATCH' },
    onJson: () => controller.abort(),
  });
  await assert.rejects(h.ctx.api('/x', { signal: controller.signal }), (err) => err.name === 'AbortError');
  assert.deepEqual(h.navigated, []);
  assert.deepEqual(h.routed, []);
});

test('filter chips never render inherited prototype values as function source', () => {
  const h = createShelfHarness({ apiHandler: async () => [] });
  const html = h.ctx.filterChipsHTML(new URLSearchParams('scrape_status=constructor'));
  assert.match(html, /匹配<strong>constructor<\/strong>/);
  assert.doesNotMatch(html, /\[native code\]/);
});

test('inline merge toggle keeps a transparent hover background and compact hint', () => {
  // Strip comments so the `(0,2,0)` notes cannot be mistaken for selector commas.
  const css = readFileSync('internal/httpserver/static/ui/app.css', 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
  let hoverBlock = null;
  for (const m of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selectors = m[1].split(',').map((s) => s.trim());
    if (selectors.includes('.filter-foot .pref-toggle-inline:hover')) {
      hoverBlock = m[2];
      break;
    }
  }
  assert.ok(hoverBlock, 'missing .filter-foot .pref-toggle-inline:hover override');
  assert.match(hoverBlock, /background:\s*transparent/);
  assert.match(hoverBlock, /border-color:\s*transparent/);

  // Three classes (0,3,0) beat `.pref-toggle:hover` (0,2,0) regardless of order.
  const genericHover = css.match(/(^|\n)\s*\.pref-toggle:hover\s*\{([^}]*)\}/);
  assert.ok(genericHover);
  assert.match(genericHover[2], /background:\s*var\(--bg-elevated\)/);

  const hintRules = [...css.matchAll(/\.filter-foot \.pref-toggle-inline \.filter-merge-hint\s*\{([^}]*)\}/g)];
  assert.ok(hintRules.some((m) => /line-height:\s*1\.4/.test(m[1])), 'hint line-height must beat .pref-toggle-text span');

  const posterSource = css.match(/\.poster-source\s*\{([^}]*)\}/);
  assert.ok(posterSource);
  assert.doesNotMatch(posterSource[1], /margin-top/, '.poster-body gap already spaces the source line');
});
