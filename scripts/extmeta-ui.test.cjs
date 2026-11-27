const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { test } = require('node:test');
const vm = require('node:vm');

const appSource = readFileSync('internal/httpserver/static/ui/app.js', 'utf8');
const cssSource = readFileSync('internal/httpserver/static/ui/app.css', 'utf8');
const htmlSource = readFileSync('internal/httpserver/static/ui/app.html', 'utf8');

function decodeHTML(str) {
  return String(str)
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&amp;/g, '&');
}

function parseAttrs(raw) {
  const attrs = new Map();
  const re = /([a-zA-Z0-9_:-]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>/]+)))?/g;
  let m;
  while ((m = re.exec(raw)) !== null) {
    const key = m[1].toLowerCase();
    const val = m[2] ?? m[3] ?? m[4] ?? '';
    attrs.set(key, decodeHTML(val));
  }
  return attrs;
}

function parseStyle(raw) {
  const style = {};
  for (const part of String(raw || '').split(';')) {
    const idx = part.indexOf(':');
    if (idx <= 0) continue;
    const k = part.slice(0, idx).trim();
    const v = part.slice(idx + 1).trim();
    style[k] = v;
  }
  return style;
}

function createFakeDOM() {
  const containers = new Set();

  function parseElementsFromHTML(html, owner) {
    const elements = [];
    const stack = [];
    const tagRe = /<\/?([a-zA-Z0-9-]+)([^>]*)>/g;
    let m;
    while ((m = tagRe.exec(html)) !== null) {
      const full = m[0];
      const tag = m[1].toLowerCase();
      const isClose = full.startsWith('</');
      if (isClose) {
        if (tag === 'select') {
          for (let i = stack.length - 1; i >= 0; i--) {
            if (stack[i].tagName === 'SELECT') {
              const sel = stack[i];
              stack.splice(i, 1);
              if (sel._options.length > 0 && !sel._hasSelectedOption) {
                sel.value = sel._options[0].value;
              }
              break;
            }
          }
        }
        continue;
      }

      const attrs = parseAttrs(m[2] || '');
      if (tag === 'option') {
        const optVal = attrs.has('value') ? attrs.get('value') : '';
        const isSel = attrs.has('selected');
        for (let i = stack.length - 1; i >= 0; i--) {
          if (stack[i].tagName === 'SELECT') {
            const sel = stack[i];
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

      const el = {
        tagName: tag.toUpperCase(),
        id: attrs.get('id') || '',
        attrs,
        style: parseStyle(attrs.get('style') || ''),
        isConnected: true,
        disabled: attrs.has('disabled'),
        checked: attrs.has('checked'),
        open: attrs.has('open'),
        value: attrs.has('value') ? attrs.get('value') : (tag === 'input' && (attrs.get('type') || '').toLowerCase() === 'checkbox' ? 'on' : ''),
        textContent: '',
        _options: [],
        _hasSelectedOption: false,
        _listeners: new Map(),
        _owner: owner,
        getAttribute(name) {
          const k = String(name).toLowerCase();
          return this.attrs.has(k) ? this.attrs.get(k) : null;
        },
        setAttribute(name, val) {
          this.attrs.set(String(name).toLowerCase(), String(val));
        },
        addEventListener(type, fn) {
          const list = this._listeners.get(type) || [];
          list.push(fn);
          this._listeners.set(type, list);
        },
        querySelectorAll(sel) {
          return queryOnList(elements, sel);
        },
        querySelector(sel) {
          return queryOnList(elements, sel)[0] || null;
        },
        contains() {
          return false;
        },
        focus() {},
        setSelectionRange() {},
        remove() {},
      };
      if (tag === 'select') {
        stack.push(el);
      }
      elements.push(el);
    }

    // Populate innerHTML / textContent for non-void tags cleanly enough for assertions
    for (const el of elements) {
      if (el.tagName === 'TEXTAREA' && el.id) {
        const taRe = new RegExp(`<textarea[^>]*id="${el.id}"[^>]*>([\\s\\S]*?)<\\/textarea>`, 'i');
        const tm = taRe.exec(html);
        if (tm) el.value = decodeHTML(tm[1]);
      }
    }
    return elements;
  }

  function matchesSelector(el, singleSel) {
    const s = singleSel.trim();
    if (!s) return false;
    if (s.startsWith('#')) {
      return el.id === s.slice(1);
    }
    const tagAttr = s.match(/^([a-zA-Z0-9-]+)?\[([a-zA-Z0-9_-]+)(?:="([^"]*)")?\]$/);
    if (tagAttr) {
      const [, t, attr, expectedVal] = tagAttr;
      if (t && el.tagName !== t.toUpperCase()) return false;
      if (!el.attrs.has(attr.toLowerCase())) return false;
      if (expectedVal !== undefined && el.attrs.get(attr.toLowerCase()) !== expectedVal) return false;
      return true;
    }
    if (/^[a-zA-Z0-9-]+$/.test(s)) {
      return el.tagName === s.toUpperCase();
    }
    return false;
  }

  function queryOnList(list, selector) {
    const parts = String(selector).split(',').map((p) => p.trim()).filter(Boolean);
    return list.filter((el) => parts.some((p) => matchesSelector(el, p)));
  }

  function makeContainer(id = '') {
    const box = {
      id,
      isConnected: true,
      style: {},
      _html: '',
      _elements: [],
      get innerHTML() {
        return this._html;
      },
      set innerHTML(val) {
        for (const old of this._elements) old.isConnected = false;
        this._html = String(val || '');
        this._elements = parseElementsFromHTML(this._html, this);
        // Handle dynamic select innerHTML updates like #bgm-source-select
        for (const el of this._elements) {
          if (el.tagName === 'SELECT') {
            let selHTML = '';
            Object.defineProperty(el, 'innerHTML', {
              get() { return selHTML; },
              set(v) {
                selHTML = String(v || '');
                const parsed = parseElementsFromHTML(`<select>${selHTML}</select>`, this);
                const innerSel = parsed[0];
                if (innerSel) {
                  el._options = innerSel._options;
                  el._hasSelectedOption = innerSel._hasSelectedOption;
                  if (el._options.length > 0) {
                    el.value = innerSel._hasSelectedOption ? innerSel.value : el._options[0].value;
                  }
                }
              },
              configurable: true,
            });
          } else if (el.tagName === 'DIV') {
            let divHTML = '';
            Object.defineProperty(el, 'innerHTML', {
              get() { return divHTML; },
              set(v) {
                divHTML = String(v || '');
                const parsed = parseElementsFromHTML(divHTML, box);
                for (const old of el._subElements || []) old.isConnected = false;
                el._subElements = parsed;
                // Replace inside box._elements
                box._elements = box._elements.filter((x) => !x._subParent || x._subParent !== el);
                for (const p of parsed) {
                  p._subParent = el;
                  box._elements.push(p);
                }
              },
              configurable: true,
            });
            el.querySelectorAll = (sel) => queryOnList(el._subElements || [], sel);
            el.querySelector = (sel) => queryOnList(el._subElements || [], sel)[0] || null;
          }
        }
      },
      querySelectorAll(sel) {
        return queryOnList(this._elements, sel);
      },
      querySelector(sel) {
        return queryOnList(this._elements, sel)[0] || null;
      },
    };
    containers.add(box);
    return box;
  }

  const createdAnchors = [];
  const doc = {
    activeElement: null,
    body: {
      appendChild() {},
    },
    createElement(tag) {
      const el = { tagName: String(tag).toUpperCase(), style: {}, click() { if (el.tagName === 'A') createdAnchors.push(el); }, remove() {} };
      return el;
    },
    getElementById(id) {
      for (const c of containers) {
        if (c.id === id) return c;
        for (const el of c._elements) {
          if (el.id === id) return el;
        }
      }
      return null;
    },
    querySelectorAll(sel) {
      const out = [];
      for (const c of containers) out.push(...queryOnList(c._elements, sel));
      return out;
    },
    querySelector(sel) {
      return doc.querySelectorAll(sel)[0] || null;
    },
  };

  return { doc, makeContainer, createdAnchors };
}

function flushMicrotasks() {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

function loadAdminScraperContext({ apiImpl, confirmResult = true, stateInit = {} } = {}) {
  const start = appSource.indexOf('  // --- Admin Bangumi Scraper Center ---');
  const end = appSource.indexOf('  // --- Admin Jobs Tab (Adaptive Polling ONLY when active!) ---', start);
  assert.ok(start >= 0 && end > start, 'expected Admin Bangumi Scraper Center boundaries');

  const { doc, makeContainer, createdAnchors } = createFakeDOM();
  const body = makeContainer('admin-tab-body');
  const modalSubhead = makeContainer('modal-subhead');
  const modalBody = makeContainer('modal-body');
  const modalFoot = makeContainer('modal-foot');

  const pollCalls = [];
  let stopPollCalls = 0;
  let lastModal = null;
  const toasts = [];
  const confirmCalls = [];
  const blobDownloads = [];

  class FakeBlob {
    constructor(parts, opts) {
      this.parts = parts;
      this.opts = opts;
    }
  }

  const ctx = vm.createContext({
    document: doc,
    location: { origin: 'http://127.0.0.1:25600', pathname: '/admin', search: '?tab=bangumi' },
    state: Object.assign({
      auth: { userId: 'admin', isAdmin: true },
      adminTab: 'bangumi',
      bgmProxyProbeHTML: '',
      bgmAgentIncludeNsfw: false,
      taxonomies: { loaded: true },
      extSourceNames: {},
      extMetaGuideOpen: false,
      extMetaRepoDraft: null,
      extMetaBatchDraft: null,
      extMetaExportDraft: null,
    }, stateInit),
    esc: (v) => (v == null ? '' : String(v)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;')),
    fmtBytes: (n) => `${Number(n || 0)} B`,
    fmtTime: (t) => t || '—',
    providerLabel: (p) => p || 'local',
    toast: (msg, kind = 'ok') => toasts.push({ msg, kind }),
    confirmModal: async (msg, opts) => {
      confirmCalls.push({ msg, opts });
      return confirmResult;
    },
    openModal: (opts) => {
      lastModal = opts;
      modalSubhead.innerHTML = opts.subheadHTML || '';
      modalBody.innerHTML = opts.bodyHTML || '';
      modalFoot.innerHTML = opts.footHTML || '';
      if (opts.onMount) opts.onMount(modalBody);
    },
    closeModal: () => {
      modalSubhead.innerHTML = '';
      modalBody.innerHTML = '';
      modalFoot.innerHTML = '';
    },
    schedulePoll: (fn, ms) => pollCalls.push({ fn, ms }),
    stopPolling: () => { stopPollCalls += 1; },
    api: apiImpl,
    Blob: FakeBlob,
    URL: {
      createObjectURL: (b) => {
        blobDownloads.push(b);
        return 'blob:fake-url';
      },
      revokeObjectURL: () => {},
    },
    setTimeout,
  });

  const exports = vm.runInContext(
    appSource.slice(start, end) + '\n({ renderAdminBangumi, paintAdminBangumi, openExtMetaSourceModal, openExtMetaAddSourceModal, openLibraryChainModal, openMappingExportPreviewModal });',
    ctx,
  );
  return {
    ...exports,
    ctx,
    doc,
    body,
    modalBody,
    modalFoot,
    pollCalls,
    getStopPollCalls: () => stopPollCalls,
    getLastModal: () => lastModal,
    toasts,
    confirmCalls,
    blobDownloads,
    createdAnchors,
  };
}

function sampleBangumiStatus() {
  return {
    totalSeries: 100,
    matchedCount: 80,
    pendingCount: 10,
    unmatchedCount: 10,
    nsfwCount: 4,
    showNsfw: false,
    proxyUrl: '',
    archive: { ready: true, subjectCount: 50000, updating: false },
    archiveSource: { source: 'zh_cn', customUrl: '' },
    archiveSchedule: { enabled: false, frequency: 'weekly', dayOfWeek: 4, timeOfDay: '06:00' },
  };
}

function sampleOverview(overrides = {}) {
  return Object.assign({
    recommendedRepoUrl: 'https://raw.githubusercontent.com/BlackMuseCode/mangaalpha-metadata-repo/main/repo.json',
    repos: [
      {
        id: 'repo-1',
        name: 'Community Open Metadata',
        url: 'https://raw.githubusercontent.com/BlackMuseCode/mangaalpha-metadata-repo/main/repo.json',
        repoVersion: 'v1.0',
        description: 'Open metadata sources',
        homepage: 'https://github.com/BlackMuseCode/mangaalpha-metadata-repo',
        enabled: true,
        lastSyncedAt: '2026-11-25T09:00:00Z',
        lastError: '',
      },
    ],
    sources: [
      {
        id: 'src-dict',
        repoId: 'repo-1',
        sourceKey: 'zh-tags',
        kind: 'tag_dictionary',
        schemaType: 'tag_translation_text_v1',
        name: '中文标签词典',
        downloadUrl: 'https://example.test/db.text.json.gz',
        enabled: true,
        ready: true,
        entryCount: 12000,
        fileSize: 1024000,
        autoUpdate: 'weekly',
        tagDisplayMode: 'zh',
        lastUpdatedAt: '2026-11-25T09:05:00Z',
        lastError: '',
      },
      {
        id: 'src-sqlite-1',
        repoId: 'repo-1',
        sourceKey: 'open-gallery',
        kind: 'offline_sqlite',
        schemaType: 'gallery_v1',
        name: '通用离线图库 A',
        downloadUrl: 'https://example.test/gallery.sqlite.zst',
        localPath: '/data/metadata/gallery.sqlite',
        tagDictSourceId: 'src-dict',
        tagDisplayMode: 'zh',
        autoUpdate: 'manual',
        onlineApiUrl: '',
        onlineEnabled: false,
        enabled: true,
        ready: true,
        entryCount: 450000,
        fileSize: 52428800,
        lastUpdatedAt: '2026-11-25T09:10:00Z',
        lastError: '',
      },
      {
        id: 'src-bgm-zh',
        repoId: 'repo-1',
        sourceKey: 'bgm-zh',
        kind: 'bangumi_archive',
        schemaType: 'bangumi_archive',
        name: 'Bangumi 中文增强库',
        enabled: true,
        ready: true,
        entryCount: 50000,
        autoUpdate: 'weekly',
        tagDisplayMode: 'zh',
      },
    ],
    libraryChains: {
      'lib-1': ['src-sqlite-1'],
    },
    activeSyncs: [],
    onlineEnrichStatus: {
      running: false,
      circuitOpen: false,
      cooldownUntil: '',
      processed: 0,
      matched: 0,
      failed: 0,
      lastError: '',
    },
  }, overrides);
}

const sampleLibraries = [
  { id: 'lib-1', name: '本地漫画库', providerType: 'local', enabled: true },
  { id: 'lib-2', name: 'WebDAV 书库', providerType: 'webdav', enabled: true },
  { id: 'lib-union', name: '联合书库', providerType: 'union', enabled: true },
  { id: 'lib-member', name: '联合子库', providerType: 'local', unionParentId: 'lib-union', enabled: false },
  { id: 'lib-hidden', name: '已隐藏独立库', providerType: 'local', enabled: false },
];

test('recommended repo pre-fill populates input without auto-submitting, then subscribes on confirm', async () => {
  const requests = [];
  const h = loadAdminScraperContext({
    apiImpl: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/bangumi/status') return sampleBangumiStatus();
      if (url.startsWith('/admin/api/bangumi/runs')) return [];
      if (url === '/admin/api/libraries') return sampleLibraries;
      if (url.startsWith('/admin/api/bangumi/tracker')) return {};
      if (url === '/admin/api/metadata/overview') return sampleOverview({ repos: [] });
      if (url === '/admin/api/metadata/scrape-runs') return [];
      return {};
    },
  });

  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, sampleOverview({ repos: [] }), []);
  assert.equal(h.pollCalls.length, 0, 'idle page must not schedule polling');
  assert.ok(h.getStopPollCalls() >= 1, 'idle page must call stopPolling');

  const repoInput = h.doc.getElementById('extmeta-repo-url');
  const fillBtn = h.doc.getElementById('extmeta-repo-fill-rec');
  const subBtn = h.doc.getElementById('extmeta-repo-subscribe-btn');

  assert.equal(repoInput.value, '');
  fillBtn.onclick();
  assert.equal(
    repoInput.value,
    'https://raw.githubusercontent.com/BlackMuseCode/mangaalpha-metadata-repo/main/repo.json',
  );
  assert.equal(requests.length, 0, 'filling recommended URL must NOT auto-submit');

  await subBtn.onclick();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/repos',
    'POST',
    { url: 'https://raw.githubusercontent.com/BlackMuseCode/mangaalpha-metadata-repo/main/repo.json' },
  ]);
});

test('source config modal, custom source creation, and sync invoke exact API contracts', async () => {
  const requests = [];
  const h = loadAdminScraperContext({
    apiImpl: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/bangumi/status') return sampleBangumiStatus();
      if (url.startsWith('/admin/api/bangumi/runs')) return [];
      if (url === '/admin/api/libraries') return sampleLibraries;
      if (url.startsWith('/admin/api/bangumi/tracker')) return {};
      if (url === '/admin/api/metadata/overview') return sampleOverview();
      if (url === '/admin/api/metadata/scrape-runs') return [];
      return {};
    },
  });

  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, sampleOverview(), []);

  // 1. Edit existing source config
  const editBtns = h.body.querySelectorAll('button[data-extmeta-src-edit="src-sqlite-1"]');
  assert.equal(editBtns.length, 1);
  editBtns[0].onclick();

  const emsImportCovers = h.doc.getElementById('ems-import-covers');
  assert.ok(emsImportCovers, 'offline_sqlite edit modal must show #ems-import-covers');
  assert.equal(emsImportCovers.checked, false, 'importCovers defaults to false when unset');
  assert.match(h.modalBody.innerHTML, /匹配时同时导入内嵌封面到本地/);
  assert.match(h.modalBody.innerHTML, /开启后将命中作品的内嵌封面复制到本地缓存，后续删除或替换源库不影响已导入封面。/);
  assert.doesNotMatch(h.modalBody.innerHTML, /原图不压缩/);
  assert.doesNotMatch(h.modalBody.innerHTML, /默认不选/);

  h.doc.getElementById('ems-enabled').checked = false;
  h.doc.getElementById('ems-tag-dict').value = 'src-dict';
  h.doc.getElementById('ems-tag-mode').value = 'bilingual';
  h.doc.getElementById('ems-auto-update').value = 'weekly';
  h.doc.getElementById('ems-local-path').value = '/data/metadata/custom.sqlite';
  h.doc.getElementById('ems-download-url').value = 'https://example.test/v2.sqlite.zst';
  h.doc.getElementById('ems-online-url').value = 'https://api.example.test/lookup';
  h.doc.getElementById('ems-online-enabled').checked = true;
  emsImportCovers.checked = true;
  await h.doc.getElementById('ems-save-btn').onclick();

  assert.deepEqual(requests[0], [
    '/admin/api/metadata/sources/src-sqlite-1',
    'PUT',
    {
      enabled: false,
      tagDictSourceId: 'src-dict',
      tagDisplayMode: 'bilingual',
      autoUpdate: 'weekly',
      localPath: '/data/metadata/custom.sqlite',
      downloadUrl: 'https://example.test/v2.sqlite.zst',
      onlineApiUrl: 'https://api.example.test/lookup',
      onlineEnabled: true,
      importCovers: true,
    },
  ]);

  // 2. Add custom source
  requests.length = 0;
  h.doc.getElementById('extmeta-source-add-btn').onclick();
  const emasImportCovers = h.doc.getElementById('emas-import-covers');
  assert.ok(emasImportCovers, 'add source modal must show #emas-import-covers for offline_sqlite');
  assert.equal(emasImportCovers.checked, false);
  assert.match(h.modalBody.innerHTML, /匹配时同时导入内嵌封面到本地/);
  assert.doesNotMatch(h.modalBody.innerHTML, /原图不压缩/);
  assert.doesNotMatch(h.modalBody.innerHTML, /默认不选/);

  h.doc.getElementById('emas-name').value = '本地单表源';
  h.doc.getElementById('emas-kind').value = 'offline_sqlite';
  h.doc.getElementById('emas-schema').value = 'mangaalpha_meta_v1';
  h.doc.getElementById('emas-download-url').value = 'https://example.test/meta.sqlite.zst';
  h.doc.getElementById('emas-local-path').value = '/data/meta.sqlite';
  h.doc.getElementById('emas-online-url').value = '';
  h.doc.getElementById('emas-tag-dict').value = 'src-dict';
  h.doc.getElementById('emas-tag-mode').value = 'zh';
  h.doc.getElementById('emas-auto-update').value = 'monthly';
  h.doc.getElementById('emas-enabled').checked = true;
  await h.doc.getElementById('emas-create-btn').onclick();

  assert.deepEqual(requests[0], [
    '/admin/api/metadata/sources',
    'POST',
    {
      name: '本地单表源',
      kind: 'offline_sqlite',
      schemaType: 'mangaalpha_meta_v1',
      downloadUrl: 'https://example.test/meta.sqlite.zst',
      localPath: '/data/meta.sqlite',
      onlineApiUrl: '',
      tagDictSourceId: 'src-dict',
      tagDisplayMode: 'zh',
      autoUpdate: 'monthly',
      enabled: true,
      importCovers: false,
    },
  ]);

  // 3. Force sync
  requests.length = 0;
  const syncBtns = h.body.querySelectorAll('button[data-extmeta-src-sync="src-sqlite-1"]');
  const forceBtn = syncBtns.find((b) => b.getAttribute('data-force') === 'true');
  assert.ok(forceBtn);
  await forceBtn.onclick();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/sources/src-sqlite-1/sync',
    'POST',
    { force: true },
  ]);
});

