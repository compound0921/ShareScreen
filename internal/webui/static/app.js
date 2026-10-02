'use strict';

// ── 元素 ──
const $ = (id) => document.getElementById(id);

const els = {
  badge: $('badge'),
  source: $('source'),
  sourceHint: $('sourceHint'),
  windowField: $('windowField'),
  windowSelect: $('windowSelect'),
  refreshWindows: $('refreshWindows'),
  windowTitle: $('windowTitle'),
  resolution: $('resolution'),
  fps: $('fps'),
  bitrate: $('bitrate'),
  encoder: $('encoder'),
  uplink: $('uplink'),
  publicHost: $('publicHost'),
  toggle: $('toggle'),
  restart: $('restart'),
  uptime: $('uptime'),
  viewers: $('viewers'),
  capacityFill: $('capacityFill'),
  capacityText: $('capacityText'),
  watchList: $('watchList'),
  qr: $('qr'),
  errorBox: $('errorBox'),
  errorText: $('errorText'),
  cmdText: $('cmdText'),
};

// ── 本地状态 ──
let config = null;
let presets = [];
let encoders = [];
let status = {};
let pollTimer = null;
let saveTimer = null;

// ── 工具 ──
const SOURCE_HINT = {
  screen_ddagrab: 'GPU 侧采集,CPU 占用最低,支持 60fps。但不支持单窗口,且同时只能有一个采集会话。',
  screen_gdigrab: '通用兜底。CPU 拷贝,帧率明显低于 GPU 采集,仅在其他方式不可用时使用。',
  window: '只能走兼容模式采集,且窗口被遮挡时画面会被遮挡物覆盖。',
  obs: '从 OBS 虚拟摄像头读取,需要先在 OBS 里启动虚拟摄像头。参数由 OBS 画布决定。',
};

function fmtUptime(ms) {
  if (!ms || ms < 0) return '';
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (h > 0) return `已运行 ${h} 小时 ${m} 分`;
  if (m > 0) return `已运行 ${m} 分 ${sec} 秒`;
  return `已运行 ${sec} 秒`;
}

function resolutionValue(w, h) { return `${w}x${h}`; }

async function api(path, opts) {
  const res = await fetch(path, opts);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `请求失败 (${res.status})`);
  return body;
}

// ── 渲染 ──

function renderPresets() {
  els.resolution.innerHTML = '';
  for (const p of presets) {
    const opt = document.createElement('option');
    opt.value = resolutionValue(p.width, p.height);
    opt.textContent = p.label;
    els.resolution.appendChild(opt);
  }
}

function renderEncoders() {
  els.encoder.innerHTML = '';
  const auto = document.createElement('option');
  auto.value = '';
  auto.textContent = '自动(优先硬件)';
  els.encoder.appendChild(auto);

  for (const e of encoders) {
    const opt = document.createElement('option');
    opt.value = e.kind;
    opt.textContent = e.label;
    els.encoder.appendChild(opt);
  }
}

function renderForm() {
  const v = config.video;
  els.source.value = v.source;
  els.windowTitle.value = v.windowTitle || '';
  els.resolution.value = resolutionValue(v.width, v.height);
  els.fps.value = String(v.fps);
  els.bitrate.value = String(v.bitrateKbps);
  els.encoder.value = v.encoder || '';
  els.uplink.value = String(config.uplinkMbps);
  els.publicHost.value = config.publicHost || '';

  // 码率可能不在预设列表里(比如手工改过配置文件)
  if (els.bitrate.value !== String(v.bitrateKbps)) {
    const opt = document.createElement('option');
    opt.value = String(v.bitrateKbps);
    opt.textContent = (v.bitrateKbps / 1000) + ' Mbps';
    els.bitrate.appendChild(opt);
    els.bitrate.value = String(v.bitrateKbps);
  }

  renderSourceFields();
}

function renderSourceFields() {
  const src = els.source.value;
  els.sourceHint.textContent = SOURCE_HINT[src] || '';
  els.windowField.hidden = src !== 'window';
  if (src === 'window') {
    loadWindows();
  }
}

