# Stage 0 验证手册

> 目标:**不写任何代码**,用现成组件跑通「屏幕 → ffmpeg → MediaMTX → 浏览器」完整链路。
> 预计耗时:30 分钟(含下载)
>
> 这一步是整个项目的可行性验证。**不通就不要往下走。**

> 📌 **这是一份历史记录。** 手册里的命令用的是 RTMP + 纯画面 —— 那正是 Stage 0
> 当时的形态。程序实现之后推流改成了 **RTSP**,并加上了**桌面音频**(为什么非改
> 不可,见[架构设计 §5.5](架构设计.md))。整条链路的步骤和排查思路仍然有效,
> 只是命令里的输出段要相应替换成 `-f rtsp -rtsp_transport tcp`。
> 日常使用直接看 [README](../README.md) 就行,不需要走这份手册。

---

## 为什么要先做这一步

架构设计里选的每一个组件(ffmpeg 采集、NVENC 编码、MediaMTX 分发)都是成熟方案,但**它们组合起来在你的这台机器上能不能跑通,只有实测才知道**。

Stage 0 一次验证掉四件事:

1. 本机有没有可用的硬件 H.264 编码器
2. 采集源(屏幕 / 窗口 / OBS)能不能正常抓取
3. MediaMTX 生成的流能不能被浏览器播放
4. 局域网其他设备能不能正常拉流(防火墙 + ICE 候选)

这四件事任何一件出问题,都会影响后面的方案。趁早暴露。

---

## 本机已验证结果(2026-10-03)

以下步骤已自动化验证完成,**不必重复**:

| 步骤 | 结果 |
|---|---|
| 1 硬件编码器 | ✅ `h264_nvenc` 可用(另有 qsv / amf / libx264) |
| 2 OBS 虚拟摄像头 | ✅ 可枚举,设备名 `OBS Virtual Camera` |
| 3 MediaMTX 启动 | ✅ v1.21.1,WebRTC 监听 :8889 |
| 4 推流(30fps 与 60fps) | ✅ `speed=0.996x`,`drop=0`,码率与设定一致 |
| 链路连通 | ✅ MediaMTX API 返回 `ready:true`,`tracks:["H264"]`,H.264 High / Level 3.2 |
| 5 本机观看画面 | ⬜ **需你手动确认**(浏览器渲染无法自动化) |
| 6 局域网观看 | ⬜ **待你手动确认** |

**本机配置:** RTX 5060 Laptop + Intel UHD,桌面 2560×1600 @ 240Hz。

**仍需你亲自做的只有步骤 5 和 6** —— 因为要亲眼看画面。其余步骤的实测数据已记录在架构设计 §5.4。

---

## 一、准备工作

### 1.1 下载两个组件

| 组件 | 下载地址 | 选哪个文件 |
|---|---|---|
| **MediaMTX** | https://github.com/bluenviron/mediamtx/releases | `mediamtx_vX.X.X_windows_amd64.zip` |
| **ffmpeg** | https://www.gyan.dev/ffmpeg/builds/ | `ffmpeg-release-essentials.zip` |

> ffmpeg 也可以从 https://github.com/BtbN/FFmpeg-Builds/releases 下载。
> **选 `essentials` 而不是 `full`** —— essentials 已包含 nvenc / qsv / amf 全部硬件编码器,体积小得多。

### 1.2 目录结构

解压后按下面摆放:

```
ShareScreen/
├── docs/
│   ├── 架构设计.md
│   └── Stage0-验证手册.md      ← 本文件
├── tools/
│   ├── mediamtx/
│   │   ├── mediamtx.exe
│   │   └── mediamtx.yml
│   └── ffmpeg/
│       └── bin/
│           └── ffmpeg.exe
└── README.md
```

> 把 ffmpeg 解压出来的路径改成 `tools/ffmpeg`(压缩包里通常带版本号,如 `ffmpeg-7.0.2-essentials_build`,重命名掉即可)。
> 后面的命令都假设是这个结构。

### 1.3 确认 MediaMTX 版本

```cmd
tools\mediamtx\mediamtx.exe --version
```

记下版本号。**v1.0 之前的配置项名称不同**,如果你下到的是旧版,配置可能需要调整。

---

## 二、验证步骤

按顺序执行,每步都有预期结果。**哪一步不符合预期就停下来,查第五节的排障表。**

### 步骤 1 —— 检查硬件编码器

```cmd
tools\ffmpeg\bin\ffmpeg.exe -hide_banner -encoders | findstr h264
```

**预期输出**(至少出现一个硬件编码器):