test('library priority chain modal reorders sources and saves ordered sourceIds', async () => {
  const requests = [];
  const h = loadAdminScraperContext({
    apiImpl: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/bangumi/status') return sampleBangumiStatus();
      if (url.startsWith('/admin/api/bangumi/runs')) return [];
      if (url === '/admin/api/libraries') return sampleLibraries;
      if (url.startsWith('/admin/api/bangumi/tracker')) return {};
      if (url === '/admin/api/metadata/overview') return sampleOverview();
      if (url === '/admin/api/metadata/scrape-runs') return [];
      return {};
    },
  });

  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, sampleOverview(), []);

  // Top-level libraries (including union library and hidden top-level libraries) appear in chains & export;
  // union member libraries (unionParentId set) must NOT appear.
  assert.equal(h.body.querySelectorAll('button[data-extmeta-chain-edit="lib-union"]').length, 1);
  assert.equal(h.body.querySelectorAll('button[data-extmeta-chain-edit="lib-member"]').length, 0);
  assert.equal(h.body.querySelectorAll('input[data-extmeta-export-lib="lib-union"]').length, 1);
  assert.equal(h.body.querySelectorAll('input[data-extmeta-export-lib="lib-member"]').length, 0);
  assert.match(h.body.innerHTML, /已隐藏独立库[\s\S]*已隐藏/);

  // Union library chain can be edited and saved
  const editUnionBtn = h.body.querySelector('button[data-extmeta-chain-edit="lib-union"]');
  assert.ok(editUnionBtn);
  editUnionBtn.onclick();
  const unionSrcCb = h.modalBody.querySelector('input[data-mchain-id="src-sqlite-1"]');
  assert.ok(unionSrcCb);
  unionSrcCb.checked = true;
  unionSrcCb.onchange();
  await h.doc.getElementById('mchain-save').onclick();
  assert.deepEqual(requests[0], [
    '/admin/api/libraries/lib-union/metadata-sources',
    'PUT',
    { sourceIds: ['src-sqlite-1'] },
  ]);

  requests.length = 0;
  const editChainBtn = h.body.querySelector('button[data-extmeta-chain-edit="lib-1"]');
  assert.ok(editChainBtn);
  editChainBtn.onclick();

  // Currently selected: ['src-sqlite-1']. Check 'bangumi' then move it up to #1.
  // Duplicate bangumi_archive market sources like 'src-bgm-zh' must NOT appear in the chain modal.
  assert.equal(h.modalBody.querySelector('input[data-mchain-id="src-bgm-zh"]'), null);
  const bgmCb = h.modalBody.querySelector('input[data-mchain-id="bangumi"]');
  assert.ok(bgmCb);
  bgmCb.checked = true;
  bgmCb.onchange();

  const moveUpBtn = h.modalBody.querySelectorAll('button[data-mchain-move="bangumi"]')
    .find((b) => b.getAttribute('data-dir') === '-1');
  assert.ok(moveUpBtn);
  moveUpBtn.onclick();

  await h.doc.getElementById('mchain-save').onclick();
  assert.deepEqual(requests[0], [
    '/admin/api/libraries/lib-1/metadata-sources',
    'PUT',
    { sourceIds: ['bangumi', 'src-sqlite-1'] },
  ]);
});

