// Kernel manager: manages the in-guest code-execution kernel (Jupyter-style).
//
// Mirrors guest/agent/kernel.go exactly:
// - One persistent driver process per sandbox (lazy start on first RunCode).
// - State persists across run calls (define x in call 1, use x in call 2).
// - Executions serialized by the Mutex wrapping KernelManager.
// - KernelUnavailable emitted as error frames (no agent crash) when the driver
//   or ipykernel is absent.
// - Only "python" (or empty) language accepted; anything else -> error frame.
// - Driver subprocess: python3 /opt/mitos/kernel_driver.py on stdin/stdout,
//   stderr routed to the agent's stderr (ipykernel noise).
// - driverEvent JSON lines read from stdout; mapped to RunCodeResponse frames.

/// Driver subprocess: launches python3 kernel_driver.py, exchanges JSON-lines
/// on stdin/stdout, and maps driverEvent kinds to RunCodeResponse frames.
pub mod driver;

pub use driver::{KernelConfig, KernelManager};
