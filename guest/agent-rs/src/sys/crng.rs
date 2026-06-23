// Fork-correctness syscall: RNDADDENTROPY to reseed the kernel CRNG.
//
// This file is the ONLY place that calls the RNDADDENTROPY ioctl.
// Every unsafe block in this file carries a SAFETY comment stating the
// invariant that makes it sound.
//
// Mirrors guest/agent/notifyforked.go reseedCRNGAt exactly:
// - Returns true ONLY when the credited ioctl succeeds.
// - Returns false (fail closed) on any error: empty entropy, open failure,
//   or ioctl failure.
// - Never panics, never unwraps, never logs secret values.

// unsafe_code is permitted in this file via the #[allow(unsafe_code)] on the
// `pub mod sys;` declaration in lib.rs. We do not repeat the allow here
// (clippy flags duplicated attributes).
#![deny(unsafe_op_in_unsafe_fn)]

use std::fs::OpenOptions;
use std::io;
use std::os::unix::io::AsRawFd;

// RNDADDENTROPY ioctl request number.
//
// From linux/random.h:
//   #define RNDADDENTROPY  _IOW( 'R', 0x03, int [2] )
//
// Verified on box1 by compiling a C program that prints
// (unsigned long)RNDADDENTROPY: output was 0x40085203.
//
// The _IOW macro on x86_64 Linux gnu produces:
//   direction=WRITE(1)=bit31..30, size=8(sizeof int[2])=bits29..16,
//   type='R'=0x52, nr=0x03
//   = (1<<30) | (8<<16) | (0x52<<8) | 0x03
//   = 0x40085203
//
// This constant is used only on Linux; the cfg guard prevents it from
// appearing in macOS builds.
#[cfg(target_os = "linux")]
const RNDADDENTROPY: libc::c_ulong = 0x4008_5203;

// rand_pool_info mirrors the kernel struct rand_pool_info (linux/random.h):
//
//   struct rand_pool_info {
//       int entropy_count;   // entropy credited, in bits
//       int buf_size;        // length of buf[] in bytes
//       __u32 buf[];         // the entropy bytes
//   };
//
// Verified field offsets on box1:
//   entropy_count_off=0, buf_size_off=4, sizeof(struct rand_pool_info)=8
//
// repr(C) ensures the struct is laid out identically to the C definition.
// The flexible array member buf[] is not representable in Rust; we pass
// a separate byte slice and point the ioctl at a contiguous heap buffer
// (header + entropy bytes) instead.
#[cfg(target_os = "linux")]
#[repr(C)]
struct RandPoolInfoHeader {
    entropy_count: libc::c_int, // entropy credited, in bits
    buf_size: libc::c_int,      // length of entropy bytes
}

/// Reseed the kernel CRNG via RNDADDENTROPY, reading and writing to
/// `path` (production: `/dev/urandom`).
///
/// Returns `true` ONLY when the credited ioctl succeeds. Returns `false`
/// (fail closed) on any error. Mirrors `reseedCRNGAt` in
/// `guest/agent/notifyforked.go`.
///
/// The fork-correctness contract requires fail-closed: a fork whose CRNG
/// was not creditably reseeded must be reported as failed so the host can
/// reap it rather than serve it sharing siblings' CRNG state.
///
/// Secret entropy bytes are never logged; only counts and error codes appear
/// in stderr output.
///
/// This function is `cfg(target_os = "linux")` because RNDADDENTROPY is a
/// Linux-specific ioctl. On non-Linux targets (macOS CI) it always returns
/// `false`; callers must not treat false as a fatal error in test contexts.
pub fn reseed_crng_at(entropy: &[u8], path: &str) -> bool {
    if entropy.is_empty() {
        return false;
    }

    #[cfg(target_os = "linux")]
    {
        reseed_crng_linux(entropy, path)
    }

    #[cfg(not(target_os = "linux"))]
    {
        let _ = path;
        false
    }
}

/// Reseed the kernel CRNG via RNDADDENTROPY at `/dev/urandom`.
///
/// Thin wrapper over `reseed_crng_at` with the production path.
pub fn reseed_crng(entropy: &[u8]) -> bool {
    reseed_crng_at(entropy, "/dev/urandom")
}

