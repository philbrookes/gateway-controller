package controller

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

	configData, err := renderProxyConfig(&proxy.Spec, proxy.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render config: %w", err)
	}

	if err := r.reconcileConfigMap(ctx, &proxy, configData); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile configmap: %w", err)
	}

	revision, err := computeRevision(configData)
	if err != nil {
		return ctrl.Result{}, err
	}
	proxy.Status.DesiredRevision = revision
	proxy.Status.PublishedRevision = revision

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

func (r *ProxyReconciler) reconcileConfigMap(ctx context.Context, proxy *proxyv1alpha1.Proxy, configData string) error {
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(proxy),
			Namespace: proxy.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, desired, func() error {
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
