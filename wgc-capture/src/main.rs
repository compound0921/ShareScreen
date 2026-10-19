//! ShareScreen 的 WGC 采集模块。
//!
//! 只负责把主显示器画面采成 NV12 并写进命名管道;编码、音频、推流仍然由
//! ffmpeg 负责。stdout 只输出一条 readiness JSON,其余日志走 stderr。
//!
//! 用法:
//!   wgc-capture.exe --pipe \\.\pipe\<name> [--fps 60] [--cursor 0|1]
//!   wgc-capture.exe --self-test
//!   wgc-capture.exe --version

mod nv12;
mod pipe;

use std::error::Error;
use std::io::{Read, Write};
use std::process;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use windows_capture::capture::{Context, GraphicsCaptureApiHandler};
use windows_capture::frame::Frame;
use windows_capture::graphics_capture_api::InternalCaptureControl;
use windows_capture::monitor::Monitor;
use windows_capture::settings::{
    ColorFormat, CursorCaptureSettings, DirtyRegionSettings, DrawBorderSettings,
    MinimumUpdateIntervalSettings, SecondaryWindowSettings, Settings,
};

use pipe::{Pipe, WriteError};

type BoxError = Box<dyn Error + Send + Sync>;

#[derive(Default)]
struct Shared {
    latest: Mutex<Option<Arc<Vec<u8>>>>,
    width: AtomicU32,
    height: AtomicU32,
    frames: AtomicU64,
    error: Mutex<Option<String>>,
}

struct Handler {
    shared: Arc<Shared>,
    scratch: Vec<u8>,
}

impl Handler {
    fn process(&mut self, frame: &mut Frame) -> Result<(), String> {
        let w = frame.width() as usize;
        let h = frame.height() as usize;
        if w == 0 || h == 0 {
            return Ok(());
        }
        if w % 2 != 0 || h % 2 != 0 {
            return Err(format!("分辨率 {w}x{h} 不是偶数,NV12 不支持"));
        }

        let prev_w = self.shared.width.load(Ordering::Relaxed) as usize;
        let prev_h = self.shared.height.load(Ordering::Relaxed) as usize;
        if prev_w != 0 && (prev_w != w || prev_h != h) {
            return Err(format!(
                "分辨率已变化({prev_w}x{prev_h} -> {w}x{h}),请重启共享"
            ));
        }

        let buf = frame.buffer().map_err(|e| e.to_string())?;
        let raw = buf.as_nopadding_buffer(&mut self.scratch);

        let mut out = vec![0u8; w * h * 3 / 2];
        nv12::bgra_to_nv12(raw, w, h, &mut out)?;

        *self.shared.latest.lock().unwrap() = Some(Arc::new(out));
        self.shared.width.store(w as u32, Ordering::Relaxed);
        self.shared.height.store(h as u32, Ordering::Relaxed);
        self.shared.frames.fetch_add(1, Ordering::Relaxed);
        Ok(())
    }
}

impl GraphicsCaptureApiHandler for Handler {
    type Flags = Arc<Shared>;
    type Error = BoxError;

    fn new(ctx: Context<Self::Flags>) -> Result<Self, Self::Error> {
        Ok(Self {
            shared: ctx.flags,
            scratch: Vec::new(),
        })
    }

    fn on_frame_arrived(
        &mut self,
        frame: &mut Frame,
        control: InternalCaptureControl,
    ) -> Result<(), Self::Error> {
        if let Err(e) = self.process(frame) {
            *self.shared.error.lock().unwrap() = Some(e);
            control.stop();
        }
        Ok(())
    }

    fn on_closed(&mut self) -> Result<(), Self::Error> {
        Ok(())
    }
}

struct Args {
    pipe: Option<String>,
    fps: u32,
    cursor: bool,
    self_test: bool,
    version: bool,
    help: bool,
}

fn usage() {
    println!(
        "wgc-capture {}
用法:
  wgc-capture.exe --pipe \\\\.\\pipe\\<name> [--fps 60] [--cursor 0|1]
  wgc-capture.exe --self-test
  wgc-capture.exe --version

第一阶段只支持主显示器。stdout 输出一条 readiness JSON。",
        env!("CARGO_PKG_VERSION")
    );
}

fn parse_args() -> Result<Args, String> {
    let mut args = Args {
        pipe: None,
        fps: 60,
        cursor: true,
        self_test: false,
        version: false,
        help: false,
    };

    let mut it = std::env::args().skip(1);
    while let Some(arg) = it.next() {
        match arg.as_str() {
            "--pipe" => args.pipe = Some(it.next().ok_or("--pipe 缺少参数")?),
            "--fps" => {
                let v = it.next().ok_or("--fps 缺少参数")?;
                let fps: u32 = v.parse().map_err(|_| "--fps 需要正整数")?;
                if !(1..=240).contains(&fps) {
                    return Err("--fps 需要在 1..240".into());
                }
                args.fps = fps;
            }
            "--cursor" => {
                let v = it.next().ok_or("--cursor 缺少参数")?;
                args.cursor = match v.as_str() {
                    "0" => false,
                    "1" => true,
                    _ => return Err("--cursor 只能是 0 或 1".into()),
                };
            }
            "--no-cursor" => args.cursor = false,
            "--self-test" => args.self_test = true,
            "--version" => args.version = true,
            "-h" | "--help" => args.help = true,
            other => return Err(format!("未知参数: {other}")),
        }
    }
    Ok(args)
}

