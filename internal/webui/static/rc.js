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
  openBtn: document.getElementById('openBtn'),
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
  // rotate 是主机要求把画面转回来的角度(0/180),见 main 里那段说明。
  rotate: 0,

  // cfg 是 /rc/config 拉回来的那份配置。留着是因为主机同意之后要重新
  // 拉一次流,而那时候不该再跑一遍整页启动流程(见 startVideoWithRetry)。
  cfg: null,

  // 下面这四个都是**主机那侧**的开关,页面只读它们:
  //
  //   remoteEnabled 远控开着没有 —— 决定这一排按钮出现的是"申请控制"
  //                 还是"请求开启共享与远控"。两者互斥。
  //   sharing       主机在不在推流 —— 决定要不要去拉流。没推流时拉必然
  //                 失败,而那句失败提示会把"等主机同意"说成"出错"。
  //   passwordSet   主机设没设控制密码 —— 决定密码框显不显示。
  //   openPending   这次的开启请求是不是还在等主机点同意。
  remoteEnabled: false,
  sharing: false,
  passwordSet: false,
  openPending: false,

  // held 表示主机此刻自己在用键鼠,注入被让开了(见 setHeld)。
  held: false,
};

// ───────────────────────── 启动 ─────────────────────────

async function main() {
  if (!TOKEN) {
    fail('链接里没有凭据', '请使用主机发来的「可控制链接」,不要手工拼地址。');
    return;
  }

  // 配置先拉一次。观看页要的东西就这几样,拉两次没意义 —— 而且局域网判断
  // 必须发生在**连任何东西之前**(见 preferLAN)。
  let cfg;
  try {
    cfg = await fetch('/rc/config', { cache: 'no-store' }).then((r) => r.json());
  } catch (e) {
    fail('拿不到画面', String(e && e.message ? e.message : e));
    return;
  }

  // 同网段却从公网地址打开的:换到局域网地址重开,这一轮到此为止。
  // 放在这里而不是 startVideo 里,是为了别先连上 WebSocket 再跳走 ——
  // 主机日志里会多一条无意义的连接,页面上还会闪一下"已连接"。
  if (await preferLAN(cfg)) return;

  // 主机的屏幕被系统转过(比如物理倒装的显示器),而采集走的是 GPU 那条
  // 零拷贝路 —— 那种采集拿到的是**未旋转的原始画面**,所以流是倒的。
  //
  // 为什么不在主机端转回来:零拷贝那点好处会被一次 CPU 翻转全吃掉,实测
  // 比 gdigrab 还慢(3.66s vs 3.06s CPU / 5 秒);GPU 翻转这个 ffmpeg 版本
  // 又不支持(d3d11 既进不了 transpose_cuda,也映射不到 cuda)。浏览器做
  // 一次 CSS 旋转不花任何代价,所以放在这里转。
  //
  // ⚠️ **坐标不用另外翻。** 旋转作用在元素的渲染结果上,画面内容跟着一起
  // 转;而 contentRect() 量的是元素盒子(180° 旋转不改变盒子)。于是
  // "屏幕上的位置 → 归一化坐标"这条映射原样成立:观众在画面上看到的那个
  // 位置,归一化之后正好就是主机上的那个位置。多翻一次会变成对角镜像 ——
  // 那种错法看着像"点哪都差一半",其实是在对角线另一头。
  //
  // 代价是流本身是倒的:录下来、或用别的播放器打开,看到的是倒的。
  state.rotate = cfg.rotate === 180 ? 180 : 0;
  if (state.rotate) {
    els.video.style.transform = `rotate(${state.rotate}deg)`;
  }

  state.cfg = cfg;
  state.remoteEnabled = !!cfg.remote;
  state.sharing = !!cfg.sharing;

  els.startBtn.hidden = false;
  els.startBtn.addEventListener('click', () => {
    // 这次点击同时解决两件事:浏览器要求"有用户操作"才允许播放声音。
    els.overlay.hidden = true;
    els.video.muted = false;
    updateMuteBtn();
  });

  // 先把 WebSocket 连起来 —— 控制权协商和画面是两件独立的事,
  // 画面慢不该拖住"申请控制"。而且共享还没开始的时候,这条连接是
  // 唯一能让主机知道"有人想连上来"的路。
  connectWS();

  // 主机还没开始推流:这时候拉流必然失败,而那句失败提示会把"在等主机
  // 同意"说成"出错了"。改成把请求按钮露出来。
  if (!state.sharing) {
    showRequestOverlay();
    return;
  }

  try {
    await startVideo(cfg);
  } catch (e) {
    fail('拿不到画面', String(e && e.message ? e.message : e));
  }
}