```
 V....D h264_amf             AMD AMF H.264 Encoder
 V....D h264_nvenc           NVIDIA NVENC H.264 encoder
 V....D h264_qsv             H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (Intel Quick Sync Video)
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10
```

**记录下来**:这台机器上实际有哪些。如果是 NVIDIA 卡就用 `h264_nvenc`,Intel 核显用 `h264_qsv`,AMD 用 `h264_amf`,只有 `libx264` 则是纯软编(CPU 占用会明显偏高)。

> 枚举出来不等于能用 —— 步骤 4 实际推流才能确认。

### 步骤 2 —— 检查 OBS 虚拟摄像头(可选)

如果没装 OBS,跳过这步。

```cmd
tools\ffmpeg\bin\ffmpeg.exe -list_devices true -f dshow -i dummy
```

**预期输出**(在 video devices 段落里看到):

```
[dshow @ ...]   "OBS Virtual Camera"
[dshow @ ...]   Alternative name "@device_sw_{...}"
```

同时记下 **Alternative name**,它在设备名有编码问题时能派上用场。

> 这一步同时验证了架构设计 §10 里的 **R1** 风险:ffmpeg 能否枚举到 OBS 虚拟摄像头。
> 注意:这只验证 ffmpeg 的 DirectShow 能看见它,不影响 OBS 走 WHIP 直推那条路。

### 步骤 3 —— 启动 MediaMTX

新开一个命令行窗口(**这个窗口要一直开着**):

```cmd
cd tools\mediamtx
mediamtx.exe
```

**预期输出**(v1.21.1 实际输出,已实测确认):

```
INF MediaMTX v1.21.1, windows, amd64
INF configuration loaded from D:\...\mediamtx.yml
INF [RTSP] started with listeners on :8554 (TCP/RTSP), :8000 (UDP/RTP), :8001 (UDP/RTCP)
INF [RTMP] started with listener on :1935 (TCP/RTMP)
INF [HLS] started with listener on :8888 (TCP/HTTP)
INF [WebRTC] started with listeners on :8889 (TCP/HTTP), :8189 (UDP/ICE)
INF [SRT] started with listener on :8890 (UDP/SRT)
INF [MoQ] started with listeners on :8892 (TCP/HTTP2), :8892 (UDP/HTTP3), :8893 (UDP/QUIC)
INF [API] started with listener on :9997 (TCP/HTTP)
```

> ⚠️ **WebRTC 在 8889,HLS 才是 8888。** 这两个端口号只差一位,是本手册最容易搞错的地方。
> 所以播放地址是 `http://localhost:8889/live`,**不是 8888**。

**记下 WebRTC 那一行**。没有的话说明配置里 `webrtc` 被关了。

> 首次启动会看到一条 `WAR [MoQ] certificate auto.key not found, generating it from scratch` —— 这是 MediaMTX v1.21 新增的 MoQ 协议自动生成自签证书,与我们的 WebRTC 无关,可以忽略。

### 步骤 4 —— 推流

**再开一个新命令行窗口**,执行(抓整个屏幕,60fps):

```cmd
tools\ffmpeg\bin\ffmpeg.exe -f lavfi -i "ddagrab=framerate=60" ^
  -vf "hwdownload,format=bgra,scale=1280:800" ^
  -fps_mode cfr ^
  -c:v h264_nvenc -preset p4 -tune ll -rc cbr ^
  -b:v 3M -maxrate 3M -bufsize 3M ^
  -g 60 -bf 0 -pix_fmt yuv420p ^
  -f flv rtmp://127.0.0.1:1935/live
```

> **两个关键点:**
> - `-fps_mode cfr` **不能省** —— 不加的话帧率会从 60 漂到 57–58
> - 分辨率按桌面比例填:2560×1600 的桌面用 `scale=1280:800`;1920×1080 用 `scale=1280:720`

**没有 N 卡**就把 `-c:v h264_nvenc -preset p4 -tune ll` 换成 `-c:v libx264 -preset ultrafast -tune zerolatency`。

**ddagrab 不可用时**改成 gdigrab 输入:

```cmd
  -f gdigrab -framerate 60 -i desktop -vf "scale=1280:800" -fps_mode cfr ^
```

**预期输出**(持续滚动的统计行):

```
frame=  600 fps= 60 q=24.0 size=  3666KiB time=00:00:10.00 bitrate=3002.9kbits/s dup=14 drop=0 speed=0.996x
```

**关键观察点:**

