(function () {
  'use strict';

  // ===== Constants =====
  var API = '/_admin/api';
  var AUTH_LABELS = { x_api_key: 'x-api-key', authorization_bearer: 'Bearer', custom_header: 'Custom Header' };
  var LOG_MODES = ['off', 'metadata', 'full'];
  var PAGE_SIZE = 25;

  // ===== State =====
  var state = {
    dashboard: null,
    selectedGroupId: '',
    logs: { open: false, filter: {}, items: [], total: 0, page: 1, selectedId: '', detail: null, tab: 'summary', responseSubTab: 'collected', advancedOpen: false }
  };

  // ===== DOM Utilities =====
  function $(id) { return document.getElementById(id); }

  function h(tag, attrs) {
    var el = document.createElement(tag);
    if (attrs) {
      for (var k in attrs) {
        if (!attrs.hasOwnProperty(k)) continue;
        var v = attrs[k];
        if (k.startsWith('on') && typeof v === 'function') {
          el.addEventListener(k.slice(2).toLowerCase(), v);
        } else if (k === 'className') {
          el.className = v;
        } else if (k === 'htmlFor') {
          el.setAttribute('for', v);
        } else if (k === 'checked') {
          el.checked = !!v;
        } else if (k === 'disabled') {
          if (v) el.setAttribute('disabled', '');
        } else if (k === 'selected') {
          el.selected = !!v;
        } else if (k === 'value') {
          el.value = v == null ? '' : v;
        } else if (v != null && v !== false) {
          el.setAttribute(k, v);
        }
      }
    }
    for (var i = 2; i < arguments.length; i++) {
      var child = arguments[i];
      if (child == null || child === false || child === undefined) continue;
      if (Array.isArray(child)) {
        child.forEach(function (c) { if (c != null && c !== false) el.appendChild(typeof c === 'string' || typeof c === 'number' ? document.createTextNode(c) : c); });
      } else if (typeof child === 'string' || typeof child === 'number') {
        el.appendChild(document.createTextNode(child));
      } else {
        el.appendChild(child);
      }
    }
    return el;
  }

  function escapeHtml(s) {
    if (!s) return '';
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  function formatTime(iso) {
    if (!iso) return '-';
    try {
      var d = new Date(iso);
      if (isNaN(d.getTime())) return iso;
      return d.toLocaleString('zh-CN', { hour12: false });
    } catch (e) { return iso; }
  }

  function relativeTime(iso) {
    if (!iso) return '-';
    try {
      var d = new Date(iso);
      var now = Date.now();
      var diff = now - d.getTime();
      if (diff < 0) return formatTime(iso);
      if (diff < 60000) return Math.floor(diff / 1000) + ' 秒前';
      if (diff < 3600000) return Math.floor(diff / 60000) + ' 分钟前';
      if (diff < 86400000) return Math.floor(diff / 3600000) + ' 小时前';
      return Math.floor(diff / 86400000) + ' 天前';
    } catch (e) { return iso; }
  }

  function formatBytes(bytes) {
    if (!bytes || bytes === 0) return '0 B';
    var units = ['B', 'KB', 'MB', 'GB'];
    var i = 0;
    var v = bytes;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i === 0 ? v : v.toFixed(1)) + ' ' + units[i];
  }

  function statusClass(code) {
    if (!code) return 'status-0';
    if (code < 300) return 'status-2xx';
    if (code < 400) return 'status-3xx';
    if (code < 500) return 'status-4xx';
    return 'status-5xx';
  }

  function maskKey(key) {
    if (!key) return '';
    if (key.length <= 6) return '\u2022\u2022\u2022\u2022' + key.slice(-2);
    return '\u2022\u2022\u2022\u2022\u2022\u2022' + key.slice(-4);
  }

  function decodeBase64(b64) {
    try {
      var binary = atob(b64);
      var bytes = new Uint8Array(binary.length);
      for (var i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
      return new TextDecoder('utf-8').decode(bytes);
    } catch (e) { return '[decode error]'; }
  }

  function prettyJSON(str) {
    try { return JSON.stringify(JSON.parse(str), null, 2); } catch (e) { return str; }
  }

  function upstreamName(id) {
    if (!id || !state.dashboard) return '';
    var ups = state.dashboard.upstreams || [];
    for (var i = 0; i < ups.length; i++) { if (ups[i].id === id) return ups[i].name; }
    return id;
  }

  // ===== API Layer =====
  function api(path, opts) {
    opts = opts || {};
    var options = { method: opts.method || 'GET', headers: {} };
    if (opts.body !== undefined) {
      options.headers['Content-Type'] = 'application/json';
      options.body = JSON.stringify(opts.body);
    }
    return fetch(API + path, options).then(function (res) {
      if (res.status === 401) {
        renderLogin();
        return Promise.reject(new Error('login_required'));
      }
      return res.json().then(function (data) {
        if (!res.ok) {
          var msg = (data && data.error && data.error.message) ? data.error.message : 'Request failed';
          return Promise.reject(new Error(msg));
        }
        return data;
      });
    });
  }

  // ===== Toast =====
  function showToast(msg, type) {
    var root = $('toast-root');
    var toast = h('div', { className: 'toast' + (type === 'error' ? ' toast-error' : '') }, msg);
    root.appendChild(toast);
    setTimeout(function () {
      toast.classList.add('removing');
      setTimeout(function () { if (toast.parentNode) toast.parentNode.removeChild(toast); }, 200);
    }, 3000);
  }

  // ===== Modal System =====
  function showModal(config) {
    var root = $('modal-root');
    root.innerHTML = '';
    var body = h('div', { className: 'modal-body' });
    config.render(body);
    var footer = h('div', { className: 'modal-footer' },
      h('button', { className: 'btn-secondary', onClick: closeModal }, '取消'),
      config.saveText ? h('button', { className: 'btn-primary', onClick: function () { config.onSave(body); } }, config.saveText) : null
    );
    var modal = h('div', { className: 'modal' },
      h('div', { className: 'modal-header' },
        h('h3', null, config.title),
        h('button', { className: 'btn-icon', onClick: closeModal, title: '关闭' }, '\u2715')
      ),
      body, footer
    );
    var backdrop = h('div', { className: 'modal-backdrop', onClick: function (e) { if (e.target === backdrop) closeModal(); } }, modal);
    root.appendChild(backdrop);
    var handler = function (e) { if (e.key === 'Escape') { closeModal(); document.removeEventListener('keydown', handler); } };
    document.addEventListener('keydown', handler);
    var firstInput = body.querySelector('input,select,textarea');
    if (firstInput) setTimeout(function () { firstInput.focus(); }, 50);
  }

  function closeModal() {
    $('modal-root').innerHTML = '';
  }

  function showConfirm(msg, onConfirm) {
    showModal({
      title: '确认操作',
      render: function (body) { body.appendChild(h('p', null, msg)); },
      saveText: '确认',
      onSave: function () { closeModal(); onConfirm(); }
    });
  }

  // ===== Login =====
  function renderLogin() {
    var app = $('app');
    app.innerHTML = '';
    var errorEl = h('div', { className: 'form-error', style: 'display:none;margin-bottom:12px' });
    var input = h('input', { type: 'password', placeholder: '请输入管理密钥', autocomplete: 'current-password' });
    var doSubmit = function () {
      var key = input.value.trim();
      if (!key) { errorEl.textContent = '请输入密钥'; errorEl.style.display = 'block'; return; }
      errorEl.style.display = 'none';
      fetch(API + '/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ admin_key: key }) })
        .then(function (res) { return res.json().then(function (data) { return { ok: res.ok, data: data }; }); })
        .then(function (r) {
          if (!r.ok) { errorEl.textContent = (r.data.error && r.data.error.message) || '登录失败'; errorEl.style.display = 'block'; return; }
          loadDashboard();
        })
        .catch(function () { errorEl.textContent = '网络错误'; errorEl.style.display = 'block'; });
    };
    input.addEventListener('keydown', function (e) { if (e.key === 'Enter') doSubmit(); });
    app.appendChild(
      h('div', { className: 'login-wrap' },
        h('div', { className: 'card login-card' },
          h('h1', null, 'Claude Code Gateway'),
          h('div', { className: 'subtitle' }, 'CC分析网关'),
          errorEl,
          h('div', { className: 'form-group' }, h('label', null, '管理密钥'), input),
          h('button', { className: 'btn-primary', style: 'width:100%', onClick: doSubmit }, '登录')
        )
      )
    );
    input.focus();
  }

  // ===== Dashboard =====
  function renderDashboard() {
    var app = $('app');
    app.innerHTML = '';
    var d = state.dashboard;
    if (!d) return;
    app.appendChild(
      h('div', { className: 'container' },
        renderTopBar(),
        renderSummary(),
        renderQuickActions(),
        renderDefaultNotice(),
        h('div', { className: 'main-grid', id: 'main-grid' }, renderUpstreams(), renderMappings()),
        h('div', { className: 'secondary-grid', id: 'secondary-grid' }, renderGroups(), renderLogSettings(), renderAdminKey())
      )
    );
  }

  function renderTopBar() {
    var d = state.dashboard;
    var modeClass = d.summary.current_log_mode === 'full' ? 'pill-green' : d.summary.current_log_mode === 'metadata' ? 'pill-blue' : 'pill-gray';
    return h('div', { className: 'top-bar' },
      h('div', { className: 'top-bar-left' },
        h('h1', null, 'Claude Code Gateway'),
        h('span', { className: 'subtitle' }, 'CC分析网关'),
        h('span', { className: 'pill ' + modeClass }, d.summary.current_log_mode)
      ),
      h('div', { className: 'top-bar-right' },
        h('button', { className: 'btn-secondary btn-sm', onClick: function () { refreshAfterConfigChange(); }, title: '刷新数据' }, '\u21BB 刷新'),
        h('button', { className: 'btn-primary btn-sm', onClick: openLogsModal, title: '查看请求日志' }, '\u2630 日志')
      )
    );
  }

  function renderSummary() {
    var s = state.dashboard.summary;
    return h('div', { className: 'summary-grid' },
      h('div', { className: 'card metric-card' }, h('div', { className: 'metric-value' }, String(s.upstream_count)), h('div', { className: 'metric-label' }, '上游数量')),
      h('div', { className: 'card metric-card' }, h('div', { className: 'metric-value' }, String(s.mapping_count)), h('div', { className: 'metric-label' }, '模型映射')),
      h('div', { className: 'card metric-card' }, h('div', { className: 'metric-value' }, String(s.log_row_count)), h('div', { className: 'metric-label' }, '日志行数')),
      h('div', { className: 'card metric-card' }, h('div', { className: 'metric-value' }, s.last_request_at ? relativeTime(s.last_request_at) : '-'), h('div', { className: 'metric-label' }, '最近请求'))
    );
  }

  function renderQuickActions() {
    return h('div', { className: 'quick-actions' },
      h('button', { className: 'btn-secondary btn-sm', onClick: function () { openUpstreamModal(); } }, '+ 新增上游'),
      h('button', { className: 'btn-secondary btn-sm', onClick: function () { openGroupModal(); } }, '+ 新增 API Key 组'),
      h('button', { className: 'btn-secondary btn-sm', onClick: openLogsModal }, '\u2630 查看日志'),
      h('button', { className: 'btn-secondary btn-sm', onClick: function () { showConfirm('确认清理日志？将按当前保留策略删除过期数据。', doCleanupLogs); } }, '\u2702 清理日志')
    );
  }

  function renderDefaultNotice() {
    if (!state.dashboard.summary.default_admin_key) return h('div');
    return h('div', { className: 'card card-notice', style: 'margin-bottom:20px' },
      h('p', null, '\u26A0 当前使用默认管理密钥 (defaultpassword)，请在下方修改管理密钥以确保安全。')
    );
  }

  // ===== Upstreams =====
  function renderUpstreams() {
    var ups = state.dashboard.upstreams || [];
    var items = ups.map(function (u) {
      return h('div', { className: 'list-item' },
        h('div', { className: 'list-item-header' },
          h('div', null,
            h('span', { className: 'status-dot ' + (u.enabled ? 'status-enabled' : 'status-disabled'), title: u.enabled ? '已启用' : '已停用' }),
            h('span', { className: 'list-item-title', style: 'margin-left:8px' }, u.name)
          ),
          h('div', { className: 'list-item-actions' },
            h('button', { className: 'btn-ghost btn-sm', onClick: function () { toggleUpstream(u); }, title: u.enabled ? '停用' : '启用' }, u.enabled ? '\u23F8' : '\u25B6'),
            h('button', { className: 'btn-ghost btn-sm', onClick: function () { openUpstreamModal(u); }, title: '编辑' }, '\u270E'),
            h('button', { className: 'btn-ghost btn-sm', onClick: function () { showConfirm('确认删除上游 "' + escapeHtml(u.name) + '"？', function () { deleteUpstream(u.id); }); }, title: '删除' }, '\u2715')
          )
        ),
        h('div', { className: 'list-item-meta' }, u.base_url),
        h('div', { className: 'list-item-meta' },
          h('span', null, AUTH_LABELS[u.auth_type] || u.auth_type),
          u.auth_type === 'custom_header' ? h('span', null, ' (' + escapeHtml(u.auth_header_name) + ')') : null,
          h('span', { style: 'margin-left:12px;color:var(--gray-300)' }, maskKey(u.api_key))
        )
      );
    });
    return h('div', { className: 'card', id: 'upstreams-card' },
      h('div', { className: 'card-header' },
        h('h2', null, '\u2191 上游配置'),
        h('button', { className: 'btn-primary btn-sm', onClick: function () { openUpstreamModal(); } }, '+ 新增')
      ),
      items.length ? h('div', null, items) : h('div', { className: 'empty-state' }, '暂无上游配置，点击新增创建第一个上游')
    );
  }

  function openUpstreamModal(u) {
    var isEdit = !!u;
    showModal({
      title: isEdit ? '编辑上游' : '新增上游',
      saveText: '保存',
      render: function (body) {
        body.appendChild(h('div', { className: 'form-group' }, h('label', null, '名称'), h('input', { type: 'text', id: 'f-up-name', value: isEdit ? u.name : '' })));
        body.appendChild(h('div', { className: 'form-group' }, h('label', null, 'Base URL'), h('input', { type: 'text', id: 'f-up-url', value: isEdit ? u.base_url : '', placeholder: 'https://api.anthropic.com' })));
        body.appendChild(h('div', { className: 'form-group' }, h('label', null, 'API Key' + (isEdit ? ' (留空保留旧值)' : '')), h('input', { type: 'password', id: 'f-up-key', value: '', placeholder: isEdit ? '留空保留旧值' : '' })));
        var authSelect = h('select', { id: 'f-up-auth', value: isEdit ? u.auth_type : 'x_api_key' },
          h('option', { value: 'x_api_key', selected: (!isEdit || u.auth_type === 'x_api_key') }, 'x-api-key'),
          h('option', { value: 'authorization_bearer', selected: isEdit && u.auth_type === 'authorization_bearer' }, 'Authorization Bearer'),
          h('option', { value: 'custom_header', selected: isEdit && u.auth_type === 'custom_header' }, 'Custom Header')
        );
        var customFields = h('div', { id: 'f-up-custom', style: (isEdit && u.auth_type === 'custom_header') ? '' : 'display:none' },
          h('div', { className: 'form-group' }, h('label', null, 'Header Name'), h('input', { type: 'text', id: 'f-up-hname', value: isEdit ? (u.auth_header_name || '') : '' })),
          h('div', { className: 'form-group' }, h('label', null, 'Value Template'), h('input', { type: 'text', id: 'f-up-htpl', value: isEdit ? (u.auth_header_value_template || '') : '', placeholder: 'Bearer {{key}}' }),
            h('div', { className: 'form-hint' }, '使用 {{key}} 作为 API Key 占位符'))
        );
        authSelect.addEventListener('change', function () { customFields.style.display = authSelect.value === 'custom_header' ? '' : 'none'; });
        body.appendChild(h('div', { className: 'form-group' }, h('label', null, '认证方式'), authSelect));
        body.appendChild(customFields);
        var enabledSel = h('select', { id: 'f-up-enabled' },
          h('option', { value: 'true', selected: !isEdit || u.enabled }, '启用'),
          h('option', { value: 'false', selected: isEdit && !u.enabled }, '停用')
        );
        body.appendChild(h('div', { className: 'form-group' }, h('label', null, '状态'), enabledSel));
        body.appendChild(h('div', { className: 'form-error', id: 'f-up-error', style: 'display:none' }));
      },
      onSave: function () {
        var data = {
          name: document.getElementById('f-up-name').value.trim(),
          base_url: document.getElementById('f-up-url').value.trim(),
          api_key: document.getElementById('f-up-key').value,
          auth_type: document.getElementById('f-up-auth').value,
          auth_header_name: document.getElementById('f-up-hname') ? document.getElementById('f-up-hname').value.trim() : '',
          auth_header_value_template: document.getElementById('f-up-htpl') ? document.getElementById('f-up-htpl').value.trim() : '',
          enabled: document.getElementById('f-up-enabled').value === 'true'
        };
        var errEl = document.getElementById('f-up-error');
        var path = isEdit ? '/upstreams/' + u.id : '/upstreams';
        var method = isEdit ? 'PATCH' : 'POST';
        api(path, { method: method, body: data })
          .then(function () { closeModal(); showToast(isEdit ? '上游已更新' : '上游已创建'); refreshAfterConfigChange(); })
          .catch(function (e) { if (e.message !== 'login_required') { errEl.textContent = e.message; errEl.style.display = 'block'; } });
      }
    });
  }

  function toggleUpstream(u) {
    var data = {
      name: u.name,
      base_url: u.base_url,
      api_key: '',
      auth_type: u.auth_type,
      auth_header_name: u.auth_header_name || '',
      auth_header_value_template: u.auth_header_value_template || '',
      enabled: !u.enabled
    };
    api('/upstreams/' + u.id, { method: 'PATCH', body: data })
      .then(function () { showToast(u.enabled ? '上游已停用' : '上游已启用'); refreshAfterConfigChange(); })
      .catch(function (e) { if (e.message !== 'login_required') showToast(e.message, 'error'); });
  }

  function deleteUpstream(id) {
    api('/upstreams/' + id, { method: 'DELETE' })
      .then(function () { showToast('上游已删除'); refreshAfterConfigChange(); })
      .catch(function (e) { if (e.message !== 'login_required') showToast(e.message, 'error'); });
  }

  // ===== API Key Groups =====
  function renderGroups() {
    var groups = state.dashboard.groups || [];
    var mc = state.dashboard.mapping_counts || {};
    var items = groups.map(function (g) {
      var isSelected = g.id === state.selectedGroupId;
      return h('div', { className: 'list-item' + (isSelected ? ' selected' : ''), onClick: function (e) {
        if (e.target.closest('button')) return;
        state.selectedGroupId = g.id;
        renderDashboard();
      }},
        h('div', { className: 'list-item-header' },
          h('div', null,
            h('span', { className: 'status-dot ' + (g.enabled ? 'status-enabled' : 'status-disabled') }),
            h('span', { className: 'list-item-title', style: 'margin-left:8px' }, g.name)
          ),
          h('div', { className: 'list-item-actions' },
            h('button', { className: 'btn-ghost btn-sm', onClick: function () { openGroupModal(g); }, title: '编辑' }, '\u270E'),
            h('button', { className: 'btn-ghost btn-sm', onClick: function () { showConfirm('确认删除 API Key 组 "' + escapeHtml(g.name) + '"？', function () { deleteGroup(g.id); }); }, title: '删除' }, '\u2715')
          )
        ),
        h('div', { className: 'list-item-meta' },
          h('span', null, '上游: ' + upstreamName(g.default_upstream_id)),
          h('span', { style: 'margin-left:12px' }, '映射: ' + (mc[g.id] || 0))
        ),
        h('div', { className: 'list-item-meta' }, 'Key: ' + maskKey(g.api_key))
      );
    });
    return h('div', { className: 'card', id: 'groups-card' },
      h('div', { className: 'card-header' },
        h('h2', null, '\uD83D\uDD11 API Key 组'),
        h('button', { className: 'btn-primary btn-sm', onClick: function () { openGroupModal(); } }, '+ 新增')
      ),
      items.length ? h('div', null, items) : h('div', { className: 'empty-state' }, '暂无 API Key 组')
    );
  }

  function openGroupModal(g) {
    var isEdit = !!g;
    var ups = state.dashboard.upstreams || [];
    showModal({
      title: isEdit ? '编辑 API Key 组' : '新增 API Key 组',
      saveText: '保存',
      render: function (body) {
        body.appendChild(h('div', { className: 'form-group' }, h('label', null, '组名称'), h('input', { type: 'text', id: 'f-g-name', value: isEdit ? g.name : '' })));
        body.appendChild(h('div', { className: 'form-group' }, h('label', null, 'API Key'), h('input', { type: 'text', id: 'f-g-key', value: isEdit ? g.api_key : '' })));
        var upOpts = ups.map(function (u) { return h('option', { value: u.id, selected: isEdit && g.default_upstream_id === u.id }, u.name); });
        body.appendChild(h('div', { className: 'form-group' }, h('label', null, '默认上游'), h('select', { id: 'f-g-upstream' }, upOpts.length ? upOpts : [h('option', { value: '' }, '-- 请先创建上游 --')])));
        body.appendChild(h('div', { className: 'form-group' }, h('label', null, '状态'), h('select', { id: 'f-g-enabled' },
          h('option', { value: 'true', selected: !isEdit || g.enabled }, '启用'),
          h('option', { value: 'false', selected: isEdit && !g.enabled }, '停用')
        )));
        body.appendChild(h('div', { className: 'form-error', id: 'f-g-error', style: 'display:none' }));
      },
      onSave: function () {
        var data = {
          name: document.getElementById('f-g-name').value.trim(),
          api_key: document.getElementById('f-g-key').value.trim(),
          default_upstream_id: document.getElementById('f-g-upstream').value,
          enabled: document.getElementById('f-g-enabled').value === 'true'
        };
        var errEl = document.getElementById('f-g-error');
        var path = isEdit ? '/groups/' + g.id : '/groups';
        var method = isEdit ? 'PATCH' : 'POST';
        api(path, { method: method, body: data })
          .then(function () { closeModal(); showToast(isEdit ? 'API Key 组已更新' : 'API Key 组已创建'); refreshAfterConfigChange(); })
          .catch(function (e) { if (e.message !== 'login_required') { errEl.textContent = e.message; errEl.style.display = 'block'; } });
      }
    });
  }

  function deleteGroup(id) {
    api('/groups/' + id, { method: 'DELETE' })
      .then(function () {
        if (state.selectedGroupId === id) state.selectedGroupId = '';
        showToast('API Key 组已删除');
        refreshAfterConfigChange();
      })
      .catch(function (e) { if (e.message !== 'login_required') showToast(e.message, 'error'); });
  }

  // ===== Model Mappings =====
  function renderMappings() {
    var gid = state.selectedGroupId;
    var allMappings = state.dashboard.mappings || [];
    var mappings = gid ? allMappings.filter(function (m) { return m.group_id === gid; }) : [];
    var ups = state.dashboard.upstreams || [];
    var groups = state.dashboard.groups || [];
    var currentGroup = groups.find(function (g) { return g.id === gid; });

    var groupTabs = groups.map(function (g) {
      return h('button', {
        className: 'btn-sm ' + (g.id === gid ? 'btn-primary' : 'btn-ghost'),
        onClick: function () { state.selectedGroupId = g.id; renderDashboard(); }
      }, g.name);
    });

    function buildRow(m, isNew) {
      var clientInput = h('input', { type: 'text', value: isNew ? '' : m.client_model, placeholder: 'claude-sonnet-4-20250514', disabled: !isNew });
      var upstreamInput = h('input', { type: 'text', value: isNew ? '' : m.upstream_model, placeholder: 'claude-sonnet-4-20250514', disabled: !isNew });
      var overrideOpts = [h('option', { value: '' }, '-- 默认 --')].concat(ups.map(function (u) {
        return h('option', { value: u.id, selected: !isNew && m.upstream_id === u.id }, u.name);
      }));
      var overrideSelect = h('select', { disabled: !isNew }, overrideOpts);
      var enabledSelect = h('select', { disabled: !isNew },
        h('option', { value: 'true', selected: isNew || m.enabled }, '启用'),
        h('option', { value: 'false', selected: !isNew && !m.enabled }, '停用')
      );
      var rowError = h('td', { colspan: '6', className: 'row-error', style: 'display:none' });
      var errorRow = h('tr', { style: 'display:none' }, rowError);

      var doSave = function () {
        var data = {
          group_id: gid,
          client_model: clientInput.value.trim(),
          upstream_model: upstreamInput.value.trim(),
          upstream_id: overrideSelect.value,
          enabled: enabledSelect.value === 'true'
        };
        var path = isNew ? '/groups/' + gid + '/mappings' : '/mappings/' + m.id;
        var method = isNew ? 'POST' : 'PATCH';
        api(path, { method: method, body: data })
          .then(function () { showToast(isNew ? '映射已创建' : '映射已更新'); refreshAfterConfigChange(); })
          .catch(function (e) {
            if (e.message !== 'login_required') {
              rowError.textContent = e.message;
              rowError.style.display = '';
              errorRow.style.display = '';
            }
          });
      };

      var doDelete = function () {
        if (isNew) { refreshAfterConfigChange(); return; }
        showConfirm('确认删除此映射？', function () {
          api('/mappings/' + m.id, { method: 'DELETE' })
            .then(function () { showToast('映射已删除'); refreshAfterConfigChange(); })
            .catch(function (e) { if (e.message !== 'login_required') showToast(e.message, 'error'); });
        });
      };

      var saveBtn = h('button', { className: 'btn-primary btn-sm', onClick: doSave, title: '保存' }, '\u2713');
      var deleteBtn = h('button', { className: 'btn-danger btn-sm', onClick: function () { doDelete(); }, title: '删除' }, '\u2715');
      var editBtns = h('div', { style: 'display:flex;gap:4px' + (isNew ? '' : ';display:none') }, saveBtn, deleteBtn);

      var setEditing = function (editing) {
        clientInput.disabled = !editing;
        upstreamInput.disabled = !editing;
        overrideSelect.disabled = !editing;
        enabledSelect.disabled = !editing;
        editBtns.style.display = editing ? 'flex' : 'none';
        pencilBtn.style.display = editing ? 'none' : '';
        if (editing) clientInput.focus();
      };

      var pencilBtn = h('button', { className: 'btn-ghost btn-sm', onClick: function () { setEditing(true); }, title: '编辑', style: isNew ? 'display:none' : '' }, '\u270E');

      // 包装 doSave/doDelete，成功后切回画笔模式（刷新会重建 DOM，这里兜底未刷新的场景）
      var origSave = doSave;
      doSave = function () { origSave(); };
      saveBtn.onclick = doSave;
      deleteBtn.onclick = function () {
        if (isNew) { refreshAfterConfigChange(); return; }
        showConfirm('确认删除此映射？', function () {
          api('/mappings/' + m.id, { method: 'DELETE' })
            .then(function () { showToast('映射已删除'); refreshAfterConfigChange(); })
            .catch(function (e) { if (e.message !== 'login_required') showToast(e.message, 'error'); });
        });
      };

      var actionTd = h('td', { style: 'white-space:nowrap' }, pencilBtn, editBtns);

      var tr = h('tr', { className: isNew ? 'row-new' : '' },
        h('td', null, clientInput),
        h('td', null, upstreamInput),
        h('td', null, overrideSelect),
        h('td', null, enabledSelect),
        actionTd
      );
      return [tr, errorRow];
    }

    var headerText = currentGroup ? '模型映射 - ' + currentGroup.name : '模型映射';
    var tableRows = [];
    mappings.forEach(function (m) {
      var rows = buildRow(m, false);
      tableRows.push(rows[0], rows[1]);
    });

    var tbody = h('tbody', null, tableRows);
    var tableEl = h('table', null,
      h('thead', null, h('tr', null,
        h('th', null, 'Client Model'),
        h('th', null, 'Upstream Model'),
        h('th', null, '覆盖上游'),
        h('th', null, '状态'),
        h('th', null, '操作')
      )),
      tbody
    );

    var addNewRow = function () {
      var rows = buildRow({}, true);
      if (tbody.firstChild) {
        tbody.insertBefore(rows[1], tbody.firstChild);
        tbody.insertBefore(rows[0], tbody.firstChild);
      } else {
        tbody.appendChild(rows[0]);
        tbody.appendChild(rows[1]);
      }
      rows[0].querySelector('input').focus();
    };

    return h('div', { className: 'card', id: 'mappings-card' },
      h('div', { className: 'card-header' },
        h('h2', null, headerText),
        gid ? h('button', { className: 'btn-primary btn-sm', onClick: addNewRow }, '+ 新增映射') : null
      ),
      groups.length > 1 ? h('div', { style: 'margin-bottom:12px;display:flex;gap:4px;flex-wrap:wrap' }, groupTabs) : null,
      !gid ? h('div', { className: 'empty-state' }, '请先选择或创建一个 API Key 组') :
        mappings.length === 0 && groups.length > 0 ? h('div', null, h('div', { className: 'table-wrap' }, tableEl), h('div', { className: 'empty-state' }, '此组暂无模型映射，点击新增添加')) :
          h('div', { className: 'table-wrap' }, tableEl)
    );
  }

  // ===== Log Settings =====
  function renderLogSettings() {
    var s = state.dashboard.settings;
    var logModeSelect = h('select', { id: 'f-ls-mode' },
      LOG_MODES.map(function (m) { return h('option', { value: m, selected: s.log_mode === m }, m); })
    );
    var fields = [
      { id: 'f-ls-retention', label: '保留天数', value: s.log_retention_days, type: 'number' },
      { id: 'f-ls-maxrows', label: '最大行数', value: s.log_max_rows, type: 'number' },
      { id: 'f-ls-interval', label: '清理间隔 (分钟)', value: s.log_cleanup_interval_minutes, type: 'number' },
      { id: 'f-ls-reqbody', label: '请求 Body 上限 (bytes)', value: s.max_request_body_bytes, type: 'number' },
      { id: 'f-ls-resbody', label: '响应 Body 上限 (bytes)', value: s.max_response_body_bytes, type: 'number' },
      { id: 'f-ls-queue', label: '队列大小', value: s.log_queue_size, type: 'number' }
    ];
    var formFields = fields.map(function (f) {
      return h('div', { className: 'form-group' },
        h('label', null, f.label),
        h('input', { type: f.type, id: f.id, value: String(f.value) })
      );
    });

    var errEl = h('div', { className: 'form-error', id: 'f-ls-error', style: 'display:none' });

    var doSave = function () {
      var rewriteEl = document.getElementById('f-ls-rewrite-model');
      var data = {
        listen_address: s.listen_address,
        log_mode: document.getElementById('f-ls-mode').value,
        log_retention_days: parseInt(document.getElementById('f-ls-retention').value, 10) || 0,
        log_max_rows: parseInt(document.getElementById('f-ls-maxrows').value, 10) || 0,
        log_cleanup_interval_minutes: parseInt(document.getElementById('f-ls-interval').value, 10) || 1,
        max_request_body_bytes: parseInt(document.getElementById('f-ls-reqbody').value, 10) || 0,
        max_response_body_bytes: parseInt(document.getElementById('f-ls-resbody').value, 10) || 0,
        log_queue_size: parseInt(document.getElementById('f-ls-queue').value, 10) || 1,
        rewrite_model_in_response: rewriteEl ? rewriteEl.checked : !!s.rewrite_model_in_response
      };
      api('/settings/logging', { method: 'PATCH', body: data })
        .then(function () { showToast('日志设置已更新'); refreshAfterConfigChange(); })
        .catch(function (e) { if (e.message !== 'login_required') { errEl.textContent = e.message; errEl.style.display = 'block'; } });
    };

    return h('div', { className: 'card' },
      h('div', { className: 'card-header' }, h('h2', null, '\u2699 日志设置')),
      h('div', { className: 'form-group' }, h('label', null, '日志模式'), logModeSelect),
      h('div', null, formFields),
      errEl,
      h('button', { className: 'btn-primary btn-sm', onClick: doSave, style: 'margin-top:4px' }, '保存设置')
    );
  }

  // ===== Other Settings =====
  function renderAdminKey() {
    var s = state.dashboard.settings;

    // Section 1: 设置新密钥
    var keyInput = h('input', { type: 'password', id: 'f-ak-new', placeholder: '输入新管理密钥' });
    var errEl = h('div', { className: 'form-error', id: 'f-ak-error', style: 'display:none' });
    var doChange = function () {
      var newKey = keyInput.value.trim();
      if (!newKey) { errEl.textContent = '密钥不能为空'; errEl.style.display = 'block'; return; }
      errEl.style.display = 'none';
      api('/admin-key', { method: 'POST', body: { admin_key: newKey } })
        .then(function () { keyInput.value = ''; showToast('管理密钥已更新'); refreshAfterConfigChange(); })
        .catch(function (e) { if (e.message !== 'login_required') { errEl.textContent = e.message; errEl.style.display = 'block'; } });
    };
    var keySection = h('div', { className: 'settings-block' },
      h('div', { className: 'settings-block-title' }, '设置新密钥'),
      h('div', { className: 'settings-block-body' },
        h('div', { style: 'display:flex;gap:8px;align-items:center' },
          keyInput,
          h('button', { className: 'btn-primary btn-sm', onClick: doChange }, '修改')
        ),
        errEl
      )
    );

    // Section 2: DB 大小
    var dbSection = h('div', { className: 'settings-block' },
      h('div', { className: 'settings-block-title' }, 'DB 大小'),
      h('div', { className: 'settings-block-body' },
        h('div', { className: 'settings-block-value' }, formatBytes(state.dashboard.summary.sqlite_file_size))
      )
    );

    // Section 3: 响应模型名回写
    var doToggleRewrite = function () {
      var checked = document.getElementById('f-ls-rewrite-model').checked;
      api('/settings/logging', { method: 'PATCH', body: {
        listen_address: s.listen_address,
        log_mode: s.log_mode,
        log_retention_days: s.log_retention_days,
        log_max_rows: s.log_max_rows,
        log_cleanup_interval_minutes: s.log_cleanup_interval_minutes,
        max_request_body_bytes: s.max_request_body_bytes,
        max_response_body_bytes: s.max_response_body_bytes,
        log_queue_size: s.log_queue_size,
        rewrite_model_in_response: checked
      }})
        .then(function () { showToast(checked ? '响应模型名回写已开启' : '响应模型名回写已关闭'); refreshAfterConfigChange(); })
        .catch(function (e) { if (e.message !== 'login_required') showToast(e.message, 'error'); });
    };
    var rewriteSection = h('div', { className: 'settings-block' },
      h('div', { className: 'settings-block-title' }, '响应模型名回写'),
      h('div', { className: 'settings-block-body' },
        h('div', { style: 'display:flex;align-items:center;gap:8px' },
          h('input', { type: 'checkbox', id: 'f-ls-rewrite-model', checked: !!s.rewrite_model_in_response, onChange: doToggleRewrite }),
          h('label', { htmlFor: 'f-ls-rewrite-model', style: 'margin:0;cursor:pointer;font-size:13px;color:var(--gray-600)' }, s.rewrite_model_in_response ? '已开启' : '已关闭')
        )
      )
    );

    return h('div', { className: 'card' },
      h('div', { className: 'card-header' }, h('h2', null, '\u2699 其他设置')),
      keySection,
      dbSection,
      rewriteSection
    );
  }

  function doCleanupLogs() {
    api('/logs/cleanup', { method: 'POST' })
      .then(function () { showToast('日志清理完成'); refreshAfterConfigChange(); })
      .catch(function (e) { if (e.message !== 'login_required') showToast(e.message, 'error'); });
  }

  // ===== Logs Viewer =====
  function openLogsModal() {
    state.logs.open = true;
    state.logs.filter = {};
    state.logs.items = [];
    state.logs.total = 0;
    state.logs.page = 1;
    state.logs.selectedId = '';
    state.logs.detail = null;
    state.logs.tab = 'summary';
    state.logs.advancedOpen = false;
    renderLogsOverlay();
    loadLogs();
  }

  function closeLogsModal() {
    state.logs.open = false;
    $('logs-overlay').innerHTML = '';
  }

  function renderLogsOverlay() {
    var root = $('logs-overlay');
    root.innerHTML = '';
    root.appendChild(
      h('div', { className: 'logs-overlay' },
        renderLogsTopbar(),
        renderLogsFilters(),
        h('div', { className: 'logs-body' },
          h('div', { className: 'logs-list', id: 'logs-list-panel' },
            h('div', { className: 'logs-list-content', id: 'logs-list-content' }),
            h('div', { className: 'logs-pagination', id: 'logs-pagination' })
          ),
          h('div', { className: 'logs-detail', id: 'logs-detail-panel' },
            h('div', { className: 'logs-detail-empty' }, '选择一条日志查看详情')
          )
        )
      )
    );
    var handler = function (e) {
      if (e.key === 'Escape') {
        closeLogsModal();
        document.removeEventListener('keydown', handler);
      }
    };
    document.addEventListener('keydown', handler);
  }

  function renderLogsTopbar() {
    return h('div', { className: 'logs-topbar' },
      h('div', null,
        h('h2', null, 'Request Logs'),
        h('span', { className: 'subtitle', style: 'margin-left:8px' }, '网关请求日志查看器')
      ),
      h('div', { className: 'logs-topbar-right' },
        h('button', { className: 'btn-danger btn-sm', onClick: function () { showConfirm('确认清理日志？', function () { doCleanupLogs(); loadLogs(); }); } }, '清理'),
        h('button', { className: 'btn-secondary btn-sm', onClick: function () { loadLogs(); } }, '刷新'),
        h('button', { className: 'btn-ghost btn-sm', onClick: closeLogsModal, title: '关闭' }, '\u2715 关闭')
      )
    );
  }

  function renderLogsFilters() {
    var f = state.logs.filter;
    var search = h('input', { type: 'text', value: f.search || '', placeholder: '搜索 path/model/session' });
    var after = h('input', { type: 'datetime-local', value: f.started_after || '' });
    var before = h('input', { type: 'datetime-local', value: f.started_before || '' });
    var statusCode = h('input', { type: 'text', value: f.status_code || '', placeholder: '如 200' });
    var clientModel = h('input', { type: 'text', value: f.client_model || '', placeholder: '' });
    var isStream = h('select', null,
      h('option', { value: '', selected: !f.is_stream }, '全部'),
      h('option', { value: 'true', selected: f.is_stream === 'true' }, '是'),
      h('option', { value: 'false', selected: f.is_stream === 'false' }, '否')
    );

    // Advanced filters
    var upstreamId = h('input', { type: 'text', value: f.upstream_id || '', placeholder: '' });
    var groupId = h('input', { type: 'text', value: f.group_id || '', placeholder: '' });
    var upstreamModel = h('input', { type: 'text', value: f.upstream_model || '', placeholder: '' });
    var sessionId = h('input', { type: 'text', value: f.session_id || '', placeholder: '' });
    var agentId = h('input', { type: 'text', value: f.agent_id || '', placeholder: '' });
    var truncated = h('select', null,
      h('option', { value: '', selected: !f.truncated }, '全部'),
      h('option', { value: 'true', selected: f.truncated === 'true' }, '是'),
      h('option', { value: 'false', selected: f.truncated === 'false' }, '否')
    );

    var advancedGrid = h('div', { className: 'filter-grid', style: state.logs.advancedOpen ? '' : 'display:none', id: 'logs-adv-filter' },
      h('div', null, h('label', null, 'Upstream ID'), upstreamId),
      h('div', null, h('label', null, 'Group ID'), groupId),
      h('div', null, h('label', null, 'Upstream Model'), upstreamModel),
      h('div', null, h('label', null, 'Session ID'), sessionId),
      h('div', null, h('label', null, 'Agent ID'), agentId),
      h('div', null, h('label', null, '截断'), truncated)
    );

    var advToggle = h('button', { className: 'advanced-toggle', onClick: function () {
      state.logs.advancedOpen = !state.logs.advancedOpen;
      advancedGrid.style.display = state.logs.advancedOpen ? '' : 'none';
      advToggle.textContent = (state.logs.advancedOpen ? '\u25BC' : '\u25B6') + ' 高级筛选';
    } }, (state.logs.advancedOpen ? '\u25BC' : '\u25B6') + ' 高级筛选');

    var doSearch = function () {
      state.logs.filter = {
        search: search.value.trim(),
        started_after: after.value ? new Date(after.value).toISOString() : '',
        started_before: before.value ? new Date(before.value).toISOString() : '',
        status_code: statusCode.value.trim(),
        client_model: clientModel.value.trim(),
        is_stream: isStream.value,
        upstream_id: upstreamId.value.trim(),
        group_id: groupId.value.trim(),
        upstream_model: upstreamModel.value.trim(),
        session_id: sessionId.value.trim(),
        agent_id: agentId.value.trim(),
        truncated: truncated.value
      };
      state.logs.page = 1;
      loadLogs();
    };

    search.addEventListener('keydown', function (e) { if (e.key === 'Enter') doSearch(); });

    return h('div', { className: 'logs-filters' },
      h('div', { className: 'filter-grid' },
        h('div', null, h('label', null, '搜索'), search),
        h('div', null, h('label', null, '开始时间'), after),
        h('div', null, h('label', null, '结束时间'), before),
        h('div', null, h('label', null, '状态码'), statusCode),
        h('div', null, h('label', null, 'Client Model'), clientModel),
        h('div', null, h('label', null, 'Stream'), isStream),
        h('div', null, h('button', { className: 'btn-primary btn-sm', onClick: doSearch, style: 'margin-top:18px;width:100%' }, '筛选'))
      ),
      advToggle,
      advancedGrid
    );
  }

  function loadLogs() {
    var f = state.logs.filter;
    var params = new URLSearchParams();
    params.set('page', state.logs.page);
    params.set('page_size', PAGE_SIZE);
    if (f.search) params.set('search', f.search);
    if (f.started_after) params.set('started_after', f.started_after);
    if (f.started_before) params.set('started_before', f.started_before);
    if (f.status_code) params.set('status_code', f.status_code);
    if (f.client_model) params.set('client_model', f.client_model);
    if (f.upstream_model) params.set('upstream_model', f.upstream_model);
    if (f.upstream_id) params.set('upstream_id', f.upstream_id);
    if (f.group_id) params.set('group_id', f.group_id);
    if (f.session_id) params.set('session_id', f.session_id);
    if (f.agent_id) params.set('agent_id', f.agent_id);
    if (f.is_stream) params.set('is_stream', f.is_stream);
    if (f.truncated) params.set('truncated', f.truncated);

    api('/logs?' + params.toString())
      .then(function (data) {
        state.logs.items = data.items || [];
        state.logs.total = data.total || 0;
        state.logs.page = data.page || 1;
        renderLogsList();
        renderLogsPagination();
      })
      .catch(function (e) {
        if (e.message !== 'login_required') {
          var el = document.getElementById('logs-list-content');
          if (el) el.innerHTML = '';
          if (el) el.appendChild(h('div', { className: 'empty-state' }, '加载日志失败: ' + e.message));
        }
      });
  }

  function renderLogsList() {
    var el = document.getElementById('logs-list-content');
    if (!el) return;
    el.innerHTML = '';
    if (state.logs.items.length === 0) {
      el.appendChild(h('div', { className: 'empty-state' }, '暂无日志记录'));
      return;
    }
    var tbody = h('tbody', null,
      state.logs.items.map(function (item) {
        var isActive = item.id === state.logs.selectedId;
        return h('tr', { className: isActive ? 'active' : '', onClick: function () { loadLogDetail(item.id); } },
          h('td', null, formatTime(item.started_at)),
          h('td', null, h('span', { className: 'status-badge ' + statusClass(item.status_code) }, String(item.status_code || '-'))),
          h('td', null, item.duration_ms + 'ms', item.is_stream ? h('span', { className: 'pill pill-blue', style: 'margin-left:4px;font-size:9px' }, 'SSE') : null),
          h('td', null, item.client_model || '-'),
          h('td', null, item.upstream_model || '-'),
          h('td', null, item.upstream_name || '-'),
          h('td', null, item.api_key_group_name || '-'),
          h('td', { title: item.path || '' }, (item.path || '').length > 30 ? (item.path || '').substring(0, 30) + '...' : (item.path || '-')),
          h('td', null, item.claude_session_id ? (item.claude_session_id.length > 10 ? item.claude_session_id.substring(0, 10) + '...' : item.claude_session_id) : '-')
        );
      })
    );
    var table = h('table', null,
      h('thead', null, h('tr', null,
        h('th', null, '时间'),
        h('th', null, '状态'),
        h('th', null, '耗时'),
        h('th', null, 'Client Model'),
        h('th', null, 'Upstream Model'),
        h('th', null, '上游'),
        h('th', null, 'Group'),
        h('th', null, 'Path'),
        h('th', null, 'Session')
      )),
      tbody
    );
    el.appendChild(table);
  }

  function renderLogsPagination() {
    var el = document.getElementById('logs-pagination');
    if (!el) return;
    el.innerHTML = '';
    var totalPages = Math.max(1, Math.ceil(state.logs.total / PAGE_SIZE));
    el.appendChild(h('span', null, '共 ' + state.logs.total + ' 条，第 ' + state.logs.page + '/' + totalPages + ' 页'));
    el.appendChild(h('div', { className: 'logs-pagination-btns' },
      h('button', { className: 'btn-secondary btn-sm', disabled: state.logs.page <= 1, onClick: function () { state.logs.page--; loadLogs(); } }, '\u25C0 上一页'),
      h('button', { className: 'btn-secondary btn-sm', disabled: state.logs.page >= totalPages, onClick: function () { state.logs.page++; loadLogs(); } }, '下一页 \u25B6')
    ));
  }

  function loadLogDetail(id) {
    state.logs.selectedId = id;
    state.logs.tab = 'summary';
    state.logs.responseSubTab = 'collected';
    renderLogsList(); // update active row
    var panel = document.getElementById('logs-detail-panel');
    if (!panel) return;
    panel.innerHTML = '';
    panel.appendChild(h('div', { className: 'logs-detail-empty' }, '加载中...'));
    api('/logs/' + id)
      .then(function (detail) {
        state.logs.detail = detail;
        renderLogDetailPanel();
      })
      .catch(function (e) {
        if (e.message !== 'login_required') {
          panel.innerHTML = '';
          panel.appendChild(h('div', { className: 'logs-detail-empty' }, e.message === 'log not found' ? '日志已被清理' : '加载失败: ' + e.message));
        }
      });
  }

  function renderLogDetailPanel() {
    var panel = document.getElementById('logs-detail-panel');
    if (!panel) return;
    panel.innerHTML = '';
    var d = state.logs.detail;
    if (!d) { panel.appendChild(h('div', { className: 'logs-detail-empty' }, '选择一条日志查看详情')); return; }

    var tabs = ['summary', 'request', 'response', 'error'];
    var tabLabels = { summary: 'Summary', request: 'Request', response: 'Response', error: 'Error' };
    var tabBar = h('div', { className: 'detail-tabs' },
      tabs.map(function (t) {
        var btn = h('button', {
          className: 'detail-tab' + (state.logs.tab === t ? ' active' : ''),
          onClick: function () { state.logs.tab = t; renderLogDetailPanel(); }
        }, tabLabels[t]);
        if (t === 'error' && d.error) {
          btn.appendChild(h('span', { className: 'detail-tab-badge' }, '!'));
        }
        return btn;
      })
    );

    var content = h('div', { className: 'detail-content' });
    if (state.logs.tab === 'summary') renderDetailSummary(content, d);
    else if (state.logs.tab === 'request') renderDetailPayload(content, d, 'request');
    else if (state.logs.tab === 'response') renderDetailPayload(content, d, 'response');
    else if (state.logs.tab === 'error') renderDetailError(content, d);

    panel.appendChild(tabBar);
    panel.appendChild(content);
  }

  function detailField(label, value) {
    return h('div', { className: 'detail-field' },
      h('label', null, label),
      h('div', { className: 'val' }, value || '-')
    );
  }

  function renderDetailSummary(el, d) {
    el.appendChild(h('div', { className: 'detail-section' },
      h('h4', null, '概览'),
      h('div', { className: 'detail-grid' },
        h('div', { className: 'detail-field' },
          h('label', null, '状态码'),
          h('div', null, h('span', { className: 'status-badge ' + statusClass(d.status_code) }, String(d.status_code || '-')))
        ),
        detailField('耗时', d.duration_ms + ' ms'),
        detailField('方法', d.method),
        detailField('路径', d.path)
      )
    ));
    el.appendChild(h('div', { className: 'detail-section' },
      h('h4', null, '路由'),
      h('div', { className: 'detail-grid' },
        detailField('目标 URL', d.target_url),
        detailField('上游', d.upstream_name || d.upstream_id || '-'),
        detailField('上游 Host', d.upstream_host),
        detailField('API Key 组', d.api_key_group_name || d.api_key_group_id || '-')
      )
    ));
    el.appendChild(h('div', { className: 'detail-section' },
      h('h4', null, '模型'),
      h('div', { className: 'detail-grid' },
        detailField('Client Model', d.client_model),
        detailField('Upstream Model', d.upstream_model)
      )
    ));
    el.appendChild(h('div', { className: 'detail-section' },
      h('h4', null, 'Claude Code'),
      h('div', { className: 'detail-grid' },
        detailField('Session ID', d.claude_session_id),
        detailField('Agent ID', d.claude_agent_id),
        detailField('Parent Agent ID', d.claude_parent_agent_id)
      )
    ));
    el.appendChild(h('div', { className: 'detail-section' },
      h('h4', null, '传输'),
      h('div', { className: 'detail-grid' },
        detailField('Stream', d.is_stream ? '是' : '否'),
        detailField('请求大小', formatBytes(d.request_bytes)),
        detailField('响应大小', formatBytes(d.response_bytes)),
        detailField('日志模式', d.log_mode),
        detailField('请求 Body 截断', d.request_body_truncated ? '是' : '否'),
        detailField('响应 Body 截断', d.response_body_truncated ? '是' : '否')
      )
    ));
    el.appendChild(h('div', { className: 'detail-section' },
      h('h4', null, '时间'),
      h('div', { className: 'detail-grid' },
        detailField('开始', formatTime(d.started_at)),
        detailField('完成', formatTime(d.completed_at))
      )
    ));
  }

  function renderDetailPayload(el, d, type) {
    var headersKey = type === 'request' ? 'request_headers_json' : 'response_headers_json';
    var bodyKey = type === 'request' ? 'request_body' : 'response_body';
    var headersRaw = d[headersKey];
    var bodyRaw = d[bodyKey];

    el.appendChild(h('div', { className: 'detail-section' },
      h('h4', null, 'Headers'),
      headersRaw ? (function () {
        try {
          var parsed = JSON.parse(headersRaw);
          var lines = Object.keys(parsed).map(function (k) { return k + ': ' + (Array.isArray(parsed[k]) ? parsed[k].join(', ') : parsed[k]); }).join('\n');
          return h('div', { className: 'pre' }, lines);
        } catch (e) { return h('div', { className: 'pre' }, headersRaw); }
      })() : h('div', { className: 'pre-placeholder' }, d.log_mode === 'metadata' ? '当前日志模式 (metadata) 未记录 headers' : d.log_mode === 'off' ? '日志已关闭' : '无 headers 数据')
    ));

    if (type === 'response' && d.response_body_mode === 'sse') {
      var subTab = state.logs.responseSubTab || 'collected';
      var tabBar = h('div', { className: 'body-sub-tabs' },
        h('button', {
          className: 'body-sub-tab' + (subTab === 'collected' ? ' active' : ''),
          onClick: function () { state.logs.responseSubTab = 'collected'; renderLogDetailPanel(); }
        }, 'Collected JSON'),
        h('button', {
          className: 'body-sub-tab' + (subTab === 'raw' ? ' active' : ''),
          onClick: function () { state.logs.responseSubTab = 'raw'; renderLogDetailPanel(); }
        }, 'Raw SSE')
      );
      var bodyContent;
      if (subTab === 'collected') {
        bodyContent = (d.summary_json && d.summary_json !== '{}')
          ? h('div', { className: 'pre' }, prettyJSON(d.summary_json))
          : h('div', { className: 'pre-placeholder' }, 'SSE 收集结果为空');
      } else {
        bodyContent = bodyRaw
          ? h('div', { className: 'pre' }, decodeBase64(bodyRaw))
          : h('div', { className: 'pre-placeholder' }, '无原始 SSE 数据');
      }
      el.appendChild(h('div', { className: 'detail-section' },
        h('h4', null, 'Body'),
        tabBar,
        bodyContent
      ));
    } else {
      el.appendChild(h('div', { className: 'detail-section' },
        h('h4', null, 'Body'),
        bodyRaw ? (function () {
          var decoded = decodeBase64(bodyRaw);
          return h('div', { className: 'pre' }, prettyJSON(decoded));
        })() : h('div', { className: 'pre-placeholder' }, d.log_mode === 'metadata' ? '当前日志模式 (metadata) 未记录 payload' : d.log_mode === 'off' ? '日志已关闭' : '无 body 数据')
      ));
    }

    if (type === 'response' && d.response_body_mode) {
      el.appendChild(h('div', { className: 'detail-section' },
        detailField('响应模式', d.response_body_mode)
      ));
    }
  }

  function renderDetailError(el, d) {
    if (d.error) {
      el.appendChild(h('div', { className: 'detail-section' },
        h('h4', null, '错误信息'),
        h('div', { className: 'pre', style: 'border-color:var(--red);background:var(--red-light)' }, d.error)
      ));
    } else {
      el.appendChild(h('div', { className: 'empty-state', style: 'color:var(--green)' }, '\u2713 此请求无错误'));
    }
    if (d.summary_json && d.summary_json !== '{}' && d.response_body_mode !== 'sse') {
      el.appendChild(h('div', { className: 'detail-section' },
        h('h4', null, 'Summary JSON'),
        h('div', { className: 'pre' }, prettyJSON(d.summary_json))
      ));
    }
  }

  // ===== Init =====
  function loadDashboard() {
    api('/dashboard')
      .then(function (data) {
        state.dashboard = data;
        state.selectedGroupId = data.summary.selected_group_id || (data.groups && data.groups.length > 0 ? data.groups[0].id : '');
        renderDashboard();
      })
      .catch(function (e) {
        if (e.message === 'login_required') return;
        var app = $('app');
        app.innerHTML = '';
        app.appendChild(h('div', { className: 'login-wrap' },
          h('div', { className: 'card', style: 'text-align:center;max-width:400px' },
            h('h2', null, '加载失败'),
            h('p', { style: 'color:var(--gray-500);margin:12px 0' }, e.message),
            h('button', { className: 'btn-primary', onClick: loadDashboard }, '重试')
          )
        ));
      });
  }

  function refreshAfterConfigChange() {
    api('/dashboard')
      .then(function (data) {
        state.dashboard = data;
        if (state.selectedGroupId) {
          var exists = data.groups && data.groups.some(function (g) { return g.id === state.selectedGroupId; });
          if (!exists) state.selectedGroupId = data.groups && data.groups.length > 0 ? data.groups[0].id : '';
        } else {
          state.selectedGroupId = data.summary.selected_group_id || (data.groups && data.groups.length > 0 ? data.groups[0].id : '');
        }
        renderDashboard();
      })
      .catch(function (e) { if (e.message !== 'login_required') showToast('刷新失败: ' + e.message, 'error'); });
  }

  document.addEventListener('DOMContentLoaded', loadDashboard);
})();
