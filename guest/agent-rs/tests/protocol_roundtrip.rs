use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;

// Spawns the agent on a unix socket and drives it like the Go vsock client would.
fn with_agent<F: FnOnce(&str)>(name: &str, f: F) {
    let sock = std::env::temp_dir().join(name);
    let _ = std::fs::remove_file(&sock);
    let p = sock.to_str().unwrap().to_string();
    let env = std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashMap::new()));
    let p2 = p.clone();
    let h = std::thread::spawn(move || sandbox_agent::transport::serve_unix(&p2, env));
    // wait for the socket to appear
    for _ in 0..100 {
        if std::path::Path::new(&p).exists() {
            break;
        }
        std::thread::sleep(std::time::Duration::from_millis(10));
    }
    f(&p);
    drop(h);
}

#[test]
fn exec_stream_emits_chunks_then_exit() {
    with_agent("agentrs_stream.sock", |p| {
        let mut s = UnixStream::connect(p).unwrap();
        s.write_all(b"{\"type\":\"exec_stream\",\"exec_stream\":{\"command\":\"printf abc\"}}\n")
            .unwrap();
        let mut r = BufReader::new(s);
        let mut kinds = vec![];
        let mut got = String::new();
        loop {
            let mut line = String::new();
            if r.read_line(&mut line).unwrap() == 0 {
                break;
            }
            let v: serde_json::Value = serde_json::from_str(&line).unwrap();
            kinds.push(v["kind"].as_str().unwrap().to_string());
            if v["kind"] == "exit" {
                break;
            }
            if let Some(d) = v["data"].as_str() {
                got.push_str(&String::from_utf8(base64_decode(d)).unwrap());
            }
        }
        assert!(kinds.last().unwrap() == "exit");
        assert_eq!(got, "abc");
    });
}

// Decode standard base64 (with padding) using the crate's own decoder.
fn base64_decode(s: &str) -> Vec<u8> {
    sandbox_agent::protocol::b64_decode(s).expect("valid base64")
}
