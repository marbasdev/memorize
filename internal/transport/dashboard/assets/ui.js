(() => {
  const showToast = msg => {
    if (!msg) return;
    const el = document.getElementById('toast');
    if (!el) return;
    el.textContent = msg;
    el.style.display = 'block';
    clearTimeout(showToast._tm);
    showToast._tm = setTimeout(() => { el.style.display = 'none'; }, 3200);
  };

  window.showMemorizeToast = showToast;

  document.body.addEventListener('memorizeToast', e => {
    const msg = e.detail && (typeof e.detail === 'string' ? e.detail : e.detail.value);
    if (msg) showToast(msg);
  });

  document.body.addEventListener('memorizeCloseModal', () => {
    document.querySelectorAll('#modalHost dialog[open]').forEach(d => d.close());
    const host = document.getElementById('modalHost');
    if (host) host.innerHTML = '';
  });

  document.body.addEventListener('memorizeClearSel', () => {
    window.dispatchEvent(new CustomEvent('memorize-clear-sel'));
  });

  document.body.addEventListener('htmx:responseError', e => {
    const xhr = e.detail && e.detail.xhr;
    if (!xhr) return;
    let msg = xhr.responseText || `HTTP ${xhr.status}`;
    try {
      const parsed = JSON.parse(msg);
      if (parsed && parsed.error) msg = parsed.error;
    } catch (_) {}
    showToast(msg);
  });

  window.subOptionsFor = (subMap, catVal) => {
    const map = subMap || {};
    const key = String(catVal || '').trim().toLowerCase();
    if (key && Array.isArray(map[key])) return map[key];
    return Array.from(new Set(Object.values(map).flat())).filter(Boolean).sort();
  };

  function fmtUptime(sec) {
    const s = Math.max(0, Math.floor(Number(sec) || 0));
    if (s < 60) return `${s}s`;
    if (s < 3600) return `${Math.floor(s / 60)}m`;
    return `${Math.floor(s / 3600)}h${Math.floor((s % 3600) / 60)}m`;
  }

  async function refreshDaemonBadge() {
    try {
      const res = await fetch('/api/daemon/status');
      if (!res.ok) return null;
      const st = await res.json();
      if (st && st.running) {
        const badge = document.getElementById('daemonBadge');
        if (badge) {
          let portPart = '';
          if (st.active_daemon_port) {
            portPart = `:${st.active_daemon_port}`;
          } else if (st.listen_addr && st.listen_addr.includes(':')) {
            portPart = ':' + st.listen_addr.split(':').pop();
          }
          const fbPart = st.daemon_fallback ? ' (fallback)' : '';
          const prefix = portPart ? `${portPart}${fbPart} · ` : '';
          badge.textContent = `${prefix}PID ${st.pid} · ${fmtUptime(st.uptime_sec || 0)}`;
          const ggufInfo = st.active_gguf_port ? ` · GGUF :${st.active_gguf_port}` : '';
          badge.title = `http://${st.listen_addr || ''} (v${st.version || ''}${ggufInfo})`;
          badge.style.display = 'inline-block';
        }
        return st;
      }
    } catch (_) {}
    return null;
  }

  if (window.htmx && window.htmx.config) {
    window.htmx.config.defaultSettleDelay = 0;
    window.htmx.config.allowScriptTags = false;
  }

  function domainToTabs(domain) {
    switch (String(domain || '').toLowerCase()) {
      case 'progress':
        return ['progress'];
      case 'memories':
        return ['memories', 'team', 'calibrate'];
      case 'guardrails':
        return ['guardrails'];
      default:
        return ['memories', 'team', 'progress', 'guardrails', 'engines', 'calibrate'];
    }
  }

  const VALID_TABS = new Set(['memories', 'team', 'progress', 'guardrails', 'engines', 'calibrate']);

  function hashTabName() {
    const raw = String(window.location.hash || '').replace(/^#/, '').trim().toLowerCase();
    return VALID_TABS.has(raw) ? raw : '';
  }

  function resolveInitialTab() {
    const fromHash = hashTabName();
    if (fromHash) return fromHash;
    const saved = String(sessionStorage.getItem('memorize_tab') || '').trim().toLowerCase();
    if (VALID_TABS.has(saved)) return saved;
    const wrap = document.querySelector('.wrap');
    return wrap && wrap.dataset && wrap.dataset.setupRequired === '1' ? 'engines' : 'memories';
  }

  window.dashboardApp = () => ({
    activeTab: resolveInitialTab(),
    distOpen: localStorage.getItem('memorize_dist_open') !== '0',
    telemetryOpen: localStorage.getItem('memorize_telemetry_open') === '1',
    progCleanDays: '7',
    progCleanConfirm: false,
    grCleanDays: '7',
    grIncBlocked: false,
    grCleanConfirm: false,
    selectedIDs: [],
    pageActiveIDs: [],
    _sse: null,
    _liveTimer: null,
    _livePillTimer: null,
    _bootID: 0,
    _lastSeq: 0,
    _tabSeq: {},
    _dirtyTabs: {},
    _sseDisconnected: false,
    _pendingLive: false,
    _pendingReload: false,

    setDistOpen(open) {
      const next = Boolean(open);
      this.distOpen = next;
      localStorage.setItem('memorize_dist_open', next ? '1' : '0');
    },

    setTelemetryOpen(open) {
      const next = Boolean(open);
      this.telemetryOpen = next;
      localStorage.setItem('memorize_telemetry_open', next ? '1' : '0');
    },

    isBusyForLiveRefresh() {
      if (document.querySelector('#modalHost dialog[open]')) return true;
      if (this.selectedIDs.length > 0 || this.progCleanConfirm || this.grCleanConfirm) return true;
      const dr = document.getElementById('commitForm');
      if (dr && dr.classList.contains('open')) return true;
      const tdr = document.getElementById('teamAddForm');
      if (tdr && tdr.classList.contains('open')) return true;
      const ae = document.activeElement;
      if (!ae) return false;
      const tag = ae.tagName;
      if (tag === 'TEXTAREA') return true;
      return tag === 'INPUT' && ae.type !== 'checkbox' && ae.type !== 'button';
    },

    refreshTabLive(tabName) {
      if (!window.htmx || !window.htmx.ajax) return;
      this._dirtyTabs[tabName] = false;
      if (tabName === 'memories') {
        const f = document.getElementById('memoryFilterForm');
        const q = f ? new URLSearchParams(new FormData(f)).toString() : '';
        window.htmx.ajax('GET', '/ui/memories' + (q ? '?' + q : ''), { target: '#memoriesSection', swap: 'innerHTML' });
      } else if (tabName === 'team') {
        const f = document.getElementById('teamFilterForm');
        const q = f ? new URLSearchParams(new FormData(f)).toString() : '';
        window.htmx.ajax('GET', '/ui/team' + (q ? '?' + q : ''), { target: '#teamSection', swap: 'innerHTML' });
      } else if (tabName === 'progress') {
        const f = document.getElementById('progressFilterForm');
        const p = f ? new URLSearchParams(new FormData(f)) : new URLSearchParams();
        const winSel = document.getElementById('progDoneWindow');
        if (winSel && winSel.value) p.set('done_window', winSel.value);
        const q = p.toString();
        window.htmx.ajax('GET', '/ui/progress' + (q ? '?' + q : ''), { target: '#progressSection', swap: 'innerHTML' });
      } else if (tabName === 'guardrails') {
        const f = document.getElementById('guardrailFilterForm');
        const q = f ? new URLSearchParams(new FormData(f)).toString() : '';
        window.htmx.ajax('GET', '/ui/guardrail-logs' + (q ? '?' + q : ''), { target: '#guardrailLogsSection', swap: 'outerHTML' });
      } else if (tabName === 'calibrate') {
        const sc = document.getElementById('calHistScope');
        const q = sc ? new URLSearchParams({ cal_scope: sc.value || '' }).toString() : '';
        window.htmx.ajax('GET', '/ui/calibrate/histogram' + (q ? '?' + q : ''), { target: '#confHistSection', swap: 'innerHTML' });
      }
      this._tabSeq[tabName] = this._lastSeq;
    },

    flushPendingLive() {
      if (this.isBusyForLiveRefresh()) return;
      if (this._pendingReload) {
        this._pendingReload = false;
        window.location.reload();
        return;
      }
      if (!this._pendingLive) return;
      this._pendingLive = false;
      this.refreshTabLive(this.activeTab);
    },

    handleConnectivityResume() {
      refreshDaemonBadge();
      if (!this._sse || (window.EventSource && this._sse.readyState === window.EventSource.CLOSED)) {
        if (this._sse) {
          try { this._sse.close(); } catch (_) {}
          this._sse = null;
        }
        this.connectLiveEvents();
      }
    },

    connectLiveEvents() {
      if (typeof window.EventSource !== 'function' || this._sse) return;
      const es = new EventSource('/api/events');
      this._sse = es;
      es.addEventListener('hello', ev => {
        let boot = 0;
        try { boot = Number(JSON.parse(ev.data || '{}').boot) || 0; } catch (_) {}
        const wasDisconnected = this._sseDisconnected;
        this._sseDisconnected = false;
        const prevBoot = this._bootID;
        this._bootID = boot || 1;
        if (!prevBoot && !wasDisconnected) return;
        if (boot && prevBoot && prevBoot !== boot) {
          if (this.isBusyForLiveRefresh()) {
            this._pendingReload = true;
            return;
          }
          window.location.reload();
          return;
        }
        this._lastSeq++;
        for (const t of domainToTabs('all')) this._dirtyTabs[t] = true;
        refreshDaemonBadge();
        if (this.isBusyForLiveRefresh()) {
          this._pendingLive = true;
          return;
        }
        this._pendingLive = false;
        this.refreshTabLive(this.activeTab);
      });
      es.addEventListener('change', ev => {
        let domain = 'all';
        try {
          const payload = JSON.parse(ev.data || '{}') || {};
          if (payload.domain) domain = String(payload.domain).toLowerCase();
        } catch (_) {}
        this._lastSeq++;
        const affected = domainToTabs(domain);
        for (const t of affected) this._dirtyTabs[t] = true;
        if (!affected.includes(this.activeTab)) return;
        clearTimeout(this._liveTimer);
        this._liveTimer = setTimeout(() => {
          if (this.isBusyForLiveRefresh()) {
            this._pendingLive = true;
            return;
          }
          this._pendingLive = false;
          this.refreshTabLive(this.activeTab);
        }, 220);
      });
      es.onerror = () => {
        this._sseDisconnected = true;
        if (window.EventSource && es.readyState === window.EventSource.CLOSED) {
          try { es.close(); } catch (_) {}
          if (this._sse === es) this._sse = null;
          setTimeout(() => {
            if (navigator.onLine !== false) this.connectLiveEvents();
          }, 2000);
        }
      };
    },

    init() {
      if (!document.getElementById('tabBtn-' + this.activeTab)) {
        this.activeTab = 'memories';
      }
      document.body.addEventListener('memorizeCloseModal', () => setTimeout(() => this.flushPendingLive(), 120));
      document.body.addEventListener('focusout', () => setTimeout(() => this.flushPendingLive(), 150));
      document.body.addEventListener('htmx:configRequest', e => {
        const path = (e.detail && e.detail.path) || '';
        if (path.indexOf('/ui/progress') === 0 && e.detail.parameters) {
          const winSel = document.getElementById('progDoneWindow');
          if (winSel && winSel.value) e.detail.parameters['done_window'] = winSel.value;
        }
      });
      document.body.addEventListener('htmx:afterSwap', () => {
        // Progress tab no longer renders client-side live sessions; SSR handles all content.
      });
      window.addEventListener('online', () => this.handleConnectivityResume());
      document.addEventListener('visibilitychange', () => {
        if (!document.hidden) this.handleConnectivityResume();
      });
      document.body.addEventListener('htmx:afterRequest', e => {
        if (!e.detail || !e.detail.successful) return;
        const path = (e.detail.pathInfo && e.detail.pathInfo.requestPath) || '';
        if (path.indexOf('/ui/progress') === 0) {
          this._dirtyTabs.progress = false;
          this._tabSeq.progress = this._lastSeq;
        } else if (path.indexOf('/ui/memories') === 0) {
          this._dirtyTabs.memories = false;
          this._tabSeq.memories = this._lastSeq;
        } else if (path.indexOf('/ui/guardrail') === 0) {
          this._dirtyTabs.guardrails = false;
          this._tabSeq.guardrails = this._lastSeq;
        } else if (path.indexOf('/ui/engines') === 0) {
          this._dirtyTabs.engines = false;
          this._tabSeq.engines = this._lastSeq;
        } else if (path.indexOf('/ui/calibrate') === 0) {
          this._dirtyTabs.calibrate = false;
          this._tabSeq.calibrate = this._lastSeq;
        }
        if (path.indexOf('/ui/progress/cleanup') === 0) {
          this.progCleanConfirm = false;
        } else if (path.indexOf('/ui/guardrail-logs/cleanup') === 0) {
          this.grCleanConfirm = false;
          this.grIncBlocked = false;
        }
      });
      document.body.addEventListener('memorizePortRedirect', e => {
        const targetUrl = e && e.detail && e.detail.url ? String(e.detail.url).trim() : '';
        if (!targetUrl) return;
        let parsed;
        try {
          parsed = new URL(targetUrl, window.location.origin);
        } catch (_) {
          return;
        }
        const host = parsed.hostname.toLowerCase();
        if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return;
        if (host !== '127.0.0.1' && host !== 'localhost' && host !== '[::1]' && host !== '::1') return;
        const cleanBase = parsed.origin;
        const dest = cleanBase + '/' + (window.location.hash || '');
        const deadline = Date.now() + 5000;
        const poll = () => {
          if (Date.now() >= deadline) {
            window.location.replace(dest);
            return;
          }
          fetch(cleanBase + '/api/dashboard/meta', { mode: 'no-cors', cache: 'no-store' })
            .then(() => { window.location.replace(dest); })
            .catch(() => setTimeout(poll, 160));
        };
        setTimeout(poll, 180);
      });
      refreshDaemonBadge();
      setInterval(() => {
        refreshDaemonBadge();
      }, 15000);
      this.connectLiveEvents();
      window.addEventListener('hashchange', () => {
        const next = hashTabName();
        if (next && next !== this.activeTab) this.switchTab(next, true);
      });
    },

    switchTab(name, fromHash = false) {
      this.activeTab = name;
      sessionStorage.setItem('memorize_tab', name);
      const btn = document.getElementById('tabBtn-' + name);
      const hasOwnHXGet = Boolean(btn && btn.hasAttribute('hx-get'));
      if ((!hasOwnHXGet || fromHash) && (this._dirtyTabs[name] || this._lastSeq > (this._tabSeq[name] || 0)) && !this.isBusyForLiveRefresh()) {
        this.refreshTabLive(name);
      }
    },

    toggleTheme() {
      const root = document.documentElement;
      const next = root.dataset.theme === 'dark' ? 'light' : 'dark';
      root.dataset.theme = next;
      localStorage.setItem('memorize_theme', next);
    },

    setPageActiveIDs(ids) {
      this.pageActiveIDs = Array.isArray(ids) ? ids : [];
    },

    isSelected(id) {
      return this.selectedIDs.includes(Number(id));
    },

    toggleID(id, checked) {
      const n = Number(id);
      if (checked) {
        if (!this.selectedIDs.includes(n)) this.selectedIDs.push(n);
      } else {
        this.selectedIDs = this.selectedIDs.filter(x => x !== n);
      }
    },

    isAllPageSelected() {
      return this.pageActiveIDs.length > 0 && this.pageActiveIDs.every(id => this.selectedIDs.includes(id));
    },

    togglePageAll(checked) {
      if (checked) {
        const set = new Set([...this.selectedIDs, ...this.pageActiveIDs]);
        this.selectedIDs = Array.from(set);
      } else {
        const pageSet = new Set(this.pageActiveIDs);
        this.selectedIDs = this.selectedIDs.filter(id => !pageSet.has(id));
      }
    },

    clearSel() {
      this.selectedIDs = [];
    },

    closeModal() {
      document.querySelectorAll('#modalHost dialog[open]').forEach(d => d.close());
    },

    async selectAllMatching() {
      const form = document.getElementById('memoryFilterForm');
      const params = form ? new URLSearchParams(new FormData(form)) : new URLSearchParams();
      params.set('ids_only', '1');
      try {
        const res = await fetch('/api/records?' + params.toString());
        const data = await res.json();
        if (Array.isArray(data.ids)) {
          this.selectedIDs = Array.from(new Set([...this.selectedIDs, ...data.ids.map(Number)]));
        }
      } catch (err) {
        showToast(err.message);
      }
    }
  });

  window.comboBox = (items, initialVal, disabled) => ({
    items: Array.isArray(items) ? items : [],
    val: initialVal || '',
    open: false,
    disabled: Boolean(disabled),
    pick(v) {
      this.val = v;
      this.open = false;
    }
  });

  window.catPresetsModal = initialMap => ({
    rows: Object.entries(initialMap || {})
      .sort((a, b) => a[0].localeCompare(b[0]))
      .map(([cat, subs]) => ({ cat, subs: (subs || []).join(', ') })),
    serialize() {
      const out = {};
      for (const r of this.rows) {
        const cat = String(r.cat || '').trim().toLowerCase().replace(/\s+/g, '_');
        if (!cat) continue;
        const subs = Array.from(new Set(
          String(r.subs || '').split(',').map(s => s.trim().toLowerCase().replace(/\s+/g, '_')).filter(Boolean)
        ));
        out[cat] = subs;
      }
      return JSON.stringify(out);
    }
  });
})();