test('multi-source batch scrape defaults allowOnline=false and polls ONLY while running', async () => {
  const requests = [];
  const h = loadAdminScraperContext({
    apiImpl: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/bangumi/status') return sampleBangumiStatus();
      if (url.startsWith('/admin/api/bangumi/runs')) return [];
      if (url === '/admin/api/libraries') return sampleLibraries;
      if (url.startsWith('/admin/api/bangumi/tracker')) return {};
      if (url === '/admin/api/metadata/overview') return sampleOverview();
      if (url === '/admin/api/metadata/scrape-runs') return [];
      return {};
    },
  });

  // Active sync triggers polling & displays progress bar
  const runningOverview = sampleOverview({
    activeSyncs: [{
      jobId: 'sync-1',
      sourceId: 'src-sqlite-1',
      status: 'running',
      phase: 'building_sidecar',
      downloadedBytes: 512,
      totalBytes: 1024,
      indexedEntries: 1200,
      error: '',
    }],
  });
  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, runningOverview, []);
  assert.equal(h.pollCalls.length, 1, 'active sync must schedule polling');
  assert.match(h.body.innerHTML, /building_sidecar/);
  assert.match(h.body.innerHTML, /50%/);

  // Batch scrape hides extmeta-batch-online when no enabled offline_sqlite has onlineApiUrl and defaults allowOnline = false
  assert.equal(h.doc.getElementById('extmeta-batch-online'), null);

  // Default selects all 3 unhidden top-level libraries; uncheck others so only lib-1 remains selected
  const selectAllEl = h.doc.getElementById('extmeta-batch-select-all');
  assert.equal(selectAllEl.checked, true);
  assert.match(h.body.innerHTML, /已选 3 \/ 3/);
  for (const libId of ['lib-2', 'lib-union']) {
    const cb = h.body.querySelector(`input[data-extmeta-batch-lib="${libId}"]`);
    cb.checked = false;
    cb.onchange();
  }
  assert.equal(selectAllEl.checked, false);
  assert.equal(selectAllEl.indeterminate, true);
  assert.equal(h.doc.getElementById('extmeta-batch-selected-count').textContent, '已选 1 / 3');

  await h.doc.getElementById('extmeta-batch-start-retry').onclick();

  assert.deepEqual(requests[0], [
    '/admin/api/metadata/scrape',
    'POST',
    { libraryIds: ['lib-1'], libraryId: 'lib-1', mode: 'retry_unmatched', allowOnline: false },
  ]);

  // Idle state cancels polling and preserves user's partial checkbox selection across repaints
  const stopBefore = h.getStopPollCalls();
  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, sampleOverview(), []);
  assert.equal(h.pollCalls.length, 1, 'idle repaint must not add new poll timer');
  assert.ok(h.getStopPollCalls() > stopBefore, 'idle repaint must stop polling');
  assert.match(h.body.innerHTML, /已选 1 \/ 3/);
  assert.equal(h.body.querySelector('input[data-extmeta-batch-lib="lib-1"]').checked, true);
  assert.equal(h.body.querySelector('input[data-extmeta-batch-lib="lib-2"]').checked, false);
});