// showRequestOverlay 是"主机还没开始共享"时那块遮罩的样子。
//
// 措辞很要紧:这时候页面一片黑不是因为坏了,而是因为主机还没开始。写成
// "拿不到画面",用户会去查网络、查防火墙,而该做的事只是点一下按钮。
function showRequestOverlay() {
  els.overlay.hidden = false;
  els.overlayMsg.textContent = '主机还没有开始共享';
  els.overlayHint.textContent =
    '点下面的「请求开启共享与远控」,主机同意之后这里会自动连上画面。';
  els.startBtn.hidden = true;
  setStatus('未开始', 'badge-idle');
  applyOpenUI();
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

// ───────────────────── 局域网优先 ─────────────────────
//
// 同一个网络里从**公网地址**打开观看页,媒体流会绕一圈互联网(出到路由器,
// 经运营商再回来)。实测延迟差 2–3 倍:公网 ~500ms,局域网 ~100–200ms ——
// 因为路径上的抖动会把浏览器接收端的抖动缓冲撑大。
//
// 而 ICE 候选列表对谁都是同一份(MediaMTX 一律给三条:网卡地址、公网地址、
// 回环),所以**光把 WHEP 地址换成局域网是没用的**。真正起作用的是这个页面
// 从哪个地址加载 —— 这一点是实测出来的:同网段那台设备,配置一个字没改,
// 用局域网链接打开是 100–200ms,用公网链接打开就是 500ms。
//
// 所以这里做的正是"人刚才手动做的那件事":发现本页不是从局域网地址打开的、
// 而某个局域网地址又够得着,就整页换过去重开。够得着是**探**出来的,不是
// 猜的 —— 猜错的表现是画面彻底出不来,而且看不出为什么。
const PRIVATE_IP = /^(10\.|127\.|169\.254\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/;

function looksLocal(host) {
  return host === 'localhost' || PRIVATE_IP.test(host);
}

// reachable 探一个局域网地址通不通。
//
// 探的是这台服务器自己的接口(同一个端口),不是媒体端口 —— 要判断的是
// "这个地址是不是通往主机的局域网那条路"。
//
// 用 no-cors 是被迫的:跨源请求如果不这么写,浏览器会因为拿不到 CORS 头
// 就判定失败,而它明明答了。这里不需要读内容,只要"答了"这一个事实,
// 拿到不透明响应正好。
function reachable(host) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), 400);
  return fetch(`${location.protocol}//${host}:${location.port}/rc/config`, {
    mode: 'no-cors',
    cache: 'no-store',
    signal: ctl.signal,
  }).then(() => true, () => false).finally(() => clearTimeout(timer));
}

// lanSwitchDisabled 读链接上那个"别自动切"的开关。?lan=off 或 ?lan=0。
//
// 为什么留这个口子:自动切换修的是"同网段却绕公网"那个坑,但它同时让
// "我就想从公网这条路连一次"变得做不到 —— 而验证公网路径、量它的延迟、
// 看它在弱网下什么样,都是必须能做的事。默认仍然是自动切。
function lanSwitchDisabled() {
  const v = (new URLSearchParams(location.search).get('lan') || '').toLowerCase();
  return v === 'off' || v === '0' || v === 'false' || v === 'no';
}