fn main() {
    match run() {
        Ok(()) => {}
        Err(e) => {
            eprintln!("ERROR: {e}");
            process::exit(1);
        }
    }
}

fn run() -> Result<(), BoxError> {
    let args = parse_args().map_err(|e| -> BoxError { e.into() })?;

    if args.help {
        usage();
        return Ok(());
    }
    if args.version {
        println!("wgc-capture {}", env!("CARGO_PKG_VERSION"));
        return Ok(());
    }
    if args.self_test {
        nv12::self_test().map_err(|e| -> BoxError { e.into() })?;
        println!("self-test OK");
        return Ok(());
    }

    let pipe_name = args
        .pipe
        .ok_or_else(|| -> BoxError { "缺少 --pipe \\\\.\\pipe\\<name>".into() })?;
    let fps = args.fps;

    let shared = Arc::new(Shared::default());
    let monitor = Monitor::primary().map_err(|e| -> BoxError { e.to_string().into() })?;

    let settings = Settings::new(
        monitor,
        if args.cursor {
            CursorCaptureSettings::WithCursor
        } else {
            CursorCaptureSettings::WithoutCursor
        },
        DrawBorderSettings::Default,
        SecondaryWindowSettings::Default,
        MinimumUpdateIntervalSettings::Default,
        DirtyRegionSettings::Default,
        ColorFormat::Bgra8,
        shared.clone(),
    );

    let control =
        Handler::start_free_threaded(settings).map_err(|e| -> BoxError { e.to_string().into() })?;
    eprintln!("capture started, waiting for first frame");

    // 等第一帧:没有它就不能确定真实尺寸,ffmpeg 也无法按固定帧大小读。
    let start = Instant::now();
    loop {
        if let Some(e) = shared.error.lock().unwrap().clone() {
            let _ = control.stop();
            return Err(e.into());
        }
        if shared.frames.load(Ordering::Relaxed) > 0 {
            break;
        }
        if start.elapsed() > Duration::from_secs(10) {
            let _ = control.stop();
            return Err("WGC 10 秒内没有产生第一帧".into());
        }
        thread::sleep(Duration::from_millis(10));
    }

    let width = shared.width.load(Ordering::Relaxed);
    let height = shared.height.load(Ordering::Relaxed);

    let pipe = match Pipe::create(&pipe_name) {
        Ok(p) => p,
        Err(e) => {
            let _ = control.stop();
            return Err(e.into());
        }
    };

    // Manager 只解析这一行 stdout;日志一律走 stderr。
    println!(
        "{{\"event\":\"ready\",\"version\":\"{}\",\"width\":{},\"height\":{},\"fps\":{}}}",
        env!("CARGO_PKG_VERSION"),
        width,
        height,
        fps
    );
    std::io::stdout().flush().ok();
    eprintln!("pipe ready: {pipe_name} ({width}x{height}@{fps})");

    // Manager 停止时先关 ffmpeg,再给本进程 stdin 发 'q'/EOF。
    let stop = Arc::new(AtomicBool::new(false));
    let stop_watch = stop.clone();
    thread::spawn(move || {
        let mut b = [0u8; 1];
        let _ = std::io::stdin().read(&mut b);
        stop_watch.store(true, Ordering::Relaxed);
    });

    if let Err(e) = pipe.connect() {
        let _ = control.stop();
        return Err(e.into());
    }
    eprintln!("ffmpeg connected");

    let period = Duration::from_secs_f64(1.0 / fps as f64);
    let mut next = Instant::now() + period;
    let mut last: Option<Arc<Vec<u8>>> = None;

    let result: Result<(), String> = loop {
        if stop.load(Ordering::Relaxed) {
            break Ok(());
        }
        if let Some(e) = shared.error.lock().unwrap().clone() {
            break Err(e);
        }

        let current = shared.latest.lock().unwrap().clone();
        if let Some(f) = current {
            last = Some(f);
        }
        let Some(frame) = last.clone() else {
            thread::sleep(Duration::from_millis(1));
            continue;
        };

        match pipe.write_all(&frame) {
            Ok(()) => {}
            Err(WriteError::Broken) => break Ok(()),
            Err(WriteError::Other(e)) => break Err(format!("写管道失败: {e}")),
        }

        let now = Instant::now();
        if next > now {
            thread::sleep(next - now);
        } else if now.duration_since(next) > Duration::from_secs(1) {
            break Err("采集跟不上或管道阻塞超过 1 秒".into());
        }
        next += period;

        if control.is_finished() && !stop.load(Ordering::Relaxed) {
            break Err("WGC 采集会话意外结束".into());
        }
    };

    let _ = control.stop();
    drop(pipe);
    result.map_err(|e| -> BoxError { e.into() })
}
