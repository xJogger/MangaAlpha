const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const ROOT = path.resolve(__dirname, '..');
const APP_JS_PATH = path.join(ROOT, 'internal/httpserver/static/ui/app.js');
const APP_CSS_PATH = path.join(ROOT, 'internal/httpserver/static/ui/app.css');

const appSource = fs.readFileSync(APP_JS_PATH, 'utf8');
const cssSource = fs.readFileSync(APP_CSS_PATH, 'utf8');

function extractSlice(startMarker, endMarker, source = appSource) {
  const start = source.indexOf(startMarker);
  if (start < 0) throw new Error(`missing start marker: ${startMarker}`);
  const end = source.indexOf(endMarker, start + startMarker.length);
  if (end < 0) throw new Error(`missing end marker: ${endMarker}`);
  return source.slice(start, end);
}

function decodeHTML(str) {
  return String(str)
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&amp;/g, '&');
}

function parseTagAttrs(raw) {
  const attrs = {};
  const attrRe = /([a-zA-Z_:][\w:.-]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>/]+)))?/g;
  let m;
  while ((m = attrRe.exec(raw)) !== null) {
    const key = m[1].toLowerCase();
    const val = m[2] !== undefined ? m[2] : (m[3] !== undefined ? m[3] : (m[4] !== undefined ? m[4] : ''));
    attrs[key] = decodeHTML(val);
  }
  return attrs;
}

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

function createFakeDOM() {
  const registry = new Map();
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
      open: Object.prototype.hasOwnProperty.call(attrMap, 'open'),
      value: attrMap.value !== undefined ? attrMap.value : (tag.toLowerCase() === 'input' && (attrMap.type || '').toLowerCase() === 'checkbox' ? 'on' : ''),
      name: attrMap.name || '',
      type: attrMap.type || '',
      textContent: '',
      title: attrMap.title || '',
      href: attrMap.href || '',
      src: attrMap.src || '',
      files: [],
      isConnected: true,
      _options: [],
      _hasSelectedOption: false,
      _listeners: {},
      _attrMap: attrMap,
      _instanceId: ++autoId,
      get id() {
        return attrMap.id || '';
      },
      set id(v) {
        attrMap.id = String(v || '');
        if (attrMap.id) registry.set(attrMap.id, el);
      },
      get innerHTML() {
        return html;
      },
      set innerHTML(val) {
        const markDisconnected = (nodes) => {
          for (const old of nodes) {
            old.isConnected = false;
            if (old.id && registry.get(old.id) === old) registry.delete(old.id);
            if (old.children && old.children.length > 0) markDisconnected(old.children);
          }
        };
        markDisconnected(childNodes);
        html = String(val || '');
        childNodes = [];
        const voidTags = new Set(['input', 'img', 'br', 'hr', 'meta', 'link']);
        const openStack = [{ el, tag: el.tagName.toLowerCase(), children: childNodes }];
        const tagRe = /<\/?([a-zA-Z0-9-]+)([^>]*)>/g;
        let m;
        while ((m = tagRe.exec(html)) !== null) {
          const full = m[0];
          const childTag = m[1].toLowerCase();
          const isClose = full.startsWith('</');
          if (isClose) {
            for (let i = openStack.length - 1; i >= 1; i -= 1) {
              if (openStack[i].tag === childTag) {
                const popped = openStack[i];
                openStack.length = i;
                if (childTag === 'select') {
                  const sel = popped.el;
                  if (sel._options.length > 0 && !sel._hasSelectedOption) {
                    sel.value = sel._options[0].value;
                  }
                }
                break;
              }
            }
            continue;
          }
          const childAttrs = parseTagAttrs(m[2] || '');
          if (childTag === 'option') {
            const optVal = Object.prototype.hasOwnProperty.call(childAttrs, 'value') ? childAttrs.value : '';
            const isSel = Object.prototype.hasOwnProperty.call(childAttrs, 'selected');
            for (let i = openStack.length - 1; i >= 1; i -= 1) {
              if (openStack[i].tag === 'select') {
                const sel = openStack[i].el;
                sel._options.push({ value: optVal, selected: isSel });
                if (isSel) {
                  sel.value = optVal;
                  sel._hasSelectedOption = true;
                }
                break;
              }
            }
            continue;
          }
          const child = makeElement(childTag, childAttrs);
          const parentEntry = openStack[openStack.length - 1];
          parentEntry.children.push(child);
          if (childAttrs.id) {
            registry.set(childAttrs.id, child);
          }
          const selfClosing = voidTags.has(childTag) || /\/>\s*$/.test(full);
          if (!selfClosing) {
            const inner = extractTagInnerHTML(html, m.index + full.length, childTag);
            child._setParsedInnerHTML(inner);
            if (childTag === 'textarea') {
              child.value = decodeHTML(inner);
            }
            openStack.push({ el: child, tag: childTag, children: child._rawChildren() });
          }
        }
      },
      _rawChildren() {
        return childNodes;
      },
      _setParsedInnerHTML(inner) {
        html = String(inner || '');
        el.textContent = decodeHTML(html.replace(/<[^>]*>/g, ''));
      },
      get children() {
        return childNodes;
      },
      get firstElementChild() {
        return childNodes[0] || null;
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
        if (lk === 'src') el.src = String(v);
      },
      getAttribute(k) {
        const lk = String(k).toLowerCase();
        return Object.prototype.hasOwnProperty.call(attrMap, lk) ? attrMap[lk] : null;
      },
      hasAttribute(k) {
        return Object.prototype.hasOwnProperty.call(attrMap, String(k).toLowerCase());
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
        return queryNodes(childNodes, selector);
      },
      querySelector(selector) {
        return this.querySelectorAll(selector)[0] || null;
      },
      closest() {
        return null;
      },
      contains() {
        return false;
      },
      scrollIntoView() {},
      focus() {},
      blur() {},
      remove() {
        el.isConnected = false;
      },
    };
    if (attrs.id) registry.set(attrs.id, el);
    return el;
  }

  function matchSingleSelector(el, rawSel) {
    const s = String(rawSel || '').trim();
    if (!s) return false;
    if (s.startsWith('#')) return el.getAttribute('id') === s.slice(1);
    if (s.startsWith('.')) return el.classList.contains(s.slice(1));
    const tagAttr = s.match(/^([a-zA-Z0-9-]+)?\[([^\]]+)\]$/);
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

  function matchesSelector(el, selector) {
    return String(selector || '')
      .split(',')
      .map((part) => part.trim())
      .filter(Boolean)
      .some((part) => matchSingleSelector(el, part));
  }

  function collectSubtree(nodes, out = []) {
    for (const node of nodes) {
      out.push(node);
      if (node.children && node.children.length > 0) {
        collectSubtree(node.children, out);
      }
    }
    return out;
  }

  function queryNodes(roots, selector) {
    const out = [];
    const seen = new Set();
    for (const node of collectSubtree(roots)) {
      if (matchesSelector(node, selector) && !seen.has(node._instanceId)) {
        seen.add(node._instanceId);
        out.push(node);
      }
    }
    return out;
  }

  const appEl = makeElement('main', { id: 'app' });
  appEl.innerHTML = '<main class="page-shell" id="page-content"></main>';
  const modalRoot = makeElement('div', { id: 'modal-root' });
  const confirmRoot = makeElement('div', { id: 'confirm-root' });
  const doc = {
    body: makeElement('body'),
    activeElement: null,
    getElementById(id) {
      if (id === 'app') return appEl;
      if (id === 'modal-root') return modalRoot;
      if (id === 'confirm-root') return confirmRoot;
      return registry.get(id) || null;
    },
    querySelectorAll(selector) {
      return queryNodes([appEl, modalRoot, confirmRoot], selector);
    },
    querySelector(selector) {
      return this.querySelectorAll(selector)[0] || null;
    },
    createElement(tag) {
      return makeElement(tag);
    },
  };

  return { doc, appEl, modalRoot, confirmRoot, makeElement };
}

function makeStorage(initial = {}) {
  const map = new Map(Object.entries(initial));
  return {
    _map: map,
    getItem(k) {
      return map.has(k) ? String(map.get(k)) : null;
    },
    setItem(k, v) {
      map.set(String(k), String(v));
    },
    removeItem(k) {
      map.delete(String(k));
    },
    keys() {
      return Array.from(map.keys());
    },
  };
}

class FakeFormData {
  constructor() {
    this._entries = [];
  }
  append(k, v) {
    this._entries.push([String(k), v]);
  }
  get(k) {
    const hit = this._entries.find(([name]) => name === k);
    return hit ? hit[1] : null;
  }
  entries() {
    return this._entries.slice();
  }
}

