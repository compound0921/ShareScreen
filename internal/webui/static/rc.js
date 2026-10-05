'use strict';

// 远程观看页。
//
// 画面本身还是原来的链路:ffmpeg 采集编码 → MediaMTX → WHEP 给浏览器。
// 这个页面额外做三件事,也是它和自己写好的 MediaMTX 播放页的区别:
//
//   1. 自己发 WHEP 请求 —— MediaMTX 自带的播放页没地方给我们挂输入处理器
//   2. 采集鼠标键盘,经 WebSocket 送回主机
//   3. 和主机协商控制权(申请 / 被批准 / 被夺回)
//
// 这里面最容易出错的是坐标映射,见 contentRect()。它错了不会报错,
// 只会表现为"点哪儿都偏一点",而且屏幕越大偏得越多。

const els = {
  video: document.getElementById('screen'),
  overlay: document.getElementById('overlay'),
  overlayMsg: document.getElementById('overlayMsg'),
  overlayHint: document.getElementById('overlayHint'),
  startBtn: document.getElementById('startBtn'),
  status: document.getElementById('status'),
  ctlBtn: document.getElementById('ctlBtn'),
  releaseBtn: document.getElementById('releaseBtn'),
  ctlInfo: document.getElementById('ctlInfo'),
  muteBtn: document.getElementById('muteBtn'),
  fullBtn: document.getElementById('fullBtn'),
  err: document.getElementById('err'),

  clipBtn: document.getElementById('clipBtn'),
  clipPanel: document.getElementById('clipPanel'),
  clipClose: document.getElementById('clipClose'),
  clipText: document.getElementById('clipText'),
  clipCopy: document.getElementById('clipCopy'),
  clipSend: document.getElementById('clipSend'),
  clipInfo: document.getElementById('clipInfo'),
};

// 令牌放在 URL 的 # 片段里。片段不会被发到服务器,所以页面本身的请求
// 在访问日志里是不带凭据的 —— 只有 WebSocket 那一次会带上。
const TOKEN = (() => {
  const m = /[#&]t=([^&]+)/.exec(location.hash || '');
  return m ? decodeURIComponent(m[1]) : '';
})();

const state = {
  whepBase: '',      // 形如 http://192.168.1.5:8889/live
  pc: null,
  sessionURL: '',
  ws: null,
  sessionId: '',
  controlling: false,
  ready: false,
  retry: 0,
  closedByUs: false,
};

// ───────────────────────── 启动 ─────────────────────────

async function main() {
  if (!TOKEN) {
    fail('链接里没有凭据', '请使用主机发来的「可控制链接」,不要手工拼地址。');
    return;
  }

  els.startBtn.hidden = false;
  els.startBtn.addEventListener('click', () => {
    // 这次点击同时解决两件事:浏览器要求"有用户操作"才允许播放声音。
    els.overlay.hidden = true;
    els.video.muted = false;
    updateMuteBtn();
  });

  // 先把 WebSocket 连起来 —— 控制权协商和画面是两件独立的事,
  // 画面慢不该拖住"申请控制"。
  connectWS();

  try {
    await startVideo();
  } catch (e) {
    fail('拿不到画面', String(e && e.message ? e.message : e));
  }
}

function fail(title, detail) {
  els.overlay.hidden = false;
  els.overlayMsg.textContent = title;
  els.overlayHint.textContent = detail || '';
  els.startBtn.hidden = true;
  setStatus('出错', 'badge-error');
}

function setStatus(text, cls) {
  els.status.textContent = text;
  els.status.className = 'badge ' + (cls || 'badge-idle');
}

function showError(msg) {
  els.err.hidden = false;
  els.err.textContent = msg;
}

// ───────────────────────── WHEP 拉流 ─────────────────────────

async function startVideo() {
  const cfg = await fetch('/rc/config', { cache: 'no-store' }).then((r) => r.json());

  // 主机名用 location.hostname:观众是从哪个地址连上来的,就按哪个地址
  // 去找流。程序不去猜他走的是局域网还是公网 —— 猜错的表现是画面出不来,
  // 而且完全看不出为什么。
  state.whepBase = `${location.protocol}//${location.hostname}:${cfg.whepPort}/${cfg.path}`;

  const pc = new RTCPeerConnection({ iceServers: [] });
  state.pc = pc;

  pc.addTransceiver('video', { direction: 'recvonly' });
  pc.addTransceiver('audio', { direction: 'recvonly' });

  pc.ontrack = (ev) => {
    if (ev.streams && ev.streams[0]) {
      els.video.srcObject = ev.streams[0];
    }
  };

  pc.onconnectionstatechange = () => {
    if (pc.connectionState === 'failed') {
      setStatus('画面中断', 'badge-error');
      showError('画面连接中断。若是刚改过分辨率或重启过共享,刷新页面即可。');
    }
  };

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await waitIceGathering(pc);

  const res = await fetch(`${state.whepBase}/whep`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/sdp' },
    body: pc.localDescription.sdp,
  });

  if (!res.ok) {
    throw new Error(`视频服务返回 ${res.status}。确认共享已开始,且端口没被防火墙挡住。`);
  }

  // MediaMTX 会在 Location 头里给出这个会话的地址,断开时要用它去 DELETE。
  // 拿不到就自己拼 —— 只是断开时可能留下一个空会话,不影响观看。
  const loc = res.headers.get('Location');
  state.sessionURL = loc
    ? new URL(loc, state.whepBase + '/').toString()
    : `${state.whepBase}/whep/${Math.random().toString(36).slice(2)}`;

  await pc.setRemoteDescription({ type: 'answer', sdp: await res.text() });

  els.video.play().catch(() => {});
  els.muteBtn.hidden = false;
  setStatus('已连接', 'badge-live');
  els.overlay.hidden = true;
  els.startBtn.hidden = true;
  state.ready = true;
}

