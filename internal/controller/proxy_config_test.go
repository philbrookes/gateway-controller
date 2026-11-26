package controller

import (
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	proxyv1alpha1 "github.com/praxis-proxy/gateway-controller/api/proxy/v1alpha1"
)

// newRenderReader returns a client.Reader over the objects the render
// path resolves policy filter references against.
func newRenderReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := proxyv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestRenderProxyConfig_Minimal(t *testing.T) {
	t.Parallel()
	spec := &proxyv1alpha1.ProxySpec{
		Image:     "ghcr.io/praxis-proxy/praxis:latest",
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	// The default bind renders the IPv4 wildcard, which never answers the
	// kubelet's dial to an IPv6 pod IP, so the controller appends the
	// dual-stack readiness listener next to it.
	if len(parsed.Listeners) != 2 {
		t.Fatalf("expected the spec listener plus the readiness listener, got %d", len(parsed.Listeners))
	}
	if parsed.Listeners[0].Address != "0.0.0.0:8080" {
		t.Errorf("expected address 0.0.0.0:8080, got %s", parsed.Listeners[0].Address)
	}
	if parsed.Listeners[1].Name != "readiness" || parsed.Listeners[1].Address != "[::]:19902" {
		t.Errorf("expected the dual-stack readiness listener, got %+v", parsed.Listeners[1])
	}
}

func TestRenderProxyConfig_AdminAndInsecure(t *testing.T) {
	t.Parallel()
	allowPriv := true
	denyUnbounded := false
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		Admin: &proxyv1alpha1.PraxisAdmin{Address: "127.0.0.1:19901"},
		InsecureOptions: &proxyv1alpha1.PraxisInsecureOptions{
			AllowPrivateEndpoints: &allowPriv,
			AllowUnboundedBody:    &denyUnbounded,
		},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	if parsed.Admin == nil || parsed.Admin.Address != "127.0.0.1:19901" {
		t.Errorf("admin address mismatch: %+v", parsed.Admin)
	}
	if parsed.InsecureOptions == nil {
		t.Fatal("expected insecure_options")
	}
	if parsed.InsecureOptions.AllowPrivateEndpoints == nil || !*parsed.InsecureOptions.AllowPrivateEndpoints {
		t.Error("expected allow_private_endpoints=true")
	}
	if parsed.InsecureOptions.AllowUnboundedBody == nil || *parsed.InsecureOptions.AllowUnboundedBody {
		t.Error("expected allow_unbounded_body=false")
	}
}

func TestRenderProxyConfig_IPv6BindAddress(t *testing.T) {
	t.Parallel()
	bindAddr := "::1"
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		Listeners: []proxyv1alpha1.PraxisListener{{
			Name:         "http",
			Port:         8080,
			BindAddress:  &bindAddr,
			FilterChains: []string{},
		}},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	if parsed.Listeners[0].Address != "[::1]:8080" {
		t.Errorf("expected [::1]:8080, got %s", parsed.Listeners[0].Address)
	}
}

func TestRenderProxyConfig_ListenerTLS(t *testing.T) {
	t.Parallel()
	bindAddr := "10.0.0.1"
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		Listeners: []proxyv1alpha1.PraxisListener{{
			Name:        "https",
			Port:        8443,
			BindAddress: &bindAddr,
			TLS:         &proxyv1alpha1.PraxisListenerTLS{CertificateRef: proxyv1alpha1.PraxisLocalReference{Name: "gw-tls"}},
		}},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	listener := parsed.Listeners[0]
	if listener.Address != "10.0.0.1:8443" {
		t.Errorf("expected 10.0.0.1:8443, got %s", listener.Address)
	}
	if listener.TLS == nil {
		t.Fatal("expected TLS config")
	}
	if listener.TLS.CertPath != "/etc/praxis/tls/https/tls.crt" {
		t.Errorf("cert_path: %s", listener.TLS.CertPath)
	}
	if listener.TLS.KeyPath != "/etc/praxis/tls/https/tls.key" {
		t.Errorf("key_path: %s", listener.TLS.KeyPath)
	}
}

func TestRenderProxyConfig_ReservedKeysProtected(t *testing.T) {
	t.Parallel()
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		FilterChains: []proxyv1alpha1.ProxyFilterChain{{
			Name: "main",
			Filters: []proxyv1alpha1.PraxisFilterEntry{{
				Name: "rl",
				InlineFilter: &proxyv1alpha1.PraxisInlineFilter{
					Type:   "rate_limiter",
					Config: &runtime.RawExtension{Raw: []byte(`{"name":"evil","filter":"evil","burst":200}`)},
				},
			}},
		}},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080, FilterChains: []string{"main"}}},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	filter := parsed.FilterChains[0].Filters[0]
	if filter["name"] != "rl" {
		t.Errorf("name should be protected, got: %v", filter["name"])
	}
	if filter["filter"] != "rate_limiter" {
		t.Errorf("filter type should be protected, got: %v", filter["filter"])
	}
	if v, ok := filter["burst"]; !ok || v != float64(200) {
		t.Errorf("non-reserved key should still be flattened, burst: %v", v)
	}
}

