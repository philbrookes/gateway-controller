package controller

import (
	"context"
	"fmt"
	"net"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	proxyv1alpha1 "github.com/praxis-proxy/gateway-controller/api/proxy/v1alpha1"
)

const (
	containerName                 = "praxis"
	defaultTerminationGracePeriod = int64(30)
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
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

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

	startupRevision, err := computeStartupRevision(&proxy.Spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileDeployment(ctx, &proxy, startupRevision); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile deployment: %w", err)
	}

	if err := r.reconcileService(ctx, &proxy); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
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

// startupRevisionAnnotation changes only for settings that Praxis cannot reload.
const startupRevisionAnnotation = "praxis.io/startup-revision"

func (r *ProxyReconciler) reconcileDeployment(ctx context.Context, proxy *proxyv1alpha1.Proxy, revision string) error {
	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxy.Name,
			Namespace: proxy.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, desired, func() error {
		if err := ensureControlledBy(desired, proxy, schema.GroupResource{Group: "apps", Resource: "deployments"}); err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(proxy, desired, r.Scheme); err != nil {
			return err
		}
		replicas := int32(1)
		if proxy.Spec.Replicas != nil {
			replicas = *proxy.Spec.Replicas
		}
		desired.Spec.Replicas = &replicas
		desired.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: proxyLabels(proxy),
		}
		desired.Spec.Template = buildPodTemplate(proxy, revision)
		return nil
	})
	return err
}
func proxyLabels(proxy *proxyv1alpha1.Proxy) map[string]string {
	instance := proxy.Name
	if len(instance) > maxLabelValueLen {
		instance = instance[:maxLabelValueLen]
	}
	return map[string]string{
		"app.kubernetes.io/name":       containerName,
		"app.kubernetes.io/instance":   instance,
		"app.kubernetes.io/managed-by": "gateway-controller",
	}
}
func buildPodTemplate(proxy *proxyv1alpha1.Proxy, revision string) corev1.PodTemplateSpec {
	volumes, mounts := buildVolumes(proxy)
	gracePeriod := defaultTerminationGracePeriod
	container := corev1.Container{
		Name:                     containerName,
		Image:                    proxy.Spec.Image,
		Command:                  []string{containerName},
		Args:                     []string{"--config", configMountPath + "/" + configFileName},
		Ports:                    buildContainerPorts(proxy),
		VolumeMounts:             mounts,
		Env:                      buildEnvVars(proxy),
		ReadinessProbe:           buildReadinessProbe(proxy),
		ImagePullPolicy:          corev1.PullIfNotPresent,
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: proxyLabels(proxy),
			Annotations: map[string]string{
				startupRevisionAnnotation: revision,
			},
		},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{container},
			Volumes:                       volumes,
			RestartPolicy:                 corev1.RestartPolicyAlways,
			DNSPolicy:                     corev1.DNSClusterFirst,
			TerminationGracePeriodSeconds: &gracePeriod,
			SchedulerName:                 corev1.DefaultSchedulerName,
		},
	}
}
func buildVolumes(proxy *proxyv1alpha1.Proxy) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := []corev1.Volume{{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(proxy)},
			},
		},
	}}
	mounts := []corev1.VolumeMount{{
		Name:      configVolumeName,
		MountPath: configMountPath,
		ReadOnly:  true,
	}}

	for _, listener := range proxy.Spec.Listeners {
		if listener.TLS == nil {
			continue
		}
		volName := "tls-" + listener.Name
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: listener.TLS.CertificateRef.Name,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: fmt.Sprintf("%s/%s", tlsMountPrefix, listener.Name),
			ReadOnly:  true,
		})
	}
	return volumes, mounts
}

