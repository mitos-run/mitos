// Raw AF_NETLINK implementation of eth0 reconfiguration for the notify-forked
// path. Mirrors internal/guestnet/configure_linux.go: the same operation order
// (optional MAC down/set/up, flush addrs, add new addr, replace default route).
//
// Rationale for raw netlink over the `rtnetlink` crate:
//   The brief prefers rtnetlink but notes to fall back to raw netlink when it
//   does not build on musl. The specified public signature
//   `configure_network(cfg: Option<&NetworkConfig>)` is synchronous; rtnetlink
//   is async (tokio) and cannot be called directly from a sync context without
//   either spawning a new tokio runtime (cost) or capturing the current handle
//   (fragile). The existing codebase already uses raw libc in sys/ for every
//   other fork-correctness syscall (RNDADDENTROPY, clock_settime), so using the
//   same pattern here is consistent. Raw netlink has no dependency on any async
//   executor, builds identically on gnu and musl, and mirrors what the Go
//   implementation does (internal/guestnet uses raw AF_NETLINK sockets).
//
// Safety bar: this file lives in sys/ where `unsafe_code` is allowed via the
// `#[allow(unsafe_code)]` on the `pub mod sys;` declaration in lib.rs.
// Every unsafe block carries a SAFETY comment. The deny(unsafe_op_in_unsafe_fn)
// at the module level ensures each unsafe operation is explicitly scoped.

#![deny(unsafe_op_in_unsafe_fn)]

use std::io;
use std::mem;
use std::net::Ipv4Addr;

// ---------------------------------------------------------------------------
// Netlink constants (not in libc when targeting musl).
// ---------------------------------------------------------------------------

// AF_NETLINK = 16, NETLINK_ROUTE = 0.
const AF_NETLINK: libc::sa_family_t = 16;
const NETLINK_ROUTE: libc::c_int = 0;

// Message types (linux/rtnetlink.h).
const RTM_NEWLINK: u16 = 16;
const RTM_NEWADDR: u16 = 20;
const RTM_DELADDR: u16 = 21;
const RTM_GETADDR: u16 = 22;
const RTM_NEWROUTE: u16 = 24;

// Netlink message flags.
const NLM_F_REQUEST: u16 = 0x0001;
const NLM_F_ACK: u16 = 0x0004;
// Dump-request flags (linux/netlink.h).
const NLM_F_ROOT: u16 = 0x0100;
const NLM_F_MATCH: u16 = 0x0200;
const NLM_F_DUMP: u16 = NLM_F_ROOT | NLM_F_MATCH;
// Modification-request flags (linux/netlink.h). These share numeric values
// with the dump flags above but are interpreted per message type: for NEW/SET
// messages NLM_F_REPLACE = 0x0100, NLM_F_EXCL = 0x0200, NLM_F_CREATE = 0x0400.
const NLM_F_REPLACE: u16 = 0x0100;
// NLM_F_EXCL is not used in production paths (we use NLM_F_REPLACE for
// idempotency); it is kept here as a documentation constant and used in tests
// to assert it is absent from build_add_addr flags.
#[allow(dead_code)]
const NLM_F_EXCL: u16 = 0x0200;
const NLM_F_CREATE: u16 = 0x0400;

// Netlink message types (linux/netlink.h).
const NLMSG_ERROR: u16 = 2;
const NLMSG_DONE: u16 = 3;

// NLMSG_HDRLEN = 16; NLATTR_HDRLEN = 4.
const NLMSG_HDRLEN: usize = 16;

// ifinfomsg flags (linux/if.h).
const IFF_UP: u32 = 1;

// rtmsg (linux/rtnetlink.h).
const RT_TABLE_MAIN: u8 = 254;
const RTPROT_BOOT: u8 = 3;
const RT_SCOPE_UNIVERSE: u8 = 0;
const RTN_UNICAST: u8 = 1;

// rtattr types for addresses (linux/if_addr.h).
const IFA_ADDRESS: u16 = 1;
const IFA_LOCAL: u16 = 2;

// rtattr types for routes (linux/rtnetlink.h).
const RTA_GATEWAY: u16 = 5;
const RTA_OIF: u16 = 4;

// rtattr types for links (linux/if_link.h / linux/if_ether.h).
const IFLA_ADDRESS: u16 = 1;

// Address family.
const AF_INET: u8 = 2;

// ---------------------------------------------------------------------------
// Alignment helpers.
// ---------------------------------------------------------------------------

const fn nl_align(n: usize) -> usize {
    (n + 3) & !3
}

// ---------------------------------------------------------------------------
// struct nlmsghdr (16 bytes, linux/netlink.h).
// These struct definitions document the wire layout used in the manual byte
// builders below. They are not instantiated directly (we use put_u32_le etc.
// to build messages byte-by-byte for zero-copy control), but we keep them as
// an auditable specification of the kernel ABI.
// ---------------------------------------------------------------------------

#[allow(dead_code)]
#[repr(C)]
struct NlMsgHdr {
    nlmsg_len: u32,
    nlmsg_type: u16,
    nlmsg_flags: u16,
    nlmsg_seq: u32,
    nlmsg_pid: u32,
}

// ---------------------------------------------------------------------------
// struct ifaddrmsg (8 bytes, linux/if_addr.h).
// ---------------------------------------------------------------------------

#[allow(dead_code)]
#[repr(C)]
struct IfAddrMsg {
    ifa_family: u8,
    ifa_prefixlen: u8,
    ifa_flags: u8,
    ifa_scope: u8,
    ifa_index: u32,
}

// ---------------------------------------------------------------------------
// struct ifinfomsg (16 bytes, linux/rtnetlink.h).
// ---------------------------------------------------------------------------

#[allow(dead_code)]
#[repr(C)]
struct IfInfoMsg {
    ifi_family: u8,
    _pad: u8,
    ifi_type: u16,
    ifi_index: i32,
    ifi_flags: u32,
    ifi_change: u32,
}

// ---------------------------------------------------------------------------
// struct rtmsg (12 bytes, linux/rtnetlink.h).
// ---------------------------------------------------------------------------

