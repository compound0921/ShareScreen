# AGENTS.md

给在这个仓库里干活的 AI agent(以及新来的人)。

这个仓库有一套自己的判据:**凡是在代码或注释里写下过具体数字的地方,都是实测出来的,
不是估的**。改之前先把原地读明白,再动手。

---

## 0. 这是什么

Windows 屏幕共享。本机采集画面(可选桌面音频),别人用浏览器打开链接就能看,不装客户端。
另有一套默认关闭的远程控制(鼠标/键盘/剪贴板)。

**只有 4 个直接依赖,这是设计目标之一,不是巧合。** 加依赖要能说清为什么现有四个不够。

| 依赖 | 干什么 |
|---|---|
| `coder/websocket` | 远控通道。选中它的理由见 `docs/架构设计.md` §12.2 |
| `energye/systray` | 托盘 |
| `huin/goupnp` | UPnP 端口映射 |
| `golang.org/x/sys` | 全部 Win32 调用(不引 CGO) |

各层边界,以及"为什么这么切":

| 层 | 谁做 | 备注 |
|---|---|---|
| 采集 + 编码 | **ffmpeg**(外部 exe) | 本程序只构造参数,`internal/ffmpeg/args.go` |
| 分发 + WebRTC | **MediaMTX**(外部 exe) | 推流走 **RTSP + Opus**,浏览器走 WHEP |
| 控制页 / 信令 / 远控 | 本程序 | **纯 Go、无 cgo、无 C 依赖** |
| 观众端 | 浏览器 | 资源全部 `go:embed`,断网也要能开 |
| 桌面音频 | 本程序 | ffmpeg 采不到,只能自己走 WASAPI 回环,见 `internal/audio` |

模块名 `sharescreen`,`go.mod` 写 `go 1.27.0`,单仓库。

---

## 1. 动手前先读哪一份

| 你想知道 | 读 |
|---|---|
| 用户看到的是什么、为什么这么设计 | [README.md](README.md) |
| 选型、**被否决的选项**、实测数字 | [docs/架构设计.md](docs/架构设计.md) |
| 最近一版改了什么、有哪些已知代价 | [docs/发布说明.md](docs/发布说明.md) |
| 手工跑链路、验收清单、排障表 | [docs/Stage0-验证手册.md](docs/Stage0-验证手册.md) |
| DDNS、端口映射、TLS | [docs/公网部署手册.md](docs/公网部署手册.md) |

排 ffmpeg 问题先去 `docs/架构设计.md` **附录 A「常用命令速查」** —— 各种采集源的输入段、
编码器兜底写法、鉴权写法都在那儿,别自己拼。

> 架构设计里那些「否决:xxx」小节是**结论**,不是历史。想重新引入某个被否决的方案,
> 先读它的否决理由,并说明现在什么变了能让它成立。

---

## 2. 环境与命令

Windows 10 1809+、Go 1.27+、`GOOS=windows`。

编译需要 exe 旁边有 `tools/ffmpeg/bin/ffmpeg.exe` 和 `tools/mediamtx/mediamtx.exe`
(下载地址见 README「准备」)。`tools/`、`*.exe`、`/dist/` 都在 `.gitignore` 里,**不要提交**。

| 目的 | 命令 |
|---|---|
| 编译本程序 | `go build -ldflags "-H=windowsgui -s -w" -o sharescreen.exe .` |
| 全包编译检查 | `go build ./...` |
| 测试 | `go test ./...` |
| 静态检查 | `go vet ./...`(**保持干净**,改完必跑) |
| 本地运行 | `go run .` |
| 只看 ffmpeg 命令 | `go run . -print-args` |
| 打发布包 | `go run ./cmd/genrelease -version <版本>` → `dist/ShareScreen-<版本>.zip` |
| 重生成 exe 图标 | `go run ./cmd/genicon` |
| 排查 UPnP | `go run ./cmd/portmapcheck`(只读)/ `-map`(试建一次再删) |

程序自己的开关(**不是** go 的 flag 语义,是给运行时的):

| 参数 | 作用 |
|---|---|
| `-no-tray` | 不建托盘(调试时省得托盘图标残留) |
| `-no-browser` | 不自动开浏览器 |
| `-print-args` | 打印将执行的 ffmpeg 命令后退出 |
| `-config <路径>` | 换配置文件,默认 `%LOCALAPPDATA%\ShareScreen\config.json` |

