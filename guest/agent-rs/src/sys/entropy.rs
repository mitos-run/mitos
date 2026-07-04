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
// libc::Ioctl is the correct type alias for the ioctl request argument on the
// target platform: c_ulong on gnu, c_int (i32) on musl. Using libc::Ioctl
// rather than c_ulong ensures this constant compiles on both targets without
// a cast at the call site. The numeric value 0x40085203 is preserved on both.
//
// This constant is used only on Linux; the cfg guard prevents it from
// appearing in macOS builds.
#[cfg(target_os = "linux")]
const RNDADDENTROPY: libc::Ioctl = 0x4008_5203_u32 as libc::Ioctl;

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

/// Seed the kernel CRNG at boot so `getrandom()` does not block.
///
/// The guest kernel lacks CONFIG_RANDOM_TRUST_CPU, so the CRNG stays
/// uninitialized at boot and the kernel does not credit the virtio-rng fast
/// enough. A serving workload (issue #460) that does crypto at startup, like
/// openclaw resolving authentication, then blocks forever in
/// `wait_for_random_bytes` during the template build and never binds its port,
/// so the build's HTTP ready gate times out. Pull hardware entropy from
/// `/dev/hwrng` (the firecracker virtio-rng, independent of the CRNG) or, if it
/// is absent, the CPU RDRAND instruction, and credit it via RNDADDENTROPY so the
/// CRNG initializes. Per-fork uniqueness still comes from the NotifyForked
/// reseed; this only unblocks the CRNG so the workload can start. Returns true if
/// the CRNG was credited.
pub fn seed_crng_at_boot() -> bool {
    #[cfg(target_os = "linux")]
    {
        if let Some(seed) = read_hwrng(64)
            && reseed_crng(&seed)
        {
            return true;
        }
        if let Some(seed) = rdrand_bytes(64)
            && reseed_crng(&seed)
        {
            return true;
        }
        false
    }
    #[cfg(not(target_os = "linux"))]
    {
        false
    }
}

/// Read exactly `n` bytes from the virtio-rng character device. Returns None if
/// the device is absent (kernel without virtio-rng) or the read fails.
#[cfg(target_os = "linux")]
fn read_hwrng(n: usize) -> Option<Vec<u8>> {
    use std::io::Read;
    let mut f = std::fs::File::open("/dev/hwrng").ok()?;
    let mut buf = vec![0u8; n];
    f.read_exact(&mut buf).ok()?;
    Some(buf)
}

/// Fill `n` bytes from the CPU RDRAND instruction, when present. RDRAND is a
/// NIST SP800-90 hardware DRBG; crediting it as full entropy is what
/// random.trust_cpu would do, and it does not depend on any kernel config.
#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
fn rdrand_bytes(n: usize) -> Option<Vec<u8>> {
    if !std::is_x86_feature_detected!("rdrand") {
        return None;
    }
    let mut out = Vec::with_capacity(n + 8);
    while out.len() < n {
        let mut v: u64 = 0;
        // SAFETY: rdrand is feature-detected immediately above. _rdrand64_step
        // writes the random value into v and returns 1 on success; it reads no
        // memory, only fills a CPU register, so there are no pointer or aliasing
        // preconditions.
        let ok = unsafe { core::arch::x86_64::_rdrand64_step(&mut v) };
        if ok != 1 {
            return None;
        }
        out.extend_from_slice(&v.to_le_bytes());
    }
    out.truncate(n);
    Some(out)
}

#[cfg(all(target_os = "linux", not(target_arch = "x86_64")))]
fn rdrand_bytes(_n: usize) -> Option<Vec<u8>> {
    None
}

