const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const ROOT = path.resolve(__dirname, '..');
const APP_JS_PATH = path.join(ROOT, 'internal/httpserver/static/ui/app.js');
const APP_CSS_PATH = path.join(ROOT, 'internal/httpserver/static/ui/app.css');
const APPLY_HOST_PATH = path.join(ROOT, 'web/reader/apply-host.py');
const BRIDGE_TS_PATH = path.join(ROOT, 'web/reader/host/KomgaHostBridge.ts');

const appSource = fs.readFileSync(APP_JS_PATH, 'utf8');
const cssSource = fs.readFileSync(APP_CSS_PATH, 'utf8');
const applyHostSource = fs.readFileSync(APPLY_HOST_PATH, 'utf8');
const bridgeSource = fs.readFileSync(BRIDGE_TS_PATH, 'utf8');

function extractSlice(startMarker, endMarker, source = appSource) {
  const start = source.indexOf(startMarker);
  if (start < 0) throw new Error(`missing start marker: ${startMarker}`);
  const end = source.indexOf(endMarker, start + startMarker.length);
  if (end < 0) throw new Error(`missing end marker: ${endMarker}`);
  return source.slice(start, end);
}

function parseTagAttrs(raw) {
  const attrs = {};
  const attrRe = /([a-zA-Z_:][\w:.-]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>/]+)))?/g;
  let m;
  while ((m = attrRe.exec(raw)) !== null) {
    const key = m[1].toLowerCase();
    const val = m[2] !== undefined ? m[2] : (m[3] !== undefined ? m[3] : (m[4] !== undefined ? m[4] : ''));
    attrs[key] = val;
  }
  return attrs;
}