| 现象 | 含义 |
|---|---|
| `fps= 60` 稳定 | 采集正常 |
| `drop=0`(或个位数) | 无丢帧;持续增长说明跟不上 |
| `speed=0.996x` 附近 | 实时;明显低于 1x 说明编码器吃力 |
| `dup=` 有值 | 正常 —— 屏幕静止时复制帧补齐 60fps |
| `bitrate` 在 3000kbits/s 附近 | 码率控制生效 |
| 报错 `Cannot load nvcuda.dll` | 没装 N 卡驱动,换编码器 |

**这个窗口也要一直开着。** ffmpeg 是持续运行的进程。

同时切回 MediaMTX 窗口,应该能看到:

```
INF [RTMP] [conn ...] opened
INF [path live] [RTMP source] created
```

### 步骤 5 —— 本机观看

浏览器打开:

```
http://localhost:8889/live
```

**预期**:MediaMTX 自带的播放器页面,几秒内出现实时画面。

**这是 Stage 0 的核心验收点。** 画面出现 = 采集、编码、传输、播放四段全部打通。

### 步骤 6 —— 局域网观看

1. 查本机内网 IP:
   ```cmd
   ipconfig | findstr IPv4
   ```
   找到形如 `192.168.x.x` 的地址(注意排除 `169.254.x.x` 这种无效地址)。

2. 用**另一台设备**(手机 / 另一台电脑,连同一个 WiFi)打开:
   ```
   http://192.168.x.x:8889/live
   ```

**预期**:同样出画面。

**如果失败** —— 这是最可能出问题的一步,按顺序排查:

| 检查项 | 操作 |
|---|---|
| 防火墙 | 放行入站 `8889/TCP` 和 `8189/UDP`(见下方命令) |
| 是否同网段 | 两台设备的 IP 前三段应相同 |
| 路由器隔离 | 部分路由器开了「AP 隔离」,禁止无线设备互访 |
| ICE 候选 | MediaMTX 日志里看候选地址;必要时在配置里加 `webrtcAdditionalHosts` |

**放行防火墙(需管理员权限的命令行):**

```cmd
netsh advfirewall firewall add rule name="ShareScreen WHEP" dir=in action=allow protocol=TCP localport=8889
netsh advfirewall firewall add rule name="ShareScreen ICE" dir=in action=allow protocol=UDP localport=8189
```

---

## 三、验收清单

全部通过才算 Stage 0 完成。

- [ ] 步骤 1:枚举出至少一个 H.264 编码器
- [ ] 步骤 2:(如装了 OBS)枚举到 OBS Virtual Camera
- [ ] 步骤 3:MediaMTX 启动,日志中有 WebRTC listener
- [ ] 步骤 4:ffmpeg 推流,`fps=60` / `speed≈1.00x` / `drop=0`,无报错
- [ ] 步骤 5:**本机浏览器看到实时画面**
- [ ] 步骤 6:**局域网其他设备看到实时画面**
- [ ] 记录实际延迟(见第四节)

### 额外验证(可选,但建议做)

- [ ] 切换采集源:窗口(`-i title="窗口标题"`)、OBS 虚拟摄像头(`-f dshow -i video="OBS Virtual Camera"`)
- [ ] 上调码率到 6M,观察是否仍然 `speed≈1.00x`
- [ ] 用任务管理器观察 ffmpeg 的 CPU 占用(NVENC 应该很低;libx264 会吃掉一个核)

---

## 四、记录结果

```
测试日期:        2026-10-03
MediaMTX 版本:   v1.21.1 windows amd64
ffmpeg 版本:     9.0.2 essentials
显卡:            NVIDIA RTX 5060 Laptop + Intel UHD
桌面:            2560×1600 @ 240Hz

可用编码器:      ☑ nvenc  ☑ qsv  ☑ amf  ☑ libx264
默认选用:        h264_nvenc

OBS 虚拟摄像头可枚举:  ☑ 是
OBS 设备名:      OBS Virtual Camera
OBS 替代名:      @device_sw_{860BB310-5D01-11D0-BD3B-00A0C911CE86}\{A3FCE0F5-3493-419F-958A-ABA1250EC20B}

采集方式:        ddagrab(首选)/ gdigrab(兜底)
                 ↑ 这是 Stage 0 当时的取舍;后来程序改成默认 gdigrab(见
                   架构设计 §5.1),因为默认值要的是"一定能出画面"
gdigrab 30fps 实测:  25fps,drop=19  ← 不可用
ddagrab 30fps 实测:  29–30fps,drop=1
ddagrab 60fps 实测:  60.0fps(需 -fps_mode cfr),drop=0

局域网拉流:      ⬜ 待确认
需要额外放行防火墙: ⬜ 待确认

画布延迟(步骤5,主观估计):  ⬜ 待确认
CPU 占用(任务管理器,ffmpeg 进程):  ⬜ 待确认

遇到的问题:
  1. WebRTC 默认端口是 8889 而非 8888(文档已修正)
  2. gdigrab 性能不达标,已改用 ddagrab
  3. 不加 -fps_mode cfr 时帧率漂移到 57–58
  4. scale_d3d11 / hwmap→cuda 均不可用,GPU 缩放走不通
```