// preferLAN 在需要且能够时把页面换到局域网地址重开。
// 返回 true 表示已经发起跳转,调用方这一轮什么都不该再做。
async function preferLAN(cfg) {
  if (lanSwitchDisabled()) return false;

  const lan = (cfg && cfg.lan) || [];
  if (!lan.length) return false;
  // 本来就是从局域网地址(或 localhost)打开的,没什么可换
  if (looksLocal(location.hostname)) return false;
  // 防循环:万一跳过去之后那个地址又不在 lan 里(比如配置里写的是域名),
  // 至少不会来回跳
  if (sessionStorage.getItem('preferLAN')) return false;

  // **并行探,不串行**:串行的话多几个候选就会把"真的在外面"的人拖上
  // 好几个超时。并行之后,该等的时间只有一个超时(局域网里则几乎是立刻,
  // 因为每个探测都马上有回音)。
  const ok = await Promise.all(lan.map(reachable));
  // 按 lan 的顺序取第一个通的 —— 顺序有意义(路由表推出来的排第一)
  const host = lan[ok.findIndex((v) => v)];
  if (!host) return false;

  sessionStorage.setItem('preferLAN', '1');
  // **必须带上 # 片段** —— 令牌就在里面,丢了这页就再也连不上了。
  location.replace(
    `${location.protocol}//${host}:${location.port}` +
    `${location.pathname}${location.search}${location.hash}`,
  );
  return true;
}

// ───────────────────────── WHEP 拉流 ─────────────────────────

async function startVideo(cfg) {
  // 主机名用 location.hostname:观众是从哪个地址连上来的,就按哪个地址
  // 去找流。走到这里说明 preferLAN 没换过页(本来就在局域网里,或者探下来
  // 一个局域网地址都不通 —— 那就是真的在外面),所以照旧。
  state.whepBase = `${location.protocol}//${location.hostname}:${cfg.whepPort}/${cfg.path}`;

  // 先把上一条会话收掉。主机同意开启之后我们会再调一次(见
  // startVideoWithRetry),不收的话每重试一轮就多留一个连接。
  closeVideo();

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

// closeVideo 把当前这条 WHEP 会话收掉(连接和 MediaMTX 那边的会话都收)。
//
// 不等 DELETE 的结果:它失败只是让主机的观众数多挂一会儿,不该拖住
// 重新拉流 —— 而"重新拉流"正是主机刚点了同意的那一刻。
function closeVideo() {
  if (state.pc) {
    try {
      state.pc.close();
    } catch { /* 关不掉就算了,下面会建一个新的 */ }
    state.pc = null;
  }
  if (state.sessionURL) {
    fetch(state.sessionURL, { method: 'DELETE' }).catch(() => {});
    state.sessionURL = '';
  }
  state.ready = false;
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
    // setControlling 只管控制权那套按钮,它会把「申请控制」放出来。远控
    // 没开的时候那个按钮是错的(点了服务端也不受理),所以再摆一次。
    applyOpenUI();
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
      // 这两个是**当下**的主机状态,不是页面缓存的。页面完全可能是主机
      // 点完同意之后才刷新出来的 —— 那时候该显示的是"申请控制",而不是
      // 一个已经过时的"请求开启"。
      state.remoteEnabled = !!m.remote;
      state.sharing = !!m.sharing;
      // 主机没开剪贴板同步就整个藏起来 —— 显示一个点了没反应的按钮
      // 比不显示更让人困惑。
      els.clipBtn.hidden = !m.clipboard;
      // 密码框同理:主机没设密码时,摆一个填了也没用的框只会让人以为
      // 必须先填点什么才能申请。填了框里的东西跟申请一起送上去,主机
      // 那侧比对通过就直接给控制权,不用他点允许。
      state.passwordSet = !!m.passwordSet;
      // 远控开着、画面也在:按钮归控制权那套状态机管,先让它摆一次。
      //
      // 重连也会走到这里,而重连是个**新会话** —— 断开时那个会话的待批
      // 申请在服务端已经作废了(removeSession → control.drop),所以这里
      // 回到"可以申请"是对的,不是把排队状态弄丢了。
      if (state.remoteEnabled && state.sharing) {
        applyCtlState('idle', '');
      }
      applyOpenUI();
      break;

    case 'ctl':
      applyCtlState(m.state, m.reason);
      break;

    case 'open':
      applyOpenState(m.state, m.reason);
      break;

    case 'hold':
      setHeld(m.state === 'on');
      break;

    case 'clip':
      onHostClipboard(m.s || '');
      break;

    case 'err':
      showError(m.msg || m.code || '主机报了一个错');
      break;
  }
}