**没有 CI、没有 Makefile、没有 golangci 配置,别假设它们存在。** 这个仓库的"检查"就是
`go build ./...` + `go vet ./...` + `go test ./...`,加上真机手测(见 §6)。

> `-H=windowsgui` 不能省,否则每次启动弹控制台黑框;`-s -w` 只去符号表。
> 产物约 8 MB,运行依赖 exe **旁边**的 `tools/`,所以拷走时要连 `tools/` 一起拷。

---

## 3. 改动落点

**这个仓库最容易出错的地方是"只改了一半"** —— 一个语义散在好几个文件里。下表是查这个的。

### 端口

端口**同时**存在于这四处,改一处等于都没改:

- `internal/config/model.go` 的默认值(以及 `Normalize` 的兜底)
- README.md 的端口表
- `docs/架构设计.md` §7.2
- `docs/公网部署手册.md`

当前分配:控制页 HTTP **8080**(localhost)、远控 WebSocket **8090**、WHEP 拉流 **8889**、
ICE 媒体 **8189/udp**、RTMP 推流入口 **1935**、SRT 推流入口 **8890**、RTSP **8554**。

### 配置字段

一个配置字段有**四个面**,要成组改:

1. `internal/config/model.go` 的默认值
2. 同文件 `(*Config).Normalize()` 的兜底(**空值往往也是有意义的状态**,别顺手补默认 ——
   `ControlPassword` 清空就是"关掉这条捷径",`Normalize` 刻意不填回来)
3. 字段上那条注释 —— 里面写的是这个值为什么是这个值
4. `internal/config/model_test.go`

### 其他

| 改了 | 连带要动 |
|---|---|
| 采集源 / 编码参数 | `internal/ffmpeg/args.go` + `args_test.go`,并**更新 `docs/架构设计.md` §5/§6 的实测表** |
| 采集范围(远控坐标) | `ffmpeg.CaptureRect` + `docs/架构设计.md` §12.3 那张表 |
| 编码器选择 | `internal/ffmpeg/priority.go`(三层选择,见 §6.1) |
| MediaMTX 配置 | `internal/mediamtx/` + 架构设计附录 B |
| 控制页前端 | `internal/webui/static/`(`index.html`/`app.css`/`app.js` 是控制页,`rc.*` 是远控页) |
| 图标 | `internal/icon/icon.go` → 跑 `go run ./cmd/genicon`(**`rsrc.syso` 已入库,不用改构建命令**) |

---

## 4. 红线

这些不是风格偏好,踩了就是坏掉。左边是规则,右边是踩了的症状 —— **症状比规则好记**。

### ffmpeg 参数

| 规则 | 踩了的症状 |
|---|---|
| 必须有 `-fps_mode cfr` | 输出帧率稳定漂到 57–58,不是 60 |
| 关键帧间隔 `-g` = FPS × 1 | WebRTC 起播、流切换变慢 |
| 必须有 `-bf 0` | 重排序缓冲,低延迟的头号杀手 |
| **永远不要加 `-multipass`** | 与 `-tune ll` 冲突,**实测直接报错** |
| 输入选项必须在**各自的** `-i` 之前 | ffmpeg 拒绝,或参数被当成输出选项 |
| 所有输入在所有输出选项之前 | 同上 |
| GPU 侧缩放不可用 | `scale_d3d11` / `hwmap=derive_device=cuda` 都报错,只能 `hwdownload` 回 CPU |
| 窗口采集**只能用 gdigrab** | ddagrab 基于 DXGI,物理上不支持单窗口 |
| 音频格式照抄设备的混音格式 | 写死 48k 立体声 → 采蓝牙耳机(44.1k)时出错 |

改完先 `go run . -print-args` 看实际命令,再谈跑没跑通。

### 端口与网络