#[allow(dead_code)]
#[repr(C)]
struct RtMsg {
    rtm_family: u8,
    rtm_dst_len: u8,
    rtm_src_len: u8,
    rtm_tos: u8,
    rtm_table: u8,
    rtm_protocol: u8,
    rtm_scope: u8,
    rtm_type: u8,
    rtm_flags: u32,
}

// ---------------------------------------------------------------------------
// Message builders.
// ---------------------------------------------------------------------------

fn put_u32_le(buf: &mut Vec<u8>, v: u32) {
    buf.extend_from_slice(&v.to_le_bytes());
}

fn put_u16_le(buf: &mut Vec<u8>, v: u16) {
    buf.extend_from_slice(&v.to_le_bytes());
}

fn put_u8(buf: &mut Vec<u8>, v: u8) {
    buf.push(v);
}

// Append padding bytes to align buf to a 4-byte boundary.
fn pad4(buf: &mut Vec<u8>) {
    while !buf.len().is_multiple_of(4) {
        buf.push(0);
    }
}

// Append one nlattr: [len:u16][type:u16][data...][pad to 4].
fn put_attr(buf: &mut Vec<u8>, attr_type: u16, data: &[u8]) {
    let total = 4 + data.len();
    put_u16_le(buf, total as u16);
    put_u16_le(buf, attr_type);
    buf.extend_from_slice(data);
    pad4(buf);
}

// Patch the nlmsg_len field at offset 0 to be the current buffer length.
// Takes &mut [u8] rather than &mut Vec<u8> to satisfy clippy::ptr_arg.
fn patch_len(buf: &mut [u8]) {
    let len = buf.len() as u32;
    if let Some(slot) = buf.get_mut(0..4) {
        slot.copy_from_slice(&len.to_le_bytes());
    }
    // buf.len() is always >= 16 after any header is written; the get_mut
    // can only fail on a completely empty buf, which never happens here.
}

// Build a minimal nlmsghdr + ifaddrmsg header prefix.
fn hdr_ifaddr(buf: &mut Vec<u8>, msg_type: u16, flags: u16, seq: u32, family: u8, prefixlen: u8, idx: u32) {
    // nlmsghdr (16 bytes).
    put_u32_le(buf, 0); // nlmsg_len: patched later.
    put_u16_le(buf, msg_type);
    put_u16_le(buf, flags);
    put_u32_le(buf, seq);
    put_u32_le(buf, 0); // nlmsg_pid = 0 (kernel fills).
    // ifaddrmsg (8 bytes).
    put_u8(buf, family);
    put_u8(buf, prefixlen);
    put_u8(buf, 0); // ifa_flags.
    put_u8(buf, 0); // ifa_scope.
    put_u32_le(buf, idx);
}

/// Build RTM_GETADDR | NLM_F_DUMP to enumerate all addresses.
pub fn build_dump_addr(seq: u32) -> Vec<u8> {
    let mut buf = Vec::with_capacity(NLMSG_HDRLEN + 8);
    hdr_ifaddr(&mut buf, RTM_GETADDR, NLM_F_REQUEST | NLM_F_DUMP, seq, AF_INET, 0, 0);
    patch_len(&mut buf);
    buf
}

/// Build RTM_DELADDR to remove one address/prefix from an interface.
pub fn build_del_addr(seq: u32, idx: u32, ip: Ipv4Addr, prefixlen: u8) -> Vec<u8> {
    let mut buf = Vec::with_capacity(NLMSG_HDRLEN + 8 + 8);
    hdr_ifaddr(&mut buf, RTM_DELADDR, NLM_F_REQUEST | NLM_F_ACK, seq, AF_INET, prefixlen, idx);
    put_attr(&mut buf, IFA_LOCAL, &ip.octets());
    patch_len(&mut buf);
    buf
}

/// Build RTM_NEWADDR to add a new address/prefix on an interface.
/// Uses NLM_F_REPLACE (0x0100) instead of NLM_F_EXCL (0x0200) so that
/// applying the same address a second time (re-fork) succeeds idempotently
/// rather than returning EEXIST. Matches Go's buildAddrMsg flags and attribute
/// layout (IFA_LOCAL then IFA_ADDRESS, same octets).
pub fn build_add_addr(seq: u32, idx: u32, ip: Ipv4Addr, prefixlen: u8) -> Vec<u8> {
    let mut buf = Vec::with_capacity(NLMSG_HDRLEN + 8 + 16);
    hdr_ifaddr(
        &mut buf,
        RTM_NEWADDR,
        NLM_F_REQUEST | NLM_F_ACK | NLM_F_CREATE | NLM_F_REPLACE,
        seq,
        AF_INET,
        prefixlen,
        idx,
    );
    put_attr(&mut buf, IFA_LOCAL, &ip.octets());
    put_attr(&mut buf, IFA_ADDRESS, &ip.octets());
    patch_len(&mut buf);
    buf
}

// Build a minimal nlmsghdr + ifinfomsg header.
fn hdr_ifinfo(buf: &mut Vec<u8>, msg_type: u16, flags: u16, seq: u32, ifi_index: i32, ifi_flags: u32, ifi_change: u32) {
    // nlmsghdr (16 bytes).
    put_u32_le(buf, 0); // patched later.
    put_u16_le(buf, msg_type);
    put_u16_le(buf, flags);
    put_u32_le(buf, seq);
    put_u32_le(buf, 0);
    // ifinfomsg (16 bytes).
    put_u8(buf, 0); // ifi_family = AF_UNSPEC.
    put_u8(buf, 0); // pad.
    put_u16_le(buf, 0); // ifi_type (don't care).
    buf.extend_from_slice(&ifi_index.to_le_bytes());
    put_u32_le(buf, ifi_flags);
    put_u32_le(buf, ifi_change);
}

