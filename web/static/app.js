// minimax-2api 管理台前端逻辑（纯 vanilla JS，无框架无 CDN）
// 页面结构：登录页 + 五个 Tab（仪表盘/账号管理/视频任务/API 密钥/设置）
// 所有请求走同源 /api/*，session 由 HttpOnly cookie 承载。

'use strict';

// ---- 基础设施 ----

let currentSection = 'dashboard';
let taskPage = 1;
const taskPageSize = 20;
let taskTimer = null;

// api 请求封装：非 2xx 抛错（携带服务端 error 消息）
async function api(path, options = {}) {
  const opts = Object.assign({ credentials: 'same-origin' }, options);
  if (opts.body && typeof opts.body !== 'string') {
    opts.body = JSON.stringify(opts.body);
    opts.headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers);
  }
  const resp = await fetch(path, opts);
  let data = null;
  try { data = await resp.json(); } catch (e) { /* 非 JSON 响应 */ }
  if (!resp.ok) {
    const msg = (data && (data.error || (data.error && data.error.message))) || ('HTTP ' + resp.status);
    throw new Error(typeof msg === 'string' ? msg : JSON.stringify(msg));
  }
  return data;
}

function toast(msg, type = 'info') {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast show toast-' + type;
  clearTimeout(el._t);
  el._t = setTimeout(() => { el.className = 'toast'; }, 3500);
}

function showLogin() {
  document.getElementById('loginPage').style.display = 'flex';
  document.getElementById('mainApp').style.display = 'none';
}

function showMain(username) {
  document.getElementById('loginPage').style.display = 'none';
  document.getElementById('mainApp').style.display = 'block';
  document.getElementById('userAvatar').textContent = (username || 'A').charAt(0).toUpperCase();
  switchSection(currentSection);
}

// ---- 登录 / 登出 ----

async function doLogin() {
  const username = document.getElementById('loginUser').value.trim();
  const password = document.getElementById('loginPass').value;
  const errEl = document.getElementById('loginError');
  errEl.style.display = 'none';
  try {
    const data = await api('/api/login', { method: 'POST', body: { username, password } });
    localStorage.setItem('mm2api_user', data.username || username);
    showMain(data.username || username);
    refreshAll();
  } catch (e) {
    errEl.textContent = e.message;
    errEl.style.display = 'block';
  }
}

async function doLogout() {
  try { await api('/api/logout', { method: 'POST' }); } catch (e) { /* 忽略 */ }
  localStorage.removeItem('mm2api_user');
  showLogin();
}

// 回车登录
document.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && document.getElementById('loginPage').style.display !== 'none') {
    doLogin();
  }
});

// ---- Tab 切换 ----

function switchSection(name) {
  currentSection = name;
  document.querySelectorAll('.pill-nav .nav-link').forEach((el) => {
    el.classList.toggle('active', el.dataset.section === name);
  });
  document.querySelectorAll('.section').forEach((el) => {
    el.classList.toggle('active', el.id === 'section-' + name);
  });
  stopTaskAutoRefresh();
  tgStopPolling(); // 离开在线测试页停止 3s 轮询（回来且有未完成任务时自动恢复）
  if (name === 'dashboard') loadStats();
  if (name === 'accounts') loadAccounts();
  if (name === 'tasks') { loadTasks(); startTaskAutoRefresh(); }
  if (name === 'test') { loadTestTab(); tgResumePolling(); }
  if (name === 'keys') loadKeys();
  if (name === 'settings') loadSettings();
}

function refreshAll() {
  if (currentSection === 'dashboard') loadStats();
  if (currentSection === 'accounts') loadAccounts();
  if (currentSection === 'tasks') loadTasks();
  if (currentSection === 'test') loadTestTab();
  if (currentSection === 'keys') loadKeys();
}

// ---- 弹窗 ----

function showModal(id) { document.getElementById(id).classList.add('show'); }
function hideModal(id) {
  document.getElementById(id).classList.remove('show');
  if (id === 'previewModal') {
    // 还原为视频容器并停止播放（图片预览时容器内容被替换过）
    const box = document.getElementById('previewMedia');
    box.innerHTML = '<video id="previewVideo" controls style="width:100%;border-radius:8px;background:#000"></video>';
  }
}
// 点击遮罩关闭
document.addEventListener('click', (e) => {
  if (e.target.classList && e.target.classList.contains('modal-overlay')) {
    hideModal(e.target.id);
  }
});

// ---- 工具函数 ----

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

function fmtTime(s) {
  if (!s) return '-';
  const d = new Date(s);
  if (isNaN(d.getTime())) return s;
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getMonth() + 1}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function fmtNum(n) {
  if (n == null) return '-';
  if (Number.isInteger(n)) return String(n);
  return n.toFixed(1);
}

// 账号状态徽章
const acctStatusBadge = {
  active: ['badge-success', '正常'],
  token_expired: ['badge-danger', 'Token失效'],
  empty: ['badge-warning', '余额耗尽'],
  cooldown: ['badge-muted', '冷却中'],
  disabled: ['badge-muted', '已禁用'],
};

// 任务状态徽章（内部状态 → 展示）
const taskStatusBadge = {
  QUEUED: ['badge-muted', '排队'],
  PRECHECK: ['badge-info', '预检'],
  UPLOADING: ['badge-info', '上传素材'],
  SUBMITTING: ['badge-info', '提交中'],
  POLLING: ['badge-info', '生成中'],
  RETRIEVING: ['badge-info', '取件'],
  DOWNLOADING: ['badge-info', '下载'],
  SUCCEEDED: ['badge-success', '成功'],
  FAILED: ['badge-danger', '失败'],
  CANCELLED: ['badge-muted', '已取消'],
  SUBMIT_UNKNOWN: ['badge-warning', '提交不明'],
};

function badge(map, key) {
  const b = map[key] || ['badge-muted', key || '-'];
  return `<span class="badge ${b[0]}">${esc(b[1])}</span>`;
}

// ---- 仪表盘 ----