### 延迟怎么估

在屏幕上放一个秒表(网页版即可),让它出现在共享画面里,**用手机拍一张同时包含真实秒表和浏览器画面的照片**,两者的时间差就是端到端延迟。

局域网下应该 < 500ms。这个数字加上公网传输延迟,就是最终体验。

---

## 五、排障表

### 步骤 1 相关

| 现象 | 原因 | 处理 |
|---|---|---|
| 只列出 `libx264` | 无独显 / 核显驱动缺失 | 先确认显卡型号;Intel 核显需装驱动才有 QSV |
| 命令报错 `findstr` 不是内部命令 | 用了 PowerShell | PowerShell 里改用 `Select-String`,或直接不带管道运行 |

### 步骤 4 相关

| 现象 | 原因 | 处理 |
|---|---|---|
| `Cannot load nvcuda.dll` | N 卡驱动未装或过旧 | 更新显卡驱动,或换 `h264_qsv` / `libx264` |
| `Error opening input ... gdigrab` | 桌面捕获被拒 | 确认不是运行在无桌面会话(RDP 断开会话可能失败) |
| `speed=0.3x` 之类远低于 1 | 编码器跟不上 / CPU 满载 | 换硬件编码器;降低分辨率或帧率 |
| 画面尺寸不对 | 高 DPI 缩放 | 用 `-vf scale=1280:720` 强制输出尺寸 |
| 抓到的是黑屏 | 多显示器 / 会话问题 | 试试 `-i desktop` 换成指定区域:`-i "desktop"` 配合 `-offset_x` 等参数 |

### 步骤 5 相关

| 现象 | 原因 | 处理 |
|---|---|---|
| 页面打不开 | MediaMTX 没起或端口占用 | 检查 MediaMTX 窗口是否还在;`netstat -ano \| findstr 8889` |
| 帧率只有 57–58 而非 60 | 漏了 `-fps_mode cfr` | 补上该参数(见步骤 4) |
| `hwdownload` 报错或花屏 | ddagrab 帧格式问题 | 确认 `format=bgra` 存在;或退回 gdigrab |
| 页面能开,播放器一直转圈 | **最常见的失败** | ① ffmpeg 是否在推流? ② 换 Chrome/Edge 试;③ 看浏览器控制台报错 |
| 画面出来但卡顿 | 码率高于网络承载 | 降码率,或检查是否 `speed` 低于 1x |
| 黑屏无报错 | 编码 profile 不兼容 | §6.4:尝试 `-profile:v main` 或 `-profile:v baseline` |
| 编码器协商失败 | H.264 fmtp 不匹配 | 同上,降 profile;这是架构设计 §10 的 R4 风险 |

### 步骤 6 相关

| 现象 | 原因 | 处理 |
|---|---|---|
| 本机能看,别人不能 | 防火墙 | 执行上面的 `netsh advfirewall` 命令 |
| 防火墙放行后仍不行 | ICE 候选是内网地址 | MediaMTX 配置加 `webrtcAdditionalHosts: [本机内网IP]` |
| 一直转圈 | 8189/UDP 被拦 | 确认 UDP 规则也加了(很多人只加了 TCP) |
| 无线设备之间不通 | 路由器 AP 隔离 | 路由器后台关闭「AP 隔离」/「客户端隔离」 |

**诊断技巧:** 浏览器打开 `chrome://webrtc-internals`,能看到 ICE 候选收集过程和连接状态,是排查 WebRTC 问题最直接的工具。

---

## 六、完成后

Stage 0 通过后:

1. 把记录的结果回填到「架构设计」§10 的风险表(R1 / R2 / R6 等)
2. 如果实测发现编码器或延迟和预期不符,回头调整 §6.3 的默认参数
3. 进入 **Stage 1**:包装成 Go 单二进制程序(见架构设计 §9)

如果步骤 5 或 6 反复失败且找不到原因,**先不要开始 Stage 1** —— 底层链路不通的情况下包装 UI 只会掩盖问题。