// ───────────────── 请求开启共享与远控 ─────────────────
//
// 远控还没开的时候,这个页面能做的只有一件事:请主机把它开起来 —— 共享
// 顺带一起开,那是主机那一次批准同时决定的两件事(措辞在主机端的弹窗上)。
//
// 两个按钮(申请控制 / 请求开启)互斥,由 state.remoteEnabled 决定出现哪
// 一个。远控开着的时候一切照旧,这套东西完全不参与。
//
// 这一整套只在**主播还没开远控**时用得上,而这个状态现在很常见:请求入口
// 让远控端口默认就听着,所以远端的人随时可能连上来,看到的是这个页面。

// applyOpenUI 决定那一排按钮里该出现哪一个。
//
// 只有这一个地方决定这几个按钮的显隐,因为"哪个按钮该在"是个整体判断。
// 分散到各处去改 hidden,必然出现两个都亮着(或者都不亮)的状态。
function applyOpenUI() {
  els.pwInput.hidden = !state.passwordSet;

  // 远控开着**而且**画面在:这一排按钮完全归控制权那套状态机管
  // (applyCtlState / setControlling),这里只把"请求开启"收起来。
  //
  // 注意"而且画面在"这半句不能省。远控开着但主机没在推流的时候,屏幕上
  // 一片黑,而观众手上得有东西可点 —— 那个东西就是"请求开启(共享)"。
  if (state.remoteEnabled && state.sharing) {
    els.openBtn.hidden = true;
    return;
  }

  // 要么远控没开、要么画面还没起来。两种情况观众唯一的出路都是"请求开启":
  // 远控没开是要开远控,没画面是要开共享(见主机端 requestOpen —— 这两种
  // 它都受理)。
  els.ctlBtn.hidden = true;
  // 请求已经发出去了就不该再点一次 —— 那个位置让给「取消请求」。
  els.openBtn.hidden = state.openPending;
  els.releaseBtn.hidden = !state.openPending;
  els.releaseBtn.textContent = '取消请求';
}

function applyOpenState(s, reason) {
  switch (s) {
    case 'pending':
      state.openPending = true;
      setStatus('等待主机同意…', 'badge-warn');
      els.ctlInfo.textContent = reason || '主机的屏幕上会出现同意提示,请稍候';
      break;

    case 'opened':
      state.openPending = false;
      // 主机同意了。远控此刻是开着的、共享也开始了 —— 本页接下来就是
      // 原来那套"看画面 + 申请控制"。
      //
      // **先**把 remoteEnabled 置上再动按钮,否则下面的 setControlling
      // 会被 applyOpenUI 当成"远控没开"而拒绝显示控制中那套界面。
      state.remoteEnabled = true;
      state.sharing = true;
      setStatus('正在连接画面…', 'badge-idle');
      // 按钮交给控制权那套状态机:紧跟其后多半会来一条 ctl:granted
      // 把它变成"控制中";没来(有人在控制)的时候,这里先摆出"申请控制"。
      applyCtlState('idle', reason || '');
      startVideoWithRetry();
      break;

    case 'denied':
      state.openPending = false;
      setStatus('未开启', 'badge-idle');
      els.ctlInfo.textContent = reason || '主机拒绝了这次请求';
      break;

    case 'failed':
      state.openPending = false;
      setStatus('开启失败', 'badge-error');
      els.ctlInfo.textContent = reason || '';
      showError(reason || '主机同意之后没能开启。');
      // 共享那一半可能成了(v0.1 起远控失败也照样开始共享,见
      // internal/server/remote.go 的 approveOpen)—— 那就先把画面接上,
      // 能看总比黑着强。
      if (/共享已开始/.test(reason || '')) startVideoWithRetry();
      break;

    case 'idle':
      state.openPending = false;
      els.ctlInfo.textContent = reason || '';
      break;
  }
  applyOpenUI();
}

