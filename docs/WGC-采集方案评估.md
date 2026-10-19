# WGC 采集方案评估

> 状态:**评估中,未实施**。日期:2026-10-19。
> 背景见 [架构设计 §5.1](架构设计.md) 的 T2 真机诊断结论。

## 1. 结论摘要

**WGC(Windows.Graphics.Capture)是诊断机这类"DXGI 输出归属错乱"机器的正确解法**,
因为它走 DWM 合成层,不要求"输出必须归属于创建设备的那块适配器"。DDA 做不到这一点。

但在投入实施前,必须先做一个**时间盒原型**:在诊断机上用 WGC 抓 30 帧并落盘。
如果原型也失败,后续架构讨论全部作废。

推荐形态:**WGC 只负责采集**,把帧交给现有 ffmpeg 编码/推流,不重写编码器和音频链路。

## 2. 为什么 DDA 在诊断机上无解

真机诊断(2026-10-19)确认:

- 桌面由 Intel 核显承载(WMI / `EnumDisplayDevices`);
- `nvidia-smi` 报 NVIDIA `Display Attached: No`、`Display Active: Disabled`;
- DXGI 却把唯一 `attached=1` 的输出报在 NVIDIA 名下,Intel 侧枚举到 0 个输出。

DDA 的语义是"复制当前适配器上正被 DWM 合成的输出":

- 设备建在 NVIDIA 上 → 该输出不承载合成结果 → `DXGI_ERROR_UNSUPPORTED`;
- 设备建在 Intel 上 → 枚举不到输出 → `Failed to enumerate DXGI output`。

**没有任何 ddagrab 参数能改变这个拓扑**。绑定适配器解决的是"选错卡",救不了
"没有可复制的输出"。

## 3. WGC 为什么能绕开

WGC 捕获的是 DWM 合成后的画面。OBS 社区与微软示例的实测结论一致:
**WGC 在混合显卡上可以跨适配器工作**,而 DDA 要求设备必须建在扫描输出所属的
适配器上。

代价是跨适配器时可能发生一次系统内存往返。规避方式:把 WGC 的 D3D11 设备建在
**驱动显示器的那块适配器**上(诊断机是 Intel),采集和编码都在同一块卡上,
走同适配器路径;只有在需要 NVENC 时才跨卡,届时再评估共享纹理方案。

这也解释了为什么"GPU 采集在部分机器上失败、与显卡品牌无关":
问题不在 N 卡或 A 卡,而在该机器的 DXGI 输出归属是否自洽。

## 4. 候选架构

| 方案 | 形态 | 优点 | 缺点 |
|---|---|---|---|
| **A. 只采集** | WGC exe 输出 NV12 原始帧 → 本地管道 → ffmpeg | 复用现有编码器探测/回退、音频合流、参数调优;改动集中在采集段 | 原始帧带宽高;需要帧率补帧 |
| B. 采集 + 编码 | WGC exe 用 Media Foundation 硬件编码 → H.264 → ffmpeg 只做封装 | 带宽低、CPU 占用低 | 要在新模块里重写编码器选择、CBR/低延迟参数,和现有 ffmpeg 参数体系分叉 |
| C. 采集 + 编码 + 推流 | 新模块直接推 RTSP | 链路最短 | 音频(Opus/WASAPI)要一起搬过去,等于重写整个推流层 |

**方案 C 直接否决**:现有音频链路、编码器回退、MediaMTX 配置都是围绕 ffmpeg 建的,
搬走等于重做一个项目。

**方案 B 暂不作为第一步**:它会把"编码器选择"这个已经有实测参数和回退逻辑的
职责复制一份,两边迟早分叉。等 A 跑通、并且确认原始帧带宽是瓶颈之后再考虑。

## 5. 推荐方案 A 的关键设计

### 5.1 进程与数据通路