test('paintAdminBangumi aligns with backend ov.syncs, indexed, ov.onlineEnrich cooldown, and scrapeRuns target names', async () => {
  const h = loadAdminScraperContext({
    apiImpl: async () => ({}),
  });

  // 1. Backend canonical ov.syncs (with `indexed`) + ov.onlineEnrich (status: 'cooldown', circuitOpen: true, cooldownUntil)
  const cooldownOverview = sampleOverview({
    activeSyncs: undefined,
    onlineEnrichStatus: undefined,
    syncs: [{
      id: 'sync-canonical-1',
      sourceId: 'src-sqlite-1',
      status: 'running',
      phase: 'indexing',
      downloadedBytes: 512,
      totalBytes: 1024,
      indexed: 4321,
    }],
    onlineEnrich: {
      runId: 'scrape-1',
      status: 'cooldown',
      circuitOpen: true,
      total: 12,
      processed: 4,
      matched: 3,
      failed: 1,
      httpStatus: 429,
      cooldownUntil: '2026-11-25T10:30:00Z',
      lastError: 'HTTP 429 Too Many Requests',
    },
  });
  const runsPayload = {
    runs: [{
      id: 'run-union-1',
      libraryId: 'lib-union',
      mode: 'incremental',
      status: 'completed',
      totalSeries: 10,
      processedSeries: 10,
      matchedCount: 9,
      unmatchedCount: 1,
      skippedCount: 0,
      startedAt: '2026-11-25T10:00:00Z',
    }],
  };
  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, cooldownOverview, runsPayload);
  assert.equal(h.pollCalls.length, 1, 'canonical syncs & onlineEnrich cooldown must trigger polling');
  assert.equal(h.pollCalls[0].ms, 2200);
  assert.match(h.body.innerHTML, /建索引中/);
  assert.match(h.body.innerHTML, /50%/);
  assert.match(h.body.innerHTML, /已建索引 4,321 条/);
  assert.match(h.body.innerHTML, /在线补全已熔断保护/);
  assert.match(h.body.innerHTML, /已处理 4 \/ 12 · 命中 3 · 失败 1/);
  assert.match(h.body.innerHTML, /HTTP 429/);
  assert.match(h.body.innerHTML, /冷却至：2026-11-25T10:30:00Z/);
  assert.match(h.body.innerHTML, /HTTP 429 Too Many Requests/);
  assert.match(h.body.innerHTML, /<td><strong>联合书库<\/strong><\/td>/);

  // 2. Pure backend OnlineStatus cooldown (only status === 'cooldown', no circuitOpen/running/matched/failed) + syncing without phase
  const pureBackendCooldown = sampleOverview({
    activeSyncs: undefined,
    onlineEnrichStatus: undefined,
    syncs: [{
      id: 'sync-canonical-2',
      sourceId: 'src-sqlite-1',
      status: 'running',
      downloadedBytes: 256,
      totalBytes: 1024,
      indexed: 100,
    }],
    onlineEnrich: {
      status: 'cooldown',
      total: 8,
      processed: 2,
      httpStatus: 429,
      cooldownUntil: '2026-11-25T10:35:00Z',
    },
  });
  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, pureBackendCooldown, []);
  assert.equal(h.pollCalls.length, 2, 'pure backend onlineEnrich cooldown must trigger polling');
  assert.match(h.body.innerHTML, /同步状态：同步中/);
  assert.match(h.body.innerHTML, /在线补全已熔断保护/);
  assert.match(h.body.innerHTML, /已处理 2 \/ 8/);

  // 3. Pure idle backend overview must not render enrich banner or poll
  const stopBefore = h.getStopPollCalls();
  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, sampleOverview({
    activeSyncs: undefined,
    onlineEnrichStatus: undefined,
    syncs: [],
    onlineEnrich: { status: 'idle', total: 0, processed: 0 },
  }), []);
  assert.equal(h.pollCalls.length, 2, 'idle canonical overview must not schedule polling');
  assert.ok(h.getStopPollCalls() > stopBefore, 'idle canonical overview must stop polling');
  assert.doesNotMatch(h.body.innerHTML, /extmeta-enrich-banner/);
});

test('sanitized mapping export defaults includeNsfw=false and invokes preview:true before download', async () => {
  const requests = [];
  const previewPayload = {
    count: 2,
    excludedNsfw: 5,
    skippedPrivacy: 1,
    sample: [
      {
        schema: 'mangaalpha_mapping_v1',
        key: '水星领航员',
        rawKey: 'ARIA',
        author: '天野梢',
        targetSource: 'bangumi',
        targetId: '539',
        isNsfw: false,
        origin: 'manual',
      },
      {
        schema: 'mangaalpha_mapping_v1',
        key: '魔法少女',
        rawKey: 'Mahou Shoujo',
        author: '作者A',
        targetSource: 'mangaalpha_meta_v1',
        targetId: 'm-1',
        isNsfw: false,
        origin: 'agent',
      },
    ],
    ndjson: '{"schema":"mangaalpha_mapping_v1","key":"水星领航员","targetSource":"bangumi","targetId":"539"}\n',
  };

  const h = loadAdminScraperContext({
    apiImpl: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/metadata/mappings/export') return previewPayload;
      return {};
    },
  });

  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, sampleOverview(), []);

  const includeNsfwCb = h.doc.getElementById('extmeta-export-include-nsfw');
  assert.equal(includeNsfwCb.checked, false, 'includeNsfw must default to unchecked');

  const originCbs = h.body.querySelectorAll('input[data-extmeta-export-origin]');
  const checkedOrigins = originCbs.filter((c) => c.checked).map((c) => c.value);
  assert.deepEqual(checkedOrigins, ['manual', 'agent']);

  await h.doc.getElementById('extmeta-export-preview-btn').onclick();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/mappings/export',
    'POST',
    {
      libraryIds: [],
      origins: ['manual', 'agent'],
      includeNsfw: false,
      preview: true,
    },
  ]);

  assert.match(h.modalBody.innerHTML, /可导出脱敏条数：2/);
  assert.match(h.modalBody.innerHTML, /已排除 NSFW：5/);
  assert.match(h.modalBody.innerHTML, /水星领航员/);

  await h.doc.getElementById('extmeta-export-download-btn').onclick();
  assert.equal(h.blobDownloads.length, 1);
  assert.equal(h.createdAnchors.length, 1);
  assert.equal(h.createdAnchors[0].download, 'mangaalpha-mappings.ndjson');
});

test('series match modal switches to ready external source, queries candidates, binds, and unbinds', async () => {
  const modalStart = appSource.indexOf('  // --- Bangumi Match Modal ---');
  const modalEnd = appSource.indexOf('  // --- Metadata Edit Modal ---', modalStart);
  const detailStart = appSource.indexOf('  // --- Series Detail View (/series/:id) ---');
  assert.ok(detailStart >= 0 && modalStart > detailStart && modalEnd > modalStart);

  const requests = [];
  const { doc, makeContainer } = createFakeDOM();
  const pageContent = makeContainer('page-content');
  const modalSubhead = makeContainer('modal-subhead');
  const modalBody = makeContainer('modal-body');
  const modalFoot = makeContainer('modal-foot');
  let detailRenders = 0;

  const ctx = vm.createContext({
    document: doc,
    location: { origin: 'http://127.0.0.1:25600', pathname: '/series/ser-1', search: '' },
    routeSeq: 1,
    state: {
      auth: { userId: 'admin', isAdmin: true },
      coverVer: 1,
      taxonomies: { loaded: true },
      extSourceNames: {},
    },
    isAdminUser: () => true,
    renderShell: () => { detailRenders += 1; },
    providerLabel: (p) => p || 'local',
    renderSeriesTrackerBadge: () => '',
    loadSeriesVersions: () => {},
    loadSeriesCollectionsBar: () => {},
    bindMaintCancelButtons: () => {},
    openMetadataEditModal: () => {},
    openAddSeriesToCollectionModal: () => {},
    openAddBookToReadListModal: () => {},
    schedulePoll: () => {},
    stopPolling: () => {},
    fmtBytes: (n) => `${n} B`,
    esc: (v) => (v == null ? '' : String(v)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;')),
    toast: () => {},
    confirmModal: async () => true,
    openModal: (opts) => {
      modalSubhead.innerHTML = opts.subheadHTML || '';
      modalBody.innerHTML = opts.bodyHTML || '';
      modalFoot.innerHTML = opts.footHTML || '';
      const coverStart = doc.getElementById('cover-start');
      if (coverStart && !coverStart.classList) coverStart.classList = { toggle() {} };
      if (opts.onMount) opts.onMount(modalBody);
    },
    closeModal: () => {},
    api: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/metadata/overview') {
        return sampleOverview();
      }
      if (url === '/admin/api/series/ser-1/detail') {
        return detail;
      }
      if (url.startsWith('/admin/api/maintenance-jobs')) {
        return [];
      }
      if (url.startsWith('/admin/api/series/ser-1/bangumi/candidates')) {
        return { parsed: { candidates: ['测试作品'] }, candidates: [] };
      }
      if (url.startsWith('/admin/api/series/ser-1/metadata/candidates')) {
        return {
          sourceId: 'src-sqlite-1',
          query: 'https://gallery.example.test/g/12345/abc/',
          candidates: [
            {
              sourceId: 'src-sqlite-1',
              entryId: '12345',
              title: '测试外部条目',
              titleAlt: 'Test External Work',
              authors: ['画师甲'],
              category: 'Non-H',
              tags: ['中文译名标签', '全彩'],
              summary: '外部条目的简介内容',
              coverUrl: 'https://cdn.example.test/thumb/12345.jpg',
              rating: 4.7,
              isNsfw: false,
              score: 0.96,
              matchReason: 'exact_id',
            },
          ],
        };
      }
      return {};
    },
  });

  const { paintSeriesDetail, openBangumiMatchModal } = vm.runInContext(
    appSource.slice(detailStart, modalEnd) + '\n({ paintSeriesDetail, openBangumiMatchModal });',
    ctx,
  );

  const detail = {
    series: { id: 'ser-1', oneshot: false, booksCount: 2, booksReadCount: 0, booksInProgressCount: 0, metadata: { title: '测试作品', status: 'ONGOING', genres: [], tags: [], alternateTitles: [] } },
    rawMeta: { bgmSubjectId: 0, extSourceId: 'src-sqlite-1', extEntryId: '12345', matchOrigin: 'manual', authors: [] },
    originalName: '测试作品',
    relativePath: '测试作品',
    totalSize: '12 MB',
    library: { id: 'lib-1', name: '本地漫画库', providerType: 'local' },
    books: [],
    bookExtras: {},
  };

  // 1. Series detail shows external source badge & unbind button
  paintSeriesDetail('ser-1', detail, []);
  assert.match(pageContent.innerHTML, /扩展源：/);
  const clearDetailBtn = doc.getElementById('btn-clear-ext-meta');
  assert.ok(clearDetailBtn);
  await clearDetailBtn.onclick();
  assert.deepEqual(requests[0], ['/admin/api/series/ser-1/metadata/bind', 'DELETE', null]);

  // 2. Match modal opens on Bangumi default, discovers ready offline source, switches, queries & binds
  openBangumiMatchModal('ser-1', detail);
  await flushMicrotasks();

  const sourceWrap = doc.getElementById('bgm-source-switcher-wrap');
  const sourceSel = doc.getElementById('bgm-source-select');
  assert.equal(sourceWrap.style.display, '');
  assert.equal(sourceSel.value, 'bangumi');

  requests.length = 0;
  sourceSel.value = 'src-sqlite-1';
  doc.getElementById('bgm-search-q').value = 'https://gallery.example.test/g/12345/abc/';
  sourceSel.onchange();
  await flushMicrotasks();

  assert.equal(
    requests[0][0],
    '/admin/api/series/ser-1/metadata/candidates?sourceId=src-sqlite-1&q=https%3A%2F%2Fgallery.example.test%2Fg%2F12345%2Fabc%2F',
  );
  const candidatesHTML = doc.getElementById('bgm-candidates-box').innerHTML;
  const candImg = modalBody.querySelector('img');
  assert.ok(candImg, 'candidate cover img should exist');
  assert.equal(candImg.getAttribute('src'), '/admin/api/metadata/sources/src-sqlite-1/entries/12345/cover');
  assert.doesNotMatch(candidatesHTML, /\?u=/);
  assert.match(candidatesHTML, /中文译名标签/);
  assert.match(candidatesHTML, /匹配度 96%/);

  const bindBtn = modalBody.querySelector('button[data-bind-ext-entry="12345"]');
  assert.ok(bindBtn);
  await bindBtn.onclick();
  assert.deepEqual(requests[1], [
    '/admin/api/series/ser-1/metadata/bind',
    'POST',
    { sourceId: 'src-sqlite-1', entryId: '12345' },
  ]);
  assert.equal('origin' in requests[1][2], false, 'bind payload must not include origin');

  // 3. Modal unbind button in fresh open modal calls DELETE /metadata/bind
  openBangumiMatchModal('ser-1', detail);
  await flushMicrotasks();
  requests.length = 0;
  const clearModalBtn = doc.getElementById('extmeta-clear-btn');
  assert.ok(clearModalBtn);
  await clearModalBtn.onclick();
  assert.deepEqual(requests[0], ['/admin/api/series/ser-1/metadata/bind', 'DELETE', null]);
  assert.ok(detailRenders >= 3);
});