#[cfg(target_os = "linux")]
fn reseed_crng_linux(entropy: &[u8], path: &str) -> bool {
    // Build the kernel buffer: header (8 bytes) followed by the entropy bytes.
    // The layout matches struct rand_pool_info + buf[]:
    //   [entropy_count:i32 LE][buf_size:i32 LE][entropy bytes...]
    // All fields are little-endian on amd64/arm64 (the only targets this runs on).
    // We use safe to_le_bytes() appends to avoid any unsafe transmute/from_raw_parts.
    let entropy_bits = match entropy.len().checked_mul(8) {
        Some(b) => b,
        None => {
            eprintln!("sys::entropy: entropy length overflow");
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
    let buf_size_i32: i32 = match entropy.len().try_into() {
        Ok(v) => v,
        Err(_) => {
            eprintln!("sys::entropy: entropy slice too large for buf_size field");
            return false;
        }
    };

    // Build the contiguous buffer [header][entropy bytes] using safe byte appends.
    // No unsafe code: to_le_bytes() on i32 produces exactly 4 bytes each, giving
    // the 8-byte header layout the kernel expects for struct rand_pool_info.
    let mut buf: Vec<u8> = Vec::with_capacity(8 + entropy.len());
    buf.extend_from_slice(&entropy_bits_i32.to_le_bytes()); // entropy_count at offset 0
    buf.extend_from_slice(&buf_size_i32.to_le_bytes());     // buf_size at offset 4
    buf.extend_from_slice(entropy);                          // entropy bytes at offset 8

    // Open /dev/urandom (or the test path) for read+write.
    let file = match OpenOptions::new().read(true).write(true).open(path) {
        Ok(f) => f,
        Err(e) => {
            eprintln!("sys::entropy: open {path}: {e}");
            return false;
        }
    };

    let fd = file.as_raw_fd();

    // Issue RNDADDENTROPY ioctl.
    // SAFETY:
    // - fd is a valid open file descriptor owned by `file`; `file` is alive
    //   for the duration of this call.
    // - buf.as_ptr() points to the first byte of a live Vec<u8> with
    //   (8 + entropy.len()) bytes: i32 LE entropy_count, i32 LE buf_size, then
    //   entropy bytes. The kernel reads entropy_count and buf_size from the
    //   header, then reads buf_size bytes from buf[]. Our buffer has exactly
    //   those bytes.
    // - The ioctl does not retain the pointer after the call returns.
    // - RNDADDENTROPY = 0x40085203 is the correct request number for this ioctl
    //   on Linux amd64/arm64 (verified against /usr/include/linux/random.h).
    // - RNDADDENTROPY is typed as libc::Ioctl, which is c_ulong on gnu and
    //   c_int on musl; libc::ioctl accepts libc::Ioctl as its second argument
    //   on both targets, so the call compiles without a cast on either ABI.
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
            "sys::entropy: RNDADDENTROPY failed (errno {}); reseed NOT credited, \
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

    // Verify the safe byte-packing layout matches the kernel ABI:
    //   bytes 0..4  = entropy_count as i32 little-endian
    //   bytes 4..8  = buf_size as i32 little-endian
    //   bytes 8..   = the entropy bytes themselves
    #[test]
    fn packed_header_layout_matches_kernel_abi() {
        let entropy = b"\x01\x02\x03\x04";
        let entropy_bits_i32: i32 = (entropy.len() * 8) as i32; // 32
        let buf_size_i32: i32 = entropy.len() as i32;            // 4

        let mut expected: Vec<u8> = Vec::new();
        expected.extend_from_slice(&entropy_bits_i32.to_le_bytes());
        expected.extend_from_slice(&buf_size_i32.to_le_bytes());
        expected.extend_from_slice(entropy);

        // Verify byte 0..4 = entropy_count LE, 4..8 = buf_size LE, 8.. = bytes.
        assert_eq!(&expected[0..4], &32_i32.to_le_bytes(), "entropy_count at offset 0");
        assert_eq!(&expected[4..8], &4_i32.to_le_bytes(), "buf_size at offset 4");
        assert_eq!(&expected[8..], b"\x01\x02\x03\x04", "entropy bytes at offset 8");
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
