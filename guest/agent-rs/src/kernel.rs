// Kernel manager: manages the in-guest code-execution kernel (Jupyter-style).
//
// This stub satisfies the shared-state signature used by SandboxService.
// Task 2.x (RunCode RPC) will replace this with the real Jupyter kernel driver.
// The stub never logs secret values; there are no values to log at this stage.

/// Manager for the in-guest code-execution kernel (Jupyter-style).
///
/// Starts lazily on the first RunCode RPC. Shared via
/// `Arc<Mutex<KernelManager>>` so the service struct can hand it to concurrent
/// RPC handlers in Phase 2.
///
/// Mirrors guestKernel in guest/agent/main.go which is a newKernelManager(kernelConfig{}).
#[derive(Debug)]
pub struct KernelManager {
    // Phase 2 fields: kernel process handle, stdin/stdout channels, etc.
    // Left empty at the stub stage; the mutex alone serializes future access.
    _private: (),
}

impl KernelManager {
    /// Create a new, idle KernelManager. The kernel process is not started
    /// until the first RunCode RPC arrives.
    pub fn new() -> Self {
        Self { _private: () }
    }
}

impl Default for KernelManager {
    fn default() -> Self {
        Self::new()
    }
}