// loadWindows 拉取当前可见窗口,填充选择器。
//
// 不缓存 —— 窗口标题随时在变,缓存只会让用户选到已经失效的标题。
async function loadWindows() {
  els.windowSelect.innerHTML = '<option value="">正在读取…</option>';

  let list;
  try {
    list = await api('/api/windows');
  } catch (err) {
    els.windowSelect.innerHTML = '<option value="">读取失败,请手动填写标题</option>';
    return;
  }

  els.windowSelect.innerHTML = '';

  const head = document.createElement('option');
  head.value = '';
  head.textContent = list.length ? '— 从列表选择 —' : '(没有找到可见窗口)';
  els.windowSelect.appendChild(head);

  for (const w of list) {
    const opt = document.createElement('option');
    // gdigrab 按子串匹配,完整标题必然匹配到它自己
    opt.value = w.title;
    opt.textContent = (w.process ? w.process + ' — ' : '') + w.title +
      (w.minimized ? '   [已最小化]' : '');
    els.windowSelect.appendChild(opt);
  }

  // 已配置的标题若还能在列表里找到对应窗口,回选它;
  // 找不到就保持空选,提醒用户这个标题已经失效了。
  const cur = els.windowTitle.value.trim();
  if (cur) {
    const match = list.find((w) => w.title === cur) ||
                  list.find((w) => w.title.includes(cur));
    if (match) {
      els.windowSelect.value = match.title;
    }
  }
}

function renderWatchURLs(urls) {
  els.watchList.innerHTML = '';
  if (!urls || urls.length === 0) {
    els.watchList.innerHTML = '<p class="hint">暂无可用地址</p>';
    els.qr.hidden = true;
    return;
  }

  for (const u of urls) {
    const row = document.createElement('div');
    row.className = 'watch-item';

    const kind = document.createElement('span');
    kind.className = 'kind';
    kind.textContent = u.label;

    const a = document.createElement('a');
    a.href = u.url;
    a.target = '_blank';
    a.rel = 'noopener';
    a.textContent = u.url;

    const btn = document.createElement('button');
    btn.textContent = '复制';
    btn.onclick = async () => {
      try {
        await navigator.clipboard.writeText(u.url);
        btn.textContent = '已复制';
        setTimeout(() => { btn.textContent = '复制'; }, 1200);
      } catch {
        btn.textContent = '复制失败';
        setTimeout(() => { btn.textContent = '复制'; }, 1200);
      }
    };

    row.append(kind, a, btn);
    els.watchList.appendChild(row);
  }

  // 二维码指向公网地址(如果配了),否则用局域网地址。
  // 手机扫码走公网,所以优先。
  const preferred = urls.find((u) => u.kind === 'public') || urls[0];
  els.qr.src = '/api/qr.png?t=' + encodeURIComponent(preferred.url);
  els.qr.hidden = false;
}

function renderStatus() {
  const running = !!status.running;
  const state = status.state || 'stopped';

  // 徽章
  let cls = 'badge-idle', text = '已停止';
  if (state === 'running') { cls = 'badge-running'; text = '共享中'; }
  else if (state === 'starting') { cls = 'badge-busy'; text = '启动中…'; }
  else if (state === 'stopping') { cls = 'badge-busy'; text = '停止中…'; }
  else if (state === 'failed') { cls = 'badge-error'; text = '启动失败'; }
  els.badge.className = 'badge ' + cls;
  els.badge.textContent = text;

  // 主按钮
  els.toggle.textContent = running ? '停止共享' : '开始共享';
  els.toggle.classList.toggle('is-stop', running);
  const busy = state === 'starting' || state === 'stopping';
  els.toggle.disabled = busy;
  els.restart.disabled = busy || !running || !config;

  els.uptime.textContent = running ? fmtUptime(status.uptimeMs) : '';

  // 观众数
  const viewers = status.viewers;
  els.viewers.textContent = viewers < 0 ? '未知' : String(viewers);

  // 容量
  const capacity = status.capacity ?? 0;
  let fillPct = 0;
  if (capacity > 0 && viewers >= 0) {
    fillPct = Math.min(100, (viewers / capacity) * 100);
  }
  els.capacityFill.style.width = fillPct + '%';

  const over = capacity < 1 || (viewers >= 0 && viewers > capacity);
  els.capacityFill.classList.toggle('over', over);

  if (capacity < 1) {
    els.capacityText.textContent = '当前码率超出上行带宽,无法推流';
  } else if (viewers < 0) {
    els.capacityText.textContent = `可支撑 ${capacity} 人(当前观众数未知)`;
  } else if (over) {
    els.capacityText.textContent = `已超出可支撑人数(${capacity} 人),所有人都会卡`;
  } else {
    els.capacityText.textContent = `可支撑 ${capacity} 人`;
  }

  // 错误
  if (status.lastError) {
    els.errorText.textContent = status.lastError;
    els.errorBox.hidden = false;
  } else {
    els.errorBox.hidden = true;
  }

  // 命令
  els.cmdText.textContent = status.command || '(尚未启动过)';
}