test('app.css external metadata classes use theme variables without hardcoded colors', () => {
  const idx = cssSource.indexOf('/* External Metadata & Multi-Source Scraper UI */');
  assert.ok(idx >= 0, 'expected new CSS section in app.css');
  const addedCSS = cssSource.slice(idx);
  assert.doesNotMatch(addedCSS, /#[0-9a-fA-F]{3,8}\b|rgb\(|rgba\(/);
});

test('footer separator renders pure CSS circle using currentColor without middle-dot glyph', () => {
  assert.match(
    htmlSource,
    /<span id="site-footer-sep" class="site-footer-sep" hidden aria-hidden="true"><\/span>/,
  );
  assert.doesNotMatch(htmlSource, /id="site-footer-sep"[^>]*>·</);

  const sepRuleMatch = cssSource.match(/\.site-footer-sep\s*\{([^}]*)\}/);
  assert.ok(sepRuleMatch, 'expected .site-footer-sep rule in app.css');
  const ruleBody = sepRuleMatch[1];
  assert.match(ruleBody, /width:\s*3px/);
  assert.match(ruleBody, /height:\s*3px/);
  assert.match(ruleBody, /border-radius:\s*50%/);
  assert.match(ruleBody, /background:\s*currentColor/);
  assert.match(ruleBody, /opacity:\s*0\.72/);
});

test('copy unification removes engine-one and Mihon-style phrasing and orders scraper cards properly', () => {
  const h = loadAdminScraperContext({
    apiImpl: async () => ({}),
  });

  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, sampleOverview(), []);
  const html = h.body.innerHTML;

  // Forbidden legacy strings
  assert.doesNotMatch(html, /引擎一：/);
  assert.doesNotMatch(html, /Mihon\s*风格/);
  assert.doesNotMatch(appSource, /引擎一：/);
  assert.doesNotMatch(appSource, /Mihon\s*风格/);

  // Stat cards & filter dropdown unification
  assert.match(html, /<div class="k">已匹配<\/div>/);
  assert.match(html, /<div class="k">未匹配<\/div>/);
  assert.doesNotMatch(html, /<div class="k">已匹配 Bangumi<\/div>/);
  assert.doesNotMatch(html, /<div class="k">自动未命中<\/div>/);
  assert.match(appSource, /<option value="matched"[^>]*>已匹配<\/option>/);
  assert.match(appSource, /<option value="unmatched"[^>]*>未匹配<\/option>/);
  assert.match(appSource, /<div class="k">元数据已匹配<\/div>/);

  // Docs retain Mihon client links while removing Mihon-style market phrasing
  for (const docPath of ['README.md', 'docs/ARCHITECTURE.md', 'docs/METADATA_SOURCES.md']) {
    const docText = readFileSync(docPath, 'utf8');
    assert.doesNotMatch(docText, /Mihon\s*风格/, `${docPath} must not contain Mihon 风格`);
  }
  const readmeText = readFileSync('README.md', 'utf8');
  assert.match(readmeText, /https:\/\/mihon\.app\//);

  // Strict card order
  const posStats = html.indexOf('总系列数');
  const posGuide = html.indexOf('id="extmeta-guide-details"');
  const posNsfw = html.indexOf('id="bgm-nsfw-info-card"');
  const posProxy = html.indexOf('网络代理设置');
  const posBgmArch = html.indexOf('<h3>Bangumi 离线索引库</h3>');
  const posBatch = html.indexOf('id="extmeta-batch-card"');
  const posMarket = html.indexOf('id="extmeta-market-card"');
  const posChains = html.indexOf('id="extmeta-chains-card"');
  const posRuns = html.indexOf('id="extmeta-runs-card"');
  const posExport = html.indexOf('id="extmeta-export-card"');
  const posAgent = html.indexOf('使用 AI Agent 协助匹配未命中作品');
  const posTracker = html.indexOf('id="bgm-oauth-app-box"');

  assert.ok(posStats >= 0 && posGuide > posStats && posNsfw > posGuide && posProxy > posNsfw);
  assert.ok(html.includes('NSFW 分级识别与按账号隔离'));
  assert.ok(html.includes('/admin?tab=users') && html.includes('/account'));
  assert.equal(h.doc.getElementById('bgm-show-nsfw'), null);
  assert.ok(posBgmArch > posProxy && posBatch > posBgmArch && posMarket > posBatch);
  assert.ok(posChains > posMarket && posRuns > posChains && posExport > posRuns);
  assert.ok(posAgent > posExport && posTracker > posAgent);

  // Unique batch scrape card with 3 direct mode buttons and unique runs table
  assert.equal((html.match(/<h3>启动批量自动刮削<\/h3>/g) || []).length, 1);
  assert.equal((html.match(/<h3>刮削任务记录<\/h3>/g) || []).length, 1);
  assert.equal(h.doc.getElementById('bgm-batch-start'), null);
  assert.equal(h.doc.getElementById('bgm-refresh-btn'), null);
  assert.equal(h.doc.getElementById('extmeta-batch-lib'), null);
  assert.equal(h.doc.getElementById('extmeta-batch-mode'), null);
  assert.equal(h.doc.getElementById('extmeta-batch-start'), null);
  assert.equal(h.doc.getElementById('extmeta-batch-start-incremental').getAttribute('type'), 'button');
  assert.equal(h.doc.getElementById('extmeta-batch-start-retry').getAttribute('type'), 'button');
  assert.equal(h.doc.getElementById('extmeta-batch-start-force').getAttribute('type'), 'button');
  assert.match(html, /多源顺延链全链并集匹配/);
  assert.match(html, />增量匹配</);
  assert.match(html, />重试未命中</);
  assert.match(html, />全量重匹配</);
  assert.doesNotMatch(html, /原图不压缩/);
  assert.doesNotMatch(html, /默认不选/);
});

test('market hides duplicate bangumi_archive sources and chain modal collapses Bangumi into single option', () => {
  const h = loadAdminScraperContext({
    apiImpl: async () => ({}),
  });

  const twoBgmOverview = sampleOverview({
    sources: [
      ...sampleOverview().sources,
      {
        id: 'src-bgm-official',
        repoId: 'repo-1',
        sourceKey: 'bgm-official',
        kind: 'bangumi_archive',
        schemaType: 'bangumi_archive',
        name: 'Bangumi 官方归档源',
        enabled: true,
        ready: true,
        entryCount: 50000,
      },
    ],
  });

  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, twoBgmOverview, []);

  const marketCardHTML = h.doc.getElementById('extmeta-market-card')
    ? h.body.innerHTML.slice(
      h.body.innerHTML.indexOf('id="extmeta-market-card"'),
      h.body.innerHTML.indexOf('id="extmeta-chains-card"'),
    )
    : '';
  assert.match(marketCardHTML, /已配置扩展数据源（2）/);
  assert.match(marketCardHTML, /2 个已就绪/);
  assert.doesNotMatch(marketCardHTML, /Bangumi 中文增强库/);
  assert.doesNotMatch(marketCardHTML, /Bangumi 官方归档源/);
  assert.match(marketCardHTML, /中文标签词典/);
  assert.match(marketCardHTML, /通用离线图库 A/);

  // Priority chain edit modal only shows single 'bangumi' + offline_sqlite sources
  const editChainBtn = h.body.querySelector('button[data-extmeta-chain-edit="lib-1"]');
  assert.ok(editChainBtn);
  editChainBtn.onclick();

  const chainCheckboxes = h.modalBody.querySelectorAll('input[data-mchain-id]');
  assert.deepEqual(
    chainCheckboxes.map((c) => c.getAttribute('data-mchain-id')),
    ['src-sqlite-1', 'bangumi'],
  );
  assert.match(h.modalBody.innerHTML, /内置 Bangumi 源/);
  assert.doesNotMatch(h.modalBody.innerHTML, /Bangumi 中文增强库/);
  assert.doesNotMatch(h.modalBody.innerHTML, /Bangumi 官方归档源/);
});