```
ShareScreen.exe
  ├─ wgc-capture.exe   (Rust, WGC 采集)
  │      └─ 命名管道 / 本地 TCP: NV12 帧
  ├─ ffmpeg.exe        (读管道 + 音频 + 编码 + RTSP)
  └─ MediaMTX.exe
```

- 新模块作为 `tools/wgc-capture.exe` 随发布包分发,Go 主程序仍是纯 Go、无 CGO。
- 主程序用现有 `proc.Job` 管理它,与 ffmpeg 一起启停,避免残留进程。
- 管道格式固定:`NV12`、宽高、帧率;不需要自定义协议。
- 推荐命名管道而不是 TCP:少一层网络栈,权限也更容易收紧。

### 5.2 帧率(必须处理)

WGC 的 `FrameArrived` 只在画面变化时触发。静止桌面不会产生新帧,直接喂给
ffmpeg 的 rawvideo 输入会让输出停滞。

**采集模块必须自己按目标 FPS 定拍**:没有新帧时重复上一帧。这与 ddagrab 的
`dup_frames=true` 语义一致,也是现有 `-fps_mode cfr` 能继续工作的前提。

### 5.3 像素格式与带宽

- WGC 帧通常是 BGRA8(每像素 4 字节)。直接在 GPU 上用 `ID3D11VideoProcessor`
  转成 NV12 再输出。
- 算一笔账(不是实测):1920×1200@60 NV12 ≈ 207 MB/s;2560×1600@60 NV12
  ≈ 368 MB/s。命名管道/内存拷贝可以承受,但要在原型里实测 CPU 占用。
- 如果先输出 BGRA,2560×1600@60 约 983 MB/s(NV12 的 2.7 倍),不建议作为默认。

### 5.4 适配器选择

- WGC 的 D3D11 设备优先建在**驱动显示器的那块适配器**上(复用
  `gpu.PrimaryVendor()` 的判定)。
- 这样采集与编码(QSV/Intel)同卡,避免跨适配器内存往返。
- 如果用户选了 NVENC,帧需要跨卡;先走系统内存下载,后续再评估
  D3D12 共享纹理(`IDXGIResource1::CreateSharedHandle` + `OpenSharedHandle`)。
- 这也是对 WGC 设备创建能力的实测项:设备能否直接建在非默认适配器上。

### 5.5 光标、旋转、多显示器

| 项 | 做法 | 版本门槛 |
|---|---|---|
| 光标 | `GraphicsCaptureSession.IsCursorCaptureEnabled`,与现有 `DrawMouse` 门控一致 | Windows 10 2004(19041) |
| 旋转 | WGC 拿到的是 DWM 合成后的画面,预期不需要像 ddagrab 那样补 180°,但**必须实测** | — |
| 多显示器 | 先只支持主显示器(与 ddagrab 一致);虚拟桌面拼接留到后续 | `HMONITOR=0` 全屏采集需 Windows 10 20H1(19041),且要拼接 |
| 边框 | `IsBorderRequired=false` 只影响屏幕上的提示,不进画面 | 构建 20348+ |

### 5.6 音频与编码

完全不动:音频仍走 WASAPI 回环 → 本地 TCP → ffmpeg;视频仍由 ffmpeg 用现有
编码器参数编码。这样 T2 的编码层修复、编码器探测和回退都继续有效。

## 6. 验证计划(先做,再决定)

### 原型 0:WGC 能不能在诊断机上抓到画面(决定性)

- 最小 Rust 程序:创建 WGC item(主显示器)、抓 30 帧、保存一帧 PNG/BMP。
- 在诊断机上运行,记录:成功/失败、帧数、分辨率、CPU/GPU 占用。
- **通过标准:能连续拿到 30 帧且内容不是全黑。**
- 如果失败,停止 WGC 方案,回到"兼容模式/独显直连"的结论。

### 原型 1:接进现有链路