// waitIceGathering 等到候选收集完再发 offer。
//
// WHEP 不带 trickle:offer 里的候选必须一次发全,否则对方只知道一部分
// 地址。超时兜底是必要的 —— 某个网卡的候选收集卡住时,一直转圈比少几条
// 候选更糟(后者至少局域网还能连上)。
function waitIceGathering(pc) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise((resolve) => {
    const done = () => {
      pc.removeEventListener('icegatheringstatechange', check);
      resolve();
    };
    const check = () => {
      if (pc.iceGatheringState === 'complete') done();
    };
    pc.addEventListener('icegatheringstatechange', check);
    setTimeout(done, 2000);
  });
}

// ───────────────────────── 控制通道 ─────────────────────────

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = `${proto}//${location.host}/ws?t=${encodeURIComponent(TOKEN)}`;

  const ws = new WebSocket(url);
  state.ws = ws;
  const openedAt = Date.now();

  ws.onopen = () => {
    state.retry = 0;
    setStatus(state.ready ? '已连接' : '准备中', state.ready ? 'badge-live' : 'badge-idle');
  };

  ws.onmessage = (ev) => {
    let m;
    try {
      m = JSON.parse(ev.data);
    } catch {
      return;
    }
    handleMsg(m);
  };

  ws.onclose = () => {
    setControlling(false);
    if (state.closedByUs) return;

    // 凭据不对时服务端是拒绝握手,浏览器只能给一个笼统的 1006 ——
    // 和网络不通长得一模一样,靠关闭码分不出来。
    //
    // 用"连续快速失败"来区分:网络不通、主机没开,握手通常要等一会儿
    // 才失败;凭据被拒是服务端立刻回绝,每次都在几毫秒内。
    const failedFast = Date.now() - openedAt < 1000;
    state.retry = failedFast ? state.retry + 1 : 0;

    if (state.retry > 4) {
      fail('连不上主机',
        '可能是链接里的凭据已经失效(主机重置过,或者远控被关掉过),也可能是主机没在运行。' +
        '请确认链接是否还有效。');
      return;
    }

    const delay = Math.min(600 * Math.pow(2, state.retry), 15000);
    setStatus('正在重连…', 'badge-warn');
    setTimeout(connectWS, delay);
  };

  ws.onerror = () => { /* onclose 会紧跟着来,在那边统一处理 */ };
}

function send(obj) {
  const ws = state.ws;
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(obj));
  }
}

function handleMsg(m) {
  switch (m.t) {
    case 'hello':
      state.sessionId = m.you;
      els.ctlBtn.hidden = false;
      // 主机没开剪贴板同步就整个藏起来 —— 显示一个点了没反应的按钮
      // 比不显示更让人困惑。
      els.clipBtn.hidden = !m.clipboard;
      break;

    case 'ctl':
      applyCtlState(m.state, m.reason);
      break;

    case 'clip':
      onHostClipboard(m.s || '');
      break;

    case 'err':
      showError(m.msg || m.code || '主机报了一个错');
      break;
  }
}

