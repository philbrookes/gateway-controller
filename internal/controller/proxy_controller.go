package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	proxyv1alpha1 "github.com/praxis-proxy/gateway-controller/api/proxy/v1alpha1"
)

type ProxyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=praxis.io,resources=proxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=praxis.io,resources=proxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=praxis.io,resources=proxies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=praxis.io,resources=policies,verbs=get;list;watch

func (r *ProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var proxy proxyv1alpha1.Proxy
	if err := r.Get(ctx, req.NamespacedName, &proxy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	patch := client.MergeFrom(proxy.DeepCopy())

	logger.V(1).Info("reconciling proxy")

	proxy.Status.ObservedGeneration = proxy.Generation

	if validationErrs := validateProxySpec(&proxy.Spec); len(validationErrs) > 0 {
		setCondition(&proxy.Status.Conditions, proxyv1alpha1.ConditionTypeAccepted,
			metav1.ConditionFalse, "ValidationError", joinValidationErrors(validationErrs), proxy.Generation)
		if err := r.Status().Patch(ctx, &proxy, patch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	setCondition(&proxy.Status.Conditions, proxyv1alpha1.ConditionTypeAccepted,
		metav1.ConditionTrue, "Valid", "Proxy specification is valid", proxy.Generation)

	rendered, err := renderProxyConfig(ctx, r.Client, &proxy.Spec, proxy.Namespace)
	if err != nil {
		setCondition(&proxy.Status.Conditions, proxyv1alpha1.ConditionTypeAccepted,
			metav1.ConditionFalse, "ConfigRenderError", err.Error(), proxy.Generation)
		setCondition(&proxy.Status.Conditions, proxyv1alpha1.ConditionTypeResolvedRefs,
			metav1.ConditionFalse, "ConfigRenderError", err.Error(), proxy.Generation)
		if patchErr := r.Status().Patch(ctx, &proxy, patch); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}
	setCondition(&proxy.Status.Conditions, proxyv1alpha1.ConditionTypeResolvedRefs,
		metav1.ConditionTrue, "ResolvedRefs", "All references resolved", proxy.Generation)

	// The policy documents collected while rendering are carried through
	// the controller for the publication work that owns them (#19, #20).
	logger.V(1).Info("rendered praxis configuration",
		"policyDocuments", len(rendered.PolicyDocuments))
	configData := rendered.Config

	if err := r.reconcileConfigMap(ctx, &proxy, configData); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile configmap: %w", err)
	}

	setCondition(&proxy.Status.Conditions, proxyv1alpha1.ConditionTypeProgrammed,
		metav1.ConditionTrue, "ConfigPublished", "Configuration rendered to ConfigMap", proxy.Generation)

	if err := r.Status().Patch(ctx, &proxy, patch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func configMapName(proxy *proxyv1alpha1.Proxy) string {
	return proxy.Name + "-config"
}

// ensureControlledBy guards a reconciliation callback against adopting or
// overwriting objects this Proxy does not control. Objects that have not
// been persisted yet (the create path of controllerutil.CreateOrUpdate,
// where the object was never fetched) always pass. An existing object with
// no controller owner, or one controlled by a different controller, yields
// a conflict error and is never written.
func ensureControlledBy(obj client.Object, proxy *proxyv1alpha1.Proxy, resource schema.GroupResource) error {
	created := obj.GetCreationTimestamp()
	if created.IsZero() {
		return nil
	}
	if metav1.IsControlledBy(obj, proxy) {
		return nil
	}
	ownerDesc := "none"
	if ref := metav1.GetControllerOf(obj); ref != nil {
		ownerDesc = fmt.Sprintf("%s %q (uid %s)", ref.Kind, ref.Name, ref.UID)
	}
	return apierrors.NewConflict(resource, obj.GetName(),
		fmt.Errorf("object exists but is not controlled by proxy %s/%s (controller owner: %s); refusing to adopt or overwrite",
			proxy.Namespace, proxy.Name, ownerDesc))
}

// reconcileConfigMap publishes the live configuration at a stable name. Pods
// mount the directory so Kubernetes' atomic projection updates reach Praxis's
// config watcher without replacing the pod template.
func (r *ProxyReconciler) reconcileConfigMap(ctx context.Context, proxy *proxyv1alpha1.Proxy, configData string) error {
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(proxy),
			Namespace: proxy.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, desired, func() error {
		if err := ensureControlledBy(desired, proxy, schema.GroupResource{Group: "", Resource: "configmaps"}); err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(proxy, desired, r.Scheme); err != nil {
			return err
		}
		desired.Data = map[string]string{configFileName: configData}
		return nil
	})
	return err
}
func (r *ProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1alpha1.Proxy{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
