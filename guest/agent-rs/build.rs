// build.rs: compile proto/sandbox/v1/sandbox.proto with tonic-build.
//
// The proto is vendored under guest/agent-rs/proto/ (a copy of the canonical
// proto/sandbox/v1/sandbox.proto at the repo root) so this crate builds without
// any out-of-crate path dependency. The rsync step in the task brief copies it
// to box1 at /root/agent-rs-sp1/proto/sandbox/v1/sandbox.proto.
//
// protoc is supplied by protoc-bin-vendored so the build requires no system
// protoc. box1 has no system protoc installed.

fn main() {
    // Point tonic-build at the vendored protoc binary.
    let protoc = protoc_bin_vendored::protoc_bin_path()
        .expect("protoc-bin-vendored: protoc binary not found");

    // SAFETY: build scripts are single-threaded; setting PROTOC here before
    // tonic_build::configure().compile_protos() is the documented pattern for
    // overriding the protoc path in build scripts. No other thread can observe
    // the mutation (build scripts do not spawn threads that read env).
    unsafe {
        std::env::set_var("PROTOC", protoc);
    }

    tonic_build::configure()
        .build_client(false)
        .build_server(true)
        .compile_protos(
            &["proto/sandbox/v1/sandbox.proto"],
            &["proto"],
        )
        .expect("tonic_build: failed to compile sandbox.proto");
}