func TestRenderProxyConfig_InlineFilter(t *testing.T) {
	t.Parallel()
	fmOpen := proxyv1alpha1.FailureModeOpen
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		FilterChains: []proxyv1alpha1.ProxyFilterChain{{
			Name: "main",
			Filters: []proxyv1alpha1.PraxisFilterEntry{{
				Name: "rl",
				InlineFilter: &proxyv1alpha1.PraxisInlineFilter{
					Type:   "rate_limiter",
					Config: &runtime.RawExtension{Raw: []byte(`{"requests_per_second":100,"burst":200}`)},
				},
				FailureMode: &fmOpen,
			}},
		}},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080, FilterChains: []string{"main"}}},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	filter := parsed.FilterChains[0].Filters[0]
	if filter["filter"] != "rate_limiter" {
		t.Errorf("filter type: %v", filter["filter"])
	}
	if filter["failure_mode"] != "open" {
		t.Errorf("failure_mode: %v", filter["failure_mode"])
	}
	if v, ok := filter["requests_per_second"]; !ok || v != float64(100) {
		t.Errorf("requests_per_second: %v", v)
	}
	if v, ok := filter["burst"]; !ok || v != float64(200) {
		t.Errorf("burst: %v", v)
	}
}

func TestRenderProxyConfig_PolicyRef(t *testing.T) {
	t.Parallel()

	policyDocument := "apiVersion: policy.praxis.io/v1\nkind: PolicyEngine\ndocument: minimal"

	t.Run("explicit namespace", func(t *testing.T) {
		t.Parallel()
		fmClosed := proxyv1alpha1.FailureModeClosed
		spec := &proxyv1alpha1.ProxySpec{
			Image: "ghcr.io/praxis-proxy/praxis:latest",
			FilterChains: []proxyv1alpha1.ProxyFilterChain{{
				Name: "policy-chain",
				Filters: []proxyv1alpha1.PraxisFilterEntry{{
					Name: "guardrails",
					Ref: &proxyv1alpha1.PraxisFilterReference{
						Group: proxyv1alpha1.PraxisGroup, Kind: proxyv1alpha1.PolicyKind,
						Namespace: "other-ns", Name: "my-policy",
					},
					FailureMode: &fmClosed,
				}},
			}},
			Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
		}
		policy := &proxyv1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other-ns", Name: "my-policy"},
			Spec:       proxyv1alpha1.PolicySpec{Document: policyDocument},
		}
		got, err := renderProxyConfig(t.Context(), newRenderReader(t, policy), spec, "default")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed nativeConfig
		if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
			t.Fatalf("invalid YAML: %v", err)
		}
		filter := parsed.FilterChains[0].Filters[0]
		if filter["filter"] != "policy" {
			t.Errorf("filter: %v", filter["filter"])
		}
		if filter["config_path"] != "/etc/praxis/policies/other-ns/my-policy.yaml" {
			t.Errorf("config_path: %v", filter["config_path"])
		}
		if got.PolicyDocuments["/etc/praxis/policies/other-ns/my-policy.yaml"] != policyDocument {
			t.Errorf("policy document collection: %v", got.PolicyDocuments)
		}
	})

	t.Run("defaults to proxy namespace", func(t *testing.T) {
		t.Parallel()
		spec := &proxyv1alpha1.ProxySpec{
			Image: "ghcr.io/praxis-proxy/praxis:latest",
			FilterChains: []proxyv1alpha1.ProxyFilterChain{{
				Name: "chain",
				Filters: []proxyv1alpha1.PraxisFilterEntry{{
					Name: "p",
					Ref: &proxyv1alpha1.PraxisFilterReference{
						Group: proxyv1alpha1.PraxisGroup, Kind: proxyv1alpha1.PolicyKind,
						Name: "local-policy",
					},
				}},
			}},
			Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
		}
		// No filter config: only the structural keys render.
		policy := &proxyv1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Namespace: "my-ns", Name: "local-policy"},
			Spec:       proxyv1alpha1.PolicySpec{Document: policyDocument},
		}
		got, err := renderProxyConfig(t.Context(), newRenderReader(t, policy), spec, "my-ns")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed nativeConfig
		if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
			t.Fatalf("invalid YAML: %v", err)
		}
		filter := parsed.FilterChains[0].Filters[0]
		if filter["config_path"] != "/etc/praxis/policies/my-ns/local-policy.yaml" {
			t.Errorf("config_path: %v", filter["config_path"])
		}
		for _, key := range []string{"body_access", "llm", "context_meta"} {
			if _, ok := filter[key]; ok {
				t.Errorf("unset policy filter option %q must not render", key)
			}
		}
		if got.PolicyDocuments["/etc/praxis/policies/my-ns/local-policy.yaml"] != policyDocument {
			t.Errorf("policy document collection: %v", got.PolicyDocuments)
		}
	})
}

