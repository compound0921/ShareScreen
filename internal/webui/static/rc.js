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
  pwInput: document.getElementById('pwInput'),
  releaseBtn: document.getElementById('releaseBtn'),
  ctlInfo: document.getElementById('ctlInfo'),
  muteBtn: document.getElementById('muteBtn'),
  fullBtn: document.getElementById('fullBtn'),
  bar: document.getElementById('bar'),
  corner: document.getElementById('corner'),
  hudBtn: document.getElementById('hudBtn'),
  exitImmersive: document.getElementById('exitImmersive'),
  kbSysBtn: document.getElementById('kbSysBtn'),
  kbPadBtn: document.getElementById('kbPadBtn'),
  rclickBtn: document.getElementById('rclickBtn'),
  keyPad: document.getElementById('keyPad'),
  keyPadGrid: document.getElementById('keyPadGrid'),
  keyPadClose: document.getElementById('keyPadClose'),
  typing: document.getElementById('typing'),
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
      // 密码框同理:主机没设密码时,摆一个填了也没用的框只会让人以为
      // 必须先填点什么才能申请。填了框里的东西跟申请一起送上去,主机
      // 那侧比对通过就直接给控制权,不用他点允许。
      els.pwInput.hidden = !m.passwordSet;
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
      // 排队中,那个按钮的身份是"取消申请" —— 而且它真的能取消(服务端
      // 的 release 认排队中的会话,见 internal/remotectl)。所以它必须一直
      // 在:等不到回应、或者想改个密码重来时,用户得有路可退。
      //
      // 「申请控制」只在密码填错那次留着(那时 reason 非空),改一个再点一次
      // 是有意义的 —— 主机那侧会把同一个人的排队升格成控制权,而不是重排。
      els.ctlBtn.hidden = !reason;
      els.releaseBtn.textContent = '取消请求';
      els.releaseBtn.hidden = false;
      els.ctlInfo.textContent = reason || '主机屏幕上会出现批准提示,请稍候';
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
      // 归还是"已归还控制权"、撤回申请是"已取消申请" —— 得说出来,
      // 否则点完取消之后界面只是按钮变回「申请控制」,像是没反应。
      els.ctlInfo.textContent = reason || '';
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
    // 输入类的东西全部收回去:软键盘、系统键盘、「右键」开关、还亮着的
    // 修饰键。留着的话,下次拿到控制权时它们会带着上一次的状态回来 ——
    // 比如 Ctrl 还亮着,而用户早就忘了自己点过它。
    setRightArmed(false);
    closeKeyPad();
    blurSoftKeyboard();
    clearMods();
  }

  state.controlling = on;

  els.video.classList.toggle('controlling', on);
  els.releaseBtn.hidden = !on;
  // 拿到控制权之后,那个按钮的身份才是"归还"(排队中是"取消申请",
  // 见 applyCtlState 的 pending 分支)。
  if (on) els.releaseBtn.textContent = '归还控制';
  els.ctlBtn.hidden = on;
  // 这三个只在控制中才有意义 —— 没控制权时敲了也送不出去,摆着只会让人
  // 以为坏了(和剪贴板按钮同一个道理)。
  els.kbSysBtn.hidden = !on;
  els.kbPadBtn.hidden = !on;
  els.rclickBtn.hidden = !on;

  if (on) {
    setStatus('控制中', 'badge-warn');
  } else if (state.ready) {
    setStatus('已连接', 'badge-live');
  }
}

els.ctlBtn.addEventListener('click', () => {
  // 密码留空也要发:那是"不填密码,走主机批准"这条正常路径。主机那侧
  // 对空密码一律不匹配(见 remotectl 的 passwordMatch),不会误放行。
  send({ t: 'ctl', act: 'request', pw: els.pwInput.value });
});
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
  gestureButton = null;
}

// gestureButton 是这一次"按下"实际用的是哪个键。
//
// 必须记下来,不能等到 pointerup 再读 e.button:触摸屏上没有右键,
// 「右键」开关会把这次点击**改**成右键(见 setRightArmed),而 pointerup
// 报回来的仍然是 0。不记的话,抬起的就是左键 —— 主机那边的右键会一直按着。
let gestureButton = null;

