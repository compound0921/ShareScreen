'use strict';

// ── 元素 ──
const $ = (id) => document.getElementById(id);

const els = {
  badge: $('badge'),
  source: $('source'),
  sourceHint: $('sourceHint'),
  captureParams: $('captureParams'),
  pushField: $('pushField'),
  pushList: $('pushList'),
  bitrateHint: $('bitrateHint'),
  windowField: $('windowField'),
  windowSelect: $('windowSelect'),
  refreshWindows: $('refreshWindows'),
  windowTitle: $('windowTitle'),
  audioField: $('audioField'),
  audioEnabled: $('audioEnabled'),
  audioBitrate: $('audioBitrate'),
  resolution: $('resolution'),
  fps: $('fps'),
  bitrate: $('bitrate'),
  encoder: $('encoder'),
  uplink: $('uplink'),
  publicHost: $('publicHost'),
  autoPortMap: $('autoPortMap'),
  portmapStatus: $('portmapStatus'),
  portmapDot: $('portmapDot'),
  portmapText: $('portmapText'),
  portmapHint: $('portmapHint'),
  portmapRetry: $('portmapRetry'),
  toggle: $('toggle'),
  restart: $('restart'),
  uptime: $('uptime'),
  viewers: $('viewers'),
  capacityFill: $('capacityFill'),
  capacityText: $('capacityText'),
  watchList: $('watchList'),
  qr: $('qr'),
  warnBox: $('warnBox'),
  warnText: $('warnText'),
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
  screen_ddagrab: 'CPU 占用最低,支持 60fps。不支持单窗口。',
  screen_gdigrab: '通用兜底,帧率明显更低。',
  window: '窗口被遮挡时,画面会被遮挡物盖住。',
  obs: '需先在 OBS 里启动虚拟摄像头。声音仍来自桌面音频。',
  obs_push: '画面和声音都由 OBS 提供,参数也在 OBS 里设。',
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
  els.autoPortMap.checked = !!config.autoPortMap;
  renderPortMap(null);

  const a = config.audio || {};
  const abr = a.bitrateKbps || 96;
  els.audioEnabled.checked = !!a.enabled;
  els.audioBitrate.value = String(abr);
  // 码率可能不在预设列表里(比如手工改过配置文件)
  if (els.audioBitrate.value !== String(abr)) {
    const opt = document.createElement('option');
    opt.value = String(abr);
    opt.textContent = abr + ' kbps';
    els.audioBitrate.appendChild(opt);
    els.audioBitrate.value = String(abr);
  }

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
  const external = src === 'obs_push';

  els.sourceHint.textContent = SOURCE_HINT[src] || '';
  els.windowField.hidden = src !== 'window';
  els.pushField.hidden = !external;
  // 分辨率/帧率/编码器/音频在直推模式下都由 OBS 决定,留着只会误导
  els.captureParams.hidden = external;
  els.audioField.hidden = external;

  renderAudioFields();

  els.bitrateHint.textContent = external
    ? '填 OBS 里设的码率,只用于估算观众数。'
    : '填实测上传速率,只用于估算观众数。';

  if (src === 'window') {
    loadWindows();
  }
}

// renderAudioFields 只在勾了音频时才显示码率 —— 没开音频时它没有意义。
function renderAudioFields() {
  els.audioBitrate.hidden = !els.audioEnabled.checked;
}

// copyText 把文本写进剪贴板并短暂改变按钮文案作为反馈。
async function copyText(text, btn) {
  const original = btn.textContent;
  try {
    await navigator.clipboard.writeText(text);
    btn.textContent = '已复制';
  } catch {
    btn.textContent = '复制失败';
  }
  setTimeout(() => { btn.textContent = original; }, 1200);
}