#[cfg(target_os = "linux")]
fn reseed_crng_linux(entropy: &[u8], path: &str) -> bool {
    // Build the kernel buffer: header (8 bytes) followed by the entropy bytes.
    // The layout matches struct rand_pool_info + buf[]:
    //   [entropy_count:i32][buf_size:i32][entropy bytes...]
    // All fields are little-endian on amd64/arm64 (the only targets this runs on).
    let entropy_bits = match entropy.len().checked_mul(8) {
        Some(b) => b,
        None => {
            eprintln!("sys::crng: entropy length overflow");
            return false;
        }
    };
    // entropy_bits must fit in c_int (i32). If not, clamp to i32::MAX which
    // is more bits than any real entropy call will supply; the ioctl will
    // accept it (the kernel caps credited bits at its pool size).
    let entropy_bits_i32 = if entropy_bits > i32::MAX as usize {
        i32::MAX
    } else {
        entropy_bits as i32
    };
    let buf_size_i32 = match entropy.len().try_into() {
        Ok(v) => v,
        Err(_) => {
            eprintln!("sys::crng: entropy slice too large for buf_size field");
            return false;
        }
    };

    // Build the contiguous buffer [header][entropy bytes].
    // SAFETY invariant: we take a raw pointer to buf[0] below; the buffer must
    // be alive for the duration of the ioctl call.
    let header = RandPoolInfoHeader {
        entropy_count: entropy_bits_i32,
        buf_size: buf_size_i32,
    };
    // Compute sizes for the contiguous allocation.
    let header_len = std::mem::size_of::<RandPoolInfoHeader>();
    let total_len = header_len + entropy.len();
    let mut buf: Vec<u8> = Vec::with_capacity(total_len);
    // SAFETY: RandPoolInfoHeader is repr(C) with no padding; casting to a
    // byte slice is sound. The slice length equals size_of::<RandPoolInfoHeader>().
    let header_bytes: &[u8] = unsafe {
        // SAFETY: header is a repr(C) struct with fields int + int (no padding,
        // no pointers, initialized). Converting it to a byte slice of exactly
        // size_of::<RandPoolInfoHeader>() bytes is sound. The slice does not
        // outlive the local `header` binding.
        std::slice::from_raw_parts(
            &header as *const RandPoolInfoHeader as *const u8,
            header_len,
        )
    };
    buf.extend_from_slice(header_bytes);
    buf.extend_from_slice(entropy);

    // Open /dev/urandom (or the test path) for read+write.
    let file = match OpenOptions::new().read(true).write(true).open(path) {
        Ok(f) => f,
        Err(e) => {
            eprintln!("sys::crng: open {path}: {e}");
            return false;
        }
    };

    let fd = file.as_raw_fd();

    // Issue RNDADDENTROPY ioctl.
    // SAFETY:
    // - fd is a valid open file descriptor owned by `file`; `file` is alive
    //   for the duration of this call.
    // - buf.as_ptr() points to the first byte of a live Vec<u8> with
    //   total_len bytes: header (8 bytes, repr(C)) followed by entropy bytes.
    //   The ioctl reads entropy_count and buf_size from the header, then reads
    //   buf_size bytes from buf[]. Our buffer has exactly that many bytes.
    // - The ioctl does not retain the pointer after the call returns.
    // - RNDADDENTROPY = 0x40085203 is the correct request number for this ioctl
    //   on Linux amd64/arm64 (verified against /usr/include/linux/random.h).
    let ret = unsafe {
        libc::ioctl(
            fd,
            RNDADDENTROPY,
            buf.as_ptr(),
        )
    };

    if ret == 0 {
        true
    } else {
        let errno = io::Error::last_os_error();
        eprintln!(
            "sys::crng: RNDADDENTROPY failed (errno {}); reseed NOT credited, \
             reporting failure so the host reaps this fork",
            errno.raw_os_error().unwrap_or(-1)
        );
        false
    }
}

#[cfg(test)]
// Tests in this module use expect, unwrap, and panic for test-assertion
// purposes; allow the production lints that would otherwise block them.
#[allow(
    clippy::expect_used,
    clippy::unwrap_used,
    clippy::panic,
    clippy::indexing_slicing
)]
mod tests {
    use super::*;