function applyCtlState(s, reason) {
  switch (s) {
    case 'granted':
      setControlling(true);
      els.ctlInfo.textContent = '你正在控制这台电脑';
      break;

    case 'pending':
      setControlling(false);
      setStatus('等待主机允许…', 'badge-warn');
      els.ctlBtn.hidden = true;
      els.releaseBtn.hidden = false;
      els.ctlInfo.textContent = '主机屏幕上会出现批准提示,请稍候';
      break;

    case 'busy':
      setControlling(false);
      els.ctlInfo.textContent = reason || '已经有人在控制这台电脑了';
      break;

    case 'denied':
      setControlling(false);
      els.ctlInfo.textContent = reason || '主机拒绝了这次申请';
      els.ctlBtn.hidden = false;
      els.releaseBtn.hidden = true;
      break;

    case 'revoked':
      setControlling(false);
      els.ctlInfo.textContent = reason || '主机收回了控制权';
      els.ctlBtn.hidden = false;
      els.releaseBtn.hidden = true;
      break;

    case 'idle':
      setControlling(false);
      els.ctlInfo.textContent = '';
      els.ctlBtn.hidden = false;
      els.releaseBtn.hidden = true;
      break;
  }
}

function setControlling(on) {
  if (state.controlling === on) return;

  // 控制权没了(被夺回、连接断了)的时候,把按着的键和鼠标键补一个抬起。
  // 主机那边收不到抬起,就会一直以为它们按着 —— 主机的用户接下来
  // 不管按什么都会变成组合键。
  if (!on && state.controlling) {
    releaseAllButtons();
    releaseAllKeys();
    pendingMove = null;
  }

  state.controlling = on;

  els.video.classList.toggle('controlling', on);
  els.releaseBtn.hidden = !on;
  els.ctlBtn.hidden = on;

  if (on) {
    setStatus('控制中', 'badge-warn');
  } else if (state.ready) {
    setStatus('已连接', 'badge-live');
  }
}

els.ctlBtn.addEventListener('click', () => send({ t: 'ctl', act: 'request' }));
els.releaseBtn.addEventListener('click', () => send({ t: 'ctl', act: 'release' }));

// ───────────────────────── 坐标映射 ─────────────────────────

// contentRect 返回画面在页面上**实际占据**的那块矩形。
//
// 这是整个文件里最容易出错的地方。video 元素通常比画面本身宽或窄
// (窗口比例和桌面比例不一样),object-fit: contain 会把画面居中缩放,
// 四周留出黑边。
//
// 直接用 video.getBoundingClientRect() 去算归一化坐标,黑边有多宽就
// 偏多远 —— 而且它不报错,只表现为"点哪儿都差一点",窗口一改尺寸偏差
// 还跟着变,极难归因。
function contentRect() {
  const r = els.video.getBoundingClientRect();
  const vw = els.video.videoWidth;
  const vh = els.video.videoHeight;
  if (!vw || !vh) return r; // 还没拿到画面尺寸,退回元素本身

  const scale = Math.min(r.width / vw, r.height / vh);
  const w = vw * scale;
  const h = vh * scale;
  return {
    left: r.left + (r.width - w) / 2,
    top: r.top + (r.height - h) / 2,
    width: w,
    height: h,
  };
}

function normalize(clientX, clientY) {
  const c = contentRect();
  const nx = (clientX - c.left) / c.width;
  const ny = (clientY - c.top) / c.height;
  // 夹到 [0,1]:指针捕获之后,拖到画面外的事件也会送过来
  return { x: clamp01(nx), y: clamp01(ny) };
}

function clamp01(v) {
  if (!isFinite(v)) return 0;
  return v < 0 ? 0 : v > 1 ? 1 : v;
}

// ───────────────────────── 输入采集 ─────────────────────────

const video = els.video;

// 鼠标移动:每个绘制帧只发最后一次位置。
//
// 不合并的话,一秒钟会发几百条,主机那边的注入队列会积压,而积压的
// 后果是操作延迟越来越大 —— 手感上是"越用越飘"。中间位置本来也没有
// 意义,观众的鼠标已经到别处了。
let pendingMove = null;
let moveScheduled = false;