async function loadStats() {
  try {
    const s = await api('/api/stats');
    document.getElementById('statAccTotal').textContent = s.accounts_total;
    document.getElementById('statAccActive').textContent = s.accounts_active;
    document.getElementById('statBalance').textContent = fmtNum(Math.round(s.balance_total));
    document.getElementById('statTasksToday').textContent = s.tasks_today;
    document.getElementById('statSucc').textContent = s.tasks_succeeded;
    document.getElementById('statFail').textContent = s.tasks_failed;
    const done = s.tasks_succeeded + s.tasks_failed;
    document.getElementById('statSuccRate').textContent =
      done > 0 ? Math.round((s.tasks_succeeded / done) * 100) + '%' : '-';
    document.getElementById('statCredits').textContent = fmtNum(Math.round(s.credits_used_total));

    const daily = s.daily || [];
    const el = document.getElementById('dailyTable');
    if (daily.length === 0) {
      el.innerHTML = '<div class="empty"><p>近 7 天暂无任务</p></div>';
      return;
    }
    let html = '<div class="table-wrap"><table><thead><tr><th>日期</th><th>任务数</th><th>消耗积分</th></tr></thead><tbody>';
    for (const d of daily) {
      html += `<tr><td>${esc(d.date)}</td><td>${d.tasks}</td><td>${fmtNum(d.credits)}</td></tr>`;
    }
    el.innerHTML = html + '</tbody></table></div>';
  } catch (e) {
    toast('加载统计失败: ' + e.message, 'error');
  }
}

// ---- 账号管理 ----

async function loadAccounts() {
  try {
    const data = await api('/api/accounts');
    const accounts = data.accounts || [];
    document.getElementById('accCount').textContent = `共 ${accounts.length} 个账号`;
    const el = document.getElementById('accountsTable');
    if (accounts.length === 0) {
      el.innerHTML = '<div class="empty"><p>暂无账号，点击「+ 导入账号」开始</p></div>';
      return;
    }
    let html = `<div class="table-wrap"><table><thead><tr>
      <th>备注</th><th>用户名</th><th>状态</th><th>余额</th>
      <th>成功/失败</th><th>任务数</th><th>最后续期</th><th>最后错误</th><th>操作</th>
    </tr></thead><tbody>`;
    for (const a of accounts) {
      const disabledTag = a.is_enabled ? '' : ' <span class="badge badge-muted">停用</span>';
      html += `<tr>
        <td title="${esc(a.id)}">${esc(a.label || '-')}</td>
        <td>${esc(a.username || a.user_id || '-')}${disabledTag}</td>
        <td>${badge(acctStatusBadge, a.status)}</td>
        <td>${a.balance < 0 ? '<span style="color:var(--c-text-lighter)">未知</span>' : fmtNum(Math.round(a.balance))}</td>
        <td><span style="color:var(--c-success-dark)">${a.success_count}</span> / <span style="color:var(--c-danger)">${a.fail_count}</span></td>
        <td>${a.total_tasks}</td>
        <td>${fmtTime(a.last_renew_at)}</td>
        <td title="${esc(a.last_error)}" style="max-width:160px">${esc(a.last_error || '-')}</td>
        <td style="white-space:nowrap">
          <button class="btn btn-sm btn-secondary" onclick="testAccount('${esc(a.id)}',this)">测试</button>
          <button class="btn btn-sm btn-secondary" onclick="refreshAccount('${esc(a.id)}',this)">刷新</button>
          <button class="btn btn-sm btn-secondary" onclick="toggleAccount('${esc(a.id)}',${!a.is_enabled})">${a.is_enabled ? '停用' : '启用'}</button>
          <button class="btn btn-sm btn-secondary" onclick="claimTrial('${esc(a.id)}',this)">领试用</button>
          <button class="btn btn-sm btn-secondary" onclick="restoreLink('${esc(a.id)}')">恢复链接</button>
          <button class="btn btn-sm btn-danger" onclick="deleteAccount('${esc(a.id)}','${esc(a.label || a.username || '')}')">删除</button>
        </td></tr>`;
    }
    el.innerHTML = html + '</tbody></table></div>';
  } catch (e) {
    toast('加载账号失败: ' + e.message, 'error');
  }
}

function showImportModal() {
  document.getElementById('importContent').value = '';
  document.getElementById('importLabelPrefix').value = '';
  document.getElementById('importResult').style.display = 'none';
  showModal('importModal');
}

async function doImport() {
  const content = document.getElementById('importContent').value;
  const labelPrefix = document.getElementById('importLabelPrefix').value.trim();
  if (!content.trim()) { toast('请粘贴深链 URL 或 JWT', 'error'); return; }
  const btn = document.getElementById('importBtn');
  btn.disabled = true; btn.textContent = '导入中（逐个验证，请稍候）...';
  const resultEl = document.getElementById('importResult');
  resultEl.style.display = 'none';
  try {
    const r = await api('/api/accounts/import', { method: 'POST', body: { content, label_prefix: labelPrefix } });
    let html = `<div class="warn-box" style="background:#f0fdf4;border-color:#bbf7d0;color:#166534">
      导入完成：新增 ${r.created}，更新 ${r.updated}，失败 ${r.failed}（共 ${r.total} 行）</div>`;
    html += '<div style="max-height:200px;overflow-y:auto;font-size:12px">';
    for (const item of r.results || []) {
      const color = item.status === 'failed' ? 'var(--c-danger)' : 'var(--c-text-light)';
      html += `<div style="padding:3px 0;color:${color}">[${item.status}] ${esc(item.summary)} — ${esc(item.message || '')}</div>`;
    }
    resultEl.innerHTML = html + '</div>';
    resultEl.style.display = 'block';
    toast(`导入完成：新增 ${r.created}，更新 ${r.updated}，失败 ${r.failed}`, r.failed > 0 ? 'info' : 'success');
    loadAccounts();
  } catch (e) {
    toast('导入失败: ' + e.message, 'error');
  } finally {
    btn.disabled = false; btn.textContent = '开始导入';
  }
}

async function refreshAccount(id, btn) {
  btn.disabled = true; btn.textContent = '刷新中...';
  try {
    const r = await api(`/api/accounts/${id}/refresh`, { method: 'POST' });
    toast(r.warning ? ('刷新完成（有警告）: ' + r.warning) : '账号刷新成功', r.warning ? 'info' : 'success');
    loadAccounts();
  } catch (e) {
    toast('刷新失败: ' + e.message, 'error');
  } finally {
    btn.disabled = false; btn.textContent = '刷新';
  }
}

async function toggleAccount(id, enabled) {
  try {
    await api(`/api/accounts/${id}/toggle`, { method: 'POST', body: { is_enabled: enabled } });
    toast(enabled ? '账号已启用' : '账号已停用', 'success');
    loadAccounts();
  } catch (e) { toast('操作失败: ' + e.message, 'error'); }
}

async function claimTrial(id, btn) {
  btn.disabled = true;
  try {
    const r = await api(`/api/accounts/${id}/claim-trial`, { method: 'POST' });
    toast(`领取结果：claimed=${r.claimed} 剩余免费次数=${r.remainingCount}`, 'success');
    loadAccounts();
  } catch (e) {
    toast('领取失败: ' + e.message, 'error');
  } finally { btn.disabled = false; }
}