- WGC → NV12 → 命名管道 → ffmpeg(rawvideo 输入)→ MediaMTX → 浏览器。
- 验证:静态桌面 30fps 不掉帧(补帧逻辑)、浏览器可看、音频同步。
- 与 gdigrab 对照:CPU 占用、端到端延迟。

### 原型 2:边界

- 光标开关、旋转屏、多显示器、HDR、远程控制开启时的光标门控。
- Windows 10 1903/2004/22H2 与 Windows 11 的 API 存在性。

## 7. 版本与兼容性

| 能力 | 最低版本 |
|---|---|
| WGC 命名空间 / `GraphicsCaptureItem` | Windows 10 1803(17134) |
| `IGraphicsCaptureItemInterop::CreateForMonitor` | Windows 10 1903(18362) |
| `IsCursorCaptureEnabled` | Windows 10 2004(19041) |
| `IsBorderRequired` | 构建 20348+ |

项目声明支持 Windows 10 1809+。因此:

- 1809(17763)没有 `CreateForMonitor`,只能走 picker 或直接判定不支持 WGC,
  回退 gdigrab。
- 1903+ 才把 WGC 作为可选采集源。
- 光标开关在 19041 以下不可用,按"始终画光标"处理。

**所有能力都要运行时探测,不能用 `#if` 编译期假设。**

## 8. 风险与开放问题

1. **WGC 在会话 10 / 混合显卡拓扑下是否真的可用** —— 必须由原型 0 证明,这是
   整个方案的生死线。
2. **构建工具链**:引入 Rust(cargo)意味着发布流程多一套工具链;`genrelease`
   要把新 exe 拷进发布包,README 的"只有 Go"前提要改。
3. **进程与管道生命周期**:采集进程崩溃、管道阻塞、ffmpeg 重启时的清理,都要
   按现有 `proc.Job` 的规矩处理。
4. **帧率与延迟**:补帧逻辑和音频时钟的长期漂移需要实测;必要时改成带时间戳的
   帧协议,而不是裸 rawvideo。
5. **跨适配器性能**:如果用户在 Intel 显示的机器上选 NVENC,帧要跨卡;先用内存
   往返,性能不达标再上 D3D12 共享纹理。
6. **受保护内容**:DRM/受保护视频 WGC 会给出黑帧,这是平台限制,需在文档里写明。
7. **依赖与许可**:Rust crate(`windows`、可能的 `windows-capture`)要评估许可
   和维护状态;这不是 Go 依赖,但仍属于项目依赖。

## 9. 工作量估算(粗估,未实测)

| 阶段 | 内容 | 估算 |
|---|---|---|
| 原型 0 | 最小 WGC 抓帧 + 诊断机验证 | 1–2 天 |
| 原型 1 | 管道 + ffmpeg 接入 + 补帧 | 3–5 天 |
| 阶段一 | 主屏采集、光标、启停、发布打包 | 1–2 周 |
| 阶段二 | 旋转/多屏/HDR/性能与跨卡优化 | 1–2 周 |

## 10. 决策建议

1. **先批准原型 0**(1–2 天),在诊断机上验证 WGC 是否真能绕开 DXGI 拓扑错位。
2. 原型 0 通过后,按方案 A 实施;`wgc-capture.exe` 作为新的采集源
   (`screen_wgc`),ddagrab 保留给能用的机器,gdigrab 继续做兼容兜底。
3. 原型 0 之前**不要改主链路**;现有 T2 修复(编码器下载、适配器绑定、错误提示)
   继续保留,它们对 ddagrab 能用的机器仍然有价值。

## 参考

- [IGraphicsCaptureItemInterop::CreateForMonitor](https://learn.microsoft.com/windows/win32/api/windows.graphics.capture.interop/nf-windows-graphics-capture-interop-igraphicscaptureiteminterop-createformonitor)
- [GraphicsCaptureSession](https://learn.microsoft.com/uwp/api/windows.graphics.capture.graphicscapturesession)
- [robmikh/Win32CaptureSample](https://github.com/robmikh/Win32CaptureSample)