func TestRenderProxyConfig_PolicyFilterConfig(t *testing.T) {
	t.Parallel()

	readWrite := proxyv1alpha1.BodyAccessReadWrite
	initTimeout := int32(45)
	maxBuffer := int64(2097152)
	allowPrivateIdp := true
	requireProtocolMetadata := false
	maxRequestBytes := int64(1048576)
	provider := "openai"
	requireModel := true
	scope := "tenant-acme"
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		FilterChains: []proxyv1alpha1.ProxyFilterChain{{
			Name: "policy-chain",
			Filters: []proxyv1alpha1.PraxisFilterEntry{{
				Name: "guardrails",
				Ref: &proxyv1alpha1.PraxisFilterReference{
					Group: proxyv1alpha1.PraxisGroup, Kind: proxyv1alpha1.PolicyKind,
					Namespace: "other-ns", Name: "my-policy",
				},
			}},
		}},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
	}
	policy := &proxyv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other-ns", Name: "my-policy"},
		Spec: proxyv1alpha1.PolicySpec{
			Document: "kind: policy-document",
			Filter: &proxyv1alpha1.PolicyFilterConfig{
				BodyAccess:              &readWrite,
				InitTimeoutSeconds:      &initTimeout,
				MaxBufferBytes:          &maxBuffer,
				AllowPrivateIdp:         &allowPrivateIdp,
				TrustedPrivateEndpoints: []string{"internal.example.com"},
				RequireProtocolMetadata: &requireProtocolMetadata,
				LLM: &proxyv1alpha1.PolicyLLMOptions{
					MaxRequestBytes: &maxRequestBytes,
					PromoteParams:   []string{"model", "route"},
					Provider:        &provider,
					RequireModel:    &requireModel,
				},
				ContextHeaders: map[string]string{"agent.conversation_id": "x-praxis-conversation"},
				ContextMeta: &proxyv1alpha1.PolicyContextMeta{
					Tags:       []string{"production"},
					Scope:      &scope,
					Properties: map[string]string{"tier": "gold"},
				},
			},
		},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t, policy), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	filter := parsed.FilterChains[0].Filters[0]
	if filter["body_access"] != "read_write" {
		t.Errorf("body_access: %v", filter["body_access"])
	}
	if filter["init_timeout_secs"] != float64(45) {
		t.Errorf("init_timeout_secs: %v", filter["init_timeout_secs"])
	}
	if filter["max_buffer_bytes"] != float64(2097152) {
		t.Errorf("max_buffer_bytes: %v", filter["max_buffer_bytes"])
	}
	if filter["allow_private_idp"] != true {
		t.Errorf("allow_private_idp: %v", filter["allow_private_idp"])
	}
	if filter["require_protocol_metadata"] != false {
		t.Errorf("require_protocol_metadata: %v", filter["require_protocol_metadata"])
	}
	if v, ok := filter["trusted_private_endpoints"]; !ok || fmt.Sprint(v) != "[internal.example.com]" {
		t.Errorf("trusted_private_endpoints: %v", v)
	}
	llm, ok := filter["llm"].(map[string]any)
	if !ok {
		t.Fatalf("llm: %v", filter["llm"])
	}
	if llm["max_request_bytes"] != float64(1048576) {
		t.Errorf("llm.max_request_bytes: %v", llm["max_request_bytes"])
	}
	if llm["provider"] != "openai" {
		t.Errorf("llm.provider: %v", llm["provider"])
	}
	if llm["require_model"] != true {
		t.Errorf("llm.require_model: %v", llm["require_model"])
	}
	if v, ok := llm["promote_params"]; !ok || fmt.Sprint(v) != "[model route]" {
		t.Errorf("llm.promote_params: %v", v)
	}
	if v, ok := filter["context_headers"].(map[string]any); !ok || v["agent.conversation_id"] != "x-praxis-conversation" {
		t.Errorf("context_headers: %v", filter["context_headers"])
	}
	meta, ok := filter["context_meta"].(map[string]any)
	if !ok {
		t.Fatalf("context_meta: %v", filter["context_meta"])
	}
	if meta["scope"] != "tenant-acme" {
		t.Errorf("context_meta.scope: %v", meta["scope"])
	}
	if v, ok := meta["tags"].([]any); !ok || len(v) != 1 || v[0] != "production" {
		t.Errorf("context_meta.tags: %v", meta["tags"])
	}
	if v, ok := meta["properties"].(map[string]any); !ok || v["tier"] != "gold" {
		t.Errorf("context_meta.properties: %v", meta["properties"])
	}
	// The filter config sits flat under the filter entry, not nested
	// under a config wrapper.
	if _, ok := filter["config"]; ok {
		t.Errorf("policy filter config must be flat, found config wrapper: %v", filter["config"])
	}
}