test('unified scrape runs table merges, deduplicates, sorts descending, and cancels both engines', async () => {
  const requests = [];
  const h = loadAdminScraperContext({
    apiImpl: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET']);
      if (url === '/admin/api/bangumi/status') return sampleBangumiStatus();
      if (url.startsWith('/admin/api/bangumi/runs')) return [];
      if (url === '/admin/api/libraries') return sampleLibraries;
      if (url.startsWith('/admin/api/bangumi/tracker')) return {};
      if (url === '/admin/api/metadata/overview') return sampleOverview();
      if (url === '/admin/api/metadata/scrape-runs') return [];
      return {};
    },
  });

  const extRuns = [
    {
      id: 'ext-run-online',
      libraryId: 'lib-1',
      mode: 'retry_unmatched',
      allowOnline: true,
      status: 'running',
      totalSeries: 20,
      processedSeries: 5,
      matchedCount: 4,
      unmatchedCount: 1,
      skippedCount: 0,
      currentSeries: '正在刮削作品 A',
      startedAt: '2026-11-25T11:00:00Z',
    },
    {
      id: 'dup-run-id',
      libraryId: 'lib-2',
      mode: 'incremental',
      allowOnline: false,
      status: 'success',
      totalSeries: 10,
      processedSeries: 10,
      matchedCount: 8,
      unmatchedCount: 2,
      skippedCount: 0,
      startedAt: '2026-11-25T09:00:00Z',
    },
  ];
  const bgmRuns = [
    {
      id: 'bgm-run-active',
      libraryId: '',
      libraryName: '',
      mode: 'incremental',
      status: 'running',
      totalSeries: 40,
      processedSeries: 10,
      matchedCount: 9,
      unmatchedCount: 1,
      skippedCount: 0,
      currentSeries: '历史 Bangumi 作品 B',
      startedAt: '2026-11-25T10:00:00Z',
    },
    {
      id: 'dup-run-id',
      libraryId: 'lib-2',
      mode: 'force',
      status: 'failed',
      totalSeries: 10,
      processedSeries: 1,
      matchedCount: 0,
      unmatchedCount: 0,
      skippedCount: 0,
      startedAt: '2026-11-25T09:00:00Z',
    },
    {
      id: 'bgm-run-queued',
      libraryId: 'lib-1',
      mode: 'incremental',
      status: 'completed',
      totalSeries: 5,
      processedSeries: 5,
      matchedCount: 5,
      unmatchedCount: 0,
      skippedCount: 0,
      startedAt: null,
      createdAt: '2026-11-25T08:00:00Z',
    },
  ];

  h.paintAdminBangumi(h.body, sampleBangumiStatus(), bgmRuns, sampleLibraries, {}, sampleOverview(), extRuns);

  const runsHTML = h.body.innerHTML.slice(
    h.body.innerHTML.indexOf('id="extmeta-runs-card"'),
    h.body.innerHTML.indexOf('id="extmeta-export-card"'),
  );

  // Deduplicated by id (4 unique runs total, extmeta wins on dup-run-id)
  assert.equal((runsHTML.match(/<tr>/g) || []).length, 5); // 1 header + 4 rows
  assert.doesNotMatch(runsHTML, /badge-danger">failed</);

  // Sorted descending by startedAt || createdAt:
  // ext-run-online (11:00) -> bgm-run-active (10:00) -> dup-run-id (09:00) -> bgm-run-queued (08:00)
  const p1 = runsHTML.indexOf('正在刮削作品 A');
  const p2 = runsHTML.indexOf('历史 Bangumi 作品 B');
  const p3 = runsHTML.indexOf('WebDAV 书库');
  const p4 = runsHTML.indexOf('2026-11-25T08:00:00Z');
  assert.ok(p1 > 0 && p2 > p1 && p3 > p2 && p4 > p3, 'runs must be sorted newest first');

  // Engine badges
  assert.match(runsHTML, /多源 · 含在线补全/);
  assert.match(runsHTML, /多源 · 纯离线/);
  assert.match(runsHTML, /<span class="badge badge-bgm">Bangumi<\/span>/);

  // Cancel buttons trigger corresponding endpoints
  const extCancelBtn = h.body.querySelector('button[data-cancel-extmeta-run="ext-run-online"]');
  const bgmCancelBtn = h.body.querySelector('button[data-cancel-bgm-run="bgm-run-active"]');
  assert.ok(extCancelBtn);
  assert.ok(bgmCancelBtn);

  await extCancelBtn.onclick();
  assert.deepEqual(requests[0], ['/admin/api/metadata/scrape-runs/ext-run-online/cancel', 'POST']);

  requests.length = 0;
  await bgmCancelBtn.onclick();
  assert.deepEqual(requests[0], ['/admin/api/bangumi/runs/bgm-run-active/cancel', 'POST']);
});

test('library card scrape button renders as 匹配离线库, supports rematch shortcut, and decouples archive cover labels', async () => {
  const libStart = appSource.indexOf('  async function renderAdminLibraries(');
  const libEnd = appSource.indexOf('  async function browse115Drawer(', libStart);
  assert.ok(libStart >= 0 && libEnd > libStart, 'expected renderAdminLibraries boundaries');

  const { doc, makeContainer } = createFakeDOM();
  const body = makeContainer('admin-tab-body');
  const modalSubhead = makeContainer('modal-subhead');
  const modalBody = makeContainer('modal-body');
  const modalFoot = makeContainer('modal-foot');
  const requests = [];
  const toasts = [];
  const navigated = [];

  const libsFixture = [
    { id: 'lib-normal', name: '普通本地库', providerType: 'local', rootPath: '/manga', enabled: true, seriesCount: 12, bookCount: 34, comicBytes: 1024, metadataChainNeedsRematch: true },
    { id: 'lib-union', name: '漫画联合库', providerType: 'union', enabled: true, seriesCount: 20, bookCount: 50, comicBytes: 2048, unionLayers: [] },
  ];

  const ctx = vm.createContext({
    document: doc,
    navigator: { userAgent: 'Mozilla/5.0' },
    CSS: { escape: (v) => String(v) },
    WEEKDAY_NAMES: ['周日', '周一', '周二', '周三', '周四', '周五', '周六'],
    state: { auth: { userId: 'admin', isAdmin: true }, libraries: [] },
    esc: (v) => (v == null ? '' : String(v)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;')),
    fmtBytes: (n) => `${Number(n || 0)} B`,
    fmtTime: (t) => t || '—',
    providerLabel: (p) => p || 'local',
    formatScheduleSummary: () => '',
    toast: (msg, kind = 'ok') => toasts.push({ msg, kind }),
    confirmModal: async () => true,
    openModal: (opts) => {
      modalSubhead.innerHTML = opts.subheadHTML || '';
      modalBody.innerHTML = opts.bodyHTML || '';
      modalFoot.innerHTML = opts.footHTML || '';
      const coverStart = doc.getElementById('cover-start');
      if (coverStart && !coverStart.classList) coverStart.classList = { toggle() {} };
      if (opts.onMount) opts.onMount(modalBody);
    },
    closeModal: () => {},
    navigate: (path) => navigated.push(path),
    browse115Drawer: () => {},
    browseLocalDrawer: () => {},
    browseWebDAVDrawer: () => {},
    api: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/libraries') return libsFixture;
      if (url === '/admin/api/webdav-accounts') return [];
      if (url === '/admin/api/schedules') return { timezone: 'Asia/Shanghai', schedules: {} };
      return {};
    },
  });

  const renderAdminLibraries = vm.runInContext(
    appSource.slice(libStart, libEnd) + '\nrenderAdminLibraries;',
    ctx,
  );
  await renderAdminLibraries(body);

  assert.match(body.innerHTML, /data-lib-bgm="lib-normal">匹配离线库<\/button>/);
  assert.match(body.innerHTML, /data-lib-bgm="lib-union">匹配离线库<\/button>/);
  assert.match(body.innerHTML, /顺延链已变更/);
  assert.match(body.innerHTML, /data-lib-rematch="lib-normal">按新顺延链重匹配<\/button>/);
  assert.match(body.innerHTML, /data-lib-cover="lib-normal"[^>]*>提取首卷作为系列封面…<\/button>/);
  assert.match(body.innerHTML, /data-lib-cover="lib-union"[^>]*>提取首卷作为系列封面…<\/button>/);
  assert.match(body.innerHTML, /data-lib-batch-covers="lib-normal"[^>]*>提取归档卷封面<\/button>/);
  assert.match(body.innerHTML, /data-lib-batch-covers="lib-union"[^>]*>提取归档卷封面<\/button>/);
  assert.match(body.innerHTML, /批量读取 ComicInfo/);
  assert.doesNotMatch(body.innerHTML, /刮削 Bangumi/);

  requests.length = 0;
  for (const libId of ['lib-normal', 'lib-union']) {
    const btn = body.querySelector(`button[data-lib-bgm="${libId}"]`);
    assert.ok(btn, `expected scrape button for ${libId}`);
    await btn.onclick();
  }

  assert.deepEqual(requests, [
    ['/admin/api/metadata/scrape', 'POST', { libraryIds: ['lib-normal'], libraryId: 'lib-normal', mode: 'retry_unmatched', allowOnline: false }],
    ['/admin/api/metadata/scrape', 'POST', { libraryIds: ['lib-union'], libraryId: 'lib-union', mode: 'retry_unmatched', allowOnline: false }],
  ]);
  assert.deepEqual(toasts, [
    { msg: '已启动本库离线库匹配', kind: 'ok' },
    { msg: '已启动本库离线库匹配', kind: 'ok' },
  ]);
  assert.deepEqual(navigated, ['/admin?tab=bangumi', '/admin?tab=bangumi']);

  // Rematch shortcut button sends mode='force'
  requests.length = 0;
  await body.querySelector('button[data-lib-rematch="lib-normal"]').onclick();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/scrape',
    'POST',
    { libraryIds: ['lib-normal'], libraryId: 'lib-normal', mode: 'force', allowOnline: false },
  ]);

  // Schedule & batch cover modals use neutral multi-source copy while preserving ids/payload keys
  body.querySelector('button[data-lib-schedule="lib-normal"]').onclick();
  assert.match(modalBody.innerHTML, /若未准备可用的离线元数据索引库，将自动跳过元数据刮削与补封面；任一扫库失败则中止后续步骤。/);
  assert.match(modalBody.innerHTML, /扫库成功后自动执行元数据刮削（增量匹配新作品）/);
  assert.ok(doc.getElementById('sch-bgm'));

  const coverBtn = body.querySelector('button[data-lib-cover="lib-normal"]');
  coverBtn.dataset = { provider: coverBtn.getAttribute('data-provider'), libCover: 'lib-normal' };
  coverBtn.onclick();
  assert.match(modalBody.innerHTML, /显式读取系列首卷归档第 1 页生成系列缩略图，默认跳过已有数据库封面或缓存的系列/);
  assert.match(modalBody.innerHTML, /仅提取未匹配元数据且缺有效封面的系列/);
  assert.ok(doc.getElementById('cover-unbound'));
});

