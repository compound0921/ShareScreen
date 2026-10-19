# ShareScreen

Windows 屏幕共享:本机采集画面(可选桌面声音),别人用浏览器打开链接就能看,不用装客户端。

发布件是一个文件夹:程序本体、`tools/` 和 `使用说明.txt`,解压后保持原样即可运行。

## 运行

双击 `ShareScreen.exe`,浏览器打开控制页,托盘出现图标。

| 托盘操作 | 效果 |
|---|---|
| 双击图标 | 打开控制页 |
| 右键 → 开始 / 停止共享 | 不切浏览器 |
| 右键 → 退出 | 停止并退出 |

**关控制页 = 关程序**(约 3 秒后退出,刷新不算关闭)。远控开着时例外,请走托盘退出。
用 `-no-browser` 启动时,程序留在托盘里待着。

日志:`%LOCALAPPDATA%\ShareScreen\sharescreen.log`。

### 端口被占用

已有 ShareScreen 在跑:打开它的控制页后退出;自家残留的 ffmpeg / mediamtx:
自动清理;其他程序占用本地端口:询问是否换端口;WebRTC 或媒体端口被占:说明原因后退出。

## 采集

| 选项 | 说明 |
|---|---|
| **屏幕(WGC 采集,推荐)** | 默认。走 DWM 合成层,GPU 采集打不开的机器也能用;只采主显示器,需要 Windows 10 1903+ |
| 屏幕(GPU 采集) | DXGI 桌面复制,CPU 占用最低;个别显卡/驱动上抓不到画面 |
| 屏幕(兼容模式) | 任何 Windows 都能用,CPU 占用更高 |
| 单个窗口 | 窗口被遮挡时,画面也会被遮挡物盖住 |
| OBS 虚拟摄像头 | 只有画面,声音来自桌面音频 |
| OBS 直推 | 画面和声音都由 OBS 提供 |

### 声音

默认开启,抓播放设备的回环,换耳机、插 HDMI、切蓝牙会自动跟随。系统默认设备不一定
是有声音的那台,听不到就换「采集哪台设备的声音」。采不到时退化为无声画面,不会
导致共享起不来。

观看链接末尾的 `?muted=0` 不能丢,否则播放页默认静音;程序生成的链接已带好。

### 参数

默认 `原始分辨率 / 60fps / 3 Mbps + 96kbps 音频`。帧率只提供 **30 / 60**。
界面会按上行带宽估算能支撑几个观众。改参数会重启采集,观众侧黑屏约 1–2 秒。

### OBS 直推

采集源选「OBS 直推」,复制页面上的地址;OBS → 设置 → 直播,服务选 **WHIP**,
地址填 `http://127.0.0.1:8889/live/whip`,点「开始推流」。

必须用 WHIP,不能用 RTMP(浏览器 WebRTC 不认 AAC)。需要 OBS 29+。

## 观看

把「观看地址」发给对方,浏览器打开即可。**链接本身就是凭据**,不要发到公开场合。

## 远程控制

默认「接受请求开启共享与远控」是开的:拿着链接的人可以请求,你在右下角弹窗点
「允许」后才开始,不点一分钟作废。不想保留这个入口就取消勾选,程序不再监听远控端口。

要主动开放:勾选「允许远程控制」,把「可控制链接」发给对方,对方点「申请控制」,
你批准后即可操作。

**控制密码**是捷径:填对密码直接拿到控制权,不用每次批准。新装默认 `123456`;
清空密码即关闭这条捷径;密码以明文存储和传输,只当临时暗号用。

边界:

- 只有整屏采集能远控;窗口采集和 OBS 不行。
- 远控期间画面里不显示主机光标,切换会重启采集(黑屏几秒)。
- 你在用键鼠时对方操作自动让开,停手 1 秒后恢复。
- 锁屏界面、UAC 弹窗、Ctrl+Alt+Del、管理员窗口无法控制。
- 剪贴板同步是单独开关,默认关闭。
- 远控默认关闭;「接受请求开启共享与远控」默认开启,是唯一默认绑 `0.0.0.0` 的功能。
- 传输是明文的,不要在不可信网络里开启。

## 公网访问

### 自动(UPnP)

控制页勾选「自动配置路由器端口映射(UPnP)」。程序会建好映射并填入公网地址,
公网 IP 变化后自动跟上。仍要放行 Windows 防火墙的 `8889/tcp` 和 `8189/udp`;
开远控时再加 `8090/tcp`。

UPnP 不可用(路由器关闭、运营商级 NAT、双重 NAT)时改走手动路线或 VPS 中转。

```bash
go run ./cmd/portmapcheck          # 只读
go run ./cmd/portmapcheck -map     # 试建一次映射,试完删除
```

### 手动

| 名称 | 协议 | 外部端口 | 内部端口 |
|---|---|---|---|
| ShareScreen-WHEP | TCP | 8443 | 8889 |
| ShareScreen-ICE | **UDP** | 8189 | 8189 |
| ShareScreen-RemoteControl | TCP | 8090 | 8090 |

- UDP `8189` 内外端口必须一致;
- **不要映射 1935**,否则任何人都能覆盖你的画面;
- 远控不开时不要加最后一条;
- 同时放行 Windows 防火墙对应端口。

然后在控制页「公网地址」里选路由器报告的地址;用 DDNS 时填 `域名:8443`。
最后用手机 4G(关 WiFi)验证公网链路。

## 端口

| 用途 | 端口 |
|---|---|
| 控制页 | 8080/tcp(localhost) |
| 远控 WebSocket | 8090/tcp |
| WHEP 拉流 | 8889/tcp |
| ICE 媒体 | 8189/udp |
| RTMP / SRT / RTSP 推流入口 | 1935/tcp、8890/udp、8554/tcp |

推流入口只绑 `127.0.0.1`,不要映射到公网。

## 停止

托盘右键 → 退出,或关闭控制页。ffmpeg、MediaMTX 和 WGC 采集进程会一起退出。

## 构建与发布

需要 Go 1.27+ 和 Rust 工具链(`cargo`)。准备组件:

| 组件 | 来源 | 放成 |
|---|---|---|
| ffmpeg | [gyan.dev](https://www.gyan.dev/ffmpeg/builds/) `ffmpeg-release-essentials.zip` | `tools/ffmpeg/bin/ffmpeg.exe` |
| MediaMTX | [GitHub Releases](https://github.com/bluenviron/mediamtx/releases) | `tools/mediamtx/mediamtx.exe` |
| wgc-capture | 本项目 `wgc-capture/` | `tools/wgc-capture.exe` |

ffmpeg 压缩包里的 `ffplay`、`ffprobe`、`doc/` 用不到,可以删。

体积优化(可选):`upx --best --lzma` 压缩三个组件,约 101 → 27 MB、54 → 23 MB、
411 KB → 147 KB。UPX 不影响硬件编码,但可能触发个别杀软误报。

```bash
cargo build --release --manifest-path wgc-capture/Cargo.toml
cp wgc-capture/target/release/wgc-capture.exe tools/wgc-capture.exe
go build -ldflags "-H=windowsgui -s -w" -o sharescreen.exe .
```

`wgc-capture/target` 约 700 MB,发布不需要;可用
`cargo clean --manifest-path wgc-capture/Cargo.toml` 释放。

打发布包:

```bash
go run ./cmd/genrelease -version 1.7.1
```

产出 `dist/ShareScreen-1.7.1.zip`(约 53 MB),包含 `ShareScreen.exe`、
`使用说明.txt` 和 `tools/` 下的三个组件。整个 zip 发给对方,解压后双击即可;
目标机器不需要装 Go 或 Rust。
