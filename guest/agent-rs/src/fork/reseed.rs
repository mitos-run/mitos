/// Credited CRNG reseed wrapper for the notify-forked path.
///
/// Mirrors reseedCRNG in guest/agent/notifyforked.go (lines 71-119).
/// Calls sys::entropy::reseed_crng, which uses the RNDADDENTROPY ioctl to
/// credit the host-supplied entropy into the kernel CRNG. Returns true ONLY
/// when the credited ioctl succeeds. Returns false (fail closed) on any error:
/// empty entropy, open failure, or ioctl failure.
///
/// Security contract: a restored fork shares the snapshot CRNG state with
/// every sibling fork. The host delivers per-fork entropy that MUST be
/// credibly injected (via RNDADDENTROPY, not a plain uncredited write) so each
/// fork diverges. An uncredited write mixes bytes into the input pool without
/// crediting entropy and does not guarantee divergence. Returning false makes
/// the host reap a fork whose CRNG could not be credibly reseeded.
///
/// Entropy bytes are never logged; only the byte count appears in log output.
pub fn reseed(entropy: &[u8]) -> bool {
    crate::sys::entropy::reseed_crng(entropy)
}

#[cfg(test)]
#[allow(
    clippy::expect_used,
    clippy::unwrap_used,
    clippy::panic,
    clippy::indexing_slicing
)]
mod tests {
    #[test]
    fn empty_entropy_is_false() {
        assert!(!super::reseed(&[]));
    }

    #[test]
    #[cfg(target_os = "linux")]
    fn nonzero_entropy_does_not_panic() {
        // Return value depends on capability; both outcomes are valid.
        let _ = super::reseed(b"divergence-material");
    }
}