    // Tests that run on all platforms (including macOS CI).

    #[test]
    fn empty_entropy_returns_false() {
        // Empty entropy slice must fail closed immediately, before any syscall.
        let result = reseed_crng_at(&[], "/dev/urandom");
        assert!(!result, "empty entropy must return false");
    }

    #[test]
    fn nonexistent_path_returns_false() {
        // A non-existent path must fail closed (open error).
        let result = reseed_crng_at(b"some entropy bytes", "/nonexistent/path/to/dev/urandom");
        assert!(!result, "bad path must return false");
    }

    #[test]
    fn reseed_crng_delegates_to_reseed_crng_at() {
        // reseed_crng is a thin wrapper; empty input must return false.
        assert!(!reseed_crng(&[]), "reseed_crng with empty input must return false");
    }

    // Linux-only tests: verify the ioctl path itself.
    #[cfg(target_os = "linux")]
    mod linux {
        use super::*;
        use std::io::Write;
        use tempfile::NamedTempFile;

        // reseed_crng_at on /dev/urandom must succeed when running as root
        // (which is the case on box1 and inside the Firecracker VM).
        // The test is best-effort: if RNDADDENTROPY returns EPERM the calling
        // process lacks CAP_SYS_ADMIN; we skip rather than fail.
        #[test]
        fn reseed_crng_at_urandom_succeeds_as_root_or_skip() {
            let entropy = b"deterministic test vector - not real entropy";
            let result = reseed_crng_at(entropy, "/dev/urandom");
            // If we got false, check errno to distinguish EPERM (skip) from
            // a real failure.
            if !result {
                let e = std::io::Error::last_os_error();
                let raw = e.raw_os_error().unwrap_or(0);
                if raw == libc::EPERM {
                    // Not running as root: acceptable in CI, skip.
                    return;
                }
                // Any other error is a real failure.
                panic!("reseed_crng_at returned false unexpectedly: errno={raw}");
            }
        }

        // Verify that the header packing matches the kernel ABI:
        // sizeof(RandPoolInfoHeader) must be exactly 8 bytes (int + int).
        #[test]
        fn rand_pool_info_header_size_is_8() {
            assert_eq!(
                std::mem::size_of::<RandPoolInfoHeader>(),
                8,
                "rand_pool_info header must be 8 bytes (int entropy_count + int buf_size)"
            );
        }

        // Verify field offsets match the kernel ABI (entropy_count at 0, buf_size at 4).
        #[test]
        fn rand_pool_info_header_field_offsets() {
            let h = RandPoolInfoHeader {
                entropy_count: 0x12345678_i32,
                buf_size: 0x9abcdef0_u32 as i32,
            };
            let bytes: [u8; 8] = unsafe {
                // SAFETY: RandPoolInfoHeader is repr(C), size 8, no padding.
                // Transmuting to [u8; 8] reads the raw byte representation.
                std::mem::transmute(h)
            };
            // entropy_count is at offset 0, little-endian.
            assert_eq!(&bytes[0..4], &0x12345678_i32.to_le_bytes());
            // buf_size is at offset 4, little-endian.
            assert_eq!(&bytes[4..8], &(0x9abcdef0_u32 as i32).to_le_bytes());
        }

        // Verify that a regular file with /dev/urandom-compatible permissions
        // causes an ioctl failure (not a crash). RNDADDENTROPY on a regular file
        // returns ENOTTY; the function must return false, not panic.
        #[test]
        fn ioctl_on_regular_file_returns_false() {
            let mut tmp = NamedTempFile::new().expect("create temp file");
            // Write some dummy content so the file open succeeds.
            tmp.write_all(b"not a device").expect("write temp file");
            let path = tmp.path().to_str().expect("temp file path is valid UTF-8");
            let result = reseed_crng_at(b"test entropy bytes that are ignored", path);
            // RNDADDENTROPY on a plain file returns ENOTTY; we must get false.
            assert!(!result, "ioctl on a plain file must return false (ENOTTY)");
        }
    }
}