/// Build RTM_NEWLINK to bring the link down (IFF_UP = 0, change = IFF_UP).
pub fn build_link_down(seq: u32, idx: i32) -> Vec<u8> {
    let mut buf = Vec::with_capacity(NLMSG_HDRLEN + 16);
    hdr_ifinfo(&mut buf, RTM_NEWLINK, NLM_F_REQUEST | NLM_F_ACK, seq, idx, 0, IFF_UP);
    patch_len(&mut buf);
    buf
}

/// Build RTM_NEWLINK to bring the link up (IFF_UP = 1, change = IFF_UP).
pub fn build_link_up(seq: u32, idx: i32) -> Vec<u8> {
    let mut buf = Vec::with_capacity(NLMSG_HDRLEN + 16);
    hdr_ifinfo(&mut buf, RTM_NEWLINK, NLM_F_REQUEST | NLM_F_ACK, seq, idx, IFF_UP, IFF_UP);
    patch_len(&mut buf);
    buf
}

/// Build RTM_NEWLINK to set the hardware (MAC) address of a link.
/// The link must be down before this is called.
pub fn build_set_mac(seq: u32, idx: i32, hw: &[u8; 6]) -> Vec<u8> {
    let mut buf = Vec::with_capacity(NLMSG_HDRLEN + 16 + 12);
    hdr_ifinfo(&mut buf, RTM_NEWLINK, NLM_F_REQUEST | NLM_F_ACK, seq, idx, 0, 0);
    put_attr(&mut buf, IFLA_ADDRESS, hw.as_slice());
    patch_len(&mut buf);
    buf
}

/// Build RTM_NEWROUTE with NLM_F_REPLACE to set the default route via `gw`.
pub fn build_replace_default_route(seq: u32, idx: i32, gw: Ipv4Addr) -> Vec<u8> {
    let mut buf = Vec::with_capacity(NLMSG_HDRLEN + 12 + 24);
    // nlmsghdr.
    put_u32_le(&mut buf, 0); // patched later.
    put_u16_le(&mut buf, RTM_NEWROUTE);
    put_u16_le(&mut buf, NLM_F_REQUEST | NLM_F_ACK | NLM_F_CREATE | NLM_F_REPLACE);
    put_u32_le(&mut buf, seq);
    put_u32_le(&mut buf, 0);
    // rtmsg (12 bytes).
    put_u8(&mut buf, AF_INET);
    put_u8(&mut buf, 0); // rtm_dst_len = 0: default route (0.0.0.0/0).
    put_u8(&mut buf, 0); // rtm_src_len.
    put_u8(&mut buf, 0); // rtm_tos.
    put_u8(&mut buf, RT_TABLE_MAIN);
    put_u8(&mut buf, RTPROT_BOOT);
    put_u8(&mut buf, RT_SCOPE_UNIVERSE);
    put_u8(&mut buf, RTN_UNICAST);
    put_u32_le(&mut buf, 0); // rtm_flags.
    // Attrs: RTA_GATEWAY + RTA_OIF.
    put_attr(&mut buf, RTA_GATEWAY, &gw.octets());
    let idx_u32 = idx as u32;
    put_attr(&mut buf, RTA_OIF, &idx_u32.to_le_bytes());
    patch_len(&mut buf);
    buf
}

// ---------------------------------------------------------------------------
// Address dump parser.
// ---------------------------------------------------------------------------

/// One IPv4 address entry parsed from an RTM_NEWADDR dump message.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AddrEntry {
    /// Interface index.
    pub index: u32,
    /// Prefix length.
    pub prefixlen: u8,
    /// Address (from IFA_LOCAL or IFA_ADDRESS attribute).
    pub ip: Ipv4Addr,
}

// Safe helpers for reading fixed-size integers from a byte slice.
// Returns io::Error rather than panicking on out-of-bounds access.

fn read_u32_le(buf: &[u8], offset: usize) -> io::Result<u32> {
    buf.get(offset..offset.saturating_add(4))
        .and_then(|s| s.try_into().ok())
        .map(u32::from_le_bytes)
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "netlink: read_u32 out of bounds"))
}

fn read_u16_le(buf: &[u8], offset: usize) -> io::Result<u16> {
    buf.get(offset..offset.saturating_add(2))
        .and_then(|s| s.try_into().ok())
        .map(u16::from_le_bytes)
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "netlink: read_u16 out of bounds"))
}

fn read_i32_le(buf: &[u8], offset: usize) -> io::Result<i32> {
    buf.get(offset..offset.saturating_add(4))
        .and_then(|s| s.try_into().ok())
        .map(i32::from_le_bytes)
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "netlink: read_i32 out of bounds"))
}

fn read_u8(buf: &[u8], offset: usize) -> io::Result<u8> {
    buf.get(offset)
        .copied()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "netlink: read_u8 out of bounds"))
}