// buildReadinessProbe gates pod readiness on Praxis actually serving.
// The runtime has no dedicated health endpoint, so the probe is a TCP
// connect against a listener the kubelet can reach through the pod IP
// (it dials the pod IP from the node): the first listener bound to the
// dual-stack IPv6 wildcard, the only bind that answers the dial on
// either pod IP family. Listeners bound to the IPv4 wildcard (the
// default bind) or to specific addresses such as loopback cannot be
// reached through an IPv6 pod IP, so the controller renders a dedicated
// dual-stack readiness listener into the config and the probe targets
// that instead. The port is referenced by name in both cases, so the
// wiring survives port renumbering; a replacement pod therefore stays
// unready during startup until Praxis has bound the listener and can
// accept traffic.
func buildReadinessProbe(proxy *proxyv1alpha1.Proxy) *corev1.Probe {
	if len(proxy.Spec.Listeners) == 0 {
		return nil
	}
	portName := readinessListenerName(&proxy.Spec)
	if reachable := podIPListener(&proxy.Spec); reachable != nil {
		portName = reachable.Name
	}
	const (
		initialDelaySeconds = 5
		periodSeconds       = 5
		timeoutSeconds      = 2
		successThreshold    = 1
		failureThreshold    = 3
	)
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromString(portName),
			},
		},
		InitialDelaySeconds: initialDelaySeconds,
		PeriodSeconds:       periodSeconds,
		TimeoutSeconds:      timeoutSeconds,
		SuccessThreshold:    successThreshold,
		FailureThreshold:    failureThreshold,
	}
}
func buildContainerPorts(proxy *proxyv1alpha1.Proxy) []corev1.ContainerPort {
	ports := make([]corev1.ContainerPort, 0, len(proxy.Spec.Listeners))
	for _, listener := range proxy.Spec.Listeners {
		ports = append(ports, corev1.ContainerPort{
			Name:          listener.Name,
			ContainerPort: listener.Port,
			Protocol:      corev1.ProtocolTCP,
		})
	}
	// The readiness probe targets the controller-rendered readiness
	// listener when no spec listener is reachable through the pod IP;
	// declare its port so the probe's named reference resolves.
	if podIPListener(&proxy.Spec) == nil {
		ports = append(ports, corev1.ContainerPort{
			Name:          readinessListenerName(&proxy.Spec),
			ContainerPort: readinessPort(&proxy.Spec),
			Protocol:      corev1.ProtocolTCP,
		})
	}
	return ports
}
func buildEnvVars(proxy *proxyv1alpha1.Proxy) []corev1.EnvVar {
	if len(proxy.Spec.SecretEnv) == 0 {
		return nil
	}
	envVars := make([]corev1.EnvVar, 0, len(proxy.Spec.SecretEnv))
	for _, secretEnv := range proxy.Spec.SecretEnv {
		envVars = append(envVars, corev1.EnvVar{
			Name: secretEnv.Name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretEnv.SecretRef.Name},
					Key:                  secretEnv.SecretRef.Key,
				},
			},
		})
	}
	return envVars
}
func (r *ProxyReconciler) reconcileService(ctx context.Context, proxy *proxyv1alpha1.Proxy) error {
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxy.Name,
			Namespace: proxy.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, desired, func() error {
		if err := ensureControlledBy(desired, proxy, schema.GroupResource{Group: "", Resource: "services"}); err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(proxy, desired, r.Scheme); err != nil {
			return err
		}
		desired.Spec.Selector = proxyLabels(proxy)
		desired.Spec.Type = serviceType(proxy)
		desired.Spec.Ports = buildServicePorts(proxy)
		families, familyPolicy := serviceIPFamilies(proxy)
		desired.Spec.IPFamilies = families
		desired.Spec.IPFamilyPolicy = &familyPolicy
		return nil
	})
	return err
}

// serviceIPFamilies returns the address families the proxy's Service
// must allocate cluster IPs in, and the policy that requests them, so a
// published Service only carries traffic the proxy's application
// listeners can accept.
//
// A listener binds IPv4 unless it names an IPv6 address: the spec default
// (and the renderer's default bind) is the IPv4 wildcard, which never
// accepts a connection to an IPv6 address, while the IPv6 wildcard ::
// accepts both families. The Service therefore follows the listeners.
// IPv4-only binds keep the IPv4 family — today's behavior on IPv4
// clusters, and a compatible choice on dual-stack clusters whose primary
// family is IPv6, where a defaulted Service would be IPv6 and its
// traffic unreachable for the default binds. Explicit IPv6 binds move
// the Service to IPv6, and a mix — or a dual-stack wildcard — asks for
// both families, which single-family clusters trim at admission.
// Clusters without any requested family reject the Service instead of
// publishing an unreachable one.
func serviceIPFamilies(proxy *proxyv1alpha1.Proxy) ([]corev1.IPFamily, corev1.IPFamilyPolicy) {
	v4, v6 := false, false
	for i := range proxy.Spec.Listeners {
		l4, l6 := listenerBindFamilies(proxy.Spec.Listeners[i].BindAddress)
		v4, v6 = v4 || l4, v6 || l6
	}
	switch {
	case v4 && v6:
		return []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, corev1.IPFamilyPolicyPreferDualStack
	case v6:
		return []corev1.IPFamily{corev1.IPv6Protocol}, corev1.IPFamilyPolicySingleStack
	default:
		return []corev1.IPFamily{corev1.IPv4Protocol}, corev1.IPFamilyPolicySingleStack
	}
}

// listenerBindFamilies reports which address families a listener's bind
// address accepts. A nil bind renders as the IPv4 wildcard. An unparseable
// bind cannot name an IPv6 address, so it is treated like the IPv4 default
// rather than pinning the Service to a family the listener may not accept.
func listenerBindFamilies(bind *string) (v4, v6 bool) {
	if bind == nil {
		return true, false
	}
	ip := net.ParseIP(*bind)
	switch {
	case ip == nil, ip.To4() != nil:
		return true, false
	case ip.IsUnspecified():
		return true, true
	default:
		return false, true
	}
}
func serviceType(proxy *proxyv1alpha1.Proxy) corev1.ServiceType {
	if proxy.Spec.Service != nil && proxy.Spec.Service.Type != nil {
		return corev1.ServiceType(*proxy.Spec.Service.Type)
	}
	return corev1.ServiceTypeClusterIP
}
func buildServicePorts(proxy *proxyv1alpha1.Proxy) []corev1.ServicePort {
	ports := make([]corev1.ServicePort, 0, len(proxy.Spec.Listeners))
	for _, listener := range proxy.Spec.Listeners {
		ports = append(ports, corev1.ServicePort{
			Name:       listener.Name,
			Port:       listener.Port,
			TargetPort: intstr.FromString(listener.Name),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return ports
}
func (r *ProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1alpha1.Proxy{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