// renderPushURLs 显示外部推流程序(OBS)要填的地址。
function renderPushURLs(urls) {
  els.pushList.innerHTML = '';
  if (!urls || !urls.length) return;

  for (const u of urls) {
    const row = document.createElement('div');
    row.className = 'watch-item';

    const kind = document.createElement('span');
    kind.className = 'kind';
    kind.textContent = u.label;

    const code = document.createElement('code');
    code.textContent = u.url;

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.textContent = '复制';
    btn.onclick = () => copyText(u.url, btn);

    row.append(kind, code, btn);
    els.pushList.appendChild(row);

    if (u.note) {
      const note = document.createElement('p');
      note.className = 'hint';
      note.textContent = u.note;
      els.pushList.appendChild(note);
    }
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
  // 外部推流模式:流是 OBS 推过来的,本程序不跑 ffmpeg。
  // 开关在 OBS 那边,所以这里不显示开始/停止按钮。
  const external = !!status.external;

  // 徽章
  let cls = 'badge-idle', text = '已停止';
  if (external) {
    cls = running ? 'badge-running' : 'badge-idle';
    text = running ? 'OBS 推流中' : '等待 OBS 推流';
  } else if (state === 'running') { cls = 'badge-running'; text = '共享中'; }
  else if (state === 'starting') { cls = 'badge-busy'; text = '启动中…'; }
  else if (state === 'stopping') { cls = 'badge-busy'; text = '停止中…'; }
  else if (state === 'failed') { cls = 'badge-error'; text = '启动失败'; }
  els.badge.className = 'badge ' + cls;
  els.badge.textContent = text;

  // 主按钮
  els.toggle.hidden = external;
  els.restart.hidden = external;
  if (!external) {
    els.toggle.textContent = running ? '停止共享' : '开始共享';
    els.toggle.classList.toggle('is-stop', running);
    const busy = state === 'starting' || state === 'stopping';
    els.toggle.disabled = busy;
    els.restart.disabled = busy || !running || !config;
  }

  // 外部模式下运行时长没有意义(我们不知道 OBS 推了多久)
  els.uptime.textContent = (!external && running) ? fmtUptime(status.uptimeMs) : '';

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

  // 警告 —— 推流本身是好的,只是少了点东西(目前只有音频采集失败)。
  // 和下面的错误分开显示,免得用户以为整个共享挂了。
  if (status.warning) {
    els.warnText.textContent = status.warning;
    els.warnBox.hidden = false;
  } else {
    els.warnBox.hidden = true;
  }

  // 错误
  if (status.lastError) {
    els.errorText.textContent = status.lastError;
    els.errorBox.hidden = false;
  } else {
    els.errorBox.hidden = true;
  }

  // 命令。外部推流模式下本程序不跑 ffmpeg,显示命令没有意义。
  els.cmdText.textContent = external
    ? '(OBS 直推模式,本程序不运行 ffmpeg)'
    : (status.command || '(尚未启动过)');
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

function collectAudio() {
  return {
    enabled: els.audioEnabled.checked,
    bitrateKbps: Number(els.audioBitrate.value) || 96,
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
    audio: collectAudio(),
    uplinkMbps: Number(els.uplink.value) || config.uplinkMbps,
    publicHost: els.publicHost.value.trim(),
    autoPortMap: els.autoPortMap.checked,
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

    if (res.portMap !== undefined) renderPortMap(res.portMap);

    // 公网地址可能被自动映射换掉了(拿到的公网 IP 变了)。换了就得重渲染
    // 观看链接,否则页面上挂的还是旧地址。
    //
    // 只在真的变了时才重渲染 —— 每 2 秒重建一次的话,二维码会一直闪。
    if (res.publicHost !== undefined && config && res.publicHost !== config.publicHost) {
      refreshWatchURLs();
    }
  } catch {
    // 轮询失败不打断界面 —— 通常意味着程序正在退出
  }
}

// refreshWatchURLs 重新拉一次观看地址并渲染。
async function refreshWatchURLs() {
  try {
    const s = await api('/api/state');
    config.publicHost = s.config.publicHost || '';
    config.publicHostAuto = !!s.config.publicHostAuto;
    // 用户正在这个框里打字时不要回填,免得把他输的半个地址冲掉
    if (document.activeElement !== els.publicHost) {
      els.publicHost.value = config.publicHost;
    }
    renderWatchURLs(s.watchUrls);
  } catch {
    // 拉不到就维持现状,下次轮询还会再试
  }
}

// renderPortMap 画自动端口映射的状态行。
//
// snap 为 null 表示还没拿到状态(刚加载完、或这一版没有这个能力),
// 这时整块藏起来 —— 宁可不显示,也不要显示一个空的状态行。
function renderPortMap(snap) {
  if (!snap || snap.state === 'disabled') {
    els.portmapStatus.hidden = true;
    els.portmapHint.hidden = true;
    els.portmapRetry.hidden = true;
    return;
  }

  els.portmapStatus.hidden = false;
  els.portmapText.textContent = snap.message || '';
  els.portmapHint.textContent = snap.hint || '';
  els.portmapHint.hidden = !snap.hint;

  // 状态同时用颜色和文字表达,不靠颜色单独传达信息
  els.portmapDot.className = 'pm-dot';
  if (snap.state === 'active') {
    els.portmapDot.classList.add('ok');
  } else if (snap.state === 'failed') {
    els.portmapDot.classList.add('bad');
  }
  // 只有失败时才给"重新检测"—— 进行中和成功都没有可点的东西
  els.portmapRetry.hidden = snap.state !== 'failed';
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
  renderPushURLs(state.pushUrls);
  renderStatus();

  // 只有采集源变化才重渲染条件字段 —— 它会顺带刷新窗口列表,
  // 挂到所有控件上会导致改个码率就去枚举一遍窗口。
  els.source.addEventListener('change', () => {
    renderSourceFields();
    scheduleSave();
  });

  for (const el of [els.resolution, els.fps, els.bitrate, els.encoder,
                    els.audioBitrate, els.uplink, els.publicHost]) {
    el.addEventListener('change', scheduleSave);
  }

  // 音频开关除了存配置,还要切换码率那行的显隐。
  els.audioEnabled.addEventListener('change', () => {
    renderAudioFields();
    scheduleSave();
  });

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
  // 用户一动这个框,地址就归他管了 —— 自动映射从此不许再覆盖。
  //
  // 只在真的发生输入时清标记,而不是每次保存都清:保存会因为改别的字段
  // (码率、帧率)被顺带触发,那种情况下地址根本没被碰过。
  els.publicHost.addEventListener('input', () => {
    config.publicHostAuto = false;
    scheduleSave();
  });

  els.autoPortMap.addEventListener('change', scheduleSave);

  els.portmapRetry.addEventListener('click', async () => {
    els.portmapRetry.disabled = true;
    try {
      await api('/api/portmap/retry', { method: 'POST' });
    } catch {
      // 重试发不出去也无所谓,轮询会带来最新状态
    }
    setTimeout(() => { els.portmapRetry.disabled = false; }, 1000);
  });

  els.toggle.addEventListener('click', toggleStream);
  els.restart.addEventListener('click', restartStream);

  pollTimer = setInterval(poll, 2000);
}

init();