| 规则 | 为什么 |
|---|---|
| **UDP 8189 内外端口必须一致** | MediaMTX 在 ICE 候选里写死了它,做了端口翻译就连不上 |
| **1935 绝不能映射到公网** | 映射出去 = 任何人都能推流覆盖你的画面 |
| 推流端口绑 `127.0.0.1`,不绑 `0.0.0.0` | 同上,能挡掉绝大多数攻击面 |
| 端口映射的内网地址由**路由表**推导,不能取 `LanHost` | 取过错的那块网卡,路由器回 **402 Invalid Args** |
| 探测到的公网地址**绝不自动填写** | CGNAT 下它会悄悄产出一条打不开的链接,比留空更糟 |
| 每个对外请求都要显式设超时 | 零值 `http.Client` 永不超时,一台半死的路由器能把进程永久挂住 |
| UPnP 退出时**不删**映射 | 崩溃路径删不掉,两种退出行为不一致更糟;残留靠租约过期 |

### 代码

| 规则 | 原因 |
|---|---|
| 启动失败必须走 `reportFatal`,**不要改成 `log.Fatalf`** | 见下方专条 |
| **不要用 `syscall.EADDRINUSE`** 判端口占用 | 实测系统返回 10048,Go 的合成值是 536870914,`errors.Is` 永远 false |
| **不要在持有 `s.mu` 时调 `notify()`**(remotectl) | `popup.go` 依赖这条不变量从 `OnChange` 里调 `Status()`,破了会**自死锁** |
| `handleConfig` 里必须 `next.RemoteControl = old.RemoteControl` | 否则改个帧率就把远控配置冲成 false,顺带清掉 Token |
| `CaptureOptsFor` 是光标门控的**唯一**定义处 | 多处各写一遍会分叉,症状是"光标有时候画、有时候不画" |
| 归一化坐标的分母是 **size-1** | 用 size 会让最右一列、最下一行永远点不到 |
| `INPUT` 结构体 **amd64 下必须 40 字节** | 少 4 字节填充,`dwExtraInfo` 整体错位(已用编译期断言钉死) |
| Win32 结构体**不能 Go 直接映射** | Go 补齐到 4 的倍数,`WAVEFORMATEX` 的 GUID 会偏 4 字节,格式识别永远失败 |
| `SetClipboardData` 成功后**不要 `GlobalFree`** | 内存所有权已归系统。另外剪贴板不用 `LockOSThread`(那是 COM 的要求) |
| 别用 `(*uint16)(unsafe.Pointer(p))` 读 Win32 内存 | `go vet` 的 `unsafeptr` 会报。改成把地址当 `uintptr` 传给 `RtlMoveMemory` |
| 平台差异用**成对文件** `xxx_windows.go` / `xxx_other.go` | 保持非 Windows 也能 `go build`,别用 `runtime.GOOS` 分支代替。现有 12 组:`dialog`、`audio/capture`、`clipboard/clipboard`、`gpu/adapter`、`input/input`、`netport/netport`、`notify/notify`、`proc/job`、`screen/rotation`、`screen/screen`、`tray/menu`、`window/window` |

**`reportFatal` 为什么绕开日志系统**(`main.go:69-84`):日志输出是 `MultiWriter(文件, stderr)`,
而 `MultiWriter` 遇到**第一个**写入错误就返回 —— 日志文件一关,写 stderr 那步永远轮不到,
错误一个字都出不来。这个组合曾让"端口被占用"变成完全静默:双击启动的用户只看到窗口闪一下。
所以它三件事都做:直接写 stderr(绕开 MultiWriter)、照常记日志、**弹一个对话框**
(双击启动没有控制台,不弹框等于没提示)。

---

## 5. 代码约定

1. **注释写"为什么",中文。** 本仓库最强的惯例。凡是踩过的坑,注释里要写清
   **症状 + 原因 + 数字**(例:"实测不加 `-fps_mode cfr`,输出帧率会稳定漂到 57–58")。
   只复述代码在做什么的注释是噪音,不要加;但**有数字的那一句务必保留**。
2. **错误要告诉用户下一步做什么**,不能只描述现象。参考 README「端口被占用时」那一套 ——
   什么时候自动处理、什么时候问、什么时候拒绝并说明原因,都是刻意分的。
3. **有代价的对外动作默认关。** 唯一刻意的例外是 `RemoteControl.RequestEntry`(默认开),
   它和 `ControlPassword` 默认 `123456` 一起,构成"把链接发给谁,谁就能自己开共享和远控"
   这个**已知取舍**。**改这两处必须在提交信息里解释**,不能顺手"修"。