test('extmeta-batch-online is hidden unless an enabled offline_sqlite source has onlineApiUrl', async () => {
  const requests = [];
  let activeOverview = sampleOverview();
  const h = loadAdminScraperContext({
    apiImpl: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/bangumi/status') return sampleBangumiStatus();
      if (url.startsWith('/admin/api/bangumi/runs')) return [];
      if (url === '/admin/api/libraries') return sampleLibraries;
      if (url.startsWith('/admin/api/bangumi/tracker')) return {};
      if (url === '/admin/api/metadata/overview') return activeOverview;
      if (url === '/admin/api/metadata/scrape-runs') return [];
      return {};
    },
  });

  // 1. Disabled offline_sqlite with onlineApiUrl + tag_dictionary with onlineApiUrl => still hidden
  activeOverview = sampleOverview({
    sources: [
      {
        id: 'src-dict-online',
        kind: 'tag_dictionary',
        name: '词典',
        enabled: true,
        onlineApiUrl: 'https://api.example.test/dict',
      },
      {
        id: 'src-sqlite-disabled',
        kind: 'offline_sqlite',
        name: '停用离线库',
        enabled: false,
        onlineEnabled: true,
        onlineApiUrl: 'https://api.example.test/lookup',
      },
      {
        id: 'src-sqlite-online-opted-out',
        kind: 'offline_sqlite',
        name: '未开启在线补全的离线库',
        enabled: true,
        ready: true,
        onlineEnabled: false,
        onlineApiUrl: 'https://api.example.test/lookup',
      },
    ],
  });
  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, activeOverview, []);
  assert.equal(h.doc.getElementById('extmeta-batch-online'), null);
  await h.doc.getElementById('extmeta-batch-start-incremental').onclick();
  await flushMicrotasks();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/scrape',
    'POST',
    { libraryIds: [], libraryId: '', mode: 'incremental', allowOnline: false },
  ]);

  // 2. Enabled offline_sqlite with onlineEnabled=true and onlineApiUrl => checkbox shown, defaults unchecked, can toggle true
  requests.length = 0;
  activeOverview = sampleOverview({
    sources: [
      {
        id: 'src-sqlite-online',
        kind: 'offline_sqlite',
        name: '支持在线图库',
        enabled: true,
        ready: true,
        onlineEnabled: true,
        onlineApiUrl: 'https://api.example.test/lookup',
      },
    ],
  });
  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, activeOverview, []);
  const cb = h.doc.getElementById('extmeta-batch-online');
  assert.ok(cb, 'expected extmeta-batch-online checkbox when enabled offline_sqlite has onlineApiUrl');
  assert.equal(cb.checked, false);

  cb.checked = true;
  cb.onchange();
  await h.doc.getElementById('extmeta-batch-start-incremental').onclick();
  await flushMicrotasks();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/scrape',
    'POST',
    { libraryIds: [], libraryId: '', mode: 'incremental', allowOnline: true },
  ]);

  // 3. Removing onlineApiUrl hides checkbox again and resets stale draft allowOnline back to false
  requests.length = 0;
  activeOverview = sampleOverview();
  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, activeOverview, []);
  assert.equal(h.doc.getElementById('extmeta-batch-online'), null);
  await h.doc.getElementById('extmeta-batch-start-incremental').onclick();
  await flushMicrotasks();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/scrape',
    'POST',
    { libraryIds: [], libraryId: '', mode: 'incremental', allowOnline: false },
  ]);
});

test('source modals wrap online API fields in collapsible details with neutral hint', () => {
  const h = loadAdminScraperContext({ apiImpl: async () => ({}) });
  const expectedHint = '高级可选：仅用于接入自建或第三方兼容 GET ?q= 协议的查询服务；日常离线图库刮削与封面加载无需填写。';

  // 1. Edit modal without onlineApiUrl/onlineEnabled: collapsed by default
  h.openExtMetaSourceModal({
    id: 'src-1',
    name: '离线源 A',
    kind: 'offline_sqlite',
    enabled: true,
    ready: true,
    onlineApiUrl: '',
    onlineEnabled: false,
  }, [], () => {});
  let detailsEl = h.doc.getElementById('ems-online-details');
  assert.ok(detailsEl && detailsEl.tagName === 'DETAILS');
  assert.equal(detailsEl.open, false);
  let detailsSlice = h.modalBody.innerHTML.slice(
    h.modalBody.innerHTML.indexOf('<details id="ems-online-details"'),
    h.modalBody.innerHTML.indexOf('</details>'),
  );
  assert.match(detailsSlice, /高级选项（可选：自定义在线 API）/);
  assert.ok(detailsSlice.includes(expectedHint));
  assert.match(detailsSlice, /id="ems-online-url"/);
  assert.match(detailsSlice, /id="ems-online-enabled"/);

  // 2. Edit modal with onlineApiUrl set: auto-open
  h.openExtMetaSourceModal({
    id: 'src-2',
    name: '在线源 B',
    kind: 'offline_sqlite',
    enabled: true,
    ready: true,
    onlineApiUrl: 'https://api.example.test/search',
    onlineEnabled: false,
  }, [], () => {});
  detailsEl = h.doc.getElementById('ems-online-details');
  assert.equal(detailsEl.open, true);

  // 3. Edit modal with onlineEnabled set: auto-open
  h.openExtMetaSourceModal({
    id: 'src-3',
    name: '在线源 C',
    kind: 'offline_sqlite',
    enabled: true,
    ready: true,
    onlineApiUrl: '',
    onlineEnabled: true,
  }, [], () => {});
  assert.equal(h.doc.getElementById('ems-online-details').open, true);

  // 4. Add custom source modal: collapsed by default and contains emas-online-url inside details
  h.openExtMetaAddSourceModal([], () => {});
  const addDetails = h.doc.getElementById('emas-online-details');
  assert.ok(addDetails && addDetails.tagName === 'DETAILS');
  assert.equal(addDetails.open, false);
  detailsSlice = h.modalBody.innerHTML.slice(
    h.modalBody.innerHTML.indexOf('<details id="emas-online-details"'),
    h.modalBody.innerHTML.indexOf('</details>'),
  );
  assert.match(detailsSlice, /高级选项（可选：自定义在线 API）/);
  assert.ok(detailsSlice.includes(expectedHint));
  assert.match(detailsSlice, /id="emas-online-url"/);
});