video.addEventListener('pointerdown', (e) => {
  if (!state.controlling) return;
  e.preventDefault();
  // 捕获指针:拖拽时手滑出画面外,事件仍然送到这里,不会中途断掉。
  try { video.setPointerCapture(e.pointerId); } catch { /* 不支持就算了 */ }

  // 先报一次位置再报按下。主机那边是绝对坐标,不先移动的话这一下会
  // 点在光标**上一次**所在的位置上。
  const p = normalize(e.clientX, e.clientY);
  send({ t: 'in', k: 'mv', x: p.x, y: p.y });

  // 「右键」开关亮着的时候,把这一次左键点击换成右键,然后开关自己灭掉。
  // 只换左键:鼠标本来的中键/右键照旧,不去动它们。
  let btn = e.button;
  if (rightArmed && btn === 0) {
    btn = 2;
    setRightArmed(false);
  }

  gestureButton = btn;
  pressed.add(btn);
  send({ t: 'in', k: 'btn', b: btn, down: true });
});

video.addEventListener('pointerup', (e) => {
  if (!state.controlling) return;
  e.preventDefault();
  try { video.releasePointerCapture(e.pointerId); } catch { /* 同上 */ }
  // 抬起用按下时记下的那个键,不是 e.button —— 理由见 gestureButton。
  const btn = gestureButton != null ? gestureButton : e.button;
  pressed.delete(btn);
  gestureButton = null;
  send({ t: 'in', k: 'btn', b: btn, down: false });
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

  // 焦点在"系统软键盘把手"上的时候,这条路由它自己的监听器负责。
  // 不挡的话,软键盘每敲一个字都会走两遍:一遍是 typing 的 input,
  // 一遍是这里冒泡上来的 keydown —— 主机上就是每个字打两遍。
  if (e.target === els.typing) return;

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

// ───────────────────────── 沉浸模式(软全屏) ─────────────────────────
//
// 主体是 CSS 撑满(body.rc.immersive,见 rc.css),**不**依赖系统全屏。
// 三条理由,都是实测/标准层面的硬约束:
//
//   · 进了系统全屏之后,从屏幕顶部往下拖是系统级的"退出全屏"手势,网页
//     拦不住 —— WebKit 明确答复过 preventDefault 和 touch-action: none
//     都不行(bug 264215),它和按 Esc、点关闭按钮一样是反钓鱼设计。而
//     这个页面的核心操作就是按住把鼠标往下拖 —— 一次拖拽就被踢出全屏,
//     那次操作也就断了。
//   · iPhone 的 Safari 根本不支持元素的 requestFullscreen(只有 iPadOS
//     支持),原来那个按钮在 iPhone 上是静默失效的 —— 失败被 .catch 吃掉。
//   · 而原来那个「全屏」按钮就在工具条里,工具条一到全屏就被藏起来
//     (见 rc.css 的 @media (display-mode: fullscreen))—— 等于进了全屏
//     就没有退出口,只剩那个手势。现在退出口是浮在画面上的,不再跟着藏。
//
// 代价:不进系统全屏,浏览器自己的工具条就还在(手机上尤其明显)。标签页里
// 没有办法去掉它,这是苹果的限制;要彻底没有,只有"添加到主屏幕"那条路。
//
// 例外:**有鼠标的设备仍然进一次系统全屏** —— 那里没有触摸手势的冲突,
// 白拿一块浏览器边框,不要白不要。这判断用的是能力媒体查询,不是猜 UA。
const wantsSystemFullscreen =
  window.matchMedia('(hover: hover) and (pointer: fine)').matches;

function isImmersive() {
  return document.body.classList.contains('immersive');
}

function setImmersive(on) {
  document.body.classList.toggle('immersive', on);
  els.corner.hidden = !on;
  // 每次进出都恢复成"工具条可见"。尤其是退出的时候 —— 否则收着工具条退出,
  // 回到小窗口连按钮都找不着了,而那时右上角那组浮标也已经跟着藏了。
  setHudVisible(true);

  if (on) {
    if (wantsSystemFullscreen) {
      document.documentElement.requestFullscreen().catch(() => {});
    }
  } else if (document.fullscreenElement) {
    document.exitFullscreen().catch(() => {});
  }
}

// setHudVisible 收起/展开下面那条工具条。
//
// 收起是给"要全画面看东西"用的:那条 HUD 虽然只有一行,但一直压着画面底部。
// 右上角那组浮标不跟着收 —— 不然就没有东西能把它叫回来了。
function setHudVisible(on) {
  els.bar.hidden = !on;
  els.hudBtn.textContent = on ? '收起' : '展开';
}

els.fullBtn.addEventListener('click', () => setImmersive(!isImmersive()));
els.exitImmersive.addEventListener('click', () => setImmersive(false));
els.hudBtn.addEventListener('click', () => setHudVisible(els.bar.hidden));

// 桌面按 Esc / 浏览器自己的退出按钮会退出系统全屏,而那是浏览器自己处理的,
// 我们收不到按键。不听这一下,工具条会一直藏着、浮出来的退出口又已经跟着
// 消失,人就真被关在里面了。
document.addEventListener('fullscreenchange', () => {
  if (!document.fullscreenElement && isImmersive()) setImmersive(false);
});

// ───────────────────────── 右键开关 ─────────────────────────
//
// 触摸屏上没有右键。这个开关亮着的时候,下一次点击按右键发出去,然后它自己
// 灭掉(见 pointerdown)。
//
// 为什么不做成"长按 = 右键"或者"双指轻点 = 右键":
//
//   · 长按:得等半秒才出右键,而且手指"慢慢起步"的那种拖拽会被误判成
//     右键 —— 而拖拽恰恰是这个页面的主要操作。
//   · 双指轻点:要认出第二根手指,就只能把**每一次单击**都先压后一小段
//     时间再发。那是给所有操作都加延迟,在远控里比多点一个按钮贵得多。
//
// 一次性开关不欠任何账:不点它,点击一点没变;点了它,下一次点就是右键。
let rightArmed = false;

function setRightArmed(on) {
  rightArmed = on;
  els.rclickBtn.classList.toggle('on', on);
}

els.rclickBtn.addEventListener('click', () => setRightArmed(!rightArmed));

// ─────────────── 系统软键盘(iPadOS / Android 自带的那个) ───────────────
//
// iOS 只在**真正的输入框**拿到焦点时才弹键盘,所以页面上必须有一个能聚焦
// 的东西 —— 就是 rc.html 里那个 .rc-typing:1 像素、全透明,看得见的是
// 键盘,不是它。
//
// 它里面**一个字都不留**:每收到一次输入就把内容取走、发给主机、清空。
// 留着就要维护光标位置、选区和"这次新增了什么",而主机那边要的是按键,
// 不是一份文本。
let composing = false;

// flushTyping 把输入框里现在的内容当文本发出去,然后清空。
function flushTyping() {
  const s = els.typing.value;
  els.typing.value = '';
  if (s) send({ t: 'in', k: 'txt', s });
}

els.typing.addEventListener('compositionstart', () => { composing = true; });
els.typing.addEventListener('compositionend', () => {
  composing = false;
  flushTyping(); // 中文输入的整个词,到这里才送出去
});

els.typing.addEventListener('input', () => {
  // 输入法正在拼的时候,框里是拼音字母,不是用户要打的字。这时候发出去,
  // 主机上会先冒出一串拼音再被中文替换 —— 等 compositionend 再说。
  if (composing) return;
  flushTyping();
});

els.typing.addEventListener('keydown', (e) => {
  // 回车和退格不产生文本,也不会出现在 input 事件里 —— 空框上的退格连
  // 内容都没得删,只有 keydown 认得出。
  //
  // 这一条按 iPadOS 做:那里的 Safari 会照实报 key='Backspace'/'Enter'。
  // 安卓的软键盘常常把它们报成 keyCode 229(Unidentified),要另外绕,
  // 暂时不管 —— 这个功能的第一个目标是 iPad。
  if (e.key === 'Backspace' || e.key === 'Enter') {
    e.preventDefault();
    sendKey(e.key);
  }
});

els.typing.addEventListener('focus', () => els.kbSysBtn.classList.add('on'));
els.typing.addEventListener('blur', () => els.kbSysBtn.classList.remove('on'));

function blurSoftKeyboard() {
  if (document.activeElement === els.typing) els.typing.blur();
}

els.kbSysBtn.addEventListener('click', () => {
  if (document.activeElement === els.typing) {
    blurSoftKeyboard();
    return;
  }
  closeKeyPad(); // 两个键盘不同时开,否则屏幕剩不下多少画面
  els.typing.value = '';
  els.typing.focus();
});

// ─────────────── 软键盘(页面里自己画的这个) ───────────────
//
// 它补的是系统软键盘给不了的那部分:Ctrl/Alt/Shift/Win、Tab、Esc、方向键、
// Del/Home/End/PgUp/PgDn。字母数字也画上了,所以不打字时也能完全靠它。
//
// 键名用的是 KeyboardEvent.code,和主机那张映射表(internal/input/keys.go)
// 是同一套名字 —— 名字对不上就是"按下去没反应",而那种 bug 在真机上很难查。
//
// 布局是数据:往 KEY_ROWS 里加一行就多一行键。
const KEY_ROWS = [
  ['Escape', 'Tab', 'ControlLeft', 'AltLeft', 'ShiftLeft', 'MetaLeft'],
  ['Digit1', 'Digit2', 'Digit3', 'Digit4', 'Digit5', 'Digit6',
   'Digit7', 'Digit8', 'Digit9', 'Digit0', 'Minus', 'Equal', 'Backspace'],
  ['KeyQ', 'KeyW', 'KeyE', 'KeyR', 'KeyT', 'KeyY', 'KeyU', 'KeyI', 'KeyO',
   'KeyP', 'BracketLeft', 'BracketRight'],
  ['KeyA', 'KeyS', 'KeyD', 'KeyF', 'KeyG', 'KeyH', 'KeyJ', 'KeyK', 'KeyL',
   'Semicolon', 'Quote', 'Enter'],
  ['KeyZ', 'KeyX', 'KeyC', 'KeyV', 'KeyB', 'KeyN', 'KeyM',
   'Comma', 'Period', 'Slash'],
  ['ArrowLeft', 'ArrowUp', 'ArrowDown', 'ArrowRight', 'Space',
   'Delete', 'Home', 'End', 'PageUp', 'PageDown'],
];

const KEY_LABELS = {
  Escape: 'Esc', Tab: 'Tab', ControlLeft: 'Ctrl', AltLeft: 'Alt',
  ShiftLeft: 'Shift', MetaLeft: 'Win', Backspace: '⌫', Enter: '⏎',
  Space: '空格', Delete: 'Del', Home: 'Home', End: 'End',
  PageUp: 'PgUp', PageDown: 'PgDn',
  ArrowLeft: '←', ArrowUp: '↑', ArrowDown: '↓', ArrowRight: '→',
  Minus: '-', Equal: '=', BracketLeft: '[', BracketRight: ']',
  Semicolon: ';', Quote: "'", Comma: ',', Period: '.', Slash: '/',
};

// 宽一点的键才像键盘
const KEY_WIDTH = { Backspace: 'w2', Enter: 'w2', Space: 'w6' };

// 修饰键:点一下亮起(下一个键带上它),不直接发出去。
const MOD_CODES = new Set(['ControlLeft', 'AltLeft', 'ShiftLeft', 'MetaLeft']);

const mods = new Set();

// 按住 Shift 时上面那两排变成什么。字母靠 toUpperCase,只要列符号。
const SHIFT_DIGITS = {
  '1': '!', '2': '@', '3': '#', '4': '$', '5': '%',
  '6': '^', '7': '&', '8': '*', '9': '(', '0': ')',
};
const SHIFT_PUNCT = {
  Minus: ['-', '_'], Equal: ['=', '+'],
  BracketLeft: ['[', '{'], BracketRight: [']', '}'],
  Semicolon: [';', ':'], Quote: ["'", '"'],
  Comma: [',', '<'], Period: ['.', '>'], Slash: ['/', '?'],
};

function keyLabel(code) {
  if (KEY_LABELS[code]) return KEY_LABELS[code];
  if (code.startsWith('Key')) return code[3].toLowerCase();
  if (code.startsWith('Digit')) return code[5];
  return code;
}

// keyText 返回这个键在不按 Ctrl/Alt/Win 时该送出去的**文本**;
// 方向键、Tab、Esc 这类没有文本含义的返回 null,表示只能走键码。
function keyText(code, shift) {
  if (code.length === 4 && code.startsWith('Key')) {
    const c = code[3].toLowerCase();
    return shift ? c.toUpperCase() : c;
  }
  if (code.length === 6 && code.startsWith('Digit')) {
    const d = code[5];
    return shift ? SHIFT_DIGITS[d] : d;
  }
  if (code === 'Space') return ' ';
  const p = SHIFT_PUNCT[code];
  if (p) return shift ? p[1] : p[0];
  return null;
}

function sendKeyDown(code, down) {
  send({ t: 'in', k: 'key', code, down });
}

// sendKey 是一次完整的"按下再抬起",给退格、回车这种单独一个键用的。
function sendKey(code) {
  sendKeyDown(code, true);
  sendKeyDown(code, false);
}

function refreshMods() {
  for (const b of els.keyPadGrid.querySelectorAll('.rc-key')) {
    if (MOD_CODES.has(b.dataset.code)) {
      b.classList.toggle('on', mods.has(b.dataset.code));
    }
  }
}

function clearMods() {
  mods.clear();
  refreshMods();
}

function tapKey(code) {
  if (!state.controlling) return;

  // 修饰键只切换亮灭,不发出去 —— 单独按下 Ctrl 在主机上什么也不该发生。
  if (MOD_CODES.has(code)) {
    if (mods.has(code)) mods.delete(code);
    else mods.add(code);
    refreshMods();
    return;
  }

  const combo = mods.has('ControlLeft') || mods.has('AltLeft') || mods.has('MetaLeft');
  const text = combo ? null : keyText(code, mods.has('ShiftLeft'));

  if (text !== null) {
    // 能算成文本就走文本通道。走键码的话,主机要按**它自己的**键盘布局
    // 再解释一遍,布局不一样就会打错字(见 onKey 里那段说明)。
    send({ t: 'in', k: 'txt', s: text });
  } else {
    // 组合键只能走键码 —— Ctrl+C 这种东西在文本通道里表达不出来。
    const held = [...mods];
    for (const m of held) sendKeyDown(m, true);
    sendKeyDown(code, true);
    sendKeyDown(code, false);
    for (const m of held.reverse()) sendKeyDown(m, false);
  }

  // 修饰键按完即灭。粘着不放的话,用户会莫名其妙地一直在按 Ctrl,
  // 而且没有任何提示告诉他为什么。
  clearMods();
}

function buildKeyPad() {
  for (const row of KEY_ROWS) {
    const line = document.createElement('div');
    line.className = 'rc-keys-row';
    for (const code of row) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'rc-key';
      if (KEY_WIDTH[code]) b.classList.add(KEY_WIDTH[code]);
      b.dataset.code = code;
      b.textContent = keyLabel(code);
      // pointerdown 而不是 click:键盘要的是"按下去就发"。等 click 等于
      // 等到手指抬起来,手感上慢一拍。
      b.addEventListener('pointerdown', (e) => {
        e.preventDefault(); // 别让浏览器把这次触摸当成滚动/缩放
        tapKey(code);
      });
      line.appendChild(b);
    }
    els.keyPadGrid.appendChild(line);
  }
}

buildKeyPad();

function closeKeyPad() {
  els.keyPad.hidden = true;
  els.kbPadBtn.classList.remove('on');
}

els.kbPadBtn.addEventListener('click', () => {
  if (!els.keyPad.hidden) {
    closeKeyPad();
    return;
  }
  blurSoftKeyboard(); // 同理,两个键盘不叠在一起
  els.keyPad.hidden = false;
  els.kbPadBtn.classList.add('on');
});

els.keyPadClose.addEventListener('click', closeKeyPad);

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