func TestRenderProxyConfig_PolicyDocumentCollection(t *testing.T) {
	t.Parallel()
	firstDocument := "kind: first"
	secondDocument := "kind: second"
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		FilterChains: []proxyv1alpha1.ProxyFilterChain{{
			Name: "shared",
			// The same policy referenced twice collects one document;
			// a second policy from another namespace collects its own.
			Filters: []proxyv1alpha1.PraxisFilterEntry{
				{Name: "guardrails-a", Ref: &proxyv1alpha1.PraxisFilterReference{
					Group: proxyv1alpha1.PraxisGroup, Kind: proxyv1alpha1.PolicyKind, Name: "shared-policy",
				}},
				{Name: "guardrails-b", Ref: &proxyv1alpha1.PraxisFilterReference{
					Group: proxyv1alpha1.PraxisGroup, Kind: proxyv1alpha1.PolicyKind, Name: "shared-policy",
				}},
			},
		}, {
			Name: "cross-ns",
			Filters: []proxyv1alpha1.PraxisFilterEntry{
				{Name: "guardrails-c", Ref: &proxyv1alpha1.PraxisFilterReference{
					Group: proxyv1alpha1.PraxisGroup, Kind: proxyv1alpha1.PolicyKind,
					Namespace: "other-ns", Name: "remote-policy",
				}},
			},
		}},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
	}
	reader := newRenderReader(t,
		&proxyv1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "shared-policy"},
			Spec:       proxyv1alpha1.PolicySpec{Document: firstDocument},
		},
		&proxyv1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other-ns", Name: "remote-policy"},
			Spec:       proxyv1alpha1.PolicySpec{Document: secondDocument},
		},
	)
	got, err := renderProxyConfig(t.Context(), reader, spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		"/etc/praxis/policies/default/shared-policy.yaml":  firstDocument,
		"/etc/praxis/policies/other-ns/remote-policy.yaml": secondDocument,
	}
	if len(got.PolicyDocuments) != len(want) {
		t.Fatalf("policy documents = %v, want exactly %v", got.PolicyDocuments, want)
	}
	for path, document := range want {
		if got.PolicyDocuments[path] != document {
			t.Errorf("policy document %q = %q, want %q", path, got.PolicyDocuments[path], document)
		}
	}
}

