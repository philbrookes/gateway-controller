package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	proxyv1alpha1 "github.com/praxis-proxy/gateway-controller/api/proxy/v1alpha1"
	"github.com/praxis-proxy/gateway-controller/internal/controller"
)

const proxyReconcileTimeout = 5 * time.Second

func validProxy(name string) *proxyv1alpha1.Proxy {
	return &proxyv1alpha1.Proxy{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-",
			Namespace:    "default",
		},
		Spec: proxyv1alpha1.ProxySpec{
			Image: "ghcr.io/praxis-proxy/praxis:latest",
			FilterChains: []proxyv1alpha1.ProxyFilterChain{{
				Name: "main",
				Filters: []proxyv1alpha1.PraxisFilterEntry{{
					Name:         "rl",
					InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"},
				}},
			}},
			Listeners: []proxyv1alpha1.PraxisListener{{
				Name:         "http",
				Port:         8080,
				FilterChains: []string{"main"},
			}},
		},
	}
}

func acceptedCondition(ctx context.Context, proxy *proxyv1alpha1.Proxy) *metav1.Condition {
	got := &proxyv1alpha1.Proxy{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(proxy), got)).To(Succeed())
	proxy.Status = got.Status
	proxy.ResourceVersion = got.ResourceVersion
	proxy.Generation = got.Generation
	return meta.FindStatusCondition(got.Status.Conditions, proxyv1alpha1.ConditionTypeAccepted)
}

func createProxy(ctx context.Context, proxy *proxyv1alpha1.Proxy) {
	Expect(k8sClient.Create(ctx, proxy)).To(Succeed())
	DeferCleanup(func(ctx context.Context) error {
		return client.IgnoreNotFound(k8sClient.Delete(ctx, proxy))
	})
}

// policyRefEntry returns a filter entry referencing a Policy in the
// proxy's namespace.
func policyRefEntry(name string) proxyv1alpha1.PraxisFilterEntry {
	return proxyv1alpha1.PraxisFilterEntry{
		Name: "policy",
		Ref: &proxyv1alpha1.PraxisFilterReference{
			Group: proxyv1alpha1.PraxisGroup, Kind: proxyv1alpha1.PolicyKind, Name: name,
		},
	}
}

