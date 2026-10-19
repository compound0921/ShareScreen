//! 最小命名管道服务端。
//!
//! 只做三件事:建管道、等 ffmpeg 连上来、写整帧。字节流没有帧边界,
//! 由 ffmpeg 的 rawvideo 按 `-video_size` 固定切分。

use std::ptr::null_mut;

use windows_sys::Win32::Foundation::{
    CloseHandle, ERROR_BROKEN_PIPE, ERROR_NO_DATA, ERROR_PIPE_CONNECTED, ERROR_PIPE_NOT_CONNECTED,
    GetLastError, HANDLE, INVALID_HANDLE_VALUE,
};
use windows_sys::Win32::Storage::FileSystem::{
    FILE_FLAG_FIRST_PIPE_INSTANCE, PIPE_ACCESS_OUTBOUND, WriteFile,
};
use windows_sys::Win32::System::Pipes::{
    ConnectNamedPipe, CreateNamedPipeW, PIPE_REJECT_REMOTE_CLIENTS, PIPE_TYPE_BYTE, PIPE_WAIT,
};

pub enum WriteError {
    /// 对端关闭(ffmpeg 停止)—— 正常收尾,不是故障。
    Broken,
    Other(u32),
}

pub struct Pipe {
    handle: HANDLE,
}

impl Pipe {
    /// 创建一个唯一实例的输出管道。`name` 形如 `\\.\pipe\sharescreen-wgc-...`。
    pub fn create(name: &str) -> Result<Self, String> {
        let mut wide: Vec<u16> = name.encode_utf16().collect();
        wide.push(0);

        let handle = unsafe {
            CreateNamedPipeW(
                wide.as_ptr(),
                PIPE_ACCESS_OUTBOUND | FILE_FLAG_FIRST_PIPE_INSTANCE,
                PIPE_TYPE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS,
                1,
                1024 * 1024,
                1024 * 1024,
                0,
                std::ptr::null(),
            )
        };
        if handle == INVALID_HANDLE_VALUE {
            return Err(format!("CreateNamedPipeW({name}) 失败: {}", unsafe {
                GetLastError()
            }));
        }
        Ok(Self { handle })
    }

    /// 阻塞等待客户端(ffmpeg)连接。
    pub fn connect(&self) -> Result<(), String> {
        let ok = unsafe { ConnectNamedPipe(self.handle, null_mut()) };
        if ok != 0 {
            return Ok(());
        }
        let err = unsafe { GetLastError() };
        // 同步模式下客户端可能在 ConnectNamedPipe 前就连上了。
        if err == ERROR_PIPE_CONNECTED {
            return Ok(());
        }
        Err(format!("ConnectNamedPipe 失败: {err}"))
    }

    /// 写完整一帧。大帧会分多次 WriteFile。
    pub fn write_all(&self, data: &[u8]) -> Result<(), WriteError> {
        let mut offset = 0usize;
        while offset < data.len() {
            let remaining = data.len() - offset;
            let chunk = remaining.min(u32::MAX as usize) as u32;
            let mut written = 0u32;
            let ok = unsafe {
                WriteFile(
                    self.handle,
                    data.as_ptr().add(offset),
                    chunk,
                    &mut written,
                    null_mut(),
                )
            };
            if ok == 0 {
                let err = unsafe { GetLastError() };
                if err == ERROR_BROKEN_PIPE
                    || err == ERROR_NO_DATA
                    || err == ERROR_PIPE_NOT_CONNECTED
                {
                    return Err(WriteError::Broken);
                }
                return Err(WriteError::Other(err));
            }
            if written == 0 {
                return Err(WriteError::Other(0));
            }
            offset += written as usize;
        }
        Ok(())
    }
}

impl Drop for Pipe {
    fn drop(&mut self) {
        unsafe {
            CloseHandle(self.handle);
        }
    }
}