func TestRenderProxyConfig_PolicyLookupError(t *testing.T) {
	t.Parallel()
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		FilterChains: []proxyv1alpha1.ProxyFilterChain{{
			Name: "chain",
			Filters: []proxyv1alpha1.PraxisFilterEntry{{
				Name: "guardrails",
				Ref: &proxyv1alpha1.PraxisFilterReference{
					Group: proxyv1alpha1.PraxisGroup, Kind: proxyv1alpha1.PolicyKind,
					Namespace: "other-ns", Name: "missing-policy",
				},
			}},
		}},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
	}
	// The referenced Policy is absent from the reader, so the render
	// fails with the lookup error.
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err == nil {
		t.Fatalf("expected a policy lookup error, got config %q", got.Config)
	}
	if !strings.Contains(err.Error(), "resolve policy other-ns/missing-policy") {
		t.Errorf("error should name the unresolved policy: %v", err)
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("lookup error should preserve the not-found cause: %v", err)
	}
	if !strings.Contains(err.Error(), "chain \"chain\" filter \"guardrails\"") {
		t.Errorf("error should name the referencing filter entry: %v", err)
	}
}

func TestRenderProxyConfig_Conditions(t *testing.T) {
	t.Parallel()
	apiPrefix := "/api"
	healthPath := "/healthz"
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		FilterChains: []proxyv1alpha1.ProxyFilterChain{{
			Name: "conditioned",
			Conditions: []proxyv1alpha1.PraxisRequestCondition{{
				When: &proxyv1alpha1.PraxisRequestMatch{
					PathPrefix: &apiPrefix,
					Methods:    []string{"POST"},
					Headers:    map[string]string{"x-tenant": "acme"},
				},
			}},
			Filters: []proxyv1alpha1.PraxisFilterEntry{{
				Name:         "f",
				InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"},
				Conditions: []proxyv1alpha1.PraxisRequestCondition{{
					Unless: &proxyv1alpha1.PraxisRequestMatch{
						Path:           &healthPath,
						HeadersPresent: []string{"x-bypass"},
					},
				}},
				ResponseConditions: []proxyv1alpha1.PraxisResponseCondition{{
					When: &proxyv1alpha1.PraxisResponseMatch{
						Status:  []int32{200, 201},
						Headers: map[string]string{"content-type": "application/json"},
					},
				}},
			}},
		}},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	chain := parsed.FilterChains[0]
	if len(chain.Conditions) != 1 {
		t.Fatalf("expected 1 chain condition, got %d", len(chain.Conditions))
	}
	when := chain.Conditions[0]["when"].(map[string]any)
	if when["path_prefix"] != "/api" {
		t.Errorf("path_prefix: %v", when["path_prefix"])
	}

	filter := chain.Filters[0]
	filterConds := filter["conditions"].([]any)
	if len(filterConds) != 1 {
		t.Fatalf("expected 1 filter condition, got %d", len(filterConds))
	}
	respConds := filter["response_conditions"].([]any)
	if len(respConds) != 1 {
		t.Fatalf("expected 1 response condition, got %d", len(respConds))
	}
}