var _ = Describe("Proxy Controller", func() {
	expectValidationError := func(ctx context.Context, proxy *proxyv1alpha1.Proxy, substring string) {
		Eventually(func(gom Gomega, ctx context.Context) {
			cond := acceptedCondition(ctx, proxy)
			gom.Expect(cond).NotTo(BeNil())
			gom.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			gom.Expect(cond.Reason).To(Equal("ValidationError"))
			gom.Expect(cond.Message).To(ContainSubstring(substring))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	}

	It("should set Accepted=True for a valid proxy spec", func(ctx context.Context) {
		proxy := validProxy("valid")
		createProxy(ctx, proxy)

		Eventually(func(gom Gomega, ctx context.Context) {
			cond := acceptedCondition(ctx, proxy)
			gom.Expect(cond).NotTo(BeNil())
			gom.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			gom.Expect(cond.Reason).To(Equal("Valid"))
			gom.Expect(proxy.Status.ObservedGeneration).To(Equal(proxy.Generation))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should reject duplicate entry names within a chain", func(ctx context.Context) {
		proxy := validProxy("dup-entry")
		proxy.Spec.FilterChains = []proxyv1alpha1.ProxyFilterChain{{
			Name: "chain",
			Filters: []proxyv1alpha1.PraxisFilterEntry{
				{Name: "dup", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
				{Name: "dup", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
			},
		}}
		proxy.Spec.Listeners[0].FilterChains = []string{"chain"}
		createProxy(ctx, proxy)

		expectValidationError(ctx, proxy, "duplicate entry name")
	})

	It("should reject terminal filter that is not last in chain", func(ctx context.Context) {
		proxy := validProxy("terminal-not-last")
		proxy.Spec.FilterChains = []proxyv1alpha1.ProxyFilterChain{{
			Name: "chain",
			Filters: []proxyv1alpha1.PraxisFilterEntry{
				{Name: "term", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "iterative_request_router"}},
				{Name: "after", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
			},
		}}
		proxy.Spec.Listeners[0].FilterChains = []string{"chain"}
		createProxy(ctx, proxy)

		expectValidationError(ctx, proxy, "terminal filter")
	})

	It("should reject listener referencing undefined chain", func(ctx context.Context) {
		proxy := validProxy("undefined-chain-ref")
		proxy.Spec.FilterChains = []proxyv1alpha1.ProxyFilterChain{{
			Name: "real",
			Filters: []proxyv1alpha1.PraxisFilterEntry{{
				Name: "f", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"},
			}},
		}}
		proxy.Spec.Listeners[0].FilterChains = []string{"real", "ghost"}
		createProxy(ctx, proxy)

		Eventually(func(gom Gomega, ctx context.Context) {
			cond := acceptedCondition(ctx, proxy)
			gom.Expect(cond).NotTo(BeNil())
			gom.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			gom.Expect(cond.Reason).To(Equal("ValidationError"))
			gom.Expect(cond.Message).To(ContainSubstring("undefined chain"))
			gom.Expect(cond.Message).To(ContainSubstring("ghost"))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should report multiple validation errors together", func(ctx context.Context) {
		proxy := validProxy("multi-error")
		proxy.Spec.FilterChains = []proxyv1alpha1.ProxyFilterChain{{
			Name: "chain",
			Filters: []proxyv1alpha1.PraxisFilterEntry{
				{Name: "dup", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
				{Name: "dup", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
			},
		}}
		proxy.Spec.Listeners[0].FilterChains = []string{"missing"}
		createProxy(ctx, proxy)

		Eventually(func(gom Gomega, ctx context.Context) {
			cond := acceptedCondition(ctx, proxy)
			gom.Expect(cond).NotTo(BeNil())
			gom.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			gom.Expect(cond.Reason).To(Equal("ValidationError"))
			gom.Expect(cond.Message).To(ContainSubstring("duplicate entry name"))
			gom.Expect(cond.Message).To(ContainSubstring("undefined chain"))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should transition Accepted from False to True when spec is fixed", func(ctx context.Context) {
		proxy := validProxy("fix-transition")
		proxy.Spec.FilterChains = []proxyv1alpha1.ProxyFilterChain{{
			Name: "chain",
			Filters: []proxyv1alpha1.PraxisFilterEntry{
				{Name: "term", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "iterative_request_router"}},
				{Name: "after", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
			},
		}}
		proxy.Spec.Listeners[0].FilterChains = []string{"chain"}
		createProxy(ctx, proxy)

		Eventually(func(gom Gomega, ctx context.Context) {
			cond := acceptedCondition(ctx, proxy)
			gom.Expect(cond).NotTo(BeNil())
			gom.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())

		proxy.Spec.FilterChains[0].Filters = []proxyv1alpha1.PraxisFilterEntry{
			{Name: "after", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
			{Name: "term", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "iterative_request_router"}},
		}
		Expect(k8sClient.Update(ctx, proxy)).To(Succeed())

		Eventually(func(gom Gomega, ctx context.Context) {
			cond := acceptedCondition(ctx, proxy)
			gom.Expect(cond).NotTo(BeNil())
			gom.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			gom.Expect(proxy.Status.ObservedGeneration).To(Equal(proxy.Generation))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should transition Accepted from True to False when spec becomes invalid", func(ctx context.Context) {
		proxy := validProxy("regress-transition")
		createProxy(ctx, proxy)

		Eventually(func(gom Gomega, ctx context.Context) {
			cond := acceptedCondition(ctx, proxy)
			gom.Expect(cond).NotTo(BeNil())
			gom.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())

		proxy.Spec.FilterChains = []proxyv1alpha1.ProxyFilterChain{{
			Name: "main",
			Filters: []proxyv1alpha1.PraxisFilterEntry{
				{Name: "term", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "iterative_request_router"}},
				{Name: "after", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
			},
		}}
		Expect(k8sClient.Update(ctx, proxy)).To(Succeed())

		Eventually(func(gom Gomega, ctx context.Context) {
			cond := acceptedCondition(ctx, proxy)
			gom.Expect(cond).NotTo(BeNil())
			gom.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			gom.Expect(cond.Reason).To(Equal("ValidationError"))
			gom.Expect(proxy.Status.ObservedGeneration).To(Equal(proxy.Generation))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should create a ConfigMap with rendered config for a valid proxy", func(ctx context.Context) {
		proxy := validProxy("cm-create")
		createProxy(ctx, proxy)

		Eventually(func(gom Gomega, ctx context.Context) {
			cm := &corev1.ConfigMap{}
			gom.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      proxy.Name + "-config",
				Namespace: proxy.Namespace,
			}, cm)).To(Succeed())
			gom.Expect(cm.Data).To(HaveKey("config.yaml"))
			gom.Expect(cm.Data["config.yaml"]).To(ContainSubstring("listeners"))
			gom.Expect(cm.OwnerReferences).To(HaveLen(1))
			gom.Expect(cm.OwnerReferences[0].Name).To(Equal(proxy.Name))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should set Programmed condition for a valid proxy", func(ctx context.Context) {
		proxy := validProxy("programmed")
		createProxy(ctx, proxy)

		Eventually(func(gom Gomega, ctx context.Context) {
			got := &proxyv1alpha1.Proxy{}
			gom.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(proxy), got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, proxyv1alpha1.ConditionTypeProgrammed)
			gom.Expect(cond).NotTo(BeNil())
			gom.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			gom.Expect(cond.Reason).To(Equal("ConfigPublished"))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should not create ConfigMap for invalid proxy", func(ctx context.Context) {
		proxy := validProxy("cm-no-create")
		proxy.Spec.FilterChains = []proxyv1alpha1.ProxyFilterChain{{
			Name: "chain",
			Filters: []proxyv1alpha1.PraxisFilterEntry{
				{Name: "dup", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
				{Name: "dup", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}},
			},
		}}
		proxy.Spec.Listeners[0].FilterChains = []string{"chain"}
		createProxy(ctx, proxy)

		expectValidationError(ctx, proxy, "duplicate entry name")

		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      proxy.Name + "-config",
			Namespace: proxy.Namespace,
		}, cm)
		Expect(err).To(HaveOccurred())
	})

	It("should not adopt a pre-existing ConfigMap it does not control", func(ctx context.Context) {
		const proxyName = "adopt-guard"
		// A user-managed ConfigMap already occupies the name the controller
		// would use for this proxy's rendered config.
		foreign := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      proxyName + "-config",
				Namespace: "default",
			},
			Data: map[string]string{"config.yaml": "user-managed"},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func(ctx context.Context) error {
			return client.IgnoreNotFound(k8sClient.Delete(ctx, foreign))
		})

		proxy := validProxy(proxyName)
		proxy.GenerateName = ""
		proxy.Name = proxyName
		createProxy(ctx, proxy)

		// The controller must never overwrite or adopt the foreign ConfigMap,
		// and reconciliation must not proceed to the Deployment.
		Consistently(func(gom Gomega, ctx context.Context) {
			got := &corev1.ConfigMap{}
			gom.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      proxyName + "-config",
				Namespace: "default",
			}, got)).To(Succeed())
			gom.Expect(got.Data["config.yaml"]).To(Equal("user-managed"))
			gom.Expect(metav1.GetControllerOf(got)).To(BeNil())

			deploy := &appsv1.Deployment{}
			gom.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      proxyName,
				Namespace: "default",
			}, deploy)).NotTo(Succeed())
		}).WithContext(ctx).WithTimeout(2 * time.Second).Should(Succeed())
	})

	It("should set Accepted=False and ResolvedRefs=False for unrenderable filter", func(ctx context.Context) {
		proxy := validProxy("render-error")
		proxy.Spec.FilterChains[0].Filters[0] = proxyv1alpha1.PraxisFilterEntry{
			Name: "unknown",
			Ref: &proxyv1alpha1.PraxisFilterReference{
				Group: "other.io",
				Kind:  "CustomFilter",
				Name:  "my-filter",
			},
		}
		createProxy(ctx, proxy)

		Eventually(func(gom Gomega, ctx context.Context) {
			got := &proxyv1alpha1.Proxy{}
			gom.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(proxy), got)).To(Succeed())
			accepted := meta.FindStatusCondition(got.Status.Conditions, proxyv1alpha1.ConditionTypeAccepted)
			gom.Expect(accepted).NotTo(BeNil())
			gom.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
			gom.Expect(accepted.Reason).To(Equal("ConfigRenderError"))
			gom.Expect(accepted.Message).To(ContainSubstring("unsupported filter"))
			resolved := meta.FindStatusCondition(got.Status.Conditions, proxyv1alpha1.ConditionTypeResolvedRefs)
			gom.Expect(resolved).NotTo(BeNil())
			gom.Expect(resolved.Status).To(Equal(metav1.ConditionFalse))
			gom.Expect(resolved.Reason).To(Equal("ConfigRenderError"))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should set Accepted=False and ResolvedRefs=False for an unresolved policy reference", func(ctx context.Context) {
		proxy := validProxy("unresolved-policy")
		proxy.Spec.FilterChains[0].Filters[0] = policyRefEntry("missing")
		createProxy(ctx, proxy)
		Eventually(func(gom Gomega, ctx context.Context) {
			got := &proxyv1alpha1.Proxy{}
			gom.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(proxy), got)).To(Succeed())
			for _, conditionType := range []string{
				proxyv1alpha1.ConditionTypeAccepted, proxyv1alpha1.ConditionTypeResolvedRefs,
			} {
				condition := meta.FindStatusCondition(got.Status.Conditions, conditionType)
				gom.Expect(condition).NotTo(BeNil())
				gom.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				gom.Expect(condition.Reason).To(Equal("ConfigRenderError"))
				gom.Expect(condition.Message).To(ContainSubstring("resolve policy"))
			}
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
		for _, object := range []client.Object{&corev1.ConfigMap{}} {
			key := client.ObjectKeyFromObject(proxy)
			if _, ok := object.(*corev1.ConfigMap); ok {
				key.Name += "-config"
			}
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, object))).To(BeTrue())
		}
	})

	It("should publish policy filter configuration for resolved policy references", func(ctx context.Context) {
		bodyAccess := proxyv1alpha1.BodyAccessReadWrite
		initTimeoutSeconds := int32(45)
		llmProvider := "openai"
		requireModel := true
		policy := &proxyv1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Name: "llm-guardrails", Namespace: "default"},
			Spec: proxyv1alpha1.PolicySpec{
				Document: "kind: policy-document",
				Filter: &proxyv1alpha1.PolicyFilterConfig{
					BodyAccess:         &bodyAccess,
					InitTimeoutSeconds: &initTimeoutSeconds,
					LLM: &proxyv1alpha1.PolicyLLMOptions{
						Provider:     &llmProvider,
						RequireModel: &requireModel,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		DeferCleanup(func(ctx context.Context) error {
			return client.IgnoreNotFound(k8sClient.Delete(ctx, policy))
		})

		proxy := validProxy("resolved-policy")
		proxy.Spec.FilterChains[0].Filters[0] = policyRefEntry("llm-guardrails")
		createProxy(ctx, proxy)
		Eventually(func(gom Gomega, ctx context.Context) {
			got := &proxyv1alpha1.Proxy{}
			gom.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(proxy), got)).To(Succeed())
			for _, conditionType := range []string{
				proxyv1alpha1.ConditionTypeAccepted, proxyv1alpha1.ConditionTypeResolvedRefs,
			} {
				condition := meta.FindStatusCondition(got.Status.Conditions, conditionType)
				gom.Expect(condition).NotTo(BeNil())
				gom.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			}
			cm := &corev1.ConfigMap{}
			gom.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: proxy.Name + "-config", Namespace: proxy.Namespace,
			}, cm)).To(Succeed())
			gom.Expect(cm.Data["config.yaml"]).To(ContainSubstring("filter: policy"))
			gom.Expect(cm.Data["config.yaml"]).To(ContainSubstring(
				"config_path: /etc/praxis/policies/default/llm-guardrails.yaml"))
			gom.Expect(cm.Data["config.yaml"]).To(ContainSubstring("body_access: read_write"))
			gom.Expect(cm.Data["config.yaml"]).To(ContainSubstring("init_timeout_secs: 45"))
			gom.Expect(cm.Data["config.yaml"]).To(ContainSubstring("provider: openai"))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should retain valid configuration when an unresolved policy reference is introduced", func(ctx context.Context) {
		proxy := validProxy("unresolved-transition")
		createProxy(ctx, proxy)
		var originalConfig string
		Eventually(func(gom Gomega, ctx context.Context) {
			got := &proxyv1alpha1.Proxy{}
			gom.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(proxy), got)).To(Succeed())
			condition := meta.FindStatusCondition(got.Status.Conditions, proxyv1alpha1.ConditionTypeProgrammed)
			gom.Expect(condition).NotTo(BeNil())
			gom.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			cm := &corev1.ConfigMap{}
			gom.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: proxy.Name + "-config", Namespace: proxy.Namespace,
			}, cm)).To(Succeed())
			originalConfig = cm.Data["config.yaml"]
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
		Eventually(func(ctx context.Context) error {
			got := &proxyv1alpha1.Proxy{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(proxy), got); err != nil {
				return err
			}
			got.Spec.FilterChains[0].Filters[0] = policyRefEntry("missing")
			return k8sClient.Update(ctx, got)
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
		Eventually(func(gom Gomega, ctx context.Context) {
			got := &proxyv1alpha1.Proxy{}
			gom.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(proxy), got)).To(Succeed())
			for _, conditionType := range []string{
				proxyv1alpha1.ConditionTypeAccepted, proxyv1alpha1.ConditionTypeResolvedRefs,
			} {
				condition := meta.FindStatusCondition(got.Status.Conditions, conditionType)
				gom.Expect(condition).NotTo(BeNil())
				gom.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				gom.Expect(condition.Reason).To(Equal("ConfigRenderError"))
				gom.Expect(condition.ObservedGeneration).To(Equal(got.Generation))
			}
			// The previously valid configuration stays published and
			// programmed at its last valid generation.
			programmed := meta.FindStatusCondition(got.Status.Conditions, proxyv1alpha1.ConditionTypeProgrammed)
			gom.Expect(programmed).NotTo(BeNil())
			gom.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
			gom.Expect(programmed.ObservedGeneration).To(BeNumerically("<", got.Generation))
			cm := &corev1.ConfigMap{}
			gom.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: proxy.Name + "-config", Namespace: proxy.Namespace,
			}, cm)).To(Succeed())
			gom.Expect(cm.Data["config.yaml"]).To(Equal(originalConfig))
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())
	})

	It("should handle deleted proxy gracefully", func(ctx context.Context) {
		proxy := validProxy("deleted")
		createProxy(ctx, proxy)

		Eventually(func(gom Gomega, ctx context.Context) {
			cond := acceptedCondition(ctx, proxy)
			gom.Expect(cond).NotTo(BeNil())
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(Succeed())

		Expect(k8sClient.Delete(ctx, proxy)).To(Succeed())

		Eventually(func(ctx context.Context) bool {
			err := mgrClient.Get(ctx, client.ObjectKeyFromObject(proxy), &proxyv1alpha1.Proxy{})
			return err != nil
		}).WithContext(ctx).WithTimeout(proxyReconcileTimeout).Should(BeTrue())

		reconciler := &controller.ProxyReconciler{
			Client: mgrClient,
			Scheme: mgrClient.Scheme(),
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(proxy),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})
})
