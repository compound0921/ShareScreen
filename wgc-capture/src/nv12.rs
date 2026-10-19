//! BGRA8 -> NV12 转换。
//!
//! 用 BT.601 limited range(和 ffmpeg 默认的 RGB->YUV 行为接近):
//!
//!   Y = 16 + (66R + 129G + 25B + 128) >> 8
//!   U = 128 + (-38R - 74G + 112B + 128) >> 8
//!   V = 128 + (112R - 94G - 18B + 128) >> 8
//!
//! 色度按 2x2 块取 RGB 平均。输入必须是 `windows-capture` 的 BGRA8
//! `as_nopadding_buffer()`(行间无 padding),否则行首会错位。

/// 把一帧 BGRA8 转成 NV12。
///
/// `src` 长度至少 `width * height * 4`;`dst` 必须正好
/// `width * height * 3 / 2`。宽高必须为偶数 —— NV12 的色度平面是 2x2 下采样,
/// 奇数尺寸没有对应的合法布局。
pub fn bgra_to_nv12(
    src: &[u8],
    width: usize,
    height: usize,
    dst: &mut [u8],
) -> Result<(), &'static str> {
    if width == 0 || height == 0 {
        return Err("宽高不能为 0");
    }
    if width % 2 != 0 || height % 2 != 0 {
        return Err("宽高必须为偶数(NV12 色度是 2x2 下采样)");
    }
    let y_size = width * height;
    if src.len() < y_size * 4 {
        return Err("输入缓冲区太小");
    }
    if dst.len() != y_size * 3 / 2 {
        return Err("输出缓冲区大小不是 NV12 帧大小");
    }

    // Y 平面
    for y in 0..height {
        let row = y * width * 4;
        let out = y * width;
        for x in 0..width {
            let i = row + x * 4;
            let b = src[i] as i32;
            let g = src[i + 1] as i32;
            let r = src[i + 2] as i32;
            let yy = ((66 * r + 129 * g + 25 * b + 128) >> 8) + 16;
            dst[out + x] = yy.clamp(0, 255) as u8;
        }
    }

    // UV 平面:每个 2x2 块一个 U、一个 V
    let uv_base = y_size;
    for by in (0..height).step_by(2) {
        for bx in (0..width).step_by(2) {
            let mut r_sum = 0i32;
            let mut g_sum = 0i32;
            let mut b_sum = 0i32;
            for dy in 0..2 {
                for dx in 0..2 {
                    let i = ((by + dy) * width + (bx + dx)) * 4;
                    b_sum += src[i] as i32;
                    g_sum += src[i + 1] as i32;
                    r_sum += src[i + 2] as i32;
                }
            }
            let r = r_sum / 4;
            let g = g_sum / 4;
            let b = b_sum / 4;

            let u = ((-38 * r - 74 * g + 112 * b + 128) >> 8) + 128;
            let v = ((112 * r - 94 * g - 18 * b + 128) >> 8) + 128;

            let uv = uv_base + (by / 2) * width + bx;
            dst[uv] = u.clamp(0, 255) as u8;
            dst[uv + 1] = v.clamp(0, 255) as u8;
        }
    }

    Ok(())
}

/// 不依赖 WGC 的自检:生成黑/白两块,检查 YUV 关键值。
pub fn self_test() -> Result<(), String> {
    let width = 64usize;
    let height = 64usize;
    let mut src = vec![0u8; width * height * 4];
    for y in 0..height {
        for x in 0..width {
            let i = (y * width + x) * 4;
            let v = if x < width / 2 { 0u8 } else { 255u8 };
            src[i] = v;
            src[i + 1] = v;
            src[i + 2] = v;
            src[i + 3] = 255;
        }
    }

    let mut dst = vec![0u8; width * height * 3 / 2];
    bgra_to_nv12(&src, width, height, &mut dst).map_err(|e| e.to_string())?;

    let black_y = dst[0];
    let white_y = dst[width - 2];
    // 白色那一侧的 2x2 块
    let uv = width * height;
    let u = dst[uv + width - 2];
    let v = dst[uv + width - 1];

    if black_y != 16 {
        return Err(format!("black Y = {black_y}, want 16"));
    }
    if !(230..=240).contains(&white_y) {
        return Err(format!("white Y = {white_y}, want ~235"));
    }
    if u != 128 || v != 128 {
        return Err(format!("grey UV = {u}/{v}, want 128/128"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn solid(width: usize, height: usize, b: u8, g: u8, r: u8) -> Vec<u8> {
        let mut src = vec![0u8; width * height * 4];
        for px in src.chunks_exact_mut(4) {
            px[0] = b;
            px[1] = g;
            px[2] = r;
            px[3] = 255;
        }
        src
    }

    #[test]
    fn black_maps_to_16_128_128() {
        let mut dst = vec![0u8; 8 * 8 * 3 / 2];
        bgra_to_nv12(&solid(8, 8, 0, 0, 0), 8, 8, &mut dst).unwrap();
        assert_eq!(dst[0], 16);
        assert_eq!(dst[8 * 8], 128);
        assert_eq!(dst[8 * 8 + 1], 128);
    }

    #[test]
    fn white_maps_to_235_128_128() {
        let mut dst = vec![0u8; 8 * 8 * 3 / 2];
        bgra_to_nv12(&solid(8, 8, 255, 255, 255), 8, 8, &mut dst).unwrap();
        assert!((230..=240).contains(&dst[0]), "white Y = {}", dst[0]);
        assert_eq!(dst[8 * 8], 128);
        assert_eq!(dst[8 * 8 + 1], 128);
    }

    #[test]
    fn red_maps_to_expected_chroma() {
        let mut dst = vec![0u8; 8 * 8 * 3 / 2];
        bgra_to_nv12(&solid(8, 8, 0, 0, 255), 8, 8, &mut dst).unwrap();
        // BT.601: 红 Y≈81, U≈90, V≈240
        assert!((78..=84).contains(&dst[0]), "red Y = {}", dst[0]);
        assert!((88..=92).contains(&dst[8 * 8]), "red U = {}", dst[8 * 8]);
        assert!(
            (238..=242).contains(&dst[8 * 8 + 1]),
            "red V = {}",
            dst[8 * 8 + 1]
        );
    }

    #[test]
    fn rejects_odd_size() {
        let mut dst = vec![0u8; 7 * 8 * 3 / 2];
        assert!(bgra_to_nv12(&solid(7, 8, 0, 0, 0), 7, 8, &mut dst).is_err());
    }

    #[test]
    fn self_test_passes() {
        self_test().unwrap();
    }
}