4. **前端无框架、无构建步骤。** `internal/webui/static/` 里是原生 HTML/CSS/JS。
   **不许引 CDN 或任何联网资源** —— 控制页在断网的局域网里也必须能打开。
5. **`internal/*` 之间尽量互不渗透。** 设计原则是"每一层可独立替换",接口清晰优先于少写代码。
   `main.go` 是唯一做全局装配的地方。
6. **ffmpeg 运行时改不了码率/帧率/分辨率** —— 三者都绑在启动参数上。改参数的实现方式就是
   **终止进程再重启**(观众侧黑屏 1–2 秒)。不要为了消掉这 1 秒去引 `zmq` 之类的方案。

---

## 6. 改完怎么验

| 改了什么 | 怎么验 |
|---|---|
| ffmpeg 参数 | `go run . -print-args` 看实际命令,对照 §4 的红线逐条过 |
| 纯逻辑 | `go test ./...`(现有 18 个 `_test.go`,新逻辑尽量补) |
| 采集 / 音频 / 远控 / 托盘 / 弹窗 | **必须在真 Windows 桌面上手测** |

采集、音频回环、GPU 采集、单窗口、OBS 两种接法、远控三档(请求开启 / 逐次批准 / 控制密码)
**在无桌面或沙箱环境里测不了 —— 不要用那种环境的结论下判断**,这一点上"跑过了"和"没跑过"
的区别就是最关键的结论。

**实测数据要写回 `docs/架构设计.md`**(并更新文件顶部的「最后更新」日期)。这个仓库里
带数字的表格就是这么来的,不写回去等于这次验证没发生。

手工验收清单和排障表在 `docs/Stage0-验证手册.md`。

---

## 7. 文档约定

**只改文档也要想清楚改哪一份:**

| 改的是 | 去哪 |
|---|---|
| 用户视角(什么时候会失败、怎么判断) | README.md |
| 原因和取舍、被否决的选项、实测数据 | `docs/架构设计.md` |
| 本次变更 | `docs/发布说明.md` |

- **README.md 是写给"用发布包的人"的,不是贡献者文档。** 构建/发布放在最后面。
  加功能就在用户视角上加一节,写清"什么时候会失败、怎么判断"。语气用"你",说明代价,不吹。
- **`docs/架构设计.md` 要留痕**:方案、否决理由、实测数据,并更新顶部「最后更新」。
  改了接口、端口、编解码参数就更新它。
- **`docs/发布说明.md` 按版本倒序**,格式 `## <版本>` + `### 新增 / 变更 / 修复 / 说明`;
  每条都要写"为什么这样取舍"和**已知代价**。当前最新是 **1.6**。发版单独一条提交。
- **`docs/使用说明.txt` 会被 `genrelease` 转成 UTF-8 BOM + CRLF 再打包**(老记事本只认这个)。
  **改内容即可,别改编码和换行。**

`docs/` 里的文档是中文,代码注释是中文,**提交信息也是中文**。

---

## 8. 提交与分支

- 提交信息**一行**讲清做了什么,需要时用 `模块:动作;动作`。真实例子:

  ```
  托盘:消息循环钉线程;右键菜单补 WM_NULL
  编码器:真探测 + 按显示器所属显卡优先 + 失败回退
  远端可请求开启共享与远控;主机操作时注入让路
  加 1.6 发布说明
  ```

- **不写 `fix:` / `feat:` 前缀。**
- 功能分支用 `Feat/<名字>`。
- **diff 里只留本次改动** —— 不要顺手格式化、重排或改无关文件。

---

## 9. 本机环境已知摩擦

这些是环境问题,**不是代码问题,遇到别去改代码**:

- 仓库在 `D:\GitNotUploaded\ShareScreen`,通常在 DSH 默认工作区之外。`workspace-write`
  沙箱下写文件需要提权,`go build` / `go test` 也会失败(报 `pipe: Access is denied`
  或 go-build 缓存 `Access is denied`)—— 换到放开沙箱的模式再跑。
- git 会因 "dubious ownership" 拒绝执行(目录属主是 Administrators):
  用 `git -c safe.directory='*' ...`,或
  `git config --global --add safe.directory D:/GitNotUploaded/ShareScreen`。
- 用 PowerShell 时不要把原生命令的输出接进管道(`go build ... | Select-Object` 会被沙箱
  判定为命名管道并拒绝),直接跑、看退出码。