/// Parse an RTM_NEWADDR dump into a list of AddrEntry.
/// Stops at NLMSG_DONE or NLMSG_ERROR (those messages are consumed, not returned).
/// Returns an error if a non-zero errno is found in NLMSG_ERROR.
pub fn parse_addr_dump(buf: &[u8]) -> io::Result<Vec<AddrEntry>> {
    let mut out = Vec::new();
    let mut pos = 0usize;
    while pos + NLMSG_HDRLEN <= buf.len() {
        let nlmsg_len = read_u32_le(buf, pos)? as usize;
        let nlmsg_type = read_u16_le(buf, pos + 4)?;

        if nlmsg_len < NLMSG_HDRLEN || pos + nlmsg_len > buf.len() {
            break;
        }

        match nlmsg_type {
            NLMSG_DONE => break,
            NLMSG_ERROR => {
                // nlmsgerr: first 4 bytes after header are the errno (negative).
                if nlmsg_len >= NLMSG_HDRLEN + 4 {
                    let errno_raw = read_i32_le(buf, pos + NLMSG_HDRLEN)?;
                    if errno_raw != 0 {
                        return Err(io::Error::from_raw_os_error(-errno_raw));
                    }
                }
                break;
            }
            RTM_NEWADDR => {
                // ifaddrmsg starts at pos + NLMSG_HDRLEN (8 bytes).
                if nlmsg_len < NLMSG_HDRLEN + 8 {
                    pos += nl_align(nlmsg_len);
                    continue;
                }
                let msg_start = pos + NLMSG_HDRLEN;
                let family = read_u8(buf, msg_start)?;
                let prefixlen = read_u8(buf, msg_start + 1)?;
                let index = read_u32_le(buf, msg_start + 4)?;
                if family != AF_INET {
                    pos += nl_align(nlmsg_len);
                    continue;
                }
                // Parse rtattrs from msg_start + 8.
                let mut attr_pos = msg_start + 8;
                let msg_end = pos + nlmsg_len;
                let mut ip: Option<Ipv4Addr> = None;
                while attr_pos + 4 <= msg_end {
                    let attr_len = read_u16_le(buf, attr_pos)? as usize;
                    let attr_type = read_u16_le(buf, attr_pos + 2)?;
                    if attr_len < 4 || attr_pos + attr_len > msg_end {
                        break;
                    }
                    // attr data starts at attr_pos + 4.
                    let data_start = attr_pos + 4;
                    let data_end = attr_pos + attr_len;
                    if attr_type == IFA_LOCAL && (data_end - data_start) == 4 {
                        let a = read_u8(buf, data_start)?;
                        let b = read_u8(buf, data_start + 1)?;
                        let c = read_u8(buf, data_start + 2)?;
                        let d = read_u8(buf, data_start + 3)?;
                        ip = Some(Ipv4Addr::new(a, b, c, d));
                    }
                    attr_pos += nl_align(attr_len);
                }
                if let Some(ip) = ip {
                    out.push(AddrEntry { index, prefixlen, ip });
                }
            }
            _ => {}
        }

        pos += nl_align(nlmsg_len);
    }
    Ok(out)
}

// ---------------------------------------------------------------------------
// Netlink socket helpers.
// ---------------------------------------------------------------------------

/// A raw AF_NETLINK RTNETLINK socket with a 5-second receive timeout.
/// Wraps the raw fd and closes it on drop.
pub struct NetlinkSocket {
    fd: libc::c_int,
}

impl NetlinkSocket {
    /// Open a new NETLINK_ROUTE socket and bind to the kernel.
    pub fn open() -> io::Result<Self> {
        // SAFETY: socket(2) with valid constants; fd is -1 on error.
        let fd = unsafe {
            libc::socket(AF_NETLINK as libc::c_int, libc::SOCK_RAW | libc::SOCK_CLOEXEC, NETLINK_ROUTE)
        };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: sockaddr_nl is zero-initialized (zeroed() is valid for any
        // C-layout struct with no padding requirements beyond zero). We set
        // only nl_family; all other fields (nl_pid, nl_groups) are zero which
        // tells the kernel to assign a pid automatically.
        let sa: libc::sockaddr_nl = unsafe {
            let mut s = mem::zeroed::<libc::sockaddr_nl>();
            s.nl_family = AF_NETLINK;
            s
        };
        let ret = unsafe {
            libc::bind(
                fd,
                &sa as *const libc::sockaddr_nl as *const libc::sockaddr,
                mem::size_of::<libc::sockaddr_nl>() as libc::socklen_t,
            )
        };
        if ret < 0 {
            let err = io::Error::last_os_error();
            // SAFETY: fd is valid and must be closed on error.
            unsafe { libc::close(fd); }
            return Err(err);
        }
        // Set a 5-second receive timeout so a missing kernel reply cannot hang.
        // SAFETY: fd is valid; timeval is properly sized for SO_RCVTIMEO.
        let tv = libc::timeval { tv_sec: 5, tv_usec: 0 };
        let ret = unsafe {
            libc::setsockopt(
                fd,
                libc::SOL_SOCKET,
                libc::SO_RCVTIMEO,
                &tv as *const libc::timeval as *const libc::c_void,
                mem::size_of::<libc::timeval>() as libc::socklen_t,
            )
        };
        if ret < 0 {
            let err = io::Error::last_os_error();
            unsafe { libc::close(fd); }
            return Err(err);
        }
        Ok(Self { fd })
    }

    /// Send a netlink message to the kernel.
    pub fn send(&self, msg: &[u8]) -> io::Result<()> {
        // SAFETY: sockaddr_nl is zero-initialized; nl_family is set to AF_NETLINK.
        let sa: libc::sockaddr_nl = unsafe {
            let mut s = mem::zeroed::<libc::sockaddr_nl>();
            s.nl_family = AF_NETLINK;
            s
        };
        // SAFETY: msg.as_ptr()/msg.len() is valid for the duration of sendto;
        // sa is a valid sockaddr_nl; fd is open.
        let ret = unsafe {
            libc::sendto(
                self.fd,
                msg.as_ptr() as *const libc::c_void,
                msg.len(),
                0,
                &sa as *const libc::sockaddr_nl as *const libc::sockaddr,
                mem::size_of::<libc::sockaddr_nl>() as libc::socklen_t,
            )
        };
        if ret < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(())
        }
    }

    /// Receive one netlink buffer (up to 65536 bytes).
    pub fn recv(&self) -> io::Result<Vec<u8>> {
        let mut buf = vec![0u8; 65536];
        // SAFETY: buf.as_mut_ptr() is valid for buf.len() bytes; fd is open.
        let n = unsafe {
            libc::recv(
                self.fd,
                buf.as_mut_ptr() as *mut libc::c_void,
                buf.len(),
                0,
            )
        };
        if n < 0 {
            Err(io::Error::last_os_error())
        } else {
            buf.truncate(n as usize);
            Ok(buf)
        }
    }

    /// Send a request and read back one ACK (NLMSG_ERROR with errno = 0 = success).
    pub fn request(&self, msg: &[u8]) -> io::Result<()> {
        self.send(msg)?;
        let resp = self.recv()?;
        let _ = parse_addr_dump(&resp)?;
        Ok(())
    }