video.addEventListener('pointermove', (e) => {
  if (!state.controlling) return;
  pendingMove = normalize(e.clientX, e.clientY);
  if (moveScheduled) return;
  moveScheduled = true;
  requestAnimationFrame(() => {
    moveScheduled = false;
    if (pendingMove && state.controlling) {
      send({ t: 'in', k: 'mv', x: pendingMove.x, y: pendingMove.y });
      pendingMove = null;
    }
  });
});

// 已经按下、还没抬起的按键。
//
// 必须要跟踪,不能只靠 pointerup:指针被系统抢走(通知弹窗、手势、
// 拖到另一个窗口)时浏览器只给一个 pointercancel,而它的 button 是 -1,
// 推不出该抬起哪一个。不补这一下,主机的鼠标键会一直卡在按下状态,
// 观众那边表现为"点什么都变成拖拽",只能重新连一次才能恢复。
const pressed = new Set();

function releaseAllButtons() {
  for (const b of pressed) {
    send({ t: 'in', k: 'btn', b, down: false });
  }
  pressed.clear();
}

video.addEventListener('pointerdown', (e) => {
  if (!state.controlling) return;
  e.preventDefault();
  // 捕获指针:拖拽时手滑出画面外,事件仍然送到这里,不会中途断掉。
  try { video.setPointerCapture(e.pointerId); } catch { /* 不支持就算了 */ }

  // 先报一次位置再报按下。主机那边是绝对坐标,不先移动的话这一下会
  // 点在光标**上一次**所在的位置上。
  const p = normalize(e.clientX, e.clientY);
  send({ t: 'in', k: 'mv', x: p.x, y: p.y });

  pressed.add(e.button);
  send({ t: 'in', k: 'btn', b: e.button, down: true });
});

video.addEventListener('pointerup', (e) => {
  if (!state.controlling) return;
  e.preventDefault();
  try { video.releasePointerCapture(e.pointerId); } catch { /* 同上 */ }
  pressed.delete(e.button);
  send({ t: 'in', k: 'btn', b: e.button, down: false });
});

// 指针被系统抢走:不知道松开的是哪个键,干脆全松开 —— 多抬一个没按的
// 键是无害的,少抬一个会让主机的鼠标卡住。
video.addEventListener('pointercancel', () => {
  if (!state.controlling) return;
  releaseAllButtons();
});

// 切走标签页时同理:这期间所有抬起事件都收不到,按键和鼠标键都要补。
window.addEventListener('blur', () => {
  releaseAllButtons();
  releaseAllKeys();
});

// 右键菜单要拦住,否则点击右键会弹出本机的浏览器菜单,而不是主机的。
video.addEventListener('contextmenu', (e) => {
  if (state.controlling) e.preventDefault();
});

video.addEventListener('wheel', (e) => {
  if (!state.controlling) return;
  e.preventDefault();

  // 浏览器:deltaY 向下滚为正。Windows:滚轮向上是正的 WHEEL_DELTA。
  // 两者符号相反,这里翻一下。
  const dy = -Math.sign(e.deltaY) * 120;
  const dx = -Math.sign(e.deltaX) * 120;
  if (dx || dy) send({ t: 'in', k: 'whl', dx, dy });
}, { passive: false });

document.addEventListener('keydown', (e) => { onKey(e, true); });
document.addEventListener('keyup', (e) => { onKey(e, false); });

// onKey 决定这一次按键走哪条通道。
//
//   带 Ctrl/Alt/Win 的 —— 走虚拟键码。这些是快捷键(Ctrl+C、Alt+Tab),
//   必须让主机按"按键"来理解,Unicode 事件产生不了组合键。
//
//   能产生文本的字符 —— 走文本通道。观众用中文输入法打出来的词在主机上
//   没有对应的按键序列,而且虚拟键码会被主机的键盘布局再解释一遍,布局
//   不同就打字错位。Unicode 事件同时绕开这两个问题。
//
//   其余(回车、退格、方向键、功能键)—— 走虚拟键码,它们本来就不产生文本。
function onKey(e, down) {
  if (!state.controlling) return;

  // 主机那边的"刹车"不能被观众按掉,浏览器也拦不住这些组合键;
  // 与其让观众以为按了有用,不如不放行。
  if (e.key === 'F5' || e.key === 'F11' || e.key === 'F12') return;

  const isText = !e.ctrlKey && !e.altKey && !e.metaKey && e.key.length === 1;

  if (isText) {
    // 文本通道只在按下时发一次:Unicode 事件自己是一对按下+抬起,
    // 再跟一个抬起就重复了。
    if (down && !e.repeat) send({ t: 'in', k: 'txt', s: e.key });
  } else {
    send({ t: 'in', k: 'key', code: e.code, down: down });
    // 记下按着的键。切走标签页时 keyup 收不到,不补的话主机会一直
    // 以为这个键没松开 —— 再按别的键就成了组合键。
    if (down) pressedKeys.add(e.code);
    else pressedKeys.delete(e.code);
  }

  e.preventDefault();
}