// ── 交互 ──

function collectVideo() {
  const [w, h] = els.resolution.value.split('x').map(Number);
  return {
    source: els.source.value,
    windowTitle: els.windowTitle.value.trim(),
    width: w,
    height: h,
    fps: Number(els.fps.value),
    bitrateKbps: Number(els.bitrate.value),
    encoder: els.encoder.value,
    keyframeSec: 1,
  };
}

function scheduleSave() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(saveConfig, 400); // 防抖:等用户改完再发
}

async function saveConfig() {
  if (!config) return;
  const next = {
    ...config,
    video: collectVideo(),
    uplinkMbps: Number(els.uplink.value) || config.uplinkMbps,
    publicHost: els.publicHost.value.trim(),
  };

  try {
    const res = await api('/api/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(next),
    });
    config = res.config;
    status.capacity = res.capacity;
    renderStatus();
    if (res.restarted) {
      els.badge.className = 'badge badge-busy';
      els.badge.textContent = '参数已生效,重启中…';
    }
  } catch (err) {
    els.errorText.textContent = err.message;
    els.errorBox.hidden = false;
  }
}

async function toggleStream() {
  els.toggle.disabled = true;
  try {
    await api(status.running ? '/api/stop' : '/api/start', { method: 'POST' });
  } catch (err) {
    els.errorText.textContent = err.message;
    els.errorBox.hidden = false;
  }
  poll();
}

async function restartStream() {
  els.restart.disabled = true;
  try {
    await api('/api/restart', { method: 'POST' });
  } catch (err) {
    els.errorText.textContent = err.message;
    els.errorBox.hidden = false;
  }
  poll();
}

// ── 轮询 ──

async function poll() {
  try {
    const res = await api('/api/status');
    status = res.status || {};
    if (typeof res.viewers === 'number') status.viewers = res.viewers;
    if (typeof res.capacity === 'number') status.capacity = res.capacity;
    renderStatus();
  } catch {
    // 轮询失败不打断界面 —— 通常意味着程序正在退出
  }
}

// ── 启动 ──

async function init() {
  let state;
  try {
    state = await api('/api/state');
  } catch (err) {
    document.body.insertAdjacentHTML('afterbegin',
      `<p style="padding:16px;color:#dc2626">无法连接到本地服务:${err.message}</p>`);
    return;
  }

  config = state.config;
  presets = state.presets || [];
  encoders = state.encoders || [];
  status = state.status || {};
  status.viewers = state.viewers;
  status.capacity = state.capacity;

  renderPresets();
  renderEncoders();
  renderForm();
  renderWatchURLs(state.watchUrls);
  renderStatus();

  // 只有采集源变化才重渲染条件字段 —— 它会顺带刷新窗口列表,
  // 挂到所有控件上会导致改个码率就去枚举一遍窗口。
  els.source.addEventListener('change', () => {
    renderSourceFields();
    scheduleSave();
  });

  for (const el of [els.resolution, els.fps, els.bitrate,
                    els.encoder, els.uplink, els.publicHost]) {
    el.addEventListener('change', scheduleSave);
  }

  // 从列表里选中窗口 → 写进标题输入框(标题框仍是唯一的数据来源)
  els.windowSelect.addEventListener('change', () => {
    if (els.windowSelect.value) {
      els.windowTitle.value = els.windowSelect.value;
      scheduleSave();
    }
  });
  els.refreshWindows.addEventListener('click', loadWindows);

  els.windowTitle.addEventListener('input', scheduleSave);
  els.uplink.addEventListener('input', scheduleSave);
  els.publicHost.addEventListener('input', scheduleSave);

  els.toggle.addEventListener('click', toggleStream);
  els.restart.addEventListener('click', restartStream);

  pollTimer = setInterval(poll, 2000);
}

init();