func TestRenderProxyConfig_UpstreamMatch(t *testing.T) {
	t.Parallel()
	appProto := "openai_chat_completions"
	appProvider := "openai"
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		FilterChains: []proxyv1alpha1.ProxyFilterChain{{
			Name: "chain",
			Conditions: []proxyv1alpha1.PraxisRequestCondition{{
				When: &proxyv1alpha1.PraxisRequestMatch{
					BoundUpstream: &proxyv1alpha1.PraxisSelectedUpstreamMatch{
						ApplicationProtocol: &appProto,
						ApplicationProvider: &appProvider,
					},
				},
			}},
			Filters: []proxyv1alpha1.PraxisFilterEntry{{
				Name:         "f",
				InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"},
			}},
		}},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	when := parsed.FilterChains[0].Conditions[0]["when"].(map[string]any)
	upstream := when["bound_upstream"].(map[string]any)
	if upstream["application_protocol"] != "openai_chat_completions" {
		t.Errorf("application_protocol: %v", upstream["application_protocol"])
	}
	if upstream["application_provider"] != "openai" {
		t.Errorf("application_provider: %v", upstream["application_provider"])
	}
}

func TestRenderProxyConfig_FilterChainRefs(t *testing.T) {
	t.Parallel()
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		FilterChains: []proxyv1alpha1.ProxyFilterChain{
			{Name: "a", Filters: []proxyv1alpha1.PraxisFilterEntry{{Name: "f", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"}}}},
			{Name: "b", Filters: []proxyv1alpha1.PraxisFilterEntry{{Name: "g", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "access_log"}}}},
		},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080, FilterChains: []string{"a", "b"}}},
	}
	got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed nativeConfig
	if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	listener := parsed.Listeners[0]
	if len(listener.FilterChains) != 2 || listener.FilterChains[0] != "a" || listener.FilterChains[1] != "b" {
		t.Errorf("filter_chains: %v", listener.FilterChains)
	}
}

