package agentcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// realProbe is the production DoctorProbe. It splits into a node layer (reads
// /dev/kvm, /proc/modules, and the staged guest kernel path; only meaningful on
// a Linux KVM node) and a cluster layer (reads Secrets and the namespace label
// through the controller-runtime client). On a non-Linux workstation the node
// reads simply report "not present", which surfaces honestly as a failing check;
// the meaningful run is on a KVM node or as an in-cluster Job.
//
// Security: every cluster read fetches an object's METADATA (Secret name,
// namespace label) to confirm presence; it never reads or returns a Secret's
// data, so no secret value can reach a report.
type realProbe struct {
	client     client.Client
	namespace  string
	kernelPath string
	// modulesPath and kvmPath are seams for tests of the node layer; production
	// leaves them at the Linux defaults.
	modulesPath string
	kvmPath     string
}

// DoctorProbeConfig configures the real probe.
type DoctorProbeConfig struct {
	// Client is the controller-runtime client for the cluster checks. Nil skips
	// the cluster checks (they report a probe error, surfaced as a failing
	// check), so `mitos doctor` still runs the node checks without a kubeconfig.
	Client client.Client
	// Namespace is the install/pool namespace the cluster checks target.
	Namespace string
	// KernelPath is where forkd boots the guest kernel from (default
	// /var/lib/mitos/vmlinux), matched to the forkd --kernel flag.
	KernelPath string
}

const defaultKernelPath = "/var/lib/mitos/vmlinux"

// NewRealProbe builds the production probe from cfg.
func NewRealProbe(cfg DoctorProbeConfig) DoctorProbe {
	ns := cfg.Namespace
	if ns == "" {
		ns = "mitos"
	}
	kp := cfg.KernelPath
	if kp == "" {
		kp = defaultKernelPath
	}
	return &realProbe{
		client:      cfg.Client,
		namespace:   ns,
		kernelPath:  kp,
		modulesPath: "/proc/modules",
		kvmPath:     "/dev/kvm",
	}
}

func (p *realProbe) Namespace() string { return p.namespace }

func (p *realProbe) KVMDevice(context.Context) (bool, error) {
	info, err := os.Stat(p.kvmPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	// A usable KVM device is a character device.
	return info.Mode()&os.ModeCharDevice != 0, nil
}

func (p *realProbe) KernelModuleLoaded(_ context.Context, name string) (bool, error) {
	f, err := os.Open(p.modulesPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No /proc/modules: either not Linux, or a fully built-in kernel. We
			// cannot confirm the module is loaded, so report absent rather than
			// claim a pass we did not verify.
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	return moduleListed(f, name), nil
}

// moduleListed scans a /proc/modules-format stream for a module by name (the
// first field of each line).
func moduleListed(r io.Reader, name string) bool {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

func (p *realProbe) GuestKernelStaged(context.Context) (bool, string, error) {
	info, err := os.Stat(p.kernelPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, p.kernelPath, nil
		}
		return false, p.kernelPath, err
	}
	// A staged kernel is a regular, non-empty file.
	return info.Mode().IsRegular() && info.Size() > 0, p.kernelPath, nil
}

func (p *realProbe) PKISecretPresent(ctx context.Context, name string) (bool, error) {
	return p.secretPresent(ctx, name)
}

// secretPresent fetches a Secret's metadata to confirm it exists. It reads the
// object but never inspects its Data, so no secret value is logged or returned.
func (p *realProbe) secretPresent(ctx context.Context, name string) (bool, error) {
	if p.client == nil {
		return false, fmt.Errorf("no cluster client configured; run with a kubeconfig or as an in-cluster Job")
	}
	var s corev1.Secret
	err := p.client.Get(ctx, client.ObjectKey{Namespace: p.namespace, Name: name}, &s)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (p *realProbe) ImagePullSecretPresent(ctx context.Context) (bool, error) {
	if p.client == nil {
		return false, fmt.Errorf("no cluster client configured; run with a kubeconfig or as an in-cluster Job")
	}
	var list corev1.SecretList
	if err := p.client.List(ctx, &list, client.InNamespace(p.namespace)); err != nil {
		return false, err
	}
	for i := range list.Items {
		// Identify a pull secret by TYPE only; its data is never read.
		if list.Items[i].Type == corev1.SecretTypeDockerConfigJson {
			return true, nil
		}
	}
	return false, nil
}

func (p *realProbe) PoolNamespacePSA(ctx context.Context) (bool, string, error) {
	if p.client == nil {
		return false, "", fmt.Errorf("no cluster client configured; run with a kubeconfig or as an in-cluster Job")
	}
	var ns corev1.Namespace
	if err := p.client.Get(ctx, client.ObjectKey{Name: p.namespace}, &ns); err != nil {
		return false, "", err
	}
	label := ns.Labels["pod-security.kubernetes.io/enforce"]
	return label == "privileged", label, nil
}

// Doctor runs the preflight against probe, writes the report to out, and returns
// the process exit code (0 on pass/warn-only, non-zero if any check failed).
// cmd/mitos builds the real probe and calls this; the testable runDoctor core is
// the same code path.
func Doctor(ctx context.Context, probe DoctorProbe, out, errw io.Writer) int {
	return runDoctor(ctx, probe, out, errw)
}