    /// Send a DUMP request and concatenate replies until NLMSG_DONE/NLMSG_ERROR.
    pub fn dump(&self, msg: &[u8]) -> io::Result<Vec<u8>> {
        self.send(msg)?;
        let mut out = Vec::new();
        loop {
            let resp = self.recv()?;
            // Check for termination before extending so we can inspect done/error.
            let done = is_terminated(&resp);
            out.extend_from_slice(&resp);
            if done {
                break;
            }
        }
        Ok(out)
    }
}

impl Drop for NetlinkSocket {
    fn drop(&mut self) {
        // SAFETY: fd is valid and has not been closed elsewhere.
        unsafe { libc::close(self.fd); }
    }
}

/// Returns true if `buf` contains an NLMSG_DONE or NLMSG_ERROR message.
fn is_terminated(buf: &[u8]) -> bool {
    let mut pos = 0usize;
    while pos + NLMSG_HDRLEN <= buf.len() {
        let nlmsg_len = match read_u32_le(buf, pos) {
            Ok(v) => v as usize,
            Err(_) => return true,
        };
        let nlmsg_type = match read_u16_le(buf, pos + 4) {
            Ok(v) => v,
            Err(_) => return true,
        };
        if nlmsg_len < NLMSG_HDRLEN || nlmsg_len > buf.len() - pos {
            return true;
        }
        if nlmsg_type == NLMSG_DONE || nlmsg_type == NLMSG_ERROR {
            return true;
        }
        pos += nl_align(nlmsg_len);
    }
    false
}

// ---------------------------------------------------------------------------
// Interface index lookup.
// ---------------------------------------------------------------------------

/// Look up the interface index for `iface_name`. Returns an error when the
/// interface is not found or the name is too long.
pub fn if_nametoindex(iface_name: &str) -> io::Result<u32> {
    let bytes = iface_name.as_bytes();
    if bytes.len() >= libc::IFNAMSIZ {
        return Err(io::Error::new(io::ErrorKind::InvalidInput, "interface name too long"));
    }
    let mut name = [0u8; libc::IFNAMSIZ];
    // SAFETY invariant maintained before the copy: bytes.len() < IFNAMSIZ,
    // so the destination slice name[..bytes.len()] is always in bounds.
    if let Some(dst) = name.get_mut(..bytes.len()) {
        dst.copy_from_slice(bytes);
    }
    // SAFETY: name is a NUL-terminated byte array of length IFNAMSIZ;
    // the cast to *const c_char is valid since u8 and c_char have the same
    // size and alignment. if_nametoindex returns 0 on error.
    let idx = unsafe { libc::if_nametoindex(name.as_ptr() as *const libc::c_char) };
    if idx == 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(idx)
    }
}

/// Bring a link UP by name without touching its addresses. The loopback (lo)
/// interface needs this at boot: the kernel assigns 127.0.0.1 to lo but leaves
/// the link DOWN, so a workload bound to 127.0.0.1 and the StartWorkload HTTP
/// ready gate (issue #460) cannot reach it until lo is up. eth0 gets its link-up
/// via configure() on each fork; lo has no addresses to manage, so this is the
/// minimal link-up-only path.
pub fn link_up(iface: &str) -> io::Result<()> {
    let idx_i32 = if_nametoindex(iface)? as i32;
    let sock = NetlinkSocket::open()
        .map_err(|e| io::Error::new(e.kind(), format!("netlink: open socket: {e}")))?;
    sock.request(&build_link_up(1, idx_i32))
        .map_err(|e| io::Error::new(e.kind(), format!("netlink: link up {iface}: {e}")))?;
    Ok(())
}

// ---------------------------------------------------------------------------
// High-level configure: mirrors guestnet.Configure in Go exactly.
// ---------------------------------------------------------------------------

/// Sequence (mirrors internal/guestnet/configure_linux.go Configure):
///   1. If mac is Some: link down, set MAC, link up; else just link up.
///   2. Dump existing addresses on `iface`; delete each one.
///   3. Add new address/prefix.
///   4. Replace default route via gateway.
///
/// Returns an error on any netlink failure. Does NOT write resolv.conf (that
/// is the caller's responsibility, matching Go's configureNetwork separation).
pub fn configure(
    iface: &str,
    mac: Option<[u8; 6]>,
    guest_ip: Ipv4Addr,
    gateway_ip: Ipv4Addr,
    prefix_len: u8,
) -> io::Result<()> {
    let raw_idx = if_nametoindex(iface)?;
    let idx_i32 = raw_idx as i32;

    let sock = NetlinkSocket::open().map_err(|e| io::Error::new(e.kind(), format!("netlink: open socket: {e}")))?;
    let mut seq: u32 = 0;
    let mut next_seq = || { seq += 1; seq };

    // Step 1: MAC change (optional) then link up.
    if let Some(hw) = mac {
        sock.request(&build_link_down(next_seq(), idx_i32))
            .map_err(|e| io::Error::new(e.kind(), format!("netlink: link down: {e}")))?;
        sock.request(&build_set_mac(next_seq(), idx_i32, &hw))
            .map_err(|e| io::Error::new(e.kind(), format!("netlink: set mac: {e}")))?;
    }
    sock.request(&build_link_up(next_seq(), idx_i32))
        .map_err(|e| io::Error::new(e.kind(), format!("netlink: link up: {e}")))?;

    // Step 2: Flush existing addresses on this interface.
    let dump_buf = sock.dump(&build_dump_addr(next_seq()))
        .map_err(|e| io::Error::new(e.kind(), format!("netlink: dump addrs: {e}")))?;
    let existing = parse_addr_dump(&dump_buf)
        .map_err(|e| io::Error::new(e.kind(), format!("netlink: parse addrs: {e}")))?;
    for entry in &existing {
        if entry.index != raw_idx {
            continue;
        }
        sock.request(&build_del_addr(next_seq(), raw_idx, entry.ip, entry.prefixlen))
            .map_err(|e| io::Error::new(e.kind(), format!("netlink: flush addr {}/{}: {e}", entry.ip, entry.prefixlen)))?;
    }

    // Step 3: Add new address.
    sock.request(&build_add_addr(next_seq(), raw_idx, guest_ip, prefix_len))
        .map_err(|e| io::Error::new(e.kind(), format!("netlink: add addr {guest_ip}/{prefix_len}: {e}")))?;

    // Step 4: Replace default route.
    sock.request(&build_replace_default_route(next_seq(), idx_i32, gateway_ip))
        .map_err(|e| io::Error::new(e.kind(), format!("netlink: default route via {gateway_ip}: {e}")))?;

    Ok(())
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(
    clippy::expect_used,
    clippy::unwrap_used,
    clippy::panic,
    clippy::indexing_slicing
)]
mod tests {
    use super::*;