func TestRenderProxyConfig_UnsupportedFilterRef(t *testing.T) {
	t.Parallel()
	spec := &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		FilterChains: []proxyv1alpha1.ProxyFilterChain{{
			Name: "chain",
			Filters: []proxyv1alpha1.PraxisFilterEntry{{
				Name: "unknown",
				Ref: &proxyv1alpha1.PraxisFilterReference{
					Group: "other.io",
					Kind:  "CustomFilter",
					Name:  "my-filter",
				},
			}},
		}},
		Listeners: []proxyv1alpha1.PraxisListener{{Name: "http", Port: 8080}},
	}
	_, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
	if err == nil {
		t.Fatal("expected error for unsupported filter reference")
	}
	if !strings.Contains(err.Error(), "unsupported filter") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRenderProxyConfig_ReadinessListener(t *testing.T) {
	t.Parallel()

	render := func(t *testing.T, spec *proxyv1alpha1.ProxySpec) nativeConfig {
		t.Helper()
		got, err := renderProxyConfig(t.Context(), newRenderReader(t), spec, "default")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed nativeConfig
		if err := yaml.Unmarshal([]byte(got.Config), &parsed); err != nil {
			t.Fatalf("invalid YAML: %v", err)
		}
		return parsed
	}

	t.Run("renders no readiness listener for dual-stack-bound listeners", func(t *testing.T) {
		t.Parallel()
		// Only the IPv6 wildcard accepts the kubelet's dial on either
		// pod IP family, so a listener bound to it is probed directly.
		dualStack := "::"
		parsed := render(t, readinessRenderSpec(&dualStack))
		if len(parsed.Listeners) != 1 {
			t.Fatalf("expected only the spec listener, got %d", len(parsed.Listeners))
		}
	})

	t.Run("renders a dedicated readiness listener for IPv4-wildcard binds", func(t *testing.T) {
		t.Parallel()
		// The IPv4 wildcard is the API default and never answers a dial
		// to an IPv6 pod IP, so it needs the dedicated listener.
		ipv4Wildcard := "0.0.0.0"
		parsed := render(t, readinessRenderSpec(&ipv4Wildcard))
		if len(parsed.Listeners) != 2 {
			t.Fatalf("expected the spec listener plus the readiness listener, got %d", len(parsed.Listeners))
		}
		readiness := parsed.Listeners[1]
		if readiness.Name != "readiness" {
			t.Errorf("readiness listener name: %q", readiness.Name)
		}
		if readiness.Address != "[::]:19902" {
			t.Errorf("readiness listener must bind the dual-stack wildcard, got: %q", readiness.Address)
		}
		if len(readiness.FilterChains) != 1 || readiness.FilterChains[0] != "readiness" {
			t.Errorf("readiness listener must reference its dedicated health-only chain, got: %v", readiness.FilterChains)
		}
		assertReadinessHealthChain(t, parsed, "readiness")
	})

	t.Run("renders a dedicated readiness listener for specific binds", func(t *testing.T) {
		t.Parallel()
		loopback := "127.0.0.1"
		parsed := render(t, readinessRenderSpec(&loopback))
		if len(parsed.Listeners) != 2 {
			t.Fatalf("expected the spec listener plus the readiness listener, got %d", len(parsed.Listeners))
		}
		readiness := parsed.Listeners[1]
		if readiness.Name != "readiness" {
			t.Errorf("readiness listener name: %q", readiness.Name)
		}
		if readiness.Address != "[::]:19902" {
			t.Errorf("readiness listener must bind the dual-stack wildcard, got: %q", readiness.Address)
		}
		if len(readiness.FilterChains) != 1 || readiness.FilterChains[0] != "readiness" {
			t.Errorf("readiness listener must reference its dedicated health-only chain, got: %v", readiness.FilterChains)
		}
		assertReadinessHealthChain(t, parsed, "readiness")
		if readiness.TLS != nil {
			t.Errorf("readiness listener must not add TLS when the spec has none, got: %+v", readiness.TLS)
		}
	})

	t.Run("mirrors the first listener's TLS for TLS-enabled proxies", func(t *testing.T) {
		t.Parallel()
		loopback := "127.0.0.1"
		spec := readinessRenderSpec(&loopback)
		spec.Listeners[0].TLS = &proxyv1alpha1.PraxisListenerTLS{
			CertificateRef: proxyv1alpha1.PraxisLocalReference{Name: "gw-tls"},
		}
		parsed := render(t, spec)
		if len(parsed.Listeners) != 2 {
			t.Fatalf("expected the spec listener plus the readiness listener, got %d", len(parsed.Listeners))
		}
		readiness := parsed.Listeners[1]
		if readiness.TLS == nil {
			t.Fatal("readiness listener must preserve the first listener's TLS")
		}
		if readiness.TLS.CertPath != "/etc/praxis/tls/http/tls.crt" {
			t.Errorf("readiness listener cert_path: %s", readiness.TLS.CertPath)
		}
		if readiness.TLS.KeyPath != "/etc/praxis/tls/http/tls.key" {
			t.Errorf("readiness listener key_path: %s", readiness.TLS.KeyPath)
		}
		if len(readiness.FilterChains) != 1 || readiness.FilterChains[0] != "readiness" {
			t.Errorf("readiness listener must reference its dedicated health-only chain, got: %v", readiness.FilterChains)
		}
		assertReadinessHealthChain(t, parsed, "readiness")
	})

	t.Run("binds the dual-stack wildcard for IPv6-specific binds", func(t *testing.T) {
		t.Parallel()
		loopback := "::1"
		parsed := render(t, readinessRenderSpec(&loopback))
		if len(parsed.Listeners) != 2 {
			t.Fatalf("expected the spec listener plus the readiness listener, got %d", len(parsed.Listeners))
		}
		readiness := parsed.Listeners[1]
		if readiness.Address != "[::]:19902" {
			t.Errorf("readiness listener must bind the dual-stack wildcard for IPv6-specific binds, got: %q", readiness.Address)
		}
	})

	t.Run("avoids spec listener names, chain names, and occupied ports", func(t *testing.T) {
		t.Parallel()
		loopback := "127.0.0.1"
		spec := readinessRenderSpec(&loopback)
		spec.Listeners[0].Name = "readiness"
		spec.Listeners[0].Port = 19902
		spec.Listeners[0].FilterChains = []string{"readiness"}
		spec.FilterChains[0].Name = "readiness"
		spec.Admin = &proxyv1alpha1.PraxisAdmin{Address: "127.0.0.1:19903"}
		parsed := render(t, spec)
		if len(parsed.Listeners) != 2 {
			t.Fatalf("expected the spec listener plus the readiness listener, got %d", len(parsed.Listeners))
		}
		readiness := parsed.Listeners[1]
		if readiness.Name != "readiness-0" {
			t.Errorf("readiness listener must avoid the spec listener name, got: %q", readiness.Name)
		}
		if readiness.Address != "[::]:19904" {
			t.Errorf("readiness listener must avoid occupied ports, got: %q", readiness.Address)
		}
		if len(readiness.FilterChains) != 1 || readiness.FilterChains[0] != "readiness-0" {
			t.Errorf("readiness chain must avoid the spec chain name, got: %v", readiness.FilterChains)
		}
		assertReadinessHealthChain(t, parsed, "readiness-0")
	})
}