test('library chain card and modal display 已停用（不生效） badge for disabled sources', () => {
  const h = loadAdminScraperContext({ apiImpl: async () => ({}) });
  const ov = sampleOverview({
    sources: [
      {
        id: 'src-disabled',
        kind: 'offline_sqlite',
        name: '已停用离线源',
        enabled: false,
        ready: true,
      },
      {
        id: 'src-enabled',
        kind: 'offline_sqlite',
        name: '已启用离线源',
        enabled: true,
        ready: true,
      },
    ],
    libraryChains: {
      'lib-1': ['src-disabled', 'bangumi'],
      'lib-2': ['src-enabled'],
    },
  });

  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, ov, []);
  const chainsHTML = h.body.innerHTML.slice(
    h.body.innerHTML.indexOf('id="extmeta-chains-card"'),
    h.body.innerHTML.indexOf('id="extmeta-runs-card"'),
  );
  assert.match(chainsHTML, /#1 已停用离线源<\/span><span class="badge badge-warn">已停用（不生效）<\/span>/);
  assert.doesNotMatch(chainsHTML, /#1 已启用离线源<\/span><span class="badge badge-warn">已停用（不生效）<\/span>/);

  h.body.querySelector('button[data-extmeta-chain-edit="lib-1"]').onclick();
  const pickerHTML = h.doc.getElementById('mchain-picker').innerHTML;
  const itemBlocks = pickerHTML.split('<div class="union-layer-item">').slice(1);
  const disabledBlock = itemBlocks.find((blk) => blk.includes('已停用离线源'));
  const enabledBlock = itemBlocks.find((blk) => blk.includes('已启用离线源'));
  assert.ok(disabledBlock && enabledBlock);
  assert.match(disabledBlock, /<span class="badge badge-warn">已停用（不生效）<\/span>/);
  assert.doesNotMatch(enabledBlock, /已停用（不生效）/);
});

test('legacy Bangumi scrape labels are unified to neutral metadata wording across UI and jump page', () => {
  const stageStart = appSource.indexOf('  function pipelineStageLabel(');
  const stageEnd = appSource.indexOf('  function toast(', stageStart);
  assert.ok(stageStart >= 0 && stageEnd > stageStart);
  const pipelineStageLabel = vm.runInContext(
    appSource.slice(stageStart, stageEnd) + '\npipelineStageLabel;',
    vm.createContext({}),
  );
  assert.equal(pipelineStageLabel('bangumi'), '元数据刮削');

  assert.doesNotMatch(appSource, /刮削 Bangumi/);
  assert.doesNotMatch(appSource, /Bangumi 刮削/);
  assert.doesNotMatch(appSource, /Bangumi 增量刮削/);
  assert.doesNotMatch(appSource, /不改变 Bangumi 绑定/);
  assert.doesNotMatch(appSource, /仅补未绑定 Bangumi/);
  assert.match(appSource, /定时扫库、元数据刮削与自动补封面已迁移至「书库管理」中的单库\/联合库「定时…」流水线配置。/);

  const jumpSource = readFileSync('internal/httpserver/bangumi_jump.go', 'utf8');
  assert.match(jumpSource, /backLabel := "返回元数据刮削中心"/);
  assert.doesNotMatch(jumpSource, /返回 Bangumi 刮削中心/);
});

test('batch multi-select checkboxes toggle all, disable buttons at 0 selected, preserve [] across polling, and send subset/single/all payloads', async () => {
  const requests = [];
  const h = loadAdminScraperContext({
    apiImpl: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/bangumi/status') return sampleBangumiStatus();
      if (url.startsWith('/admin/api/bangumi/runs')) return [];
      if (url === '/admin/api/libraries') return sampleLibraries;
      if (url.startsWith('/admin/api/bangumi/tracker')) return {};
      if (url === '/admin/api/metadata/overview') return sampleOverview();
      if (url === '/admin/api/metadata/scrape-runs') return [];
      return {};
    },
  });

  const extRuns = [
    {
      id: 'run-multi-1',
      libraryIds: ['lib-1', 'lib-2'],
      libraryName: '本地漫画库、WebDAV 书库',
      mode: 'force',
      status: 'completed',
      totalSeries: 10,
      processedSeries: 10,
      matchedCount: 9,
      unmatchedCount: 1,
      skippedCount: 0,
      startedAt: '2026-11-26T10:00:00Z',
    },
    {
      id: 'run-multi-2',
      libraryIds: ['lib-2', 'lib-union'],
      mode: 'incremental',
      status: 'completed',
      totalSeries: 5,
      processedSeries: 5,
      matchedCount: 5,
      unmatchedCount: 0,
      skippedCount: 0,
      startedAt: '2026-11-26T09:00:00Z',
    },
  ];

  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, sampleOverview(), extRuns);

  // ScrapeRun displays r.libraryName and falls back to mapping r.libraryIds via libMap
  assert.match(h.body.innerHTML, /<strong>本地漫画库、WebDAV 书库<\/strong>/);
  assert.match(h.body.innerHTML, /<strong>WebDAV 书库、联合书库<\/strong>/);

  // Initial state: all 3 selectableLibs checked (excludes hidden lib-hidden and member lib-member)
  const libCbs = h.body.querySelectorAll('input[data-extmeta-batch-lib]');
  assert.deepEqual(libCbs.map((c) => c.value), ['lib-1', 'lib-2', 'lib-union']);
  assert.ok(libCbs.every((c) => c.checked));

  const selectAll = h.doc.getElementById('extmeta-batch-select-all');
  const incBtn = h.doc.getElementById('extmeta-batch-start-incremental');
  const retryBtn = h.doc.getElementById('extmeta-batch-start-retry');
  const forceBtn = h.doc.getElementById('extmeta-batch-start-force');
  const emptyHint = h.doc.getElementById('extmeta-batch-empty-hint');

  assert.equal(selectAll.checked, true);
  assert.equal(selectAll.indeterminate, false);
  assert.equal(emptyHint.style.display, 'none');

  // Uncheck all via select-all -> 0 selected disables 3 buttons, reveals hint, and stores [] (not null)
  selectAll.checked = false;
  selectAll.onchange();
  assert.deepEqual(JSON.parse(JSON.stringify(h.ctx.state.extMetaBatchDraft.libraryIds)), []);
  assert.equal(h.doc.getElementById('extmeta-batch-selected-count').textContent, '已选 0 / 3');
  assert.equal(incBtn.disabled, true);
  assert.equal(retryBtn.disabled, true);
  assert.equal(forceBtn.disabled, true);
  assert.equal(emptyHint.style.display, '');

  // Repaint on fresh container (simulating poll/tab switch) preserves empty [] without resetting to select-all
  h.paintAdminBangumi(h.body, sampleBangumiStatus(), [], sampleLibraries, {}, sampleOverview(), extRuns);
  assert.equal(h.doc.getElementById('extmeta-batch-select-all').checked, false);
  assert.match(h.body.innerHTML, /已选 0 \/ 3/);
  assert.equal(h.doc.getElementById('extmeta-batch-start-force').disabled, true);

  // Select 2-library subset -> indeterminate selectAll, sends libraryIds:['lib-1','lib-2'], libraryId:''
  const cb1 = h.body.querySelector('input[data-extmeta-batch-lib="lib-1"]');
  const cb2 = h.body.querySelector('input[data-extmeta-batch-lib="lib-2"]');
  cb1.checked = true;
  cb1.onchange();
  cb2.checked = true;
  cb2.onchange();
  assert.equal(h.doc.getElementById('extmeta-batch-select-all').checked, false);
  assert.equal(h.doc.getElementById('extmeta-batch-select-all').indeterminate, true);
  assert.equal(h.doc.getElementById('extmeta-batch-start-force').disabled, false);

  await h.doc.getElementById('extmeta-batch-start-force').onclick();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/scrape',
    'POST',
    { libraryIds: ['lib-1', 'lib-2'], libraryId: '', mode: 'force', allowOnline: false },
  ]);

  // Select all back via select-all -> sends libraryIds:[]
  requests.length = 0;
  const selAll2 = h.doc.getElementById('extmeta-batch-select-all');
  selAll2.checked = true;
  selAll2.onchange();
  assert.equal(h.doc.getElementById('extmeta-batch-selected-count').textContent, '已选 3 / 3');
  await h.doc.getElementById('extmeta-batch-start-incremental').onclick();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/scrape',
    'POST',
    { libraryIds: [], libraryId: '', mode: 'incremental', allowOnline: false },
  ]);
});

test('chain change warning banner, rematch shortcut, Bangumi tankobon reimport banner, and importCovers badge work end-to-end', async () => {
  const requests = [];
  const bgmStatus = sampleBangumiStatus();
  bgmStatus.archive.needsTankobonReimport = true;

  const ov = sampleOverview({
    sources: sampleOverview().sources.map((s) => (s.id === 'src-sqlite-1' ? { ...s, importCovers: true } : s)),
    libraryChainStates: {
      'lib-1': { chainVersion: 'v2', appliedChainVersion: 'v1', needsRematch: true },
      'lib-2': { chainVersion: 'v1', appliedChainVersion: 'v1', needsRematch: false },
    },
  });

  const h = loadAdminScraperContext({
    apiImpl: async (url, opts = {}) => {
      requests.push([url, opts.method || 'GET', opts.body ? JSON.parse(JSON.stringify(opts.body)) : null]);
      if (url === '/admin/api/bangumi/status') return bgmStatus;
      if (url.startsWith('/admin/api/bangumi/runs')) return [];
      if (url === '/admin/api/libraries') return sampleLibraries;
      if (url.startsWith('/admin/api/bangumi/tracker')) return {};
      if (url === '/admin/api/metadata/overview') return ov;
      if (url === '/admin/api/metadata/scrape-runs') return [];
      return {};
    },
  });

  h.paintAdminBangumi(h.body, bgmStatus, [], sampleLibraries, {}, ov, []);

  const html = h.body.innerHTML;
  assert.doesNotMatch(html, /原图不压缩/);
  assert.doesNotMatch(html, /默认不选/);
  assert.doesNotMatch(appSource, /原图不压缩/);
  assert.doesNotMatch(appSource, /默认不选/);

  // Market list displays importCovers badge
  assert.match(html, /匹配时导入内嵌封面/);

  // Bangumi old v4 archive reimport banner + POST /admin/api/bangumi/archive/sync { force: true }
  const reimportBanner = h.doc.getElementById('bgm-arch-tankobon-banner');
  const reimportBtn = h.doc.getElementById('bgm-arch-reimport-tankobon-btn');
  assert.ok(reimportBanner && reimportBtn);
  assert.match(html, /当前 Bangumi 离线库为旧版本（缺少单行本分卷关系索引），重新导入后即可自动匹配并显示单卷封面。/);
  await reimportBtn.onclick();
  assert.deepEqual(requests[0], ['/admin/api/bangumi/archive/sync', 'POST', { force: true }]);

  // Chain rematch banner + row badge + shortcut button
  requests.length = 0;
  const chainBanner = h.doc.getElementById('extmeta-chains-rematch-banner');
  assert.ok(chainBanner);
  assert.match(html, /有 1 个书库的顺延链或源数据已变更/);
  assert.match(html, /顺延链已变更 · 待重匹配/);
  const rematchLib1 = h.body.querySelector('button[data-extmeta-chain-rematch="lib-1"]');
  const rematchLib2 = h.body.querySelector('button[data-extmeta-chain-rematch="lib-2"]');
  assert.ok(rematchLib1);
  assert.equal(rematchLib2, null);

  await rematchLib1.onclick();
  assert.deepEqual(requests[0], [
    '/admin/api/metadata/scrape',
    'POST',
    { libraryIds: ['lib-1'], libraryId: 'lib-1', mode: 'force', allowOnline: false },
  ]);

  // Chain modal save shows updated reminder toast, and add modal hides importCovers for non-sqlite kinds
  requests.length = 0;
  h.body.querySelector('button[data-extmeta-chain-edit="lib-1"]').onclick();
  await h.doc.getElementById('mchain-save').onclick();
  assert.equal(
    h.toasts.at(-1).msg,
    '顺延链已保存，请点击「按新顺延链重匹配」或使用下方全量重匹配应用新优先级',
  );

  h.doc.getElementById('extmeta-source-add-btn').onclick();
  const kindSel = h.doc.getElementById('emas-kind');
  const wrap = h.doc.getElementById('emas-import-covers-wrap');
  assert.equal(wrap.style.display, '');
  kindSel.value = 'tag_dictionary';
  kindSel.onchange();
  assert.equal(wrap.style.display, 'none');

  // Switching to upload tab forces offline_sqlite and reveals importCovers checkbox again
  const uploadTabBtn = h.modalBody.querySelector('button[data-emas-tab="upload"]');
  uploadTabBtn.onclick();
  assert.equal(wrap.style.display, '');

  // Quick-toggle in market list preserves src.importCovers
  await flushMicrotasks();
  requests.length = 0;
  const toggleCb = h.body.querySelector('input[data-extmeta-src-toggle="src-sqlite-1"]');
  assert.ok(toggleCb);
  toggleCb.checked = false;
  await toggleCb.onchange();
  const putReq = requests.find((r) => r[0] === '/admin/api/metadata/sources/src-sqlite-1' && r[1] === 'PUT');
  assert.ok(putReq);
  assert.equal(putReq[2].importCovers, true);
});