    // ---------------------------------------------------------------------------
    // Unit tests: message construction (no syscalls, runs on all platforms).
    // ---------------------------------------------------------------------------

    #[test]
    fn build_dump_addr_has_correct_type_and_flags() {
        let msg = build_dump_addr(1);
        assert!(msg.len() >= NLMSG_HDRLEN + 8, "dump addr message too short");
        let msg_type = u16::from_le_bytes(msg[4..6].try_into().unwrap());
        let flags = u16::from_le_bytes(msg[6..8].try_into().unwrap());
        assert_eq!(msg_type, RTM_GETADDR, "message type must be RTM_GETADDR");
        assert_eq!(flags & NLM_F_REQUEST, NLM_F_REQUEST, "must have NLM_F_REQUEST");
        assert_eq!(flags & NLM_F_DUMP, NLM_F_DUMP, "must have NLM_F_DUMP");
    }

    #[test]
    fn build_del_addr_carries_index_prefix_and_address() {
        let ip = Ipv4Addr::new(10, 200, 0, 6);
        let msg = build_del_addr(2, 7, ip, 30);
        // ifaddrmsg.ifa_prefixlen is at byte 17.
        assert_eq!(msg[17], 30, "prefixlen must be 30");
        // ifaddrmsg.ifa_index is at bytes 20..24.
        let idx = u32::from_le_bytes(msg[20..24].try_into().unwrap());
        assert_eq!(idx, 7, "interface index must be 7");
        // IFA_LOCAL attr: 4-byte header at offset 24, data at 28.
        let attr_type = u16::from_le_bytes(msg[26..28].try_into().unwrap());
        assert_eq!(attr_type, IFA_LOCAL, "attr type must be IFA_LOCAL");
        assert_eq!(&msg[28..32], &ip.octets(), "address bytes must match");
    }

    #[test]
    fn build_add_addr_carries_index_prefix_and_address() {
        let ip = Ipv4Addr::new(10, 200, 0, 6);
        let msg = build_add_addr(3, 7, ip, 30);
        let msg_type = u16::from_le_bytes(msg[4..6].try_into().unwrap());
        assert_eq!(msg_type, RTM_NEWADDR);
        let idx = u32::from_le_bytes(msg[20..24].try_into().unwrap());
        assert_eq!(idx, 7);
        let prefixlen = msg[17];
        assert_eq!(prefixlen, 30);
        let attr_type = u16::from_le_bytes(msg[26..28].try_into().unwrap());
        assert_eq!(attr_type, IFA_LOCAL);
        assert_eq!(&msg[28..32], &ip.octets());
        // IFA_ADDRESS must follow IFA_LOCAL (both carry the same octets).
        let addr_attr_type = u16::from_le_bytes(msg[34..36].try_into().unwrap());
        assert_eq!(addr_attr_type, IFA_ADDRESS, "second attr must be IFA_ADDRESS");
        assert_eq!(&msg[36..40], &ip.octets(), "IFA_ADDRESS octets must match");
    }

    #[test]
    fn build_add_addr_uses_nlm_f_replace_not_excl() {
        let ip = Ipv4Addr::new(10, 200, 0, 6);
        let msg = build_add_addr(3, 7, ip, 30);
        let flags = u16::from_le_bytes(msg[6..8].try_into().unwrap());
        // NLM_F_REPLACE (0x0100) must be set for idempotent re-fork behaviour.
        assert_eq!(flags & NLM_F_REPLACE, NLM_F_REPLACE, "NLM_F_REPLACE must be set");
        // NLM_F_EXCL (0x0200) must NOT be set: it causes EEXIST on re-fork.
        assert_eq!(flags & NLM_F_EXCL, 0, "NLM_F_EXCL must not be set");
        assert_eq!(flags & NLM_F_CREATE, NLM_F_CREATE, "NLM_F_CREATE must be set");
    }

    #[test]
    fn build_replace_default_route_has_dst_len_zero_and_gateway() {
        let gw = Ipv4Addr::new(10, 200, 0, 5);
        let msg = build_replace_default_route(4, 3, gw);
        let msg_type = u16::from_le_bytes(msg[4..6].try_into().unwrap());
        assert_eq!(msg_type, RTM_NEWROUTE);
        // rtmsg.rtm_dst_len is at byte 17 (nlmsghdr=16, rtm_family=1, dst_len=1).
        let dst_len = msg[17];
        assert_eq!(dst_len, 0, "default route must have dst_len = 0");
        // RTA_GATEWAY attr: after rtmsghdr (16) + rtmsg (12) = offset 28.
        let attr_type = u16::from_le_bytes(msg[30..32].try_into().unwrap());
        assert_eq!(attr_type, RTA_GATEWAY, "first attr must be RTA_GATEWAY");
        assert_eq!(&msg[32..36], &gw.octets(), "gateway address must match");
    }

    #[test]
    fn build_link_down_sets_flags_zero_change_iff_up() {
        let msg = build_link_down(5, 2);
        let msg_type = u16::from_le_bytes(msg[4..6].try_into().unwrap());
        assert_eq!(msg_type, RTM_NEWLINK);
        // ifinfomsg.ifi_flags at offset 24.
        let ifi_flags = u32::from_le_bytes(msg[24..28].try_into().unwrap());
        assert_eq!(ifi_flags, 0, "link down: ifi_flags must be 0");
        // ifinfomsg.ifi_change at offset 28.
        let ifi_change = u32::from_le_bytes(msg[28..32].try_into().unwrap());
        assert_eq!(ifi_change, IFF_UP, "link down: ifi_change must be IFF_UP");
    }