// startVideoWithRetry 在主机刚同意之后接画面。
//
// **必须重试。** "主机点了同意"和"MediaMTX 里真的有一条流"之间隔着 ffmpeg
// 起来、编码器探测、推上去这几步,实测要一两秒。只拉一次的话,那几秒里页面
// 会停在"拿不到画面" —— 而用户刚看到主机同意了,这是最容易被当成"点了没用"
// 的一刻。
async function startVideoWithRetry() {
  const delays = [0, 500, 1000, 1500, 2000, 2500];
  let lastErr = null;

  for (const d of delays) {
    if (d) await new Promise((r) => setTimeout(r, d));
    // 期间主机可能又把远控关了(或者用户自己跳走了),那就别再试了。
    if (state.closedByUs) return;
    try {
      await startVideo(state.cfg);
      promptUnmute();
      return;
    } catch (e) {
      lastErr = e;
      setStatus('正在连接画面…', 'badge-idle');
    }
  }

  fail('拿不到画面', String((lastErr && lastErr.message) || lastErr));
}

// promptUnmute 在这条路上补一次"点一下才有声音"。
//
// 正常进来的时候这一步由遮罩上的「开始观看」承担 —— 浏览器要求有用户操作
// 才允许播放声音。而这条路是主机那边点出来的,用户自己那一下还没发生,
// 不补的话画面回来了却没有声音,而页面上没有任何地方解释为什么。
function promptUnmute() {
  if (!els.video.muted) return;
  els.overlay.hidden = false;
  els.overlayMsg.textContent = '画面已接上';
  els.overlayHint.textContent = '点「开始观看」把声音打开。';
  els.startBtn.hidden = false;
}

function applyCtlState(s, reason) {
  // 远控没开的时候收到控制权状态,只可能是一次被拒的旧申请(服务端会回
  // "主机还没有开启远程控制")。那时候这一排按钮归 applyOpenUI 管,这里
  // 别去动它们 —— 动了就会出现两个按钮都亮着。
  if (!state.remoteEnabled) {
    els.ctlInfo.textContent = reason || '';
    return;
  }

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

// setHeld 处理"主机自己在用键鼠,注入让路"。
//
// 主机一碰键鼠,他那边的注入就停了(见 remotectl 的 injectLoop)。观众这边
// 看到的是"我点什么都没反应"—— 不说明的话那是个彻头彻尾的谜,而且很容易
// 被当成连接断了。停多久由主机那边定(他停下 1 秒就恢复),所以这里只做
// 两件事:把状态说出来,以及把本地那份"按着"的记录清掉。
function setHeld(on) {
  if (state.held === on) return;
  state.held = on;

  if (on) {
    // 主机那边已经把按着的东西补过抬起了(服务端 releaseHeld)。本地这份
    // 记录跟着清掉,免得恢复之后补发一个多余的抬起 —— 那会松开主机刚刚
    // 自己按下的东西。
    releaseAllButtons();
    releaseAllKeys();
    if (state.controlling) {
      setStatus('主机正在操作…', 'badge-warn');
      els.ctlInfo.textContent = '主机正在操作,你的输入暂时不会生效';
    }
    return;
  }

  if (state.controlling) {
    setStatus('控制中', 'badge-warn');
    els.ctlInfo.textContent = '你正在控制这台电脑';
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

  // 控制权变了,让路的提示就作废了 —— 不管是刚拿到还是刚失去。
  // 放在这一行**之后**:setHeld 要按新的值决定要不要动那行状态文字。
  setHeld(false);

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

// 请求开启共享与远控。密码同样可以跳过主机批准 —— 和上面那个框是同一个
// 暗号,主机端那边两条路共用一套校验(见 remotectl/open.go)。
els.openBtn.addEventListener('click', () => {
  // 立刻反映出去,不等服务端回话:这一下去的是网络请求,而按钮要马上
  // 变成"取消请求",否则用户会以为没点上、再点一次。
  state.openPending = true;
  setStatus('等待主机同意…', 'badge-warn');
  els.ctlInfo.textContent = '主机的屏幕上会出现同意提示,请稍候';
  applyOpenUI();
  send({ t: 'open', act: 'request', pw: els.pwInput.value });
});

els.releaseBtn.addEventListener('click', () => {
  // 同一个按钮,两种处境:远控没开的时候撤的是"开启请求",开着的时候
  // 归还的是控制权。服务端也是按消息类型分两条路走的,这里分错了就是
  // "点了取消但什么都没发生"。
  if (!state.remoteEnabled) {
    send({ t: 'open', act: 'release' });
    return;
  }
  send({ t: 'ctl', act: 'release' });
});

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
