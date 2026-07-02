// Command net-fork-smoke drives the real KVM-backed fork engine with per-fork
// networking enabled to prove that each fork gets a DISTINCT guest network
// identity (fork-correctness row 4): two forks of one snapshot must end up with
// different guest eth0 MAC addresses and different guest IPs, and neither MAC may
// be the shared placeholder baked into the template snapshot. Each fork's MAC is
// the locally-administered address derived from its sandbox id (netconf), set on
// eth0 by the guest agent over the NotifyForked network config.
//
// This binary only does real work on a KVM host (it needs /dev/kvm plus the host
// network stack: tap creation and nftables, exactly as forkd uses). It compiles
// on any platform so cross-build checks pass. A setup error exits 2; an assertion
// failure exits 1.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/guestgrpc"
	"mitos.run/mitos/internal/netconf"
	"mitos.run/mitos/internal/network"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

const placeholderMAC = "02:00:00:00:00:01"

func main() {
	image := flag.String("image", "", "rootfs.ext4 path (agent as /init, with busybox) to build the template from")
	dataDir := flag.String("data-dir", "", "engine data directory")
	fcBin := flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
	kernel := flag.String("kernel", "", "path to the guest kernel (vmlinux)")
	agentBin := flag.String("agent-bin", "", "path to the guest agent binary")
	flag.Parse()
	if *image == "" || *dataDir == "" || *kernel == "" || *agentBin == "" {
		fmt.Fprintln(os.Stderr, "net-fork-smoke: --image, --data-dir, --kernel and --agent-bin are required")
		os.Exit(2)
	}
	if err := run(*image, *dataDir, *fcBin, *kernel, *agentBin); err != nil {
		fmt.Fprintf(os.Stderr, "net-fork-smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("net-fork-smoke: PASS: two forks have distinct guest MAC + IP, neither the placeholder")
}

func run(image, dataDir, fcBin, kernel, agentBin string) error {
	alloc, err := netconf.NewAllocator("10.202.0.0/24", "nfsmoke")
	if err != nil {
		return setupErr(fmt.Errorf("new allocator: %w", err))
	}
	mgr := network.NewManager(network.Options{SubnetCIDR: "10.202.0.0/16", EnableForwarding: true})

	engine, err := fork.NewEngine(dataDir, fcBin, kernel, firecracker.JailerConfig{}, fork.EngineOpts{
		AllowUnverified: true,
		AgentBinPath:    agentBin,
		NetManager:      mgr,
		NetAllocator:    alloc,
	})
	if err != nil {
		return setupErr(fmt.Errorf("new engine: %w", err))
	}

	templateID := "nf-tmpl"
	if err := engine.CreateTemplate(templateID, image, nil, nil, nil, nil, false); err != nil {
		return setupErr(fmt.Errorf("create template: %w", err))
	}

	netOpts := &fork.NetworkOpts{EgressPolicy: "deny"}
	type forkRes struct{ id, mac, ip string }
	var results []forkRes
	for _, id := range []string{"nf-fork-a", "nf-fork-b"} {
		res, err := engine.Fork(templateID, id, fork.ForkOpts{Network: netOpts})
		if err != nil {
			return setupErr(fmt.Errorf("fork %s: %w", id, err))
		}
		defer func(sid string) { _ = engine.Terminate(sid) }(id)

		client, err := connect(res.VsockPath)
		if err != nil {
			return setupErr(fmt.Errorf("connect to %s guest: %w", id, err))
		}
		// Deliver the fork-correctness handshake the daemon would: the reseed
		// entropy PLUS this fork's network config. The agent's NotifyForked
		// handler applies the per-fork MAC + IP to eth0 (the behavior under test).
		entropy := make([]byte, 32)
		if _, err := rand.Read(entropy); err != nil {
			_ = client.Close()
			return setupErr(fmt.Errorf("entropy: %w", err))
		}

		var protoNetwork *internalv1.NotifyForkedNetwork
		if res.GuestNetwork != nil {
			protoNetwork = &internalv1.NotifyForkedNetwork{
				GuestIp:    res.GuestNetwork.GuestIP,
				GatewayIp:  res.GuestNetwork.GatewayIP,
				PrefixLen:  int32(res.GuestNetwork.PrefixLen), //nolint:gosec // network prefix (0-32)
				GuestMac:   res.GuestNetwork.GuestMAC,
				ResolverIp: res.GuestNetwork.ResolverIP,
			}
		}
		ctx := context.Background()
		resp, err := client.Control.NotifyForked(ctx, &internalv1.NotifyForkedRequest{
			Generation:         1,
			HostWallClockNanos: time.Now().UnixNano(),
			Entropy:            entropy,
			Network:            protoNetwork,
		})
		if err != nil {
			_ = client.Close()
			return setupErr(fmt.Errorf("notify-forked %s: %w", id, err))
		}
		if resp == nil || !resp.GetReseededRng() {
			_ = client.Close()
			return fmt.Errorf("%s did not reseed after fork", id)
		}
		mac, err := execOut(client, "cat /sys/class/net/eth0/address")
		if err != nil {
			_ = client.Close()
			return fmt.Errorf("read %s eth0 MAC: %w", id, err)
		}
		ipOut, err := execOut(client, "ip -o -4 addr show dev eth0 | awk '{print $4}'")
		if err != nil {
			_ = client.Close()
			return fmt.Errorf("read %s eth0 IP: %w", id, err)
		}
		_ = client.Close()
		mac = strings.TrimSpace(strings.ToLower(mac))
		ipOut = strings.TrimSpace(ipOut)
		fmt.Printf("net-fork-smoke: %s eth0 MAC=%s IP=%s\n", id, mac, ipOut)
		results = append(results, forkRes{id: id, mac: mac, ip: ipOut})
	}

	a, b := results[0], results[1]
	if a.mac == "" || b.mac == "" {
		return fmt.Errorf("a fork reported an empty eth0 MAC (a=%q b=%q)", a.mac, b.mac)
	}
	if a.mac == placeholderMAC || b.mac == placeholderMAC {
		return fmt.Errorf("a fork still has the shared placeholder MAC %s (a=%s b=%s): per-fork MAC not applied", placeholderMAC, a.mac, b.mac)
	}
	if a.mac == b.mac {
		return fmt.Errorf("two forks share the same guest MAC %s: per-fork MAC reissue violated", a.mac)
	}
	if a.ip == "" || b.ip == "" || a.ip == b.ip {
		return fmt.Errorf("two forks do not have distinct guest IPs (a=%q b=%q)", a.ip, b.ip)
	}
	fmt.Printf("net-fork-smoke: distinct MAC (%s != %s) and IP (%s != %s)\n", a.mac, b.mac, a.ip, b.ip)
	return nil
}

func setupErr(err error) error {
	fmt.Fprintf(os.Stderr, "net-fork-smoke: SETUP: %v\n", err)
	os.Exit(2)
	return err
}

func execOut(client *guestgrpc.Client, command string) (string, error) {
	ctx := context.Background()
	stream, err := client.Sandbox.ExecStream(ctx, &sandboxv1.ExecStreamRequest{
		Command:        command,
		Cwd:            "/",
		TimeoutSeconds: 60,
	})
	if err != nil {
		return "", fmt.Errorf("exec stream: %w", err)
	}
	var stdout, stderr strings.Builder
	var exitCode int32
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("recv exec frame: %w", err)
		}
		switch m := msg.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			stdout.Write(m.Stdout)
		case *sandboxv1.ExecResponse_Stderr:
			stderr.Write(m.Stderr)
		case *sandboxv1.ExecResponse_Exit:
			exitCode = m.Exit.GetExitCode()
			if spawnErr := m.Exit.GetError(); spawnErr != "" {
				return stdout.String(), fmt.Errorf("exec spawn error: %s", spawnErr)
			}
		}
	}
	if exitCode != 0 {
		return stdout.String(), fmt.Errorf("command %q exited %d: %s", command, exitCode, stderr.String())
	}
	return stdout.String(), nil
}

func connect(udsPath string) (*guestgrpc.Client, error) {
	ctx := context.Background()
	return guestgrpc.WaitReady(ctx, udsPath, 30*time.Second)
}