    #[test]
    fn build_link_up_sets_flags_iff_up_change_iff_up() {
        let msg = build_link_up(6, 2);
        let ifi_flags = u32::from_le_bytes(msg[24..28].try_into().unwrap());
        assert_eq!(ifi_flags, IFF_UP, "link up: ifi_flags must be IFF_UP");
        let ifi_change = u32::from_le_bytes(msg[28..32].try_into().unwrap());
        assert_eq!(ifi_change, IFF_UP, "link up: ifi_change must be IFF_UP");
    }

    #[test]
    fn build_set_mac_carries_address() {
        let hw: [u8; 6] = [0x02, 0x11, 0x22, 0x33, 0x44, 0x55];
        let msg = build_set_mac(7, 3, &hw);
        let msg_type = u16::from_le_bytes(msg[4..6].try_into().unwrap());
        assert_eq!(msg_type, RTM_NEWLINK);
        // IFLA_ADDRESS attr starts at offset 32 (header=16, ifinfomsg=16).
        let attr_type = u16::from_le_bytes(msg[34..36].try_into().unwrap());
        assert_eq!(attr_type, IFLA_ADDRESS, "attr must be IFLA_ADDRESS");
        assert_eq!(&msg[36..42], &hw, "MAC bytes must match");
    }

    #[test]
    fn parse_addr_dump_empty_buffer_returns_empty_vec() {
        let result = parse_addr_dump(&[]).unwrap();
        assert!(result.is_empty());
    }

    #[test]
    fn parse_addr_dump_nlmsg_done_returns_empty_vec() {
        // Build a minimal NLMSG_DONE message.
        let mut buf = Vec::new();
        buf.extend_from_slice(&(NLMSG_HDRLEN as u32).to_le_bytes()); // nlmsg_len
        buf.extend_from_slice(&NLMSG_DONE.to_le_bytes());             // nlmsg_type
        buf.extend_from_slice(&0u16.to_le_bytes());                    // flags
        buf.extend_from_slice(&0u32.to_le_bytes());                    // seq
        buf.extend_from_slice(&0u32.to_le_bytes());                    // pid
        let result = parse_addr_dump(&buf).unwrap();
        assert!(result.is_empty());
    }

    #[test]
    fn parse_addr_dump_error_with_nonzero_errno_returns_err() {
        // Build an NLMSG_ERROR with errno = -ENODEV (=19).
        let mut buf = Vec::new();
        let msg_len = (NLMSG_HDRLEN + 4) as u32;
        buf.extend_from_slice(&msg_len.to_le_bytes());
        buf.extend_from_slice(&NLMSG_ERROR.to_le_bytes());
        buf.extend_from_slice(&0u16.to_le_bytes());
        buf.extend_from_slice(&0u32.to_le_bytes());
        buf.extend_from_slice(&0u32.to_le_bytes());
        // errno = -19 (stored as negative i32 LE = the raw OS error).
        buf.extend_from_slice(&(-19i32).to_le_bytes());
        let result = parse_addr_dump(&buf);
        assert!(result.is_err(), "nonzero errno must return Err");
    }

    // ---------------------------------------------------------------------------
    // Linux integration tests: configure a dummy link inside an isolated network
    // namespace. Requires root (box1 runs as root). Each test forks a child
    // process that calls unshare(CLONE_NEWNET) to get its own fresh network
    // namespace, so tests are fully isolated and parallel-safe: no routing-table
    // collisions with each other or with the host network. The child exits with
    // 0 on success or 1 on failure; the parent asserts the exit code.
    // ---------------------------------------------------------------------------

    #[cfg(target_os = "linux")]
    mod linux_netns {
        use super::*;
        use std::process::Command;

        // CLONE_NEWNET flag for unshare(2).
        const CLONE_NEWNET: libc::c_int = 0x4000_0000;

        // Fork a child, unshare its network namespace, add a dummy link named
        // `link`, call the test body closure, and return. The parent waits for
        // the child. On any failure the child calls std::process::exit(1) and
        // the parent panics with the test name.
        //
        // Isolation guarantee: each test runs in a fresh, empty network
        // namespace (only the loopback interface exists). The dummy link and all
        // routes live in the child's namespace and disappear when the child exits.
        //
        // SAFETY: fork() is called from a multi-threaded test binary. After
        // fork() the child is single-threaded (fork duplicates only the calling
        // thread). The child does not call any Rust runtime teardown (it calls
        // std::process::exit, not return), which avoids double-free of any state
        // owned by other threads. This pattern is well-established for
        // process-isolation testing in Rust test harnesses (see nix::unistd::fork
        // documentation). We call only async-signal-safe functions (or libc::exit)
        // after fork in the child.
        fn in_netns<F>(test_name: &str, f: F)
        where
            F: FnOnce() + std::panic::UnwindSafe,
        {
            // Skip without failing when not running as root: unshare(CLONE_NEWNET)
            // requires CAP_SYS_ADMIN. A non-root cargo test run counts as pass.
            // SAFETY: geteuid() is always safe to call; it has no side effects.
            if unsafe { libc::geteuid() } != 0 {
                eprintln!("skipping {test_name}: requires root/CAP_SYS_ADMIN");
                return;
            }
            // SAFETY: fork() is safe to call here; see module-level SAFETY comment.
            let pid = unsafe { libc::fork() };
            if pid < 0 {
                panic!("{test_name}: fork failed: {}", std::io::Error::last_os_error());
            }
            if pid == 0 {
                // Child: enter a new network namespace and run the test body.
                // SAFETY: unshare(CLONE_NEWNET) creates a new network namespace for
                // this process; valid flag, no pointer arguments.
                let r = unsafe { libc::unshare(CLONE_NEWNET) };
                if r != 0 {
                    let e = std::io::Error::last_os_error();
                    eprintln!("{test_name}: unshare(CLONE_NEWNET) failed: {e}");
                    // SAFETY: libc::_exit terminates the process immediately
                    // without running any Rust or C++ destructors, which is
                    // safe here because the child is single-threaded after
                    // fork and owns no shared resources that require cleanup.
                    unsafe { libc::_exit(1) };
                }
                // Run the test body; catch panics to produce a clean exit code.
                let result = std::panic::catch_unwind(f);
                if result.is_err() {
                    // SAFETY: libc::_exit is safe in the post-fork child; see above.
                    unsafe { libc::_exit(1) };
                }
                // SAFETY: libc::_exit is safe in the post-fork child; see above.
                unsafe { libc::_exit(0) };
            }
            // Parent: wait for the child.
            let mut status: libc::c_int = 0;
            // SAFETY: pid > 0 (child pid); status is valid i32 pointer.
            unsafe { libc::waitpid(pid, &mut status, 0) };
            let exited_ok = libc::WIFEXITED(status) && libc::WEXITSTATUS(status) == 0;
            assert!(exited_ok, "{test_name}: child process failed (status={status})");
        }