const pressedKeys = new Set();

function releaseAllKeys() {
  for (const code of pressedKeys) {
    send({ t: 'in', k: 'key', code, down: false });
  }
  pressedKeys.clear();
}

// ───────────────────────── 工具条 ─────────────────────────

els.fullBtn.addEventListener('click', () => {
  if (document.fullscreenElement) {
    document.exitFullscreen();
  } else {
    document.documentElement.requestFullscreen().catch(() => {});
  }
});

els.muteBtn.addEventListener('click', () => {
  els.video.muted = !els.video.muted;
  updateMuteBtn();
});

function updateMuteBtn() {
  els.muteBtn.textContent = els.video.muted ? '声音:关' : '声音:开';
}

// ───────────────────────── 剪贴板 ─────────────────────────

// 浏览器对剪贴板的限制很硬,而且这里踩中了两条:
//
//   1. navigator.clipboard 只在安全上下文里可用。观看页是
//      http://192.168.x.x,不是安全上下文(localhost 才算),所以那里
//      的 navigator.clipboard 是 undefined。
//   2. 即使可用,读剪贴板还要用户授权,写剪贴板要求页面有焦点。
//
// 所以这个面板的定位是"手动的桥梁",而不是无感的双向同步:
// 主机复制的东西自动出现在框里(那条路不受限制),从这边送出去需要
// 用户点一下。硬要做成无感的话,只能靠 document.execCommand('paste'),
// 而那个在现代浏览器里已经被禁掉了。

function showClipboardPanel(on) {
  els.clipPanel.hidden = !on;
  if (on) {
    els.clipText.focus();
    els.clipText.select();
  }
}

els.clipBtn.addEventListener('click', () => showClipboardPanel(els.clipPanel.hidden));
els.clipClose.addEventListener('click', () => showClipboardPanel(false));

els.clipCopy.addEventListener('click', async () => {
  const text = els.clipText.value;
  if (!text) return;

  // 先试标准 API
  try {
    await navigator.clipboard.writeText(text);
    flashClip('已复制');
    return;
  } catch {
    // 落到下面那条路
  }

  // 非安全上下文下的兜底:选中 + execCommand。它虽然被标记为过时,
  // 但在 http 页面上仍然是唯一能写剪贴板的办法。
  els.clipText.select();
  try {
    if (document.execCommand('copy')) {
      flashClip('已复制');
    } else {
      flashClip('复制失败,请手动 Ctrl+C');
    }
  } catch {
    flashClip('复制失败,请手动 Ctrl+C');
  }
});

els.clipSend.addEventListener('click', () => {
  const text = els.clipText.value;
  if (!text) return;
  if (!state.controlling) {
    flashClip('要先拿到控制权才能改主机的剪贴板');
    return;
  }
  send({ t: 'clip', s: text });
  flashClip('已发送');
});

function flashClip(msg) {
  els.clipInfo.textContent = msg;
  setTimeout(() => { els.clipInfo.textContent = ''; }, 2000);
}

// 主机剪贴板变了。写进框里,但不覆盖用户正在编辑的内容 ——
// 他可能正在这边敲一段准备发过去的文本。
function onHostClipboard(text) {
  if (document.activeElement === els.clipText && els.clipText.value) {
    flashClip('主机剪贴板变了(未覆盖你正在编辑的内容)');
    return;
  }
  els.clipText.value = text;
  showClipboardPanel(true);
  flashClip('来自主机');
}

// 离开页面时把 WHEP 会话关掉。不关的话 MediaMTX 那边会留一个空会话,
// 主机看到的观众数会多一个。
window.addEventListener('pagehide', () => {
  state.closedByUs = true;
  releaseAllButtons();
  releaseAllKeys();
  if (state.ws) state.ws.close();
  if (state.sessionURL) {
    // keepalive 让请求在页面卸载过程中也能发出去
    fetch(state.sessionURL, { method: 'DELETE', keepalive: true }).catch(() => {});
  }
});

main();