// assertReadinessHealthChain verifies the named chain is the readiness
// listener's health-only pipeline: a single static_response filter that
// answers with a fixed response without contacting any upstream — the
// form the Praxis configuration reference documents for health checks.
// The runtime rejects chainless HTTP listeners, so the chain is what
// keeps the rendered config valid.
func assertReadinessHealthChain(t *testing.T, parsed nativeConfig, name string) {
	t.Helper()
	var chain *nativeChain
	for i := range parsed.FilterChains {
		if parsed.FilterChains[i].Name == name {
			chain = &parsed.FilterChains[i]
		}
	}
	if chain == nil {
		t.Fatalf("rendered config must define the %q health-only chain", name)
	}
	if len(chain.Filters) != 1 {
		t.Fatalf("health-only chain must carry a single filter, got: %v", chain.Filters)
	}
	filter := chain.Filters[0]
	if filter["filter"] != "static_response" {
		t.Errorf("health-only chain must use the static_response filter, got: %v", filter["filter"])
	}
	status, ok := filter["status"].(float64)
	if !ok || status != 200 {
		t.Errorf("health-only chain must answer with a fixed 200 response, got: %v", filter["status"])
	}
}

// readinessRenderSpec returns a minimal valid spec whose single listener
// binds bindAddress (nil meaning the wildcard default).
func readinessRenderSpec(bindAddress *string) *proxyv1alpha1.ProxySpec {
	return &proxyv1alpha1.ProxySpec{
		Image: "ghcr.io/praxis-proxy/praxis:latest",
		Listeners: []proxyv1alpha1.PraxisListener{{
			Name:         "http",
			Port:         8080,
			BindAddress:  bindAddress,
			FilterChains: []string{"main"},
		}},
		FilterChains: []proxyv1alpha1.ProxyFilterChain{{
			Name: "main",
			Filters: []proxyv1alpha1.PraxisFilterEntry{{
				Name:         "rl",
				InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"},
			}},
		}},
	}
}
