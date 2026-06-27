package facade

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	runv1 "mitos.run/mitos/api/v1"
)

const (
	// TemplateAnnotation is the bridge annotation stamped on our mitos.run
	// SandboxPool that links it back to the upstream
	// extensions.agents.x-k8s.io SandboxTemplate it was created from. It records
	// the bridge in the same single-annotation style as PoolAnnotation
	// (docs/adr/0001-facade-and-naming.md): the value is the upstream
	// SandboxTemplate name. After the API v1 consolidation (ADR 0007) the upstream
	// SandboxTemplate maps onto a v1 SandboxPool with an inline spec.template, so
	// the bridge annotation lives on that pool.
	TemplateAnnotation = "mitos.run/template"

	// WarmPoolAnnotation is the bridge annotation stamped on our mitos.run
	// SandboxPool that links it back to the upstream
	// extensions.agents.x-k8s.io SandboxWarmPool it was created from. The value is
	// the upstream SandboxWarmPool name.
	WarmPoolAnnotation = "mitos.run/warmpool"
)

// SandboxTemplateReconciler maps an upstream
// extensions.agents.x-k8s.io/v1beta1 SandboxTemplate onto our consolidated
// mitos.run/v1 SandboxPool with an inline spec.template (ADR 0007 folded the
// standalone mitos.run SandboxTemplate kind into the pool's inline template). It
// owns exactly one of our SandboxPool objects per upstream template (same name +
// namespace, owner-referenced for GC), mapping the upstream podTemplate's first
// container (image, command, env) onto the pool's spec.template fields. Unmapped
// upstream fields (volumeClaimTemplates, networkPolicy, securityContext, ports,
// multiple containers, envVarsInjectionPolicy, service) are documented justified
// exceptions in docs/facade-conformance.md (no silent divergence): the husk pool
// pins resources at build time and our engine is fork-from-snapshot, not
// pod-native.
//
// The bridged pool carries only the inline template (no warm autoscaler); a
// SandboxWarmPool referencing this template is mapped by the warm pool
// reconciler onto its own pool (named after the warm pool) carrying the
// warm-slot count. The template's pool and the warm pool's pool are distinct
// mitos.run objects bridged from distinct upstream objects.
type SandboxTemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxpools,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures our mitos.run SandboxPool mirrors the upstream template.
// Deletion is handled by the owner-reference garbage collector: our pool carries
// an owner reference to the upstream template, so deleting theirs GCs ours.
func (r *SandboxTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var src extv1beta1.SandboxTemplate
	if err := r.Get(ctx, req.NamespacedName, &src); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !src.DeletionTimestamp.IsZero() {
		// Owner-reference GC removes our pool; nothing to do.
		return ctrl.Result{}, nil
	}

	if err := r.ensureTemplate(ctx, &src); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("mirrored upstream SandboxTemplate", "template", req.NamespacedName)
	return ctrl.Result{}, nil
}

// ensureTemplate creates or updates our SandboxPool (inline template) for an
// upstream SandboxTemplate. Our pool is named after the upstream template, lives
// in the same namespace, and is owner-referenced to it (for GC + the watch
// back-link).
func (r *SandboxTemplateReconciler) ensureTemplate(ctx context.Context, src *extv1beta1.SandboxTemplate) error {
	pool := &runv1.SandboxPool{
		ObjectMeta: metaName(src.Name, src.Namespace),
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pool, func() error {
		if pool.Annotations == nil {
			pool.Annotations = map[string]string{}
		}
		pool.Annotations[TemplateAnnotation] = src.Name

		if pool.Spec.Template == nil {
			pool.Spec.Template = &runv1.PoolTemplateSpec{}
		}
		container := firstTemplateContainer(src)
		if container != nil {
			pool.Spec.Template.Image = container.Image
			pool.Spec.Template.Command = container.Command
			pool.Spec.Template.Env = container.Env
		}
		return controllerutil.SetControllerReference(src, pool, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure SandboxPool for upstream template %s/%s: %w", src.Namespace, src.Name, err)
	}
	return nil
}

// metaName builds an ObjectMeta naming an object in a given namespace; the
// bridged objects share name + namespace with their upstream source.
func metaName(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace}
}

// firstTemplateContainer returns the first container of the upstream template's
// podTemplate, or nil when the template carries none. Sandboxes are
// single-workload by construction; additional containers are a documented
// exception (docs/facade-conformance.md).
func firstTemplateContainer(src *extv1beta1.SandboxTemplate) *corev1.Container {
	containers := src.Spec.PodTemplate.Spec.Containers
	if len(containers) == 0 {
		return nil
	}
	return &containers[0]
}

// SetupWithManager wires the reconciler to watch upstream SandboxTemplates and
// own our SandboxPool objects.
func (r *SandboxTemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extv1beta1.SandboxTemplate{}).
		Owns(&runv1.SandboxPool{}).
		Complete(r)
}