// ---- 账号恢复链接（生成官方桌面端深链，导入的逆过程） ----

let restoreAccountId = '';

async function restoreLink(id) {
  restoreAccountId = id;
  document.getElementById('restoreRenew').checked = false;
  await fetchRestoreLink(id, false);
}

async function fetchRestoreLink(id, renew) {
  try {
    const r = await api(`/api/accounts/${encodeURIComponent(id)}/restore-link` + (renew ? '?renew=1' : ''));
    document.getElementById('restoreAccountId').textContent = id;
    document.getElementById('restoreLinkText').value = r.url;
    let meta = `用户 ID：${esc(r.user_id || '-')}`;
    if (r.token_exp_human) {
      meta += r.token_expired
        ? ` · <span style="color:var(--c-danger)">token 已过期（${esc(r.token_exp_human)}），请勾选"先续期"重新生成</span>`
        : ` · token 有效期至 ${esc(r.token_exp_human)}`;
    }
    if (r.renewed) meta += ' · <span style="color:var(--c-success-dark)">已续期</span>';
    if (r.warning) meta += `<div style="color:var(--c-danger);margin-top:4px">${esc(r.warning)}</div>`;
    document.getElementById('restoreMeta').innerHTML = meta;
    showModal('restoreModal');
    if (r.warning) toast(r.warning, 'info');
  } catch (e) {
    toast('生成恢复链接失败: ' + e.message, 'error');
  }
}

async function regenRestoreLink(btn) {
  const renew = document.getElementById('restoreRenew').checked;
  btn.disabled = true; btn.textContent = renew ? '续期并生成中...' : '生成中...';
  try {
    await fetchRestoreLink(restoreAccountId, renew);
    if (renew) loadAccounts(); // 续期会更新 token/时间，刷新列表
  } finally {
    btn.disabled = false; btn.textContent = '重新生成';
  }
}

function copyRestoreLink() {
  const ta = document.getElementById('restoreLinkText');
  ta.select();
  navigator.clipboard.writeText(ta.value)
    .then(() => toast('已复制到剪贴板', 'success'))
    .catch(() => { document.execCommand('copy'); toast('已复制到剪贴板', 'success'); });
}

function openRestoreLink() {
  const url = document.getElementById('restoreLinkText').value.trim();
  if (!url) { toast('链接为空', 'error'); return; }
  // 浏览器会弹出"打开 MiniMax Design？"确认；未装客户端则无响应
  window.location.href = url;
  toast('已尝试唤起 MiniMax Design，请在系统弹窗中确认打开', 'info');
}