function createFakeDOM() {
  const registry = new Map();
  const allNodes = [];
  let autoId = 0;

  function makeElement(tag = 'div', attrs = {}) {
    let html = '';
    let childNodes = [];
    const attrMap = { ...attrs };
    const el = {
      tagName: tag.toUpperCase(),
      style: {},
      dataset: {},
      className: attrMap.class || '',
      disabled: Object.prototype.hasOwnProperty.call(attrMap, 'disabled'),
      checked: Object.prototype.hasOwnProperty.call(attrMap, 'checked'),
      hidden: Object.prototype.hasOwnProperty.call(attrMap, 'hidden'),
      value: attrMap.value !== undefined ? attrMap.value : '',
      name: attrMap.name || '',
      type: attrMap.type || '',
      textContent: '',
      title: attrMap.title || '',
      href: attrMap.href || '',
      _listeners: {},
      _attrMap: attrMap,
      _instanceId: ++autoId,
      get innerHTML() {
        return html;
      },
      set innerHTML(val) {
        html = String(val || '');
        childNodes = [];
        const tagRe = /<(div|span|a|button|form|input|select|textarea|label|section|h[1-4]|p|img|option)\b([^>]*)>/gi;
        let m;
        while ((m = tagRe.exec(html)) !== null) {
          const childTag = m[1];
          const childAttrs = parseTagAttrs(m[2] || '');
          const child = makeElement(childTag, childAttrs);
          childNodes.push(child);
          if (childAttrs.id) {
            registry.set(childAttrs.id, child);
          }
        }
      },
      get children() {
        return childNodes;
      },
      get elements() {
        const map = {};
        const pool = childNodes.length ? childNodes : allNodes;
        for (const c of pool) {
          if (c.name) map[c.name] = c;
        }
        return map;
      },
      get classList() {
        return {
          add(...cls) {
            const cur = new Set(String(el.className || '').split(/\s+/).filter(Boolean));
            cls.forEach((c) => cur.add(c));
            el.className = Array.from(cur).join(' ');
          },
          remove(...cls) {
            const cur = new Set(String(el.className || '').split(/\s+/).filter(Boolean));
            cls.forEach((c) => cur.delete(c));
            el.className = Array.from(cur).join(' ');
          },
          contains(c) {
            return String(el.className || '').split(/\s+/).includes(c);
          },
          toggle(c, force) {
            if (force === undefined) {
              if (this.contains(c)) this.remove(c);
              else this.add(c);
              return this.contains(c);
            }
            if (force) this.add(c);
            else this.remove(c);
            return Boolean(force);
          },
        };
      },
      setAttribute(k, v) {
        const lk = String(k).toLowerCase();
        attrMap[lk] = String(v);
        if (lk === 'id') registry.set(String(v), el);
        if (lk === 'class') el.className = String(v);
      },
      getAttribute(k) {
        const lk = String(k).toLowerCase();
        return Object.prototype.hasOwnProperty.call(attrMap, lk) ? attrMap[lk] : null;
      },
      removeAttribute(k) {
        const lk = String(k).toLowerCase();
        delete attrMap[lk];
        if (lk === 'disabled') el.disabled = false;
      },
      addEventListener(type, fn) {
        if (!el._listeners[type]) el._listeners[type] = [];
        el._listeners[type].push(fn);
      },
      removeEventListener(type, fn) {
        if (!el._listeners[type]) return;
        el._listeners[type] = el._listeners[type].filter((f) => f !== fn);
      },
      async dispatch(type, ev = {}) {
        const eventObj = { target: el, currentTarget: el, preventDefault() {}, ...ev };
        const prop = el[`on${type}`];
        if (typeof prop === 'function') await prop.call(el, eventObj);
        for (const fn of (el._listeners[type] || []).slice()) {
          await fn.call(el, eventObj);
        }
      },
      querySelectorAll(selector) {
        return queryNodes(childNodes.length ? childNodes : allNodes, selector);
      },
      querySelector(selector) {
        return this.querySelectorAll(selector)[0] || null;
      },
      closest() {
        return null;
      },
      scrollIntoView() {},
      focus() {},
      remove() {},
    };
    if (attrs.id) registry.set(attrs.id, el);
    allNodes.push(el);
    return el;
  }

  function matchesSelector(el, selector) {
    const s = String(selector || '').trim();
    if (!s) return false;
    if (s.startsWith('#')) return el.getAttribute('id') === s.slice(1);
    if (s.startsWith('.')) return el.classList.contains(s.slice(1));
    const tagAttr = s.match(/^([a-zA-Z]+)?\[([^\]]+)\]$/);
    if (tagAttr) {
      const wantTag = tagAttr[1] ? tagAttr[1].toUpperCase() : '';
      if (wantTag && el.tagName !== wantTag) return false;
      const inner = tagAttr[2];
      const eq = inner.indexOf('=');
      if (eq < 0) return el.getAttribute(inner) !== null;
      const attrName = inner.slice(0, eq).trim();
      const attrVal = inner.slice(eq + 1).trim().replace(/^["']|["']$/g, '');
      return el.getAttribute(attrName) === attrVal;
    }
    return el.tagName === s.toUpperCase();
  }

  function queryNodes(nodes, selector) {
    const out = [];
    const seen = new Set();
    for (const node of nodes) {
      if (matchesSelector(node, selector) && !seen.has(node._instanceId)) {
        seen.add(node._instanceId);
        out.push(node);
      }
      for (const child of node.children || []) {
        if (matchesSelector(child, selector) && !seen.has(child._instanceId)) {
          seen.add(child._instanceId);
          out.push(child);
        }
      }
    }
    return out;
  }

  const appEl = makeElement('main', { id: 'app' });
  const doc = {
    body: makeElement('body'),
    getElementById(id) {
      if (id === 'app') return appEl;
      return registry.get(id) || null;
    },
    querySelectorAll(selector) {
      return queryNodes([appEl, ...Array.from(registry.values())], selector);
    },
    querySelector(selector) {
      return this.querySelectorAll(selector)[0] || null;
    },
    createElement(tag) {
      return makeElement(tag);
    },
  };

  return { doc, appEl, registry };
}

function makeStorage(initial = {}) {
  const map = new Map(Object.entries(initial));
  return {
    getItem(k) {
      return map.has(k) ? String(map.get(k)) : null;
    },
    setItem(k, v) {
      map.set(String(k), String(v));
    },
    removeItem(k) {
      map.delete(String(k));
    },
  };
}

function createV33Harness(options = {}) {
  const { doc, appEl } = createFakeDOM();
  const storage = makeStorage(options.storage || {});
  const apiCalls = [];
  const toasts = [];
  let currentPath = options.pathname || '/';
  let currentSearch = options.search || '';
  let resetDisplayPrefsCalls = 0;
  let ensureDisplayPrefsCalls = 0;

  const state = {
    auth: options.auth || {
      claimed: true,
      authenticated: true,
      role: 'user',
      userId: 'u-1',
      username: 'alice',
      hasHiddenLibraries: false,
      authorizedLibrariesCount: 0,
    },
    libraries: options.libraries || [],
    taxonomies: { loaded: false, genres: [], tags: [], publishers: [] },
    searchQuery: '',
    displayPrefsLoaded: false,
    userMenuOpen: false,
  };

  const displayPrefs = {
    homeContinueSize: 6,
    homeUpdatedSize: 18,
    homeAddedSize: 12,
    shelfPageSize: 24,
    seriesPageSize: 24,
    historyPreviewCount: 3,
    defaultBrowseView: 'series',
    defaultSort: 'createdDate,desc',
    mergeBangumiWorks: true,
    ...(options.displayPrefs || {}),
  };

  const api = async (url, opts = {}) => {
    apiCalls.push({ url, opts });
    if (options.apiHandler) {
      return options.apiHandler(url, opts, { state, apiCalls });
    }
    return {};
  };

  const esc = (v) => String(v ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');

  const routeListeners = {};
  const ctx = {
    console,
    Date,
    Math,
    JSON,
    navigator: { userAgent: 'Mozilla/5.0' },
    URLSearchParams,
    Map,
    Set,
    Promise,
    AbortController,
    CSS: { escape: (s) => String(s).replace(/([^a-zA-Z0-9_-])/g, '\\$1') },
    FormData: class FakeFormData {
      constructor(form) {
        this._entries = [];
        for (const el of form.querySelectorAll('input') || []) {
          if (!el.name) continue;
          if (el.type === 'checkbox') {
            if (el.checked) this._entries.push([el.name, el.value || 'on']);
          } else {
            this._entries.push([el.name, el.value]);
          }
        }
      }
      get(k) {
        const hit = this._entries.find(([name]) => name === k);
        return hit ? hit[1] : null;
      }
      has(k) {
        return this._entries.some(([name]) => name === k);
      }
    },
    document: doc,
    window: {
      __appShellRendered: true,
      location: {
        get pathname() { return currentPath; },
        get search() { return currentSearch; },
        get origin() { return 'http://127.0.0.1:25600'; },
      },
      history: {
        replaceState(_s, _t, next) { currentPath = String(next); },
        pushState(_s, _t, next) { currentPath = String(next); },
      },
      addEventListener() {},
      scrollTo() {},
    },
    location: {
      get pathname() { return currentPath; },
      get search() { return currentSearch; },
      get origin() { return 'http://127.0.0.1:25600'; },
      href: 'http://127.0.0.1:25600/',
    },
    history: {
      replaceState(_s, _t, next) { currentPath = String(next); },
      pushState(_s, _t, next) { currentPath = String(next); },
    },
    localStorage: storage,
    state,
    api,
    esc,
    setTimeout,
    clearTimeout,
    toast: (msg, type = 'info') => { toasts.push({ msg, type }); },
    navigate: (p) => { currentPath = String(p); },
    safeRedirectTarget: (p) => (String(p || '').startsWith('/') ? String(p) : '/'),
    isAdmin: () => String(state.auth?.role || '') === 'admin',
    canEdit: () => String(state.auth?.role || '') === 'admin',
    displayPref: (k) => displayPrefs[k],
    mergedWorks: () => {
      const saved = storage.getItem('fk115_merge_bangumi');
      if (saved === '1') return true;
      if (saved === '0') return false;
      return Boolean(displayPrefs.mergeBangumiWorks);
    },
    resetDisplayPrefs: () => { resetDisplayPrefsCalls += 1; },
    ensureDisplayPrefs: async () => {
      ensureDisplayPrefsCalls += 1;
      if (options.onEnsureDisplayPrefs) {
        await options.onEnsureDisplayPrefs({ appEl, state, apiCalls });
      }
    },
    renderShell: (_active, html) => {
      appEl.innerHTML = `<div id="page-content">${html}</div>`;
    },
    isAdminUser: () => String(state.auth?.role || '') === 'admin',
    providerLabel: (pt, acc) => (pt === 'webdav' && acc ? `WebDAV (${acc})` : (pt || '')),
    seriesVersionURL: (sid) => `/series/${encodeURIComponent(sid)}?versions=1`,
    teardownShelfObserver: () => {},
    closeConfirmModal: () => {},
    confirmModal: async () => true,
    paintAdminUpdateBanner: () => {},
    updateNavSearchInput: () => {},
    syncShelfMergeToggle: () => {},
    setFilterExpanded: () => {},
    normalizeBrowseView: (v) => v || 'series',
    librarySourceText: (libId) => {
      const lib = (state.libraries || []).find((l) => l.id === libId);
      return lib ? lib.name : (libId ? `书库 ${libId}` : '');
    },
    renderGrid: (items) => `<div class="grid">${(items || []).map((it) => `<div class="card">${esc(it.title || it.name)}</div>`).join('')}</div>`,
    bookTitleText: (b) => b.metadata?.title || b.name || '',
    fmtMiB: (b) => `${Math.round((Number(b) || 0) / (1024 * 1024))} MiB`,
    fmtBytes: (b) => `${Number(b) || 0} B`,
    fmtTime: (t) => String(t || '—'),
    fmtBgmCollectionType: () => '',
    displayPrefCardHTML: () => '<div id="display-pref-card"></div>',
    bindDisplayPrefControls: () => {},
    bytesToMiBInput: (b) => String(Math.round((Number(b) || 0) / (1024 * 1024))),
    mibInputToBytes: (v, min) => Math.max(Number(min) || 0, Number(v || 0) * 1024 * 1024),
    thumbURL: (url, params) => `${url}?${new URLSearchParams(params).toString()}`,
    renderPager: () => '',
    bindSeriesGridCards: () => {},
    bindLazyCovers: () => {},
    bindFavoriteButtons: () => {},
    buildFilterBarHTML: () => '<div id="filter-bar"></div>',
    bindFilterBarEvents: () => {},
    filterBarLoading: () => {},
    renderTaxonomyCheckboxes: () => '',
    refreshLibrarySelects: async () => {},
    ensureTaxonomies: async () => {},
    ensureLibrariesLoaded: async () => state.libraries,
    buildSeriesSearchHref: () => '/libraries',
    syncAdminTabChrome: () => {},
    loadAdminJobs: async () => {},
    renderSetup: () => { appEl.innerHTML = '<div id="setup-view"></div>'; },
    renderLogin: () => { appEl.innerHTML = '<div id="login-view"></div>'; },
    renderCollections: async () => { appEl.innerHTML = '<div id="collections-view"></div>'; },
    renderCollectionDetail: async () => {},
    renderReadLists: async () => {},
    renderReadListDetail: async () => {},
    renderSeriesDetail: async () => {},
    renderBookDetail: async () => {},
    renderAgentAuthPage: async () => {},
    renderAdmin: async () => {},
    addEventListener: (k, fn) => { routeListeners[k] = fn; },
    routeSeq: 1,
  };

  vm.createContext(ctx);
  if (options.includeHomeAndLibraries) {
    const helperSlice = extractSlice('  function posterSkeletonHTML(', '  // --- Setup & Login Views ---');
    const homeLibSlice = extractSlice('  function renderHome(', '  // --- Series Detail View (/series/:id) ---');
    vm.runInContext(helperSlice + '\n' + homeLibSlice, ctx);
  }
  if (options.includeHistory) {
    const historySlice = extractSlice('  // --- Reading History Timeline (/history) ---', '  // --- Personal Account Settings Page (/account) ---');
    vm.runInContext(historySlice, ctx);
  }
  if (options.includeAccount) {
    const accountSlice = extractSlice('  // --- Personal Account Settings Page (/account) ---', '  // --- AI Agent One-Click Authorization Page (/agent-auth/:requestID) ---');
    vm.runInContext(accountSlice, ctx);
  }
  if (options.includeAdminSettings) {
    const settingsSlice = extractSlice('  // --- Admin Cache & Settings Tab ---', '  // --- Admin Security & API Keys Tab ---');
    vm.runInContext(settingsSlice, ctx);
  }
  if (options.includeRouter) {
    const routerSlice = extractSlice('  // --- Router ---', '  route();\n})();');
    vm.runInContext(routerSlice, ctx);
  }

  return {
    ctx,
    doc,
    appEl,
    state,
    storage,
    apiCalls,
    toasts,
    getResetDisplayPrefsCalls: () => resetDisplayPrefsCalls,
    getEnsureDisplayPrefsCalls: () => ensureDisplayPrefsCalls,
    getCurrentPath: () => currentPath,
    setLocation: (p, s = '') => { currentPath = p; currentSearch = s; },
  };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

test('/account library visibility filter renders authorized libraries, saves changes, enforces in-flight guard, and reverts on error', async () => {
  let mePayload = {
    user: {
      id: 'u-1',
      username: 'alice',
      role: 'user',
      allowNsfw: true,
      showNsfw: true,
    },
    authorizedLibraries: [
      { id: 'lib-1', name: '115 主库', providerType: '115', hidden: false },
      { id: 'lib-2', name: 'WebDAV 仓库', providerType: 'webdav', webdavAccountName: 'NAS', hidden: true },
    ],
    hiddenLibraryIds: ['lib-2'],
    visibleLibraries: [
      { id: 'lib-1', name: '115 主库', providerType: '115' },
    ],
    apiKeys: [],
    bangumiTracker: { enabled: false, hasToken: false, nsfwMode: 'skip' },
    bangumiOAuthConfigured: false,
  };

  let pendingPut = null;
  let failNextPut = false;
  const latestLibs = [{ id: 'lib-1', name: '115 主库' }];

  const h = createV33Harness({
    includeAccount: true,
    libraries: latestLibs,
    apiHandler: async (url, opts) => {
      if (url === '/admin/api/me') return mePayload;
      if (url === '/admin/api/me/preferences' && opts.method === 'PUT') {
        if (pendingPut) return pendingPut.promise;
        if (failNextPut) {
          failNextPut = false;
          throw new Error('保存失败');
        }
        return { allowNsfw: true, showNsfw: true, hiddenLibraryIds: opts.body.hiddenLibraryIds };
      }
      if (url === '/api/v1/libraries') {
        return mePayload._nextLibs || latestLibs;
      }
      if (url === '/admin/api/auth/api-keys' || url === '/admin/api/auth/sessions') {
        return [];
      }
      if (url.startsWith('/admin/api/bangumi/tracker')) {
        return null;
      }
      return {};
    },
  });

  await h.ctx.renderAccountPage();

  const filterBox = h.doc.getElementById('acc-lib-filter-box');
  assert.ok(filterBox, 'expected #acc-lib-filter-box when authorizedLibraries is non-empty');
  const checkboxes = h.appEl.querySelectorAll('input[data-acc-lib-id]');
  assert.equal(checkboxes.length, 2);
  assert.equal(checkboxes[0].checked, true);
  assert.equal(checkboxes[1].checked, false);

  const showAllBtn = h.doc.getElementById('acc-libs-show-all');
  assert.equal(showAllBtn.disabled, false);

  // Guard against duplicate in-flight PUTs
  pendingPut = deferred();
  checkboxes[0].checked = false;
  const firstSave = checkboxes[0].dispatch('change');
  assert.equal(showAllBtn.disabled, true);
  assert.equal(checkboxes[0].disabled, true);

  // Trigger a duplicate change while first request is still pending
  await checkboxes[1].dispatch('change');
  const putCallsDuringFlight = h.apiCalls.filter((c) => c.url === '/admin/api/me/preferences');
  assert.equal(putCallsDuringFlight.length, 1);

  mePayload._nextLibs = [];
  const p = pendingPut;
  pendingPut = null;
  p.resolve({ allowNsfw: true, showNsfw: true, hiddenLibraryIds: ['lib-1', 'lib-2'] });
  await firstSave;
  await new Promise((r) => setTimeout(r, 0));

  assert.equal(h.state.auth.hasHiddenLibraries, true);
  assert.equal(h.state.libraries.length, 0);
  assert.ok(h.doc.getElementById('acc-visible-libs').innerHTML.includes('已全部隐藏'));
  assert.ok(h.toasts.some((t) => t.msg.includes('已隐藏全部已授权书库')));

  // Error rollback restores previous hidden state
  failNextPut = true;
  checkboxes[0].checked = true;
  await checkboxes[0].dispatch('change');
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(checkboxes[0].checked, false, 'failed PUT should revert checkbox to previous checked=false');
  assert.ok(h.toasts.some((t) => t.type === 'error' && t.msg.includes('保存失败')));

  // Clicking "全部显示" clears hiddenLibraryIds and refreshes state.libraries
  mePayload._nextLibs = [
    { id: 'lib-1', name: '115 主库' },
    { id: 'lib-2', name: 'WebDAV 仓库' },
  ];
  await showAllBtn.dispatch('click');
  await new Promise((r) => setTimeout(r, 0));
  const lastPut = h.apiCalls.filter((c) => c.url === '/admin/api/me/preferences').pop();
  assert.deepEqual(JSON.parse(JSON.stringify(lastPut.opts.body)), { hiddenLibraryIds: [] });
  assert.equal(h.state.auth.hasHiddenLibraries, false);
  assert.equal(h.state.libraries.length, 2);
  assert.equal(showAllBtn.disabled, true);
  assert.ok(checkboxes.every((cb) => cb.checked === true));
  assert.ok(h.toasts.some((t) => t.msg.includes('已设为显示全部已授权书库')));
});

test('/account hides library filter block when user has zero authorized libraries', async () => {
  const h = createV33Harness({
    includeAccount: true,
    apiHandler: async (url) => {
      if (url === '/admin/api/me') {
        return {
          user: {
            id: 'u-2',
            username: 'bob',
            role: 'user',
            allowNsfw: false,
            showNsfw: false,
            authorizedLibraries: [],
            hiddenLibraryIds: [],
          },
          apiKeys: [],
          bangumiTracker: { enabled: false, hasToken: false, nsfwMode: 'skip' },
          bangumiOAuthConfigured: false,
        };
      }
      if (url === '/admin/api/auth/api-keys' || url === '/admin/api/auth/sessions') {
        return [];
      }
      if (url.startsWith('/admin/api/bangumi/tracker')) {
        return null;
      }
      return {};
    },
  });

  await h.ctx.renderAccountPage();
  assert.equal(h.doc.getElementById('acc-lib-filter-box'), null);
});

test('renderHome and renderLibraries display friendly all-hidden recovery card instead of no-library errors', async () => {
  const emptyApi = async (url) => {
    if (url === '/api/v1/libraries') return [];
    if (url.startsWith('/api/v1/genres') || url.startsWith('/api/v1/tags') || url.startsWith('/api/v1/publishers') || url.startsWith('/api/v1/authors')) return [];
    if (url.startsWith('/admin/api/bookshelf/continue-reading')) return { content: [], totalElements: 0 };
    if (url.startsWith('/api/v1/series')) return { content: [], totalElements: 0, totalPages: 0 };
    return {};
  };

  // Case 1: regular user with hasHiddenLibraries=true
  const hHidden = createV33Harness({
    includeHomeAndLibraries: true,
    auth: {
      claimed: true,
      authenticated: true,
      role: 'user',
      userId: 'u-1',
      hasHiddenLibraries: true,
      authorizedLibrariesCount: 2,
    },
    libraries: [],
    apiHandler: emptyApi,
  });

  await hHidden.ctx.renderHome();
  await new Promise((r) => setTimeout(r, 0));
  const homeLibsHidden = hHidden.doc.getElementById('home-libs');
  assert.ok(homeLibsHidden && homeLibsHidden.innerHTML.includes('当前已隐藏全部已授权书库'));
  assert.ok(homeLibsHidden.innerHTML.includes('href="/account"'));
  assert.ok(!homeLibsHidden.innerHTML.includes('请联系管理员分配权限'));

  await hHidden.ctx.renderLibraries('');
  const pageContentHidden = hHidden.doc.getElementById('page-content');
  assert.ok(pageContentHidden && pageContentHidden.innerHTML.includes('当前已隐藏全部已授权书库'));
  assert.ok(pageContentHidden.innerHTML.includes('前往账号设置恢复显示'));
  assert.ok(!pageContentHidden.innerHTML.includes('请联系管理员分配权限'));

  // Case 2: zero authorized libraries and none hidden -> original empty guidance remains
  const hNone = createV33Harness({
    includeHomeAndLibraries: true,
    auth: {
      claimed: true,
      authenticated: true,
      role: 'user',
      userId: 'u-2',
      hasHiddenLibraries: false,
      authorizedLibrariesCount: 0,
    },
    libraries: [],
    apiHandler: emptyApi,
  });

  await hNone.ctx.renderHome();
  await new Promise((r) => setTimeout(r, 0));
  const homeLibsNone = hNone.doc.getElementById('home-libs');
  assert.ok(homeLibsNone && homeLibsNone.innerHTML.includes('分配书库访问权限'));
  assert.ok(!homeLibsNone.innerHTML.includes('当前已隐藏全部已授权书库'));
});

test('/admin?tab=settings removes show_nsfw checkbox and omits show_nsfw from PUT payload', async () => {
  const h = createV33Harness({
    includeAdminSettings: true,
    auth: { claimed: true, authenticated: true, role: 'admin', userId: 'u-admin' },
    apiHandler: async (url, opts) => {
      if (url === '/admin/api/settings' && !opts.method) {
        return {
          show_nsfw: false,
          api_rate_per_second: 5,
          downurl_ttl_seconds: 1200,
          page_prefetch_count: 6,
          volume_index_prefetch_enabled: true,
          volume_index_prefetch_remaining_pages: 4,
          range_cache_max_bytes: 512 * 1024 * 1024,
          page_cache_max_bytes: 1024 * 1024 * 1024,
          thumb_cache_max_bytes: 512 * 1024 * 1024,
        };
      }
      if (url === '/admin/api/cache/stats') return { range_bytes: 0, page_bytes: 0, thumb_bytes: 0 };
      if (url === '/admin/api/archive-passwords') return [];
      if (url === '/admin/api/settings' && opts.method === 'PUT') return { ok: true };
      return {};
    },
  });

  const container = h.doc.createElement('div');
  await h.ctx.renderAdminSettings(container);

  assert.ok(!container.innerHTML.includes('name="show_nsfw"'));
  assert.ok(container.innerHTML.includes('/admin?tab=users') && container.innerHTML.includes('/account'));

  const form = h.doc.getElementById('sys-settings-form');
  assert.ok(form);
  await form.dispatch('submit');

  const putCall = h.apiCalls.find((c) => c.url === '/admin/api/settings' && c.opts.method === 'PUT');
  assert.ok(putCall, 'expected PUT /admin/api/settings');
  assert.equal(Object.prototype.hasOwnProperty.call(putCall.opts.body, 'show_nsfw'), false);
});

test('renderHome uses new continue-reading aggregate endpoint with merge_bangumi and renders otherInProgressCount badge', async () => {
  const h = createV33Harness({
    includeHomeAndLibraries: true,
    storage: { fk115_merge_bangumi: '1' },
    libraries: [{ id: 'lib-1', name: '主库' }],
    apiHandler: async (url) => {
      if (url.startsWith('/admin/api/bookshelf/continue-reading')) {
        return {
          content: [
            {
              groupKey: 'bgm:998',
              mergedByBangumi: true,
              bgmSubjectId: '998',
              otherInProgressCount: 2,
              primary: {
                id: 'b-2',
                seriesId: 's-1',
                seriesTitle: '链锯人',
                name: 'Vol.02.cbz',
                number: 2,
                media: { pagesCount: 100 },
                readProgress: { page: 25, completed: false },
                metadata: { title: 'Vol.02' },
              },
              others: [],
            },
            {
              groupKey: 'series:s-2',
              mergedByBangumi: false,
              otherInProgressCount: 0,
              primary: {
                id: 'b-3',
                seriesId: 's-2',
                seriesTitle: '胆大党',
                name: 'Vol.01.cbz',
                number: 1,
                media: { pagesCount: 50 },
                readProgress: { page: 10, completed: false },
                metadata: { title: 'Vol.01' },
              },
              others: [],
            },
          ],
          totalElements: 2,
        };
      }
      if (url.startsWith('/api/v1/series')) return { content: [], totalElements: 0, totalPages: 0 };
      return {};
    },
  });

  await h.ctx.renderHome();
  await new Promise((r) => setTimeout(r, 0));

  const crCall = h.apiCalls.find((c) => c.url.startsWith('/admin/api/bookshelf/continue-reading'));
  assert.ok(crCall);
  assert.match(crCall.url, /merge_bangumi=1/);

  const homeContinue = h.doc.getElementById('home-continue');
  assert.equal(h.doc.getElementById('home-libs').innerHTML, '');
  assert.ok(!h.doc.getElementById('page-content').innerHTML.includes('browse-head'));
  assert.ok(!h.doc.getElementById('page-content').innerHTML.includes('lib-strip'));
  assert.ok(homeContinue.innerHTML.includes('另有 2 卷在读'));
  assert.ok(homeContinue.innerHTML.includes('?versions=1'));
  // Second item has otherInProgressCount=0 so count only appears once
  assert.equal(homeContinue.innerHTML.split('卷在读').length - 1, 1);
});

test('renderHistory uses aggregate history API, displays totalBooks, shows sub-source on cross-source merge, and syncs fk115_merge_bangumi toggle', async () => {
  const nowSec = Math.floor(Date.now() / 1000);
  const h = createV33Harness({
    includeHistory: true,
    storage: { fk115_merge_bangumi: '1' },
    libraries: [
      { id: 'lib-1', name: '115 主库' },
      { id: 'lib-2', name: 'WebDAV 备库' },
    ],
    apiHandler: async (url) => {
      if (url.startsWith('/admin/api/bookshelf/history')) {
        return {
          content: [
            {
              dayKey: '2026-11-26',
              groupKey: 'bgm:1234',
              mergedByBangumi: true,
              bgmSubjectId: '1234',
              seriesTitle: '葬送的芙莉莲',
              primary: {
                id: 'b-10',
                seriesId: 's-10',
                libraryId: 'lib-1',
                seriesTitle: '葬送的芙莉莲',
                name: 'Vol.05.cbz',
                number: 5,
                media: { pagesCount: 180 },
                readProgress: { page: 40, completed: false, readDate: nowSec },
              },
              others: [
                {
                  id: 'b-11',
                  seriesId: 's-11',
                  libraryId: 'lib-2',
                  seriesTitle: '葬送的芙莉莲 (单行本)',
                  name: 'Vol.01.cbz',
                  number: 1,
                  media: { pagesCount: 180 },
                  readProgress: { page: 180, completed: true, readDate: nowSec - 600 },
                },
                {
                  id: 'b-09',
                  seriesId: 's-10',
                  libraryId: 'lib-1',
                  seriesTitle: '葬送的芙莉莲',
                  name: 'Vol.04.cbz',
                  number: 4,
                  media: { pagesCount: 180 },
                  readProgress: { page: 180, completed: true, readDate: nowSec - 1200 },
                },
              ],
            },
          ],
          page: 0,
          size: 30,
          totalElements: 1,
          totalBooks: 42,
          totalPages: 1,
        };
      }
      return {};
    },
  });

  await h.ctx.renderHistory();

  const histCall1 = h.apiCalls.find((c) => c.url.startsWith('/admin/api/bookshelf/history'));
  assert.ok(histCall1);
  assert.match(histCall1.url, /size=30/);
  assert.match(histCall1.url, /tz_offset=/);
  assert.match(histCall1.url, /merge_bangumi=1/);

  const histContent = h.doc.getElementById('page-content').innerHTML;
  assert.ok(histContent.includes('共 42 卷记录'));
  assert.ok(histContent.includes('当天另有 2 卷'));
  // b-11 from lib-2 should show source badge, while b-09 from same s-10 & lib-1 should appear only once for lib-2
  assert.ok(histContent.includes('WebDAV 备库'));
  assert.ok(!histContent.includes('<span class="history-sub-source" title="115 主库">'));

  const mergeCb = h.doc.getElementById('history-merge-bgm');
  assert.ok(mergeCb && mergeCb.checked === true);
  mergeCb.checked = false;
  await mergeCb.dispatch('change');

  assert.equal(h.storage.getItem('fk115_merge_bangumi'), '0');
  const histCall2 = h.apiCalls.filter((c) => c.url.startsWith('/admin/api/bookshelf/history')).pop();
  assert.match(histCall2.url, /merge_bangumi=0/);
});

test('route() clears stale DOM, libraries, taxonomies, and displayPrefs immediately on user switch before ensureDisplayPrefs resolves', async () => {
  let seenDuringEnsure = null;
  const h = createV33Harness({
    includeRouter: true,
    pathname: '/',
    auth: { claimed: true, authenticated: true, role: 'user', userId: 'u-old', username: 'olduser' },
    libraries: [{ id: 'lib-old', name: 'Old Lib' }],
    apiHandler: async (url) => {
      if (url === '/admin/api/auth/status') {
        return { claimed: true, authenticated: true, role: 'user', userId: 'u-new', username: 'newuser' };
      }
      return {};
    },
    onEnsureDisplayPrefs: async ({ appEl, state }) => {
      seenDuringEnsure = {
        appHtml: appEl.innerHTML,
        librariesLen: state.libraries.length,
        taxonomiesLoaded: state.taxonomies.loaded,
      };
    },
  });

  h.appEl.innerHTML = '<div id="old-user-banner">old secret shelf</div>';
  h.state.taxonomies.loaded = true;
  h.ctx.renderHome = async () => { h.appEl.innerHTML = '<div id="new-home">new home</div>'; };

  await h.ctx.route();

  assert.deepEqual(seenDuringEnsure, {
    appHtml: '',
    librariesLen: 0,
    taxonomiesLoaded: false,
  });
  assert.equal(h.getResetDisplayPrefsCalls(), 1);
  assert.equal(h.getEnsureDisplayPrefsCalls(), 1);
  assert.equal(h.appEl.innerHTML, '<div id="new-home">new home</div>');
});

test('route() preserves #app DOM when same userId reloads route', async () => {
  let seenAppHtml = null;
  const h = createV33Harness({
    includeRouter: true,
    pathname: '/collections',
    auth: { claimed: true, authenticated: true, role: 'user', userId: 'u-1', username: 'alice' },
    libraries: [{ id: 'lib-1', name: 'Main Lib' }],
    apiHandler: async (url) => {
      if (url === '/admin/api/auth/status') {
        return { claimed: true, authenticated: true, role: 'user', userId: 'u-1', username: 'alice' };
      }
      return {};
    },
    onEnsureDisplayPrefs: async ({ appEl }) => {
      seenAppHtml = appEl.innerHTML;
    },
  });

  h.appEl.innerHTML = '<div class="keep">existing</div>';
  await h.ctx.route();

  assert.equal(seenAppHtml, '<div class="keep">existing</div>');
  assert.equal(h.getResetDisplayPrefsCalls(), 0);
  assert.equal(h.state.libraries.length, 1);
});

test('reader auto-webtoon page sampling helpers exclude covers/tiny/wide pages and trigger at >=2x(2.5) or >=1x(3.5)', () => {
  const fnStart = bridgeSource.indexOf('export function shouldRecordAutoWebtoonSample');
  const fnEnd = bridgeSource.indexOf('interface KomgaBookDTO');
  assert.ok(fnStart >= 0 && fnEnd > fnStart);
  const rawSlice = bridgeSource.slice(fnStart, fnEnd)
    .replace(/export function /g, 'function ')
    .replace('(sample: AutoWebtoonPageSample): number | null', '(sample)')
    .replace('(sampledRatios: ReadonlyMap<number, number>): boolean', '(sampledRatios)');
  const readerCtx = { Map, Number };
  vm.createContext(readerCtx);
  vm.runInContext(rawSlice, readerCtx);

  const { shouldRecordAutoWebtoonSample, isAutoWebtoonRatioTriggered } = readerCtx;

  // Cover page (index 0) with totalPages >= 4 and ratio < 2.2 is skipped
  assert.equal(shouldRecordAutoWebtoonSample({ pageIndex: 0, naturalWidth: 800, naturalHeight: 1400, totalPages: 20 }), null);
  // Short gallery (<4 pages) allows index 0
  assert.equal(shouldRecordAutoWebtoonSample({ pageIndex: 0, naturalWidth: 800, naturalHeight: 2100, totalPages: 3 }), 2.625);
  // Cover with extreme strip ratio >= 2.2 still counts
  assert.equal(shouldRecordAutoWebtoonSample({ pageIndex: 0, naturalWidth: 800, naturalHeight: 2800, totalPages: 20 }), 3.5);
  // Tiny decoration < 200px wide or wide spread (ratio < 1) or pageIndex >= 5 are skipped
  assert.equal(shouldRecordAutoWebtoonSample({ pageIndex: 1, naturalWidth: 150, naturalHeight: 900, totalPages: 20 }), null);
  assert.equal(shouldRecordAutoWebtoonSample({ pageIndex: 1, naturalWidth: 1600, naturalHeight: 1100, totalPages: 20 }), null);
  assert.equal(shouldRecordAutoWebtoonSample({ pageIndex: 5, naturalWidth: 800, naturalHeight: 3200, totalPages: 20 }), null);

  // Ratio trigger rules
  const singleMedium = new Map([[1, 2.7]]);
  assert.equal(isAutoWebtoonRatioTriggered(singleMedium), false);

  const twoMedium = new Map([[1, 2.5], [2, 2.6]]);
  assert.equal(isAutoWebtoonRatioTriggered(twoMedium), true);

  const oneExtreme = new Map([[3, 3.5]]);
  assert.equal(isAutoWebtoonRatioTriggered(oneExtreme), true);
});

test('reader apply-host.py and KomgaHostBridge.ts restore shouldUseAutoWebtoonMode=true, narrow tags, and wire readingDirection + memory', () => {
  // Default shouldUseAutoWebtoonMode is NOT forced to false
  assert.ok(!applyHostSource.includes('shouldUseAutoWebtoonMode: false'));

  // Narrow MANGA_TAGS_BY_MANGA_TYPE replacement block excludes generic regional words
  const firstTbl = applyHostSource.indexOf('export const MANGA_TAGS_BY_MANGA_TYPE');
  const secondTbl = applyHostSource.indexOf('export const MANGA_TAGS_BY_MANGA_TYPE', firstTbl + 1);
  assert.ok(secondTbl > firstTbl, 'expected replacement MANGA_TAGS_BY_MANGA_TYPE table');
  const webtoonStart = applyHostSource.indexOf('[MangaType.WEBTOON]:', secondTbl);
  const tblEnd = applyHostSource.indexOf('};', webtoonStart);
  const tagsSection = applyHostSource.slice(webtoonStart, tblEnd);
  for (const forbidden of ["'韩漫'", "'国漫'", "'Manhwa'", "'Manhua'", "'만화'", "'漫画'"]) {
    assert.ok(!tagsSection.includes(forbidden), `generic tag ${forbidden} must not force webtoon`);
  }
  for (const required of ["'条漫'", "'长图'", "'Long Strip'", "'Webtoon'", "'웹툰'", "'Vertical'"]) {
    assert.ok(tagsSection.includes(required), `explicit strip tag ${required} missing`);
  }

  // Host bridge & reader wiring checks
  assert.ok(bridgeSource.includes('tags?: string[]'));
  assert.ok(bridgeSource.includes('readingDirectionLock?: boolean'));
  assert.ok(bridgeSource.includes('forceWebtoon'));
  assert.ok(bridgeSource.includes('suppressAutoWebtoon'));
  assert.ok(bridgeSource.includes('fk115_autoWebtoonDetected'));
  assert.ok(bridgeSource.includes('items'));
  assert.ok(applyHostSource.includes('markAutoWebtoonDetected'));
  assert.ok(applyHostSource.includes('shouldRecordAutoWebtoonSample'));
  assert.ok(applyHostSource.includes('isAutoWebtoonRatioTriggered'));
});

test('app.css new v3.3.0 UI classes use theme CSS variables without hardcoded colors', () => {
  const selectors = [
    '.continue-badges',
    '.acc-lib-filter-box',
    '.acc-lib-item',
    '.history-toolbar .history-merge-toggle',
    '.history-sub-meta',
    '.history-sub-source',
  ];
  for (const sel of selectors) {
    const start = cssSource.indexOf(sel);
    assert.ok(start >= 0, `expected ${sel} in app.css`);
    const closeBrace = cssSource.indexOf('}', start);
    const block = cssSource.slice(start, closeBrace + 1);
    assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(block), `hardcoded hex color in ${sel}: ${block}`);
    assert.ok(!/\brgba?\(/.test(block), `hardcoded rgb/rgba color in ${sel}: ${block}`);
  }
});