function createV34Harness(options = {}) {
  const { doc, appEl, modalRoot } = createFakeDOM();
  const storage = makeStorage(options.storage || {});
  const apiCalls = [];
  const xhrCalls = [];
  const toasts = [];
  const navigated = [];
  const observers = [];
  let currentPath = options.pathname || '/';
  let currentSearch = options.search || '';
  let pollScheduled = null;
  let stopPollCalls = 0;

  class FakeIntersectionObserver {
    constructor(cb, opts) {
      this.cb = cb;
      this.opts = opts;
      this.observed = [];
      this.disconnected = false;
      observers.push(this);
    }
    observe(target) {
      this.observed.push(target);
    }
    unobserve(target) {
      this.observed = this.observed.filter((t) => t !== target);
    }
    disconnect() {
      this.disconnected = true;
    }
    trigger(entries) {
      if (!this.disconnected) this.cb(entries);
    }
  }

  class FakeXMLHttpRequest {
    constructor() {
      this.headers = {};
      this.withCredentials = false;
      this.status = 200;
      this.responseText = '';
      this.upload = {};
      xhrCalls.push(this);
    }
    open(method, url, asyncFlag = true) {
      this.method = method;
      this.url = url;
      this.asyncFlag = asyncFlag;
    }
    setRequestHeader(k, v) {
      this.headers[k] = v;
    }
    send(body) {
      this.body = body;
      if (options.xhrHandler) {
        options.xhrHandler(this);
      } else {
        this.status = 200;
        this.responseText = JSON.stringify({ id: 'src-uploaded', name: 'Uploaded Source', schemaFormat: 'komga_sqlite_v1' });
        if (typeof this.onload === 'function') this.onload();
      }
    }
  }

  const prefsSlice = extractSlice('  const DISPLAY_PREFS = {', '  async function copyTextSafeGlobal(');
  const recoverySlice = extractSlice('  function recoverUserContext()', '  let modalScroll = null;');
  const xhrSlice = extractSlice('  function xhrUploadFormData(', '  function navigate(target, replace = false)');
  const accountSlice = extractSlice('  // --- Personal Account Settings Page (/account) ---', '  // --- AI Agent One-Click Authorization Page (/agent-auth/:requestID) ---');
  const seriesDetailSlice = extractSlice('  // --- Series Detail View (/series/:id) ---', '  // --- Metadata Edit Modal ---');
  const bookDetailSlice = extractSlice('  // --- Book Detail View (/book/:id & /books/:id) ---', '  // --- Admin Console (/admin) ---');
  const adminLibsSlice = extractSlice('  // --- Admin Libraries Tab ---', '  async function browse115Drawer(');
  const scraperSlice = extractSlice('  // --- Admin Bangumi Scraper Center ---', '  // --- Admin Jobs Tab (Adaptive Polling ONLY when active!) ---');
  const adminJobsSlice = extractSlice('  // --- Admin Jobs Tab (Adaptive Polling ONLY when active!) ---', '  // --- Admin Cache & Settings Tab ---');
  const routerSlice = extractSlice('  // --- Router ---', '  route();\n})();');

  let currentRenderTarget = null;

  const ctx = {
    console,
    Date,
    Math,
    JSON,
    Number,
    String,
    Boolean,
    Array,
    Object,
    Map,
    Set,
    Promise,
    Error,
    AbortController,
    URLSearchParams,
    FormData: FakeFormData,
    XMLHttpRequest: options.disableXHR ? undefined : FakeXMLHttpRequest,
    IntersectionObserver: options.disableObserver ? undefined : FakeIntersectionObserver,
    navigator: { userAgent: 'Mozilla/5.0' },
    CSS: { escape: (s) => String(s).replace(/([^a-zA-Z0-9_-])/g, '\\$1') },
    THEME_MODE_KEY: 'fk115_theme_mode',
    WEEKDAY_NAMES: ['周日', '周一', '周二', '周三', '周四', '周五', '周六'],
    document: doc,
    localStorage: storage,
    setTimeout,
    clearTimeout,
    location: {
      get pathname() { return currentPath; },
      get search() { return currentSearch; },
      get origin() { return 'http://127.0.0.1:25600'; },
      get href() { return `http://127.0.0.1:25600${currentPath}${currentSearch}`; },
    },
    history: {
      replaceState(_s, _t, next) {
        const u = new URL(String(next), 'http://127.0.0.1:25600');
        currentPath = u.pathname;
        currentSearch = u.search;
      },
      pushState(_s, _t, next) {
        const u = new URL(String(next), 'http://127.0.0.1:25600');
        currentPath = u.pathname;
        currentSearch = u.search;
      },
    },
    window: {
      location: {
        get pathname() { return currentPath; },
        get search() { return currentSearch; },
        get origin() { return 'http://127.0.0.1:25600'; },
      },
      history: {
        replaceState(_s, _t, next) {
          const u = new URL(String(next), 'http://127.0.0.1:25600');
          currentPath = u.pathname;
          currentSearch = u.search;
        },
        pushState(_s, _t, next) {
          const u = new URL(String(next), 'http://127.0.0.1:25600');
          currentPath = u.pathname;
          currentSearch = u.search;
        },
      },
      scrollTo() {},
    },
    esc: (v) => (v == null ? '' : String(v)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;')),
    fmtBytes: (n) => {
      const v = Number(n || 0);
      if (!v || v < 1024) return `${v || 0} B`;
      const units = ['KB', 'MB', 'GB', 'TB'];
      let size = v;
      for (const u of units) {
        size /= 1024;
        if (size < 1024 || u === 'TB') return `${size.toFixed(1)} ${u}`;
      }
      return `${v} B`;
    },
    fmtTime: (t) => String(t || '—'),
    fmtBgmCollectionType: () => '',
    renderSeriesTrackerBadge: () => '',
    providerLabel: (pt, acc) => (pt === 'webdav' && acc ? `WebDAV · ${acc}` : (pt === 'local' ? '本地 / SMB' : (pt === 'union' ? '联合库' : '115 网盘'))),
    formatScheduleSummary: () => '',
    safeRedirectTarget: (p) => (String(p || '').startsWith('/') ? String(p) : '/'),
    toast: (msg, kind = 'ok') => { toasts.push({ msg, kind }); },
    confirmModal: async () => (options.confirmResult !== undefined ? options.confirmResult : true),
    closeConfirmModal: () => {},
    openModal: ({ title, subheadHTML = '', bodyHTML = '', footHTML = '', onMount }) => {
      modalRoot.innerHTML = `<div class="modal-backdrop" id="modal-backdrop"><div class="modal-dialog"><div class="modal-head"><h3>${ctx.esc(title)}</h3></div>${subheadHTML ? `<div class="modal-subhead" id="modal-subhead">${subheadHTML}</div>` : ''}<div class="modal-body" id="modal-body">${bodyHTML}</div>${footHTML ? `<div class="modal-foot">${footHTML}</div>` : ''}</div></div>`;
      if (onMount) onMount(doc.getElementById('modal-body'));
    },
    closeModal: () => {
      modalRoot.innerHTML = '';
    },
    renderShell: (_active, html) => {
      appEl.innerHTML = `<main class="page-shell" id="page-content">${html}</main>`;
    },
    schedulePoll: (fn, ms) => { pollScheduled = { fn, ms }; ctx.state.pollFn = fn; },
    stopPolling: () => { pollScheduled = null; ctx.state.pollFn = null; stopPollCalls += 1; },
    updateFooterVersion: () => {},
    loadSeriesVersions: () => {},
    loadSeriesCollectionsBar: () => {},
    loadBookReadListsBar: () => {},
    renderMaintJobRows: () => '',
    bindMaintCancelButtons: () => {},
    openAddSeriesToCollectionModal: () => {},
    openAddBookToReadListModal: () => {},
    openMetadataEditModal: () => {},
    browse115Drawer: () => {},
    browseLocalDrawer: () => {},
    browseWebDAVDrawer: () => {},
    renderSetup: () => { currentRenderTarget = 'setup'; },
    renderLogin: () => { currentRenderTarget = 'login'; },
    renderHome: async () => { currentRenderTarget = 'home'; },
    renderLibraries: async (lid) => { currentRenderTarget = `libraries:${lid}`; },
    renderCollections: async () => { currentRenderTarget = 'collections'; },
    renderCollectionDetail: async (cid) => { currentRenderTarget = `collection:${cid}`; },
    renderReadLists: async () => { currentRenderTarget = 'readlists'; },
    renderReadListDetail: async (rid) => { currentRenderTarget = `readlist:${rid}`; },
    renderHistory: async () => { currentRenderTarget = 'history'; },
    renderAgentAuthPage: async () => { currentRenderTarget = 'agent-auth'; },
    renderAdmin: async () => { currentRenderTarget = 'admin'; },
    navigate: (target) => {
      navigated.push(String(target));
      const u = new URL(String(target), 'http://127.0.0.1:25600');
      currentPath = u.pathname;
      currentSearch = u.search;
    },
    routeSeq: 1,
    api: async (url, opts = {}) => {
      apiCalls.push({ url, method: (opts.method || 'GET').toUpperCase(), opts });
      if (options.apiHandler) {
        return options.apiHandler(url, opts, { ctx, apiCalls });
      }
      return {};
    },
  };
  if (options.disableObserver) delete ctx.IntersectionObserver;
  if (options.disableXHR) delete ctx.XMLHttpRequest;

  vm.createContext(ctx);
  vm.runInContext(prefsSlice + '\n' + recoverySlice + '\n' + xhrSlice + '\nthis.state = state; this.DISPLAY_PREFS = DISPLAY_PREFS;', ctx);

  if (options.auth) {
    Object.assign(ctx.state.auth, options.auth);
  } else {
    Object.assign(ctx.state.auth, {
      claimed: true,
      authenticated: true,
      userId: 'u-admin',
      username: 'admin',
      role: 'admin',
      isAdmin: true,
    });
  }

  if (options.includeAccount) vm.runInContext(accountSlice, ctx);
  if (options.includeSeriesDetail) vm.runInContext(seriesDetailSlice, ctx);
  else vm.runInContext(extractSlice('  function currentUserId()', '  function isSeriesPath('), ctx);
  if (options.includeBookDetail) vm.runInContext(bookDetailSlice, ctx);
  if (options.includeAdminLibraries) vm.runInContext(adminLibsSlice, ctx);
  if (options.includeScraper) vm.runInContext(scraperSlice, ctx);
  if (options.includeAdminJobs) vm.runInContext(adminJobsSlice, ctx);
  if (options.includeRouter) {
    vm.runInContext(routerSlice.replace('  let routeSeq = 0;', '  var routeSeq = 0;'), ctx);
  }

  return {
    ctx,
    doc,
    appEl,
    modalRoot,
    state: ctx.state,
    storage,
    apiCalls,
    xhrCalls,
    toasts,
    navigated,
    observers,
    getPollScheduled: () => pollScheduled,
    getStopPollCalls: () => stopPollCalls,
    getCurrentRenderTarget: () => currentRenderTarget,
    getCurrentPath: () => currentPath,
    getCurrentSearch: () => currentSearch,
    setLocation: (p, s = '') => { currentPath = p; currentSearch = s; },
  };
}

function flushMicrotasks() {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

function makeSeriesFixture(seriesId, count) {
  return {
    series: { id: seriesId, booksCount: count, booksReadCount: 0, metadata: { title: seriesId } },
    originalName: seriesId,
    books: Array.from({ length: count }, (_, i) => ({
      id: seriesId + '-b' + (i + 1),
      name: 'Volume ' + (i + 1),
      metadata: { number: String(i + 1) },
    })),
  };
}

// ---------------------------------------------------------------------------
// 1. DISPLAY_PREFS.seriesBooksView & /account rendering
// ---------------------------------------------------------------------------
test('DISPLAY_PREFS seriesBooksView normalizes, serializes, persists, and renders in /account', async () => {
  const h = createV34Harness({
    includeAccount: true,
    auth: { claimed: true, authenticated: true, userId: 'u-1', username: 'alice', role: 'user', isAdmin: false },
    apiHandler: async (url, opts) => {
      if (url === '/api/v1/client-settings/user/list') {
        return {
          'webui.series.booksView': { value: 'cover', allowUnauthenticated: false },
        };
      }
      if (url === '/api/v1/client-settings/user' && opts.method === 'PATCH') {
        return null;
      }
      if (url === '/admin/api/me') {
        return {
          user: { id: 'u-1', username: 'alice', role: 'user', allowNsfw: false, showNsfw: false },
          authorizedLibraries: [],
          hiddenLibraryIds: [],
          visibleLibraries: [],
        };
      }
      if (url === '/admin/api/auth/api-keys' || url === '/admin/api/auth/sessions') return [];
      if (url.startsWith('/admin/api/bangumi/tracker')) return null;
      return {};
    },
  });

  // Validation & normalization
  const spec = h.ctx.DISPLAY_PREFS.seriesBooksView;
  assert.equal(spec.key, 'webui.series.booksView');
  assert.equal(h.ctx.normalizeDisplayPref(spec, 'COVER'), 'cover');
  assert.equal(h.ctx.normalizeDisplayPref(spec, 'invalid-val'), 'list');
  assert.equal(h.ctx.serializeDisplayPref(spec, 'cover'), 'cover');

  await h.ctx.ensureDisplayPrefs();
  assert.equal(h.ctx.displayPref('seriesBooksView'), 'cover');
  assert.equal(h.storage.getItem('fk115_display.u-1.seriesBooksView'), 'cover');

  await h.ctx.renderAccountPage();
  const select = h.doc.getElementById('disp-pref-seriesBooksView');
  assert.ok(select, 'expected #disp-pref-seriesBooksView in /account display preferences card');
  assert.equal(select.value, 'cover');
  assert.match(h.doc.getElementById('page-content').innerHTML, /系列默认卷视图/);
  assert.match(h.doc.getElementById('page-content').innerHTML, /列表模式/);
  assert.match(h.doc.getElementById('page-content').innerHTML, /封面模式/);

  select.value = 'list';
  await select.dispatch('change');
  const patchCall = h.apiCalls.find((c) => c.url === '/api/v1/client-settings/user' && c.method === 'PATCH');
  assert.ok(patchCall);
  assert.deepEqual(JSON.parse(JSON.stringify(patchCall.opts.body)), {
    'webui.series.booksView': { value: 'list' },
  });
  assert.equal(h.ctx.displayPref('seriesBooksView'), 'list');

  // resetDisplayPrefs clears override state too
  h.state.seriesBooksViewOverride = { seriesId: 's-1', mode: 'cover' };
  h.ctx.resetDisplayPrefs();
  assert.deepEqual(JSON.parse(JSON.stringify(h.state.seriesBooksViewOverride)), { seriesId: '', mode: '' });
  assert.equal(h.ctx.displayPref('seriesBooksView'), 'list');
});

// ---------------------------------------------------------------------------
// 2. Temporary series books view lifecycle across routes (zero persistence)
// ---------------------------------------------------------------------------
test('series temporary view override survives /series/A -> /book/b1 -> /series/A, resets on /series/B or other routes, and never writes client-settings/localStorage/query', async () => {
  const makeSeriesDetailPayload = (sid) => ({
    series: {
      id: sid,
      oneshot: false,
      booksCount: 2,
      booksReadCount: 0,
      booksInProgressCount: 0,
      metadata: { title: `系列 ${sid}`, status: 'ONGOING', genres: [], tags: [], alternateTitles: [] },
    },
    rawMeta: { coverSource: 'auto', authors: [] },
    originalName: `系列 ${sid}`,
    relativePath: `系列 ${sid}`,
    totalSize: '20 MB',
    library: { id: 'lib-1', name: '主库', providerType: '115', oneshotsDirectory: '' },
    books: [
      { id: `${sid}-b1`, name: 'Vol.01.cbz', size: '10 MB', number: 1, media: { pagesCount: 20 }, metadata: { title: '第 1 卷', number: '1' } },
      { id: `${sid}-b2`, name: 'Vol.02.cbz', size: '10 MB', number: 2, media: { pagesCount: 20 }, metadata: { title: '第 2 卷', number: '2' } },
    ],
    bookExtras: {},
  });

  const makeBookDetailPayload = (bid, sid) => ({
    book: {
      id: bid,
      name: 'Vol.01.cbz',
      size: '10 MB',
      media: { pagesCount: 20 },
      metadata: { title: '第 1 卷', number: '1', numberSort: 1, authors: [], tags: [], links: [] },
    },
    rawBookMeta: { coverSource: 'auto', comicInfoStatus: '' },
    seriesId: sid,
    seriesTitle: `系列 ${sid}`,
    originalSeries: `系列 ${sid}`,
    libraryId: 'lib-1',
    libraryName: '主库',
    providerType: '115',
    archiveType: 'CBZ',
    relativePath: `系列 ${sid}/Vol.01.cbz`,
    indexStatus: '20 页（已建索引）',
    downloadSpeed: '—',
  });

  const h = createV34Harness({
    includeSeriesDetail: true,
    includeBookDetail: true,
    includeRouter: true,
    pathname: '/series/A',
    apiHandler: async (url) => {
      if (url === '/admin/api/auth/status') {
        return { claimed: true, authenticated: true, userId: 'u-admin', username: 'admin', role: 'admin', isAdmin: true };
      }
      if (url === '/api/v1/client-settings/user/list') {
        return { 'webui.series.booksView': { value: 'list' } };
      }
      if (url === '/admin/api/series/A/detail') return makeSeriesDetailPayload('A');
      if (url === '/admin/api/series/B/detail') return makeSeriesDetailPayload('B');
      if (url === '/admin/api/books/A-b1/detail') return makeBookDetailPayload('A-b1', 'A');
      if (url.startsWith('/admin/api/maintenance-jobs')) return [];
      return {};
    },
  });

  // 1. Enter /series/A -> starts in global default 'list'
  await h.ctx.route();
  assert.ok(h.doc.getElementById('series-books-body').querySelector('.volume-list'), 'should start in list view');

  // 2. Toggle temporarily to 'cover' -> zero API / localStorage / query changes, only #series-books-body updates
  const callsBeforeToggle = h.apiCalls.length;
  const lsBeforeToggle = h.storage.getItem('fk115_display.u-admin.seriesBooksView');
  const coverBtn = h.doc.getElementById('page-content').querySelector('button[data-books-view="cover"]');
  assert.ok(coverBtn);
  coverBtn.onclick();
  assert.equal(h.apiCalls.length, callsBeforeToggle, 'temporary toggle must make zero network requests');
  assert.equal(h.getCurrentSearch(), '', 'temporary toggle must not mutate URL query');
  assert.equal(h.storage.getItem('fk115_display.u-admin.seriesBooksView'), lsBeforeToggle, 'temporary toggle must not overwrite localStorage');
  assert.ok(h.doc.getElementById('series-books-body').querySelector('.series-books-cover-grid'));

  // 3. Navigate to /book/A-b1 -> override stays intact
  h.setLocation('/book/A-b1');
  await h.ctx.route();
  assert.deepEqual(JSON.parse(JSON.stringify(h.state.seriesBooksViewOverride)), { seriesId: 'A', mode: 'cover' });

  // 4. Navigate back to /series/A -> retains temporary 'cover' mode
  h.setLocation('/series/A');
  await h.ctx.route();
  assert.ok(h.doc.getElementById('series-books-body').querySelector('.series-books-cover-grid'), 'returning to /series/A must preserve temporary cover mode');

  // 5. Navigate to /series/B -> clears override and falls back to global 'list'
  h.setLocation('/series/B');
  await h.ctx.route();
  assert.deepEqual(JSON.parse(JSON.stringify(h.state.seriesBooksViewOverride)), { seriesId: '', mode: '' });
  assert.ok(h.doc.getElementById('series-books-body').querySelector('.volume-list'), '/series/B must fall back to global default list view');

  // 6. Toggle cover on B, then visit /libraries -> clears override
  h.doc.getElementById('page-content').querySelector('button[data-books-view="cover"]').onclick();
  assert.equal(h.state.seriesBooksViewOverride.seriesId, 'B');
  h.setLocation('/libraries');
  await h.ctx.route();
  assert.deepEqual(JSON.parse(JSON.stringify(h.state.seriesBooksViewOverride)), { seriesId: '', mode: '' });

  // Verify zero PATCH /api/v1/client-settings/user calls happened anywhere
  assert.equal(
    h.apiCalls.filter((c) => c.url === '/api/v1/client-settings/user').length,
    0,
  );
});

// ---------------------------------------------------------------------------
// 3. Cover mode 60-volume batching, IntersectionObserver lazy loading & bounded batch read/unread
// ---------------------------------------------------------------------------
test('series cover mode renders 60 books per batch with IntersectionObserver lazy covers and bounded-concurrency batch read/unread', async () => {
  const books = Array.from({ length: 130 }, (_, i) => {
    const idx = i + 1;
    return {
      id: `b-${idx}`,
      name: `Vol.${String(idx).padStart(3, '0')}.cbz`,
      size: '15 MB',
      number: idx,
      media: { pagesCount: 100 },
      readProgress: idx <= 2 ? { page: 100, completed: true } : null,
      metadata: { title: `第 ${idx} 卷`, number: String(idx) },
    };
  });

  const detail = {
    series: { id: 's-long', oneshot: false, booksCount: 130, booksReadCount: 2, booksInProgressCount: 0, metadata: { title: '长篇连载', status: 'ONGOING', genres: [], tags: [], alternateTitles: [] } },
    rawMeta: { coverSource: 'auto', authors: [] },
    originalName: '长篇连载',
    relativePath: '长篇连载',
    totalSize: '1.9 GB',
    library: { id: 'lib-1', name: '主库', providerType: 'local' },
    books,
    bookExtras: {
      'b-1': { comicInfoStatus: 'parsed', coverSource: 'custom', hasCustomCover: true },
    },
  };

  let inFlight = 0;
  let maxInFlight = 0;
  const h = createV34Harness({
    includeSeriesDetail: true,
    pathname: '/series/s-long',
    storage: { 'fk115_display.u-admin.seriesBooksView': 'cover' },
    apiHandler: async (url, opts) => {
      if (url.includes('/read-progress')) {
        inFlight += 1;
        if (inFlight > maxInFlight) maxInFlight = inFlight;
        await new Promise((r) => setTimeout(r, 8));
        inFlight -= 1;
        return null;
      }
      return {};
    },
  });
  h.state.displayPrefs.seriesBooksView = 'cover';

  h.ctx.paintSeriesDetail('s-long', detail, []);

  // First batch renders 60 cards + IntersectionObserver on data-src images
  let cards = h.doc.getElementById('series-books-body').querySelectorAll('.book-poster-card');
  assert.equal(cards.length, 60);
  assert.match(h.doc.getElementById('series-books-more-bar').innerHTML, /已显示 <strong>60<\/strong> \/ 130 卷/);

  assert.equal(h.observers.length, 1);
  assert.equal(h.observers[0].observed.length, 60);
  const firstImg = cards[0].querySelector('img');
  assert.equal(firstImg.getAttribute('src'), null, 'lazy cover must not set src before intersection');
  assert.match(firstImg.getAttribute('data-src'), /^\/api\/v1\/books\/b-1\/thumbnail\?v=/);

  // Simulate intersecting first 2 images
  h.observers[0].trigger([
    { isIntersecting: true, target: firstImg },
    { isIntersecting: false, target: cards[1].querySelector('img') },
  ]);
  assert.match(firstImg.getAttribute('src'), /^\/api\/v1\/books\/b-1\/thumbnail\?v=/);
  assert.equal(firstImg.getAttribute('data-src'), null);
  assert.equal(cards[1].querySelector('img').getAttribute('src'), null);

  // Load more 60 -> 120
  h.doc.getElementById('btn-books-load-more').onclick();
  assert.equal(h.observers[0].disconnected, true, 'previous observer must disconnect on batch re-render');
  cards = h.doc.getElementById('series-books-body').querySelectorAll('.book-poster-card');
  assert.equal(cards.length, 120);
  assert.match(h.doc.getElementById('series-books-more-bar').innerHTML, /已显示 <strong>120<\/strong> \/ 130 卷/);

  // Batch select mode: select 10 books -> mark read with bounded concurrency (<= 4) -> partial refresh
  h.doc.getElementById('btn-books-batch-toggle').onclick();
  const cbs = h.doc.getElementById('series-books-body').querySelectorAll('input[data-book-select]');
  assert.equal(cbs.length, 120);
  for (let i = 2; i < 12; i += 1) {
    cbs[i].checked = true;
    cbs[i].onchange();
  }
  assert.equal(h.doc.getElementById('series-books-selected-count').textContent, '10');

  const callsBeforeBatch = h.apiCalls.length;
  await h.doc.getElementById('btn-books-batch-read').onclick();
  assert.ok(maxInFlight > 1 && maxInFlight <= 4, `expected bounded concurrency 2..4, got ${maxInFlight}`);
  const batchCalls = h.apiCalls.slice(callsBeforeBatch);
  assert.equal(batchCalls.length, 10);
  assert.ok(batchCalls.every((c) => c.method === 'PATCH' && c.url.endsWith('/read-progress') &&
    c.opts.body.completed === true && c.opts.headers['X-FK115-User-Id'] === 'u-admin'));
  // Partial refresh keeps 120 rendered books and updates b-3..b-12 to read
  assert.equal(h.doc.getElementById('series-books-body').querySelectorAll('.book-poster-card').length, 120);
  assert.equal(detail.books[2].readProgress.completed, true);

  // Select all visible (120) -> clear selection -> pick 2 -> batch unread (DELETE)
  h.doc.getElementById('btn-books-select-all').onclick();
  assert.equal(h.doc.getElementById('series-books-selected-count').textContent, '120');
  h.doc.getElementById('btn-books-select-clear').onclick();
  assert.equal(h.doc.getElementById('series-books-selected-count').textContent, '0');

  const cbsAfter = h.doc.getElementById('series-books-body').querySelectorAll('input[data-book-select]');
  cbsAfter[0].checked = true;
  cbsAfter[0].onchange();
  cbsAfter[1].checked = true;
  cbsAfter[1].onchange();
  await h.doc.getElementById('btn-books-batch-unread').onclick();
  const deleteCalls = h.apiCalls.filter((c) => c.method === 'DELETE' && c.url.endsWith('/read-progress'));
  assert.equal(deleteCalls.length, 2);
  assert.ok(deleteCalls.every((c) => c.opts.headers['X-FK115-User-Id'] === 'u-admin'));
  assert.equal(detail.books[0].readProgress, null);

  // Fallback without IntersectionObserver sets src directly with loading="lazy"
  const hNoIO = createV34Harness({
    includeSeriesDetail: true,
    disableObserver: true,
  });
  hNoIO.state.displayPrefs.seriesBooksView = 'cover';
  hNoIO.ctx.paintSeriesDetail('s-long', detail, []);
  const fallbackImg = hNoIO.doc.getElementById('series-books-body').querySelector('img');
  assert.match(fallbackImg.getAttribute('src'), /^\/api\/v1\/books\/b-1\/thumbnail\?v=/);
  assert.equal(fallbackImg.getAttribute('loading'), 'lazy');
});

for (const [cause, method] of [['account change', 'PATCH'], ['logout', 'DELETE'],
  ['route change', 'PATCH'], ['series change', 'DELETE'], ['409', 'DELETE']]) {
  test('batch read progress stops dispatch on ' + cause, async () => {
    const h = createV34Harness({ includeSeriesDetail: true, pathname: '/series/work' });
    let refreshes = 0;
    h.ctx.route = () => { refreshes += 1; };
    vm.runInContext(extractSlice('  async function api(url, opts = {}) {', '  let modalScroll = null;'), h.ctx);
    const requests = [];
    h.ctx.fetch = (url, opts) => {
      const { promise, resolve } = Promise.withResolvers();
      requests.push({ url, opts, resolve });
      if (requests.length > 4) resolve(new Response(null, { status: 204 }));
      return promise;
    };
    h.ctx.paintSeriesDetail('work', makeSeriesFixture('work', 8), []);
    h.doc.getElementById('btn-books-batch-toggle').onclick();
    h.doc.getElementById('btn-books-select-all').onclick();
    const pending = h.doc.getElementById(method === 'PATCH' ? 'btn-books-batch-read' : 'btn-books-batch-unread').onclick();
    assert.equal(requests.length, 4);

    if (cause === 'account change') h.state.auth.userId = 'new-user';
    if (cause === 'logout') h.state.auth.authenticated = false;
    if (cause === 'route change') h.ctx.routeSeq += 1;
    if (cause === 'series change') h.setLocation('/series/another');
    if (cause === '409') {
      requests[0].resolve(Response.json({ error: 'USER_CONTEXT_MISMATCH' }, { status: 409 }));
      await flushMicrotasks();
      assert.equal(refreshes, 1);
    }
    requests.slice(cause === '409' ? 1 : 0, 4).forEach(({ resolve }) => {
      resolve(new Response(null, { status: 204 }));
    });
    await pending;
    assert.equal(requests.length, 4);
    assert.ok(requests.every(({ opts }) => opts.method === method && opts.headers['X-FK115-User-Id'] === 'u-admin'));
  });
}

test('series maintenance repaint preserves loaded batches, selections and an in-flight batch', async () => {
  const seriesID = 'work';
  const queued = { id: 'maintenance', status: 'queued', totalItems: 130 };
  const finished = { id: queued.id, status: 'success', totalItems: 130, processedItems: 130, generatedCount: 130 };
  const batchWrite = Promise.withResolvers();
  let userId = 'u-admin';
  const h = createV34Harness({
    includeSeriesDetail: true,
    includeRouter: true,
    pathname: '/series/work',
    apiHandler: async (url) => {
      if (url === '/admin/api/auth/status') return { claimed: true, authenticated: true, isAdmin: true, userId };
      if (url.startsWith('/admin/api/maintenance-jobs')) return [finished];
      if (url.endsWith('/read-progress')) return batchWrite.promise;
      if (url === '/admin/api/series/work/detail') return makeSeriesFixture(seriesID, 130);
      if (url === '/admin/api/series/another/detail') return makeSeriesFixture('another', 130);
      return {};
    },
  });
  h.ctx.paintSeriesDetail(seriesID, makeSeriesFixture(seriesID, 130), [queued]);
  h.doc.getElementById('btn-books-load-more').onclick();
  h.doc.getElementById('btn-books-batch-toggle').onclick();
  const firstBody = h.doc.getElementById('series-books-body');
  const select = firstBody.querySelectorAll('input[data-book-select]')[75];
  select.checked = true;
  select.onchange();
  const pending = h.doc.getElementById('btn-books-batch-read').onclick();

  await h.getPollScheduled().fn();
  assert.equal(h.getPollScheduled(), null);
  const refreshedBody = h.doc.getElementById('series-books-body');
  assert.notEqual(refreshedBody, firstBody);
  assert.equal(refreshedBody.querySelectorAll('input[data-book-select]').length, 120);
  assert.equal(refreshedBody.querySelectorAll('input[data-book-select]')[75].checked, true);
  assert.equal(h.doc.getElementById('btn-books-batch-toggle').textContent, '退出批量选择');
  assert.equal(h.doc.getElementById('btn-books-batch-read').disabled, true);
  assert.equal(h.state.seriesBooks.batchBusy, true);
  await h.doc.getElementById('btn-books-batch-read').onclick();
  assert.equal(h.apiCalls.filter(({ url }) => url.endsWith('/read-progress')).length, 1);

  batchWrite.resolve(null);
  await pending;
  assert.equal(h.state.seriesBooks.batchBusy, false);
  assert.ok(refreshedBody.querySelector('button[data-book-unread="work-b76"]'));
  assert.equal(refreshedBody.querySelectorAll('input[data-book-select]')[75].checked, false);
  assert.equal(refreshedBody.querySelectorAll('input[data-book-select]').length, 120);

  userId = 'another-user';
  await h.ctx.route();
  assert.equal(h.doc.getElementById('btn-books-batch-toggle').textContent, '批量选择');
  assert.match(h.doc.getElementById('series-books-more-bar').innerHTML, /<strong>60<\/strong> \/ 130/);
  h.doc.getElementById('btn-books-load-more').onclick();
  h.doc.getElementById('btn-books-batch-toggle').onclick();
  h.doc.getElementById('btn-books-select-all').onclick();
  h.setLocation('/series/another');
  await h.ctx.route();
  assert.equal(h.doc.getElementById('btn-books-batch-toggle').textContent, '批量选择');
  assert.match(h.doc.getElementById('series-books-more-bar').innerHTML, /<strong>60<\/strong> \/ 130/);
  assert.equal(h.state.seriesBooks.selectedBookIds.size, 0);
});

test('series and admin maintenance rows render DTO progress, error lists, and all statuses', () => {
  const h = createV34Harness({ includeSeriesDetail: true, includeAdminJobs: true });
  const jobs = [
    { id: 'waiting', status: 'queued', totalItems: 12, processedItems: 0 },
    { id: 'active', status: 'running', totalItems: 12, processedItems: 7, generatedCount: 5, skippedCount: 1, failedCount: 1, errors: ['第 2 卷：读取失败 <fixture>'] },
    { id: 'done', status: 'success', totalItems: 12, processedItems: 12, succeededItems: 10, skippedItems: 2, failedItems: 0 },
    { id: 'partial', status: 'partial', totalItems: 12, processedItems: 12, generatedCount: 10, skippedCount: 1, failedCount: 1 },
    { id: 'failed', status: 'failed', totalItems: 2, processedItems: 2, failedCount: 2, errors: ['索引损坏', '图片不可用'] },
    { id: 'canceled', status: 'canceled', totalItems: 12, processedItems: 3 },
  ];
  h.ctx.paintSeriesDetail('work', makeSeriesFixture('work', 12), jobs);
  const seriesJobs = h.doc.getElementById('series-maint-jobs').innerHTML;
  const adminBody = h.doc.createElement('div');
  h.ctx.paintAdminJobs(adminBody, [], [], [], jobs);
  for (const html of [seriesJobs, adminBody.innerHTML]) {
    assert.doesNotMatch(html, /0\/0/);
    assert.match(html, /7\/12 · 成功 5 · 跳过 1 · 失败 1/);
    assert.match(html, /12\/12 · 成功 10 · 跳过 2 · 失败 0/);
    assert.match(html, /<li>第 2 卷：读取失败 &lt;fixture&gt;<\/li>/);
    assert.match(html, /<li>索引损坏<\/li><li>图片不可用<\/li>/);
    for (const label of ['排队中', '运行中', '已完成', '部分成功', '失败', '已取消']) {
      assert.ok(html.includes('>' + label + '</span>'));
    }
    assert.deepEqual([...html.matchAll(/data-cancel-maint="([^"]+)"/g)].map((m) => m[1]), ['waiting', 'active']);
  }
});

// ---------------------------------------------------------------------------
// 4. Single-volume series One-shot toggle & batch maintenance job modals
// ---------------------------------------------------------------------------
test('series detail exposes One-shot toggle when canToggleOneShot=true, read-only badge when fileBackedOneShot=true, and batch maintenance modals', async () => {
  const h = createV34Harness({
    includeSeriesDetail: true,
    pathname: '/series/s-one',
    apiHandler: async (url, opts) => {
      if (url === '/admin/api/series/s-one/detail') {
        return {
          series: { id: 's-one', oneshot: true, booksCount: 1, booksReadCount: 0, booksInProgressCount: 0, metadata: { title: '短篇集', status: 'ENDED', genres: [], tags: [], alternateTitles: [] } },
          rawMeta: { coverSource: 'auto', authors: [] },
          originalName: '短篇集',
          relativePath: '短篇集',
          totalSize: '25 MB',
          oneShotOverride: true,
          fileBackedOneShot: false,
          effectiveOneShot: true,
          canToggleOneShot: true,
          library: { id: 'lib-1', name: '主库', providerType: 'local', oneshotsDirectory: '_oneshots' },
          books: [{ id: 'b-one', name: 'OneShot.cbz', size: '25 MB', number: 1, media: { pagesCount: 40 } }],
          bookExtras: {},
        };
      }
      if (url.startsWith('/admin/api/maintenance-jobs?')) return [];
      return {};
    },
  });

  // 1. Toggleable single-volume series
  h.ctx.paintSeriesDetail('s-one', {
    series: { id: 's-one', oneshot: false, booksCount: 1, booksReadCount: 0, booksInProgressCount: 0, metadata: { title: '短篇集', status: 'ONGOING', genres: [], tags: [], alternateTitles: [] } },
    rawMeta: { coverSource: 'auto', authors: [] },
    originalName: '短篇集',
    relativePath: '短篇集',
    totalSize: '25 MB',
    oneShotOverride: false,
    fileBackedOneShot: false,
    effectiveOneShot: false,
    canToggleOneShot: true,
    library: { id: 'lib-1', name: '主库', providerType: 'local' },
    books: [{ id: 'b-one', name: 'OneShot.cbz', size: '25 MB', number: 1, media: { pagesCount: 40 } }],
    bookExtras: {},
  }, []);

  const toggle = h.doc.getElementById('series-oneshot-toggle');
  assert.ok(toggle);
  assert.equal(toggle.checked, false);
  toggle.checked = true;
  await toggle.onchange();

  const patchOneshot = h.apiCalls.find((c) => c.url === '/admin/api/series/s-one/oneshot' && c.method === 'PATCH');
  assert.ok(patchOneshot);
  assert.deepEqual(JSON.parse(JSON.stringify(patchOneshot.opts.body)), { oneShotOverride: true });
  await flushMicrotasks();

  // 2. File-backed one-shot shows read-only badge and no toggle
  h.ctx.routeSeq += 1;
  h.ctx.paintSeriesDetail('s-file', {
    series: { id: 's-file', oneshot: true, booksCount: 1, booksReadCount: 0, booksInProgressCount: 0, metadata: { title: '文件单篇', status: 'ENDED', genres: [], tags: [], alternateTitles: [] } },
    rawMeta: { coverSource: 'auto', authors: [] },
    originalName: '文件单篇',
    relativePath: '_oneshots/文件单篇.cbz',
    totalSize: '12 MB',
    oneShotOverride: false,
    fileBackedOneShot: true,
    effectiveOneShot: true,
    canToggleOneShot: false,
    library: { id: 'lib-1', name: '主库', providerType: 'local', oneshotsDirectory: '_oneshots' },
    books: [{ id: 'b-file', name: '文件单篇.cbz', size: '12 MB', number: 1, media: { pagesCount: 30 } }],
    bookExtras: {},
  }, []);
  assert.equal(h.doc.getElementById('series-oneshot-toggle'), null);
  assert.match(h.doc.getElementById('page-content').innerHTML, /文件型单篇/);

  // 3. Batch book covers modal (fallbackPage1 defaults false) & Batch ComicInfo modal
  h.doc.getElementById('btn-series-batch-covers').onclick();
  const extraCb1 = h.doc.getElementById('bm-job-extra');
  const forceCb1 = h.doc.getElementById('bm-job-force');
  assert.equal(extraCb1.checked, false, 'fallbackPage1 must default to false');
  assert.equal(forceCb1.checked, false);
  forceCb1.checked = true;
  await h.doc.getElementById('bm-job-start').onclick();

  const coverJobCall = h.apiCalls.filter((c) => c.url === '/admin/api/maintenance-jobs' && c.method === 'POST').pop();
  assert.deepEqual(JSON.parse(JSON.stringify(coverJobCall.opts.body)), {
    operation: 'batch_book_covers',
    targetType: 'series',
    targetId: 's-file',
    fallbackPage1: false,
    force: true,
  });

  h.doc.getElementById('btn-series-batch-comicinfo').onclick();
  h.doc.getElementById('bm-job-extra').checked = true;
  await h.doc.getElementById('bm-job-start').onclick();

  const ciJobCall = h.apiCalls.filter((c) => c.url === '/admin/api/maintenance-jobs' && c.method === 'POST').pop();
  assert.deepEqual(JSON.parse(JSON.stringify(ciJobCall.opts.body)), {
    operation: 'batch_comicinfo',
    targetType: 'series',
    targetId: 's-file',
    extractPage1: true,
    force: false,
  });
});

// ---------------------------------------------------------------------------
// 5. Book detail two-column view, 9 fields + 9 locks, cover source, cover upload, ComicInfo refresh
// ---------------------------------------------------------------------------
test('book detail renders two-column metadata, edits 9 fields + 9 locks + clearNumberSort, switches cover source, uploads cover, and refreshes ComicInfo', async () => {
  const sampleBookDetail = {
    book: {
      id: 'bk-1',
      name: 'Vol.01.cbz',
      size: '42 MB',
      media: { pagesCount: 196 },
      readProgress: { page: 20, completed: false },
      metadata: {
        title: '第 1 卷：出发',
        titleLock: true,
        summary: '卷简介文本',
        summaryLock: false,
        number: '1',
        numberLock: true,
        numberSort: 1.5,
        numberSortLock: true,
        releaseDate: '2024-05-01',
        releaseDateLock: false,
        isbn: '9784088800000',
        isbnLock: true,
        authors: [{ name: '作者甲', role: 'writer' }, { name: '插画乙', role: 'illustrator' }],
        authorsLock: false,
        tags: ['History, Modern', '热血'],
        tagsLock: true,
        links: [{ label: 'BGM', url: 'https://bgm.tv/subject/100' }],
        linksLock: false,
      },
    },
    rawBookMeta: {
      coverSource: 'ext',
      hasCustomCover: true,
      customCoverUrl: 'https://cdn.example.test/c1.jpg',
      bgmSubjectId: 100,
      extSourceId: 'src-1',
      extEntryId: 'e-9',
      hasExtCover: true,
      seriesGroup: '主线篇章',
      storyArc: '东海篇',
      storyArcNumber: '1',
      comicInfoStatus: 'parsed',
      matchOrigin: 'comicinfo',
      numberSortExplicit: true,
    },
    seriesId: 'ser-1',
    seriesTitle: '示例作品',
    originalSeries: '示例作品',
    libraryId: 'lib-1',
    libraryName: '本地主库',
    providerType: 'local',
    archiveType: 'CBZ',
    relativePath: '示例作品/Vol.01.cbz',
    previousBookId: '',
    nextBookId: 'bk-2',
    indexStatus: '196 页（已建索引）',
    downloadSpeed: '直读无缓存',
  };

  const h = createV34Harness({
    includeBookDetail: true,
    pathname: '/book/bk-1',
    apiHandler: async (url, opts) => {
      if (url === '/admin/api/books/bk-1/detail') return sampleBookDetail;
      if (url.startsWith('/admin/api/maintenance-jobs')) return [];
      if (url === '/admin/api/books/bk-1/comicinfo/refresh') return { bookId: 'bk-1', status: 'parsed' };
      if (url === '/admin/api/books/bk-1/cover' && opts.method === 'POST') {
        Object.assign(sampleBookDetail.rawBookMeta, { coverSource: 'custom', customCoverUrl: opts.body.url });
      }
      return {};
    },
    xhrHandler: (xhr) => {
      Object.assign(sampleBookDetail.rawBookMeta, { coverSource: 'custom', customCoverUrl: '' });
      xhr.responseText = JSON.stringify({ coverSource: 'custom', hasCustomCover: true });
      xhr.onload();
    },
  });

  h.ctx.paintBookDetail('bk-1', sampleBookDetail, []);

  const pageHTML = h.doc.getElementById('page-content').innerHTML;
  assert.match(pageHTML, /class="detail-hero"/);
  assert.match(pageHTML, /class="detail-cover-col"/);
  assert.match(pageHTML, /\/api\/v1\/books\/bk-1\/thumbnail\?v=/);
  assert.match(pageHTML, /ComicInfo：已解析/);
  assert.match(pageHTML, /分组：主线篇章/);
  assert.match(pageHTML, /故事线：东海篇 #1/);
  assert.match(pageHTML, /9784088800000/);
  assert.match(pageHTML, /插画乙 \(illustrator\)/);
  assert.match(pageHTML, /https:\/\/bgm\.tv\/subject\/100/);

  // 1. Cover source switch
  const bgmCoverBtn = h.doc.getElementById('page-content').querySelector('button[data-book-cover-src="bangumi"]');
  await bgmCoverBtn.onclick();
  await flushMicrotasks();
  const csCall = h.apiCalls.find((c) => c.url === '/admin/api/books/bk-1/cover-source' && c.method === 'PATCH');
  assert.deepEqual(JSON.parse(JSON.stringify(csCall.opts.body)), { coverSource: 'bangumi' });

  // 2. Clear custom cover
  await h.doc.getElementById('bk-clear-cover').onclick();
  await flushMicrotasks();
  const delCoverCall = h.apiCalls.find((c) => c.url === '/admin/api/books/bk-1/cover' && c.method === 'DELETE');
  assert.ok(delCoverCall);

  // 3. Upload custom cover file via FormData + XHR
  const bookPage = () => h.doc.getElementById('page-content');
  const prepareExternalCover = () => {
    sampleBookDetail.rawBookMeta.coverSource = 'ext';
    h.state.coverVer = 1;
    h.ctx.paintBookDetail('bk-1', sampleBookDetail, []);
    assert.ok(bookPage().querySelector('button[data-book-cover-src="ext"]').classList.contains('active'));
  };
  const assertCustomCoverReloaded = () => {
    assert.ok(bookPage().querySelector('button[data-book-cover-src="custom"]').classList.contains('active'));
    assert.notEqual(h.state.coverVer, 1);
    assert.ok(bookPage().querySelector('img').src.endsWith('/thumbnail?v=' + h.state.coverVer));
  };
  prepareExternalCover();
  h.doc.getElementById('bk-upload-cover').onclick();
  const fakeImgFile = { name: 'cover.jpg', size: 2048 };
  h.doc.getElementById('bkc-file').files = [fakeImgFile];
  await h.doc.getElementById('bkc-submit').onclick();
  await flushMicrotasks();
  assert.equal(h.xhrCalls.length, 1);
  assert.equal(h.xhrCalls[0].url, '/admin/api/books/bk-1/cover');
  assert.equal(h.xhrCalls[0].withCredentials, true);
  assert.equal(h.xhrCalls[0].headers['X-FK115-User-Id'], 'u-admin');
  assert.equal(h.xhrCalls[0].body.get('file'), fakeImgFile);
  assertCustomCoverReloaded();

  prepareExternalCover();
  h.doc.getElementById('bk-upload-cover').onclick();
  h.modalRoot.querySelector('button[data-bkc-tab="url"]').onclick();
  const coverURL = 'https://covers.example.invalid/another.jpg';
  h.doc.getElementById('bkc-url').value = coverURL;
  await h.doc.getElementById('bkc-submit').onclick();
  await flushMicrotasks();
  const coverURLCall = h.apiCalls.find((c) => c.url === '/admin/api/books/bk-1/cover' && c.method === 'POST');
  assert.equal(coverURLCall.opts.body.url, coverURL);
  assertCustomCoverReloaded();

  // Saving the inherited/default values unchanged sends no PATCH.
  sampleBookDetail.rawBookMeta.numberSortExplicit = false;
  h.doc.getElementById('bk-edit-meta').onclick();
  assert.equal(h.doc.getElementById('bm-edit-tags').value, 'History, Modern\n热血');
  await h.doc.getElementById('bm-edit-save').onclick();
  const metadataPatches = () => h.apiCalls.filter((c) => c.url === '/api/v1/books/bk-1/metadata' && c.method === 'PATCH');
  assert.equal(metadataPatches().length, 0);
  h.doc.getElementById('bk-edit-meta').onclick();
  h.doc.getElementById('bm-edit-summary').value = '只改简介';
  await h.doc.getElementById('bm-edit-save').onclick();
  await flushMicrotasks();
  assert.deepEqual(JSON.parse(JSON.stringify(metadataPatches()[0].opts.body)), { summary: '只改简介' });

  // 4. Edit 9 metadata fields + 9 locks
  h.doc.getElementById('bk-edit-meta').onclick();
  h.doc.getElementById('bm-edit-title').value = '新卷标题';
  h.doc.getElementById('bm-edit-title-lock').checked = true;
  h.doc.getElementById('bm-edit-number').value = '01';
  h.doc.getElementById('bm-edit-number-lock').checked = false;
  h.doc.getElementById('bm-edit-numberSort').value = '2.5';
  h.doc.getElementById('bm-edit-numberSort-lock').checked = true;
  h.doc.getElementById('bm-edit-releaseDate').value = '2025-01-10';
  h.doc.getElementById('bm-edit-releaseDate-lock').checked = true;
  h.doc.getElementById('bm-edit-isbn').value = '9780000000001';
  h.doc.getElementById('bm-edit-isbn-lock').checked = false;
  h.doc.getElementById('bm-edit-authors').value = '作者新 | writer\n助手 | inker';
  h.doc.getElementById('bm-edit-authors-lock').checked = true;
  h.doc.getElementById('bm-edit-tags').value += '\n标签A';
  h.doc.getElementById('bm-edit-tags-lock').checked = true;
  h.doc.getElementById('bm-edit-links').value = '官网 | https://example.org/book';
  h.doc.getElementById('bm-edit-links-lock').checked = true;
  h.doc.getElementById('bm-edit-summary').value = '更新后的简介';
  h.doc.getElementById('bm-edit-summary-lock').checked = true;

  await h.doc.getElementById('bm-edit-save').onclick();
  await flushMicrotasks();
  const patchMetaCall = metadataPatches().at(-1);
  assert.ok(patchMetaCall);
  assert.deepEqual(JSON.parse(JSON.stringify(patchMetaCall.opts.body)), {
    title: '新卷标题',
    number: '01',
    numberLock: false,
    numberSort: 2.5,
    releaseDate: '2025-01-10',
    releaseDateLock: true,
    isbn: '9780000000001',
    isbnLock: false,
    authors: [{ name: '作者新', role: 'writer' }, { name: '助手', role: 'inker' }],
    authorsLock: true,
    tags: ['History, Modern', '热血', '标签A'],
    links: [{ label: '官网', url: 'https://example.org/book' }],
    linksLock: true,
    summary: '更新后的简介',
    summaryLock: true,
  });

  // 4b. Test clearNumberSort: true & unlock-all
  h.doc.getElementById('bk-edit-meta').onclick();
  h.doc.getElementById('bm-edit-unlock-all').onclick();
  const clearCb = h.doc.getElementById('bm-edit-clear-numberSort');
  clearCb.checked = true;
  clearCb.onchange();
  assert.equal(h.doc.getElementById('bm-edit-numberSort').disabled, true);
  await h.doc.getElementById('bm-edit-save').onclick();
  await flushMicrotasks();
  const patchClearCall = h.apiCalls.filter((c) => c.url === '/api/v1/books/bk-1/metadata' && c.method === 'PATCH').pop();
  assert.equal(patchClearCall.opts.body.clearNumberSort, true);
  assert.equal(Object.prototype.hasOwnProperty.call(patchClearCall.opts.body, 'numberSort'), false);
  assert.equal(patchClearCall.opts.body.titleLock, false);

  // 5. Refresh ComicInfo
  await h.doc.getElementById('bk-refresh-comicinfo').onclick();
  await flushMicrotasks();
  const ciCall = h.apiCalls.find((c) => c.url === '/admin/api/books/bk-1/comicinfo/refresh' && c.method === 'POST');
  assert.deepEqual(JSON.parse(JSON.stringify(ciCall.opts.body)), { force: true });
});

for (const [stage, change] of [['jobs', 'route'], ['jobs', 'book'], ['jobs', 'user'],
  ['detail', 'route'], ['detail', 'book'], ['detail', 'user']]) {
  test('book maintenance drops stale ' + stage + ' response after ' + change + ' changes', async () => {
    const deferred = Promise.withResolvers();
    const h = createV34Harness({
      includeBookDetail: true,
      pathname: '/books/volume',
      apiHandler: async (url) => {
        if (url.startsWith('/admin/api/maintenance-jobs')) {
          return stage === 'jobs' ? deferred.promise : [{ status: 'success' }];
        }
        if (url.endsWith('/detail')) return deferred.promise;
        return {};
      },
    });
    let repaints = 0;
    h.ctx.paintBookDetail = () => { repaints += 1; };
    h.ctx.renderShell('libraries', '<div id="book-maint-jobs">old page</div>');
    const pending = h.ctx.pollBookMaint('volume');
    if (stage === 'detail') {
      await flushMicrotasks();
      assert.equal(h.apiCalls.at(-1).url, '/admin/api/books/volume/detail');
    }
    if (change === 'route') h.ctx.routeSeq += 1;
    if (change === 'book') h.setLocation('/book/another');
    if (change === 'user') h.state.auth.userId = 'another-user';
    h.ctx.renderShell('libraries', '<div id="book-maint-jobs">new page</div>');
    h.ctx.schedulePoll(() => {}, 200);
    const newPoll = h.getPollScheduled();

    deferred.resolve(stage === 'jobs' ? [{ status: 'running' }] : { book: { id: 'volume' } });
    await pending;
    assert.equal(h.doc.getElementById('book-maint-jobs').innerHTML, 'new page');
    assert.equal(repaints, 0);
    assert.equal(h.getPollScheduled(), newPoll);
    assert.ok(h.apiCalls.every(({ opts }) => opts.headers['X-FK115-User-Id'] === 'u-admin'));
  });
}

// ---------------------------------------------------------------------------
// 6. Admin Libraries oneshotsDirectory + batch buttons & Metadata Center streaming upload + -wal warning
// ---------------------------------------------------------------------------
test('admin libraries supports oneshotsDirectory quick-fill, create/edit payload, and library-level batch jobs', async () => {
  const libs = [
    { id: 'lib-1', name: '本地主库', providerType: 'local', rootPath: '/comics', enabled: true, oneShot: false, oneshotsDirectory: '_oneshots', seriesCount: 10, bookCount: 40, comicBytes: 1024 },
  ];

  const h = createV34Harness({
    includeSeriesDetail: true, // provides openBatchMaintModal
    includeAdminLibraries: true,
    apiHandler: async (url) => {
      if (url === '/admin/api/libraries') return libs;
      if (url === '/admin/api/webdav-accounts') return [];
      if (url === '/admin/api/schedules') return { timezone: 'Asia/Shanghai', schedules: {} };
      return {};
    },
  });

  const body = h.doc.createElement('div');
  body.id = 'admin-tab-body';
  await h.ctx.renderAdminLibraries(body);

  assert.match(body.innerHTML, /单篇目录：_oneshots/);
  assert.ok(body.querySelector('button[data-lib-batch-covers="lib-1"]'));
  assert.ok(body.querySelector('button[data-lib-batch-comicinfo="lib-1"]'));

  // Quick-fill _oneshots in create form & create library
  h.doc.getElementById('nl-oneshots-fill').onclick();
  assert.equal(h.doc.getElementById('nl-oneshots-dir').value, '_oneshots');
  h.doc.getElementById('nl-provider').value = 'local';
  h.doc.getElementById('nl-name').value = '新建测试库';
  h.doc.getElementById('nl-local-path').value = '/new-comics';
  await h.doc.getElementById('nl-create-btn').onclick({ currentTarget: h.doc.getElementById('nl-create-btn') });

  const postLib = h.apiCalls.find((c) => c.url === '/admin/api/libraries' && c.method === 'POST');
  assert.ok(postLib);
  assert.equal(postLib.opts.body.oneshotsDirectory, '_oneshots');

  // Edit library modal preserves & updates oneshotsDirectory + oneShot hint
  body.querySelector('button[data-lib-edit]').onclick();
  assert.equal(h.doc.getElementById('el-oneshots-dir').value, '_oneshots');
  const elOneShot = h.doc.getElementById('el-oneshot');
  elOneShot.checked = true;
  elOneShot.onchange();
  assert.match(h.doc.getElementById('el-oneshots-dir-hint').textContent, /全库所有压缩包已作为单篇处理/);
  h.doc.getElementById('el-oneshots-dir').value = 'shorts';
  await h.doc.getElementById('el-save').onclick();

  const putLib = h.apiCalls.find((c) => c.url === '/admin/api/libraries/lib-1' && c.method === 'PUT');
  assert.deepEqual(JSON.parse(JSON.stringify(putLib.opts.body)), {
    name: '本地主库',
    enabled: true,
    oneShot: true,
    oneshotsDirectory: 'shorts',
  });

  // Library-level batch cover job
  body.querySelector('button[data-lib-batch-covers="lib-1"]').onclick();
  await h.doc.getElementById('bm-job-start').onclick();
  const libBatchCall = h.apiCalls.find((c) => c.url === '/admin/api/maintenance-jobs' && c.method === 'POST');
  assert.deepEqual(JSON.parse(JSON.stringify(libBatchCall.opts.body)), {
    operation: 'batch_book_covers',
    targetType: 'library',
    targetId: 'lib-1',
    fallbackPage1: false,
    force: false,
  });
});

test('metadata center renders schemaFormat badges, -wal warnings, and streams SQLite uploads via XHR + FormData with progress', async () => {
  let progressSnapshots = [];
  const overview = {
    recommendedRepoUrl: 'https://example.test/repo.json',
    repos: [],
    sources: [
      {
        id: 'src-komga',
        kind: 'offline_sqlite',
        schemaFormat: 'komga_sqlite_v1',
        name: 'Komga DB 挂载源',
        localPath: '/data/komga/database.sqlite',
        enabled: true,
        ready: true,
        entryCount: 1200,
        tagDictSourceId: 'dict-1',
        walWarning: true,
        walWarningBytes: 4 * 1024 * 1024,
        walWarningMessage: '检测到同目录存在未 Checkpoint 的 -wal 日志，可能缺少最新增量数据；请先停止 Komga 或执行 Checkpoint 后再同步',
      },
      {
        id: 'src-v2',
        kind: 'offline_sqlite',
        schemaFormat: 'mangaalpha_meta_v2',
        name: '开放包 v2',
        repoId: 'fixture-repo',
        tagDictSourceId: 'dict-1',
        enabled: true,
        ready: true,
        entryCount: 500,
      },
      {
        id: 'src-v1',
        kind: 'offline_sqlite',
        schemaFormat: 'mangaalpha_meta_v1',
        name: '开放包 v1',
        enabled: true,
        ready: true,
        entryCount: 300,
      },
      {
        id: 'src-gal',
        kind: 'offline_sqlite',
        schemaFormat: 'gallery_v1',
        name: '图库源',
        enabled: true,
        ready: true,
        entryCount: 900,
      },
    ],
    libraryChains: {},
    syncs: [],
    scrapeRuns: [],
  };

  const bgmStatus = {
    totalSeries: 10,
    matchedCount: 5,
    pendingCount: 2,
    unmatchedCount: 3,
    archive: { ready: true, subjectCount: 100 },
  };

  const h = createV34Harness({
    includeScraper: true,
    pathname: '/admin',
    search: '?tab=bangumi',
    apiHandler: async (url) => {
      if (url === '/admin/api/bangumi/status') return bgmStatus;
      if (url.startsWith('/admin/api/bangumi/runs')) return [];
      if (url === '/admin/api/libraries') return [];
      if (url.startsWith('/admin/api/bangumi/tracker')) return {};
      if (url === '/admin/api/metadata/overview') return overview;
      if (url === '/admin/api/metadata/scrape-runs') return [];
      return {};
    },
    xhrHandler: (xhr) => {
      if (xhr.upload && typeof xhr.upload.onprogress === 'function') {
        xhr.upload.onprogress({ lengthComputable: true, loaded: 512 * 1024, total: 1024 * 1024 });
        progressSnapshots.push(h.doc.getElementById('emas-upload-status')?.textContent || h.doc.getElementById('ems-upload-status')?.textContent || '');
        xhr.upload.onprogress({ lengthComputable: true, loaded: 1024 * 1024, total: 1024 * 1024 });
      }
      xhr.status = 201;
      xhr.responseText = JSON.stringify({
        id: 'src-komga-up',
        name: 'Komga 导入库',
        kind: 'offline_sqlite',
        schemaFormat: 'komga_sqlite_v1',
        walWarning: false,
      });
      xhr.onload();
    },
  });

  const body = h.doc.createElement('div');
  body.id = 'admin-tab-body';

  h.ctx.paintAdminBangumi(body, bgmStatus, [], [], {}, overview, []);

  // Friendly schemaFormat badges & walWarning alert
  assert.match(body.innerHTML, />Komga DB</);
  assert.match(body.innerHTML, />开放规范 v2</);
  assert.match(body.innerHTML, />开放规范 v1</);
  assert.match(body.innerHTML, />图库 v1</);
  assert.match(body.innerHTML, /extmeta-wal-warning/);
  assert.match(body.innerHTML, /未 Checkpoint 的 -wal 日志/);

  // 1. Add source modal: path tab shows -wal hint; upload tab uses XHR + FormData with metadata fields before file
  h.doc.getElementById('extmeta-source-add-btn').onclick();
  assert.match(h.doc.getElementById('emas-pane-path').innerHTML, /请先停止 Komga 或执行 WAL Checkpoint，勿遗漏 -wal/);

  const uploadTabBtn = h.modalRoot.querySelector('button[data-emas-tab="upload"]');
  uploadTabBtn.onclick();
  assert.match(h.doc.getElementById('emas-pane-upload').innerHTML, /client_max_body_size/);

  h.doc.getElementById('emas-name').value = '我的 Komga DB';
  const fakeDbFile = { name: 'database.sqlite', size: 1024 * 1024 };
  h.doc.getElementById('emas-upload-file').files = [fakeDbFile];
  await h.doc.getElementById('emas-upload-btn').onclick();
  await flushMicrotasks();

  assert.equal(h.xhrCalls.length, 1);
  const addXhr = h.xhrCalls[0];
  assert.equal(addXhr.method, 'POST');
  assert.equal(addXhr.url, '/admin/api/metadata/sources/upload');
  assert.equal(addXhr.withCredentials, true);
  assert.equal(addXhr.headers['X-FK115-User-Id'], 'u-admin');
  assert.equal(addXhr.headers['X-FK115-UI'], '1');
  const addEntries = addXhr.body.entries();
  assert.equal(addEntries[0][0], 'name', 'metadata fields must precede file part for direct streaming');
  assert.equal(addEntries[addEntries.length - 1][0], 'file');
  assert.equal(addEntries[addEntries.length - 1][1], fakeDbFile);
  assert.match(progressSnapshots[0], /50%/);

  // 2. Edit source modal: shows walWarning & supports streaming upload replacement with sourceId before file
  body.querySelector('button[data-extmeta-src-edit="src-komga"]').onclick();
  assert.match(h.doc.getElementById('modal-body').innerHTML, /未 Checkpoint 的 -wal 日志/);
  assert.match(h.doc.getElementById('ems-pane-path').innerHTML, /请先停止 Komga 或执行 WAL Checkpoint/);

  h.modalRoot.querySelector('button[data-ems-tab="upload"]').onclick();
  const dict = h.doc.getElementById('ems-tag-dict');
  assert.equal(dict.value, 'dict-1');
  dict.value = '';
  h.doc.getElementById('ems-upload-file').files = [fakeDbFile];
  await h.doc.getElementById('ems-upload-btn').onclick();
  await flushMicrotasks();

  assert.equal(h.xhrCalls.length, 2);
  const editEntries = h.xhrCalls[1].body.entries();
  assert.deepEqual(editEntries[0], ['sourceId', 'src-komga']);
  assert.equal(editEntries[editEntries.length - 1][0], 'file');
  assert.equal(h.xhrCalls[1].body.get('tagDictSourceId'), '');

  body.querySelector('button[data-extmeta-src-edit="src-v2"]').onclick();
  h.modalRoot.querySelector('button[data-ems-tab="upload"]').onclick();
  assert.match(h.doc.getElementById('ems-pane-upload').innerHTML, /仓库订阅管理.*关联标签词典/);
  for (const id of ['ems-enabled', 'ems-tag-dict', 'ems-tag-mode', 'ems-auto-update']) {
    assert.equal(h.doc.getElementById(id).disabled, true);
  }
  h.doc.getElementById('ems-upload-file').files = [fakeDbFile];
  await h.doc.getElementById('ems-upload-btn').onclick();
  assert.equal(h.xhrCalls[2].body.get('tagDictSourceId'), 'dict-1');

  body.querySelector('button[data-extmeta-src-edit="src-v2"]').onclick();
  h.modalRoot.querySelector('button[data-ems-tab="upload"]').onclick();
  h.modalRoot.querySelector('button[data-ems-tab="path"]').onclick();
  assert.equal(h.doc.getElementById('ems-tag-dict').disabled, false);
});

test('upload authentication failures close the modal and recover the current route', async () => {
  const h = createV34Harness({
    includeRouter: true,
    pathname: '/admin',
    search: '?tab=bangumi',
    apiHandler: async (url) => {
      if (url === '/admin/api/auth/status') {
        return { claimed: true, authenticated: true, userId: 'new-admin', isAdmin: true };
      }
      return {};
    },
    xhrHandler: () => {},
  });
  const upload = (status, code, changeUser = false) => {
    h.ctx.openModal({ title: 'Upload', bodyHTML: '<input type="file">' });
    const pending = h.ctx.xhrUploadFormData('/admin/api/metadata/sources/upload', new FakeFormData());
    const xhr = h.xhrCalls.at(-1);
    if (changeUser) h.state.auth.userId = 'new-admin';
    xhr.status = status;
    xhr.responseText = JSON.stringify({ error: code });
    xhr.onload();
    return pending;
  };
  await assert.rejects(upload(401, 'UNAUTHORIZED'), { status: 401, code: 'UNAUTHORIZED' });
  assert.equal(h.modalRoot.innerHTML, '');
  assert.deepEqual(h.navigated, ['/login?redirect=%2Fadmin%3Ftab%3Dbangumi']);

  h.setLocation('/admin', '?tab=bangumi');
  await assert.rejects(upload(409, 'OTHER_CONFLICT'), { status: 409, code: 'OTHER_CONFLICT' });
  assert.ok(h.modalRoot.firstElementChild);
  await assert.rejects(upload(409, 'USER_CONTEXT_MISMATCH'), { status: 409, code: 'USER_CONTEXT_MISMATCH' });
  await flushMicrotasks();
  assert.equal(h.modalRoot.innerHTML, '');
  assert.equal(h.state.auth.userId, 'new-admin');
  assert.equal(h.getCurrentRenderTarget(), 'admin');

  h.state.auth.userId = 'old-admin';
  await assert.rejects(upload(200, '', true), { code: 'USER_CONTEXT_MISMATCH' });
  await flushMicrotasks();
  assert.equal(h.modalRoot.innerHTML, '');

  h.state.auth.userId = 'old-admin';
  h.ctx.openModal({ title: 'Upload', bodyHTML: '<input type="file">' });
  await h.ctx.route();
  assert.equal(h.modalRoot.innerHTML, '');
});

// ---------------------------------------------------------------------------
// 7. CSS variable & 360px viewport safety check
// ---------------------------------------------------------------------------
test('app.css v3.4.0 additions use theme CSS variables without hardcoded colors', () => {
  const start = cssSource.indexOf('/* MangaAlpha v3.4.0');
  assert.ok(start >= 0, 'expected v3.4.0 CSS section');
  const slice = cssSource.slice(start);
  assert.doesNotMatch(slice, /#[0-9a-fA-F]{3,8}\b/);
  assert.doesNotMatch(slice, /\brgba?\(/);
  assert.match(cssSource, /\.poster-badge-top-left\s*\{[^}]*color:\s*var\(--warn\)[^}]*background:\s*var\(--overlay-strong\)/);
  assert.doesNotMatch(slice, /\.book-vol-badge\s*\{/);
});