async function deleteAccount(id, label) {
  if (!confirm(`确认删除账号「${label || id}」？该操作不可恢复。`)) return;
  try {
    await api(`/api/accounts/${id}`, { method: 'DELETE' });
    toast('账号已删除', 'success');
    loadAccounts();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

// ---- 视频任务 ----

async function loadTasks() {
  try {
    const status = document.getElementById('taskStatusFilter').value;
    let url = `/api/tasks?page=${taskPage}&page_size=${taskPageSize}`;
    if (status) url += `&status=${encodeURIComponent(status)}`;
    const data = await api(url);
    const tasks = data.tasks || [];
    document.getElementById('taskCount').textContent = `共 ${data.total} 个任务`;
    const el = document.getElementById('tasksTable');
    if (tasks.length === 0) {
      el.innerHTML = '<div class="empty"><p>暂无任务</p></div>';
    } else {
      let html = `<div class="table-wrap"><table><thead><tr>
        <th>ID</th><th>媒体</th><th>模型</th><th>提示词</th><th>来源</th><th>账号</th><th>状态</th>
        <th>进度</th><th>积分</th><th>创建时间</th><th>操作</th>
      </tr></thead><tbody>`;
      for (const t of tasks) {
        const canCancel = !['SUCCEEDED', 'FAILED', 'CANCELLED', 'SUBMIT_UNKNOWN'].includes(t.status);
        const canRetry = t.status === 'FAILED' || t.status === 'SUBMIT_UNKNOWN';
        const canPreview = t.status === 'SUCCEEDED';
        const srcBadge = t.source === 'admin_test'
          ? '<span class="badge badge-info">在线测试</span>'
          : '<span class="badge badge-muted">API</span>';
        const mediaBadge = t.media_type === 'image'
          ? '<span class="badge badge-warning">图片</span>'
          : '<span class="badge badge-muted">视频</span>';
        html += `<tr>
          <td class="mono" title="${esc(t.id)}">${esc(t.id.slice(0, 14))}...</td>
          <td>${mediaBadge}</td>
          <td>${esc(t.model)}</td>
          <td title="${esc(t.prompt)}" style="max-width:200px">${esc((t.prompt || '').slice(0, 30))}${(t.prompt || '').length > 30 ? '...' : ''}</td>
          <td>${srcBadge}</td>
          <td class="mono">${esc(t.account_id ? t.account_id.slice(5, 11) : '-')}</td>
          <td>${badge(taskStatusBadge, t.status)}${t.error_code ? `<div style="font-size:11px;color:var(--c-danger)" title="${esc(t.error_msg)}">${esc(t.error_code)}</div>` : ''}</td>
          <td><div class="progress-bar"><div class="fill" style="width:${t.progress}%"></div></div> <span style="font-size:11px">${t.progress}%</span></td>
          <td>${t.credits_estimated > 0 ? fmtNum(t.credits_estimated) : '-'}</td>
          <td>${fmtTime(t.created_at)}</td>
          <td style="white-space:nowrap">
            ${canPreview ? `<button class="btn btn-sm btn-secondary" onclick="previewTask('${esc(t.id)}','${esc(t.media_type || 'video')}')">预览</button>` : ''}
            ${canCancel ? `<button class="btn btn-sm btn-secondary" onclick="cancelTask('${esc(t.id)}')">取消</button>` : ''}
            ${canRetry ? `<button class="btn btn-sm btn-secondary" onclick="retryTask('${esc(t.id)}')">重试</button>` : ''}
            <button class="btn btn-sm btn-secondary" onclick="showEvents('${esc(t.id)}')">事件</button>
          </td></tr>`;
      }
      el.innerHTML = html + '</tbody></table></div>';
    }
    // 分页控件
    const totalPages = Math.max(1, Math.ceil(data.total / taskPageSize));
    document.getElementById('tasksPager').innerHTML = `
      <button ${taskPage <= 1 ? 'disabled' : ''} onclick="taskPage--;loadTasks()">上一页</button>
      <span>第 ${taskPage} / ${totalPages} 页</span>
      <button ${taskPage >= totalPages ? 'disabled' : ''} onclick="taskPage++;loadTasks()">下一页</button>`;
  } catch (e) {
    toast('加载任务失败: ' + e.message, 'error');
  }
}

function startTaskAutoRefresh() {
  stopTaskAutoRefresh();
  if (document.getElementById('taskAutoRefresh').checked) {
    taskTimer = setInterval(() => {
      if (currentSection === 'tasks') loadTasks();
    }, 5000);
  }
}

function stopTaskAutoRefresh() {
  if (taskTimer) { clearInterval(taskTimer); taskTimer = null; }
}

function toggleTaskAutoRefresh() {
  if (currentSection === 'tasks') startTaskAutoRefresh();
}

async function cancelTask(id) {
  if (!confirm('确认取消任务 ' + id + '？')) return;
  try {
    await api(`/api/tasks/${id}/cancel`, { method: 'POST' });
    toast('已请求取消', 'success');
    loadTasks();
  } catch (e) { toast('取消失败: ' + e.message, 'error'); }
}

async function retryTask(id) {
  try {
    const r = await api(`/api/tasks/${id}/retry`, { method: 'POST' });
    toast('已创建重试任务 ' + r.task.id, 'success');
    loadTasks();
  } catch (e) { toast('重试失败: ' + e.message, 'error'); }
}

async function showEvents(id) {
  document.getElementById('eventsTaskId').textContent = id;
  const listEl = document.getElementById('eventsList');
  listEl.innerHTML = '<div class="empty"><p>加载中...</p></div>';
  showModal('eventsModal');
  try {
    const data = await api(`/api/tasks/${id}`);
    const events = data.events || [];
    if (events.length === 0) {
      listEl.innerHTML = '<div class="empty"><p>暂无事件</p></div>';
      return;
    }
    listEl.innerHTML = events.map((e) => `
      <div class="event-item">
        <span class="ts">${esc(fmtTime(e.ts))}</span>
        ${e.to_status ? `<span class="st">${esc(e.to_status)}</span>` : ''}
        ${esc(e.message || '')}
      </div>`).join('');
  } catch (e) {
    listEl.innerHTML = `<div class="empty"><p>加载失败: ${esc(e.message)}</p></div>`;
  }
}

// previewTask 任务列表预览：视频用 <video>，图片用 <img>（按 media_type 切换）
function previewTask(id, mediaType) {
  document.getElementById('previewTaskId').textContent = id;
  const box = document.getElementById('previewMedia');
  const url = `/api/tasks/${encodeURIComponent(id)}/content`;
  if (mediaType === 'image') {
    box.innerHTML = `<img src="${url}" alt="预览图片" style="max-width:100%;max-height:60vh;border-radius:8px;background:#0f172a;display:block;margin:0 auto">
      <div style="margin-top:10px;text-align:right"><a class="btn btn-sm btn-primary" href="${url}" download="${esc(id)}.png">下载图片</a></div>`;
  } else {
    box.innerHTML = `<video id="previewVideo" controls style="width:100%;border-radius:8px;background:#000" src="${url}"></video>`;
  }
  showModal('previewModal');
}

// ---- 账号健康测试 ----

// testAccount 单账号测试：user/info + balance + trial 三连，结果弹窗展示
async function testAccount(id, btn) {
  btn.disabled = true; btn.textContent = '测试中...';
  try {
    const r = await api(`/api/accounts/${encodeURIComponent(id)}/test`, { method: 'POST' });
    showAcctTestResult(r);
    loadAccounts();
  } catch (e) {
    toast('测试失败: ' + e.message, 'error');
  } finally {
    btn.disabled = false; btn.textContent = '测试';
  }
}

function showAcctTestResult(r) {
  const el = document.getElementById('acctTestBody');
  const okBadge = r.token_valid
    ? '<span class="badge badge-success">Token 有效</span>'
    : '<span class="badge badge-danger">Token 失效</span>';
  let trialHtml = '<span style="color:var(--c-text-lighter)">查询失败或活动未开放</span>';
  if (r.trial) {
    trialHtml = `claimed=${esc(r.trial.claimed)} · claimable=${esc(r.trial.claimable)} · `
      + `免费次数=${esc(r.trial.freeCount)} · 剩余=${esc(r.trial.remainingCount)} · `
      + `活动进行中=${esc(r.trial.activityActive)}`;
  }
  el.innerHTML = `
    <div style="margin-bottom:12px">${okBadge}
      <span style="font-size:13px;color:var(--c-text-light);margin-left:8px">${esc(r.message || '')}</span></div>
    <table style="width:100%"><tbody>
      <tr><td style="width:110px;color:var(--c-text-light)">账号</td><td class="mono">${esc(r.account_id)}</td></tr>
      <tr><td style="color:var(--c-text-light)">用户 ID</td><td>${esc(r.user_id || '-')}</td></tr>
      <tr><td style="color:var(--c-text-light)">用户名</td><td>${esc(r.username || '-')}</td></tr>
      <tr><td style="color:var(--c-text-light)">余额</td><td>${r.balance < 0 ? '<span style="color:var(--c-text-lighter)">查询失败</span>' : fmtNum(Math.round(r.balance * 10) / 10)}</td></tr>
      <tr><td style="color:var(--c-text-light)">试用状态</td><td style="font-size:12px">${trialHtml}</td></tr>
      <tr><td style="color:var(--c-text-light)">耗时</td><td>${esc(r.latency_ms)} ms</td></tr>
    </tbody></table>`;
  showModal('acctTestModal');
}

// testAllAccounts 批量测试所有 enabled 账号（服务端串行 + 0.5s 间隔，token 失效自动禁用）
async function testAllAccounts() {
  if (!confirm('确认批量测试所有已启用账号？\n串行执行（每个间隔 0.5 秒防风控），token 失效的账号将被自动禁用。')) return;
  const el = document.getElementById('batchTestBody');
  el.innerHTML = '<div class="empty"><p>批量测试中（串行 + 0.5s 间隔），请勿关闭页面…</p></div>';
  showModal('batchTestModal');
  try {
    const r = await api('/api/accounts/test-all', { method: 'POST' });
    let html = `<div style="margin-bottom:12px;font-size:13.5px">共 ${r.total} 个 · 通过 <b style="color:var(--c-success-dark)">${r.ok_count}</b>`
      + ` · 失败 <b style="color:var(--c-danger)">${r.fail_count}</b>`
      + (r.disabled_count ? ` · <b style="color:var(--c-danger)">自动禁用 ${r.disabled_count}</b>` : '') + '</div>';
    if (r.message) html += `<div class="warn-box">${esc(r.message)}</div>`;
    if ((r.results || []).length) {
      html += '<div class="table-wrap" style="max-height:46vh;overflow-y:auto"><table><thead><tr><th>账号</th><th>Token</th><th>余额</th><th>说明</th></tr></thead><tbody>';
      for (const it of r.results) {
        const name = it.label || it.username || it.account_id;
        const tok = it.token_valid
          ? '<span class="badge badge-success">有效</span>'
          : '<span class="badge badge-danger">失效</span>';
        const dis = it.disabled ? ' <span class="badge badge-danger">已自动禁用</span>' : '';
        const rowStyle = it.disabled ? ' style="background:#fef2f2"' : '';
        html += `<tr${rowStyle}><td title="${esc(it.account_id)}">${esc(name)}</td><td style="white-space:nowrap">${tok}${dis}</td>`
          + `<td>${it.balance < 0 ? '-' : fmtNum(Math.round(it.balance))}</td>`
          + `<td style="white-space:normal;font-size:12px;color:var(--c-text-light)">${esc(it.message || '')}</td></tr>`;
      }
      html += '</tbody></table></div>';
    }
    el.innerHTML = html;
    loadAccounts();
  } catch (e) {
    el.innerHTML = `<div class="warn-box" style="background:#fef2f2;border-color:#fecaca;color:#991b1b">批量测试失败: ${esc(e.message)}</div>`;
  }
}

// ---- 在线测试生成 ----

let tgSupported = null;   // /api/models/supported 数据（管道支持模型 + 取值范围）
let tgRemote = null;      // /api/models/remote 数据（云端目录或内置降级）
let tgTaskId = null;      // 当前跟踪的测试任务
let tgPollTimer = null;   // 3s 轮询定时器
let tgTaskDone = false;   // 当前任务是否已到终态（终态停止轮询）
let tgMediaType = 'video'; // 当前测试媒体类型：video | image

// loadTestTab 进入在线测试页：加载支持模型/账号下拉/远端目录（保留用户已填表单）
async function loadTestTab() {
  tgApplyMediaVisibility();
  await Promise.all([tgLoadSupported(), tgLoadAccounts(), tgLoadRemoteModels(false)]);
}

// tgSetMediaType 媒体类型切换：重建模型/比例/分辨率下拉，隐藏视频专属字段
function tgSetMediaType(mt) {
  tgMediaType = mt;
  document.querySelectorAll('#tgMediaToggle .seg-btn').forEach((b) => {
    b.classList.toggle('active', b.dataset.media === mt);
  });
  tgApplyMediaVisibility();
  tgFillModelSelect();
  tgFillRatioSelect();
  tgModelChanged();
  document.getElementById('tgEstimateResult').textContent = '';
  document.getElementById('tgPrompt').placeholder = mt === 'image'
    ? '描述你想生成的图片内容，例如：一只橘猫趴在窗台上晒太阳，水彩风格'
    : '描述你想生成的视频内容，例如：一只猫在月球上跳舞，电影感光影';
}

function tgApplyMediaVisibility() {
  document.querySelectorAll('#section-test .tg-video-only').forEach((el) => {
    el.style.display = tgMediaType === 'video' ? '' : 'none';
  });
  const refLabel = document.getElementById('tgRefLabel');
  if (refLabel) {
    refLabel.textContent = tgMediaType === 'image'
      ? '参考图 URLs（可选，图生图，每行一个，≤10 张、单图 ≤7MB）'
      : '参考图 URLs（可选，每行一个，≤9 张；与首尾帧互斥）';
  }
  const ratioLabel = document.getElementById('tgRatioLabel');
  if (ratioLabel) ratioLabel.textContent = tgMediaType === 'image' ? '比例 aspect_ratio' : '画幅 ratio';
}

// tgImageModel 当前选中的图片模型规格
function tgImageModel() {
  const id = document.getElementById('tgModel').value;
  return ((tgSupported && tgSupported.image_models) || []).find((x) => x.id === id);
}

async function tgLoadSupported() {
  try {
    tgSupported = await api('/api/models/supported');
    tgFillRatioSelect();
    tgFillModelSelect();
    tgModelChanged();
  } catch (e) {
    toast('加载支持模型失败: ' + e.message, 'error');
  }
}

// tgFillModelSelect 模型下拉：按当前媒体类型填充。
// 视频：supported 为主，远端目录里管道不支持的追加为禁用项（仅展示）；
// 图片：supported.image_models（本期仅 nano_banana_2_flash）。
function tgFillModelSelect() {
  const sel = document.getElementById('tgModel');
  const prev = sel.value;
  if (tgMediaType === 'image') {
    const imgs = (tgSupported && tgSupported.image_models) || [];
    sel.innerHTML = imgs.map((m) => `<option value="${esc(m.id)}">${esc(m.display_name || m.id)}</option>`).join('');
    return;
  }
  const models = (tgSupported && tgSupported.models) || [];
  let html = '';
  for (const m of models) {
    html += `<option value="${esc(m.id)}">${esc(m.display_name || m.id)}</option>`;
  }
  const aliasSet = new Set();
  for (const m of models) {
    aliasSet.add(String(m.id).toLowerCase());
    for (const a of (m.aliases || [])) aliasSet.add(String(a).toLowerCase());
  }
  const remoteVideos = (tgRemote && Array.isArray(tgRemote.models)) ? tgRemote.models : [];
  const seen = new Set(models.map((m) => m.id));
  for (const rm of remoteVideos) {
    const id = rm.id || rm.model_name || '';
    if (!id || seen.has(id)) continue;
    seen.add(id);
    const cands = [rm.id, rm.model_name, rm.mention_name].filter(Boolean);
    const supported = cands.some((c) => aliasSet.has(String(c).toLowerCase()));
    if (supported) continue;
    const name = rm.display_name || rm.model_name || id;
    html += `<option value="${esc(id)}" disabled>${esc(name)}（仅展示）</option>`;
  }
  sel.innerHTML = html;
  // 尽量保留原选择
  if (prev && [...sel.options].some((o) => o.value === prev && !o.disabled)) sel.value = prev;
}

// tgFillRatioSelect 比例下拉：视频=画幅 ratio（含 adaptive）；图片=aspect_ratio（auto=自动）
function tgFillRatioSelect() {
  const sel = document.getElementById('tgRatio');
  const prev = sel.value;
  let ratios;
  if (tgMediaType === 'image') {
    ratios = (tgSupported && tgSupported.image_models && tgSupported.image_models[0]
      && tgSupported.image_models[0].aspect_ratios)
      || ['auto', '16:9', '9:16', '1:1', '4:3', '3:4', '3:2', '2:3', '5:4', '4:5', '21:9'];
  } else {
    ratios = (tgSupported && tgSupported.ratios) || ['adaptive', '16:9', '9:16', '1:1', '4:3', '3:4', '21:9'];
  }
  sel.innerHTML = ratios.map((r) => {
    const label = r === 'auto' ? 'auto（自动）' : (r === 'adaptive' ? 'adaptive（自适应）' : r);
    return `<option value="${esc(r)}">${esc(label)}</option>`;
  }).join('');
  if (prev && [...sel.options].some((o) => o.value === prev)) sel.value = prev;
}

async function tgLoadAccounts() {
  try {
    const data = await api('/api/accounts');
    const sel = document.getElementById('tgAccount');
    const prev = sel.value;
    let html = '<option value="">自动选号</option>';
    for (const a of (data.accounts || [])) {
      if (!a.is_enabled) continue;
      const name = a.label || a.username || a.user_id || a.id;
      const bal = a.balance < 0 ? '余额未知' : ('余额 ' + fmtNum(Math.round(a.balance)));
      const stName = (acctStatusBadge[a.status] || ['', a.status])[1];
      const st = a.status !== 'active' ? ` · ${stName}` : '';
      html += `<option value="${esc(a.id)}">${esc(name)}（${esc(bal)}${esc(st)}）</option>`;
    }
    sel.innerHTML = html;
    if (prev && [...sel.options].some((o) => o.value === prev)) sel.value = prev;
  } catch (e) {
    // 静默：账号下拉保持「自动选号」
  }
}

// tgModelChanged 模型联动：
// 视频 → duration 范围（H3 4-15 / Max 5-15）与 resolution 选项（H3 768P|2K / Max 480P|768P）；
// 图片 → resolution 选项（1K/2K/4K）
function tgModelChanged() {
  if (tgMediaType === 'image') {
    const im = tgImageModel();
    if (!im) return;
    const res = document.getElementById('tgResolution');
    const prevRes = res.value;
    res.innerHTML = (im.resolutions || ['1K', '2K', '4K']).map((r) => `<option value="${esc(r)}">${esc(r)}</option>`).join('');
    if (prevRes && [...res.options].some((o) => o.value === prevRes)) res.value = prevRes;
    else res.value = im.default_resolution || '1K';
    return;
  }
  const id = document.getElementById('tgModel').value;
  const m = ((tgSupported && tgSupported.models) || []).find((x) => x.id === id);
  if (!m) return;
  const dur = document.getElementById('tgDuration');
  dur.min = m.duration_min; dur.max = m.duration_max;
  const cur = parseInt(dur.value, 10);
  if (isNaN(cur) || cur < m.duration_min || cur > m.duration_max) dur.value = m.duration_default;
  document.getElementById('tgDurationHint').textContent = `（${m.duration_min}-${m.duration_max} 秒）`;
  const res = document.getElementById('tgResolution');
  const prevRes = res.value;
  res.innerHTML = (m.resolutions || []).map((r) => `<option value="${esc(r)}">${esc(r)}</option>`).join('');
  if (prevRes && [...res.options].some((o) => o.value === prevRes)) res.value = prevRes;
  else res.value = m.default_resolution;
}

function tgUpdatePromptCount() {
  const v = document.getElementById('tgPrompt').value;
  const max = (tgSupported && tgSupported.prompt_max_length) || 10000;
  const el = document.getElementById('tgPromptCount');
  el.textContent = `${v.length} / ${max} 字符`;
  el.style.color = v.length > max ? 'var(--c-danger)' : 'var(--c-text-lighter)';
}

// tgCollectBody 收集表单为请求体（视频与 POST /v1/videos 同构；图片带 media_type=image）
function tgCollectBody() {
  const refImages = document.getElementById('tgRefImages').value
    .split('\n').map((s) => s.trim()).filter(Boolean);
  if (tgMediaType === 'image') {
    const body = {
      media_type: 'image',
      model: document.getElementById('tgModel').value,
      prompt: document.getElementById('tgPrompt').value.trim(),
      ratio: document.getElementById('tgRatio').value,       // 服务端映射为 aspect_ratio（auto→自动）
      resolution: document.getElementById('tgResolution').value,
    };
    if (refImages.length) body.reference_images = refImages;
    return body;
  }
  const body = {
    model: document.getElementById('tgModel').value,
    prompt: document.getElementById('tgPrompt').value.trim(),
    duration: parseInt(document.getElementById('tgDuration').value, 10) || 5,
    resolution: document.getElementById('tgResolution').value,
    ratio: document.getElementById('tgRatio').value,
    generate_audio: document.getElementById('tgAudio').checked,
  };
  const ff = document.getElementById('tgFirstFrame').value.trim();
  const lf = document.getElementById('tgLastFrame').value.trim();
  if (ff) body.first_frame_image = ff;
  if (lf) body.last_frame_image = lf;
  if (refImages.length) body.reference_images = refImages;
  return body;
}

// tgEstimate 估算积分：预计消耗 + 所用账号余额（视频/图片各自的计价参数）
async function tgEstimate() {
  const acc = document.getElementById('tgAccount').value;
  const form = tgCollectBody();
  let payload;
  if (tgMediaType === 'image') {
    payload = {
      media_type: 'image', model: form.model, resolution: form.resolution,
      prompt: form.prompt, images: form.reference_images || [], account_id: acc,
    };
  } else {
    payload = Object.assign({ account_id: acc }, form);
  }
  const btn = document.getElementById('tgEstimateBtn');
  const out = document.getElementById('tgEstimateResult');
  btn.disabled = true; out.textContent = '估算中...';
  try {
    const r = await api('/api/estimate-cost', { method: 'POST', body: payload });
    const balTxt = r.balance < 0 ? '余额未知' : ('余额 <b>' + fmtNum(Math.round(r.balance * 10) / 10) + '</b>');
    out.innerHTML = `预计消耗 <b style="color:var(--c-primary-dark)">${fmtNum(Math.round(r.estimated_credits * 10) / 10)}</b> 积分 · 账号 <span class="mono">${esc((r.account_id || '').slice(5, 11))}</span> ${balTxt}`;
  } catch (e) {
    out.textContent = '';
    toast('估算失败: ' + e.message, 'error');
  } finally {
    btn.disabled = false;
  }
}

// tgSubmit 提交测试任务并开始轮询
async function tgSubmit() {
  const body = tgCollectBody();
  if (!body.prompt) { toast('请填写提示词', 'error'); return; }
  const acc = document.getElementById('tgAccount').value;
  if (acc) body.account_id = acc;
  const btn = document.getElementById('tgSubmitBtn');
  btn.disabled = true;
  try {
    const r = await api('/api/test/generate', { method: 'POST', body });
    toast('测试任务已提交: ' + r.id, 'success');
    tgStartPolling(r.id);
  } catch (e) {
    toast('提交失败: ' + e.message, 'error');
  } finally {
    btn.disabled = false;
  }
}

function tgStopPolling() {
  if (tgPollTimer) { clearInterval(tgPollTimer); tgPollTimer = null; }
}

// tgResumePolling 回到在线测试页时，若有未完成任务则恢复轮询
function tgResumePolling() {
  if (tgTaskId && !tgTaskDone && !tgPollTimer) {
    tgPollTimer = setInterval(tgPollTask, 3000);
  }
}

function tgStartPolling(taskId) {
  tgTaskId = taskId;
  tgTaskDone = false;
  tgStopPolling();
  document.getElementById('tgTaskCard').style.display = '';
  document.getElementById('tgTaskId').textContent = taskId;
  document.getElementById('tgTaskBody').innerHTML =
    '<div class="empty" style="padding:20px"><p>任务已入队，等待执行…</p></div>';
  tgPollTask();
  tgPollTimer = setInterval(tgPollTask, 3000);
}

async function tgPollTask() {
  if (!tgTaskId) return;
  try {
    const data = await api('/api/tasks/' + encodeURIComponent(tgTaskId));
    tgRenderTask(data.task);
  } catch (e) {
    // 瞬时网络抖动：忽略，下个周期重试
  }
}

function tgRenderTask(t) {
  if (!t) return;
  const terminal = ['SUCCEEDED', 'FAILED', 'CANCELLED', 'SUBMIT_UNKNOWN'];
  const el = document.getElementById('tgTaskBody');
  let html = `<div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin-bottom:14px">
    ${badge(taskStatusBadge, t.status)}
    <div class="progress-bar" style="width:220px"><div class="fill" style="width:${t.progress || 0}%"></div></div>
    <span style="font-size:12.5px;color:var(--c-text-light)">${t.progress || 0}%</span>`;
  if (t.estimated_remaining_sec > 0 && !terminal.includes(t.status)) {
    html += `<span style="font-size:12.5px;color:var(--c-text-light)">预计剩余 ${esc(t.estimated_remaining_sec)}s</span>`;
  }
  if (t.account_id) {
    html += `<span class="mono" style="font-size:11.5px;color:var(--c-text-lighter)">账号 ${esc(t.account_id.slice(5, 11))}</span>`;
  }
  if (t.credits_estimated > 0) {
    html += `<span style="font-size:12px;color:var(--c-text-lighter)">预估 ${fmtNum(t.credits_estimated)} 积分</span>`;
  }
  html += '</div>';

  if (t.status === 'SUCCEEDED') {
    const contentUrl = `/api/tasks/${encodeURIComponent(t.id)}/content`;
    if (t.media_type === 'image') {
      const dims = (t.width > 0 && t.height > 0) ? `<span style="font-size:12px;color:var(--c-text-lighter);margin-left:8px">${esc(t.width)}×${esc(t.height)}</span>` : '';
      html += `<img style="max-width:100%;max-height:520px;border-radius:10px;background:#0f172a" src="${contentUrl}" alt="生成图片">
      <div style="margin-top:12px;display:flex;gap:8px;align-items:center">
        <a class="btn btn-primary" href="${contentUrl}" download="${esc(t.id)}.png">下载图片</a>${dims}
      </div>`;
    } else {
      html += `<video controls style="width:100%;max-width:720px;border-radius:10px;background:#000" src="${contentUrl}"></video>
      <div style="margin-top:12px;display:flex;gap:8px">
        <a class="btn btn-primary" href="${contentUrl}" download="${esc(t.id)}.mp4">下载视频</a>
      </div>`;
    }
  } else if (t.status === 'FAILED' || t.status === 'SUBMIT_UNKNOWN') {
    html += `<div class="warn-box" style="background:#fef2f2;border-color:#fecaca;color:#991b1b">
      <b>${esc(t.error_code || t.status)}</b>：${esc(t.error_msg || '无详细信息')}</div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-primary" onclick="tgRetryTask('${esc(t.id)}',this)">重试</button>
      </div>`;
  } else if (t.status === 'CANCELLED') {
    html += '<div class="warn-box">任务已取消</div>';
  } else {
    html += `<div style="font-size:12.5px;color:var(--c-text-lighter)">每 3 秒自动刷新状态…（当前阶段 ${esc(t.status)}）</div>`;
  }
  el.innerHTML = html;

  if (terminal.includes(t.status)) {
    tgTaskDone = true;
    tgStopPolling();
  }
}

async function tgRetryTask(id, btn) {
  btn.disabled = true;
  try {
    const r = await api(`/api/tasks/${encodeURIComponent(id)}/retry`, { method: 'POST' });
    toast('已创建重试任务 ' + r.task.id, 'success');
    tgStartPolling(r.task.id);
  } catch (e) {
    toast('重试失败: ' + e.message, 'error');
    btn.disabled = false;
  }
}

// tgLoadRemoteModels 远端模型目录（refresh=true 绕过服务端 10 分钟缓存）
async function tgLoadRemoteModels(refresh) {
  const meta = document.getElementById('tgRemoteMeta');
  const body = document.getElementById('tgRemoteBody');
  try {
    if (refresh) meta.textContent = '拉取中...';
    tgRemote = await api('/api/models/remote' + (refresh ? '?refresh=1' : ''));
    if (tgRemote.source === 'remote') {
      meta.textContent = `远端目录 · ${fmtTime(tgRemote.fetched_at)}${tgRemote.cached ? '（缓存）' : ''} · 视频模型 ${(tgRemote.models || []).length} 个`;
    } else {
      meta.textContent = '内置降级（远端不可用）';
    }
    tgRenderRemoteModels();
    tgFillModelSelect(); // 远端目录合并进模型下拉（不支持的追加为「仅展示」）
  } catch (e) {
    meta.textContent = '';
    body.innerHTML = `<div class="warn-box">加载远端目录失败: ${esc(e.message)}</div>`;
  }
}

function tgRenderRemoteModels() {
  const body = document.getElementById('tgRemoteBody');
  if (!tgRemote) return;
  if (tgRemote.source !== 'remote') {
    body.innerHTML = `<div class="warn-box">远端目录不可用，已降级展示内置模型信息：${esc(tgRemote.error || '未知原因')}</div>`
      + tgModelRows(tgRemote.models || []);
    return;
  }
  body.innerHTML = tgModelRows(tgRemote.models || []);
}

function tgModelRows(models) {
  if (!models.length) return '<div class="empty"><p>目录为空</p></div>';
  let html = '';
  for (const m of models) {
    const name = m.display_name || m.model_name || m.id || '(无名称)';
    const hot = m.hot ? '<span class="badge badge-warning" style="margin-left:6px">HOT</span>' : '';
    const pml = (m.promptMaxLength != null) ? `<span style="font-size:11.5px;color:var(--c-text-lighter);margin-left:8px">prompt ≤ ${esc(m.promptMaxLength)}</span>` : '';
    const desc = m.description
      ? `<div style="font-size:12px;color:var(--c-text-light);margin-top:2px;white-space:normal">${esc(m.description)}</div>` : '';
    html += `<div class="model-row"><span class="mono" style="color:var(--c-text-lighter)">${esc(m.id || '')}</span> <b>${esc(name)}</b>${hot}${pml}${desc}</div>`;
  }
  return html;
}

function tgToggleRemotePanel() {
  const body = document.getElementById('tgRemoteBody');
  const btn = document.getElementById('tgRemoteToggle');
  const show = body.style.display === 'none';
  body.style.display = show ? '' : 'none';
  btn.textContent = show ? '收起' : '展开';
}

// ---- API 密钥 ----

async function loadKeys() {
  try {
    const data = await api('/api/keys');
    const keys = data.keys || [];
    const el = document.getElementById('keysTable');
    if (keys.length === 0) {
      el.innerHTML = '<div class="empty"><p>暂无密钥，点击「+ 创建密钥」</p></div>';
      return;
    }
    let html = `<div class="table-wrap"><table><thead><tr>
      <th>前缀</th><th>名称</th><th>用量</th><th>过期时间</th><th>最后使用</th><th>状态</th><th>操作</th>
    </tr></thead><tbody>`;
    for (const k of keys) {
      html += `<tr>
        <td class="mono">${esc(k.key_prefix)}...</td>
        <td>${esc(k.name)}</td>
        <td>${k.used_tasks}${k.max_tasks > 0 ? ' / ' + k.max_tasks : ''}</td>
        <td>${k.expires_at ? fmtTime(k.expires_at) : '永不过期'}</td>
        <td>${fmtTime(k.last_used_at)}</td>
        <td>${k.is_enabled ? '<span class="badge badge-success">启用</span>' : '<span class="badge badge-muted">停用</span>'}</td>
        <td style="white-space:nowrap">
          <button class="btn btn-sm btn-secondary" onclick="toggleKey('${esc(k.id)}',${!k.is_enabled})">${k.is_enabled ? '停用' : '启用'}</button>
          <button class="btn btn-sm btn-danger" onclick="deleteKey('${esc(k.id)}','${esc(k.name)}')">删除</button>
        </td></tr>`;
    }
    el.innerHTML = html + '</tbody></table></div>';
  } catch (e) { toast('加载密钥失败: ' + e.message, 'error'); }
}

function showCreateKeyModal() {
  document.getElementById('keyName').value = '';
  document.getElementById('keyMaxTasks').value = '0';
  document.getElementById('keyExpiresDays').value = '0';
  showModal('createKeyModal');
}

async function doCreateKey() {
  const name = document.getElementById('keyName').value.trim();
  const maxTasks = parseInt(document.getElementById('keyMaxTasks').value, 10) || 0;
  const expiresDays = parseInt(document.getElementById('keyExpiresDays').value, 10) || 0;
  if (!name) { toast('请填写名称', 'error'); return; }
  try {
    const r = await api('/api/keys', { method: 'POST', body: { name, max_tasks: maxTasks, expires_days: expiresDays } });
    hideModal('createKeyModal');
    document.getElementById('keyPlainText').value = r.key;
    showModal('keyPlainModal');
    loadKeys();
  } catch (e) { toast('创建失败: ' + e.message, 'error'); }
}

function copyKeyPlain() {
  const ta = document.getElementById('keyPlainText');
  ta.select();
  navigator.clipboard.writeText(ta.value)
    .then(() => toast('已复制到剪贴板', 'success'))
    .catch(() => document.execCommand('copy'));
}

async function toggleKey(id, enabled) {
  try {
    await api(`/api/keys/${id}/toggle`, { method: 'POST', body: { is_enabled: enabled } });
    toast(enabled ? '密钥已启用' : '密钥已停用', 'success');
    loadKeys();
  } catch (e) { toast('操作失败: ' + e.message, 'error'); }
}

async function deleteKey(id, name) {
  if (!confirm(`确认删除密钥「${name}」？使用该密钥的调用方将立即失效。`)) return;
  try {
    await api(`/api/keys/${id}`, { method: 'DELETE' });
    toast('密钥已删除', 'success');
    loadKeys();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

// ---- 设置 ----

async function loadSettings() {
  try {
    const s = await api('/api/settings');
    document.getElementById('setPoll').value = s.poll_interval_sec;
    document.getElementById('setWorkers').value = s.worker_count;
    document.getElementById('setConc').value = s.per_account_concurrency;
    document.getElementById('setSafety').value = s.balance_safety_factor;
  } catch (e) { toast('加载设置失败: ' + e.message, 'error'); }
}

async function saveSettings() {
  const body = {
    poll_interval_sec: parseInt(document.getElementById('setPoll').value, 10) || 0,
    worker_count: parseInt(document.getElementById('setWorkers').value, 10) || 0,
    per_account_concurrency: parseInt(document.getElementById('setConc').value, 10) || 0,
    balance_safety_factor: parseFloat(document.getElementById('setSafety').value) || 0,
  };
  try {
    await api('/api/settings', { method: 'PUT', body });
    toast('设置已保存并热更生效', 'success');
    loadSettings();
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

// ---- 启动 ----

(async function init() {
  // 用轻量请求探测 session 是否有效（401 → 显示登录页）
  try {
    await api('/api/settings');
    showMain(localStorage.getItem('mm2api_user') || 'admin');
    refreshAll();
  } catch (e) {
    showLogin();
  }
})();