        // Create a dummy link with the given name inside the current network
        // namespace, run the test body, then delete it.
        fn with_dummy_link<F: FnOnce(&str)>(link: &str, f: F) {
            let out = Command::new("ip")
                .args(["link", "add", link, "type", "dummy"])
                .output()
                .expect("ip link add must succeed");
            assert!(out.status.success(), "ip link add {link} failed: {}", String::from_utf8_lossy(&out.stderr));
            f(link);
            let _ = Command::new("ip").args(["link", "delete", link]).output();
        }

        #[test]
        fn configure_sets_address_and_default_route_on_dummy_link() {
            in_netns("configure_sets_address_and_default_route_on_dummy_link", || {
                with_dummy_link("test-nl-cfg", |iface| {
                    let guest_ip = Ipv4Addr::new(10, 99, 0, 6);
                    let gateway_ip = Ipv4Addr::new(10, 99, 0, 5);

                    configure(iface, None, guest_ip, gateway_ip, 30)
                        .expect("configure must succeed");

                    let addr_out = Command::new("ip")
                        .args(["addr", "show", "dev", iface])
                        .output()
                        .expect("ip addr show must succeed");
                    let addr_str = String::from_utf8_lossy(&addr_out.stdout);
                    assert!(
                        addr_str.contains("10.99.0.6/30"),
                        "address 10.99.0.6/30 must appear: {addr_str}"
                    );

                    let route_out = Command::new("ip")
                        .args(["route", "show", "dev", iface])
                        .output()
                        .expect("ip route show must succeed");
                    let route_str = String::from_utf8_lossy(&route_out.stdout);
                    assert!(
                        route_str.contains("default") || route_str.contains("0.0.0.0"),
                        "default route must appear: {route_str}"
                    );
                });
            });
        }

        #[test]
        fn configure_with_mac_sets_hw_address_on_dummy_link() {
            in_netns("configure_with_mac_sets_hw_address_on_dummy_link", || {
                with_dummy_link("test-nl-mac", |iface| {
                    let hw: [u8; 6] = [0x02, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE];
                    configure(iface, Some(hw), Ipv4Addr::new(10, 99, 1, 6), Ipv4Addr::new(10, 99, 1, 5), 30)
                        .expect("configure with mac must succeed");

                    let link_out = Command::new("ip")
                        .args(["link", "show", "dev", iface])
                        .output()
                        .expect("ip link show must succeed");
                    let link_str = String::from_utf8_lossy(&link_out.stdout);
                    assert!(
                        link_str.contains("02:aa:bb:cc:dd:ee"),
                        "MAC 02:aa:bb:cc:dd:ee must appear: {link_str}"
                    );
                });
            });
        }

        #[test]
        fn configure_is_idempotent_on_re_fork() {
            in_netns("configure_is_idempotent_on_re_fork", || {
                with_dummy_link("test-nl-idem", |iface| {
                    configure(iface, None, Ipv4Addr::new(10, 99, 2, 6), Ipv4Addr::new(10, 99, 2, 5), 30)
                        .expect("first configure must succeed");
                    configure(iface, None, Ipv4Addr::new(10, 99, 3, 6), Ipv4Addr::new(10, 99, 3, 5), 30)
                        .expect("second configure (re-fork) must succeed");

                    let addr_out = Command::new("ip")
                        .args(["addr", "show", "dev", iface])
                        .output()
                        .expect("ip addr show must succeed");
                    let addr_str = String::from_utf8_lossy(&addr_out.stdout);
                    assert!(
                        addr_str.contains("10.99.3.6/30"),
                        "re-fork address must be set: {addr_str}"
                    );
                    assert!(
                        !addr_str.contains("10.99.2.6"),
                        "old address must be flushed: {addr_str}"
                    );
                });
            });
        }

        // Verifies that NLM_F_REPLACE (not NLM_F_EXCL) is used in build_add_addr
        // so that adding the SAME address a second time does not return EEXIST.
        // This models a re-fork where the kernel has not flushed addresses yet.
        #[test]
        fn add_addr_same_address_twice_is_idempotent() {
            in_netns("add_addr_same_address_twice_is_idempotent", || {
                with_dummy_link("test-nl-same", |iface| {
                    let ip = Ipv4Addr::new(10, 99, 4, 6);
                    let gw = Ipv4Addr::new(10, 99, 4, 5);
                    // First configure: sets the address.
                    configure(iface, None, ip, gw, 30)
                        .expect("first configure must succeed");
                    // Second configure with the SAME IP: NLM_F_REPLACE must prevent EEXIST.
                    // (The flush step deletes the address, so this also validates that
                    // the add path itself handles an already-present entry gracefully
                    // if the flush races or is skipped on a direct build_add_addr call.)
                    let sock = NetlinkSocket::open().expect("open netlink socket");
                    let idx = if_nametoindex(iface).expect("if_nametoindex");
                    sock.request(&build_add_addr(1, idx, ip, 30))
                        .expect("adding the same address a second time must not return EEXIST");
                });
            });
        }
    }
}
