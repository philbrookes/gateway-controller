package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"strconv"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	proxyv1alpha1 "github.com/praxis-proxy/gateway-controller/api/proxy/v1alpha1"
	"sigs.k8s.io/yaml"
)

const (
	configVolumeName   = "praxis-config"
	configMountPath    = "/etc/praxis"
	configFileName     = "config.yaml"
	configSubPath      = configFileName
	defaultBindAddress = "0.0.0.0"
	policyFilterType   = "policy"
	policyMountPath    = "/etc/praxis/policies"
	tlsMountPrefix     = "/etc/praxis/tls"
)

type nativeConfig struct {
	Admin           *nativeAdmin     `json:"admin,omitempty"`
	InsecureOptions *nativeInsecure  `json:"insecure_options,omitempty"`
	Listeners       []nativeListener `json:"listeners"`
	FilterChains    []nativeChain    `json:"filter_chains,omitempty"`
}

type nativeAdmin struct {
	Address string `json:"address"`
}

type nativeInsecure struct {
	AllowPrivateEndpoints *bool `json:"allow_private_endpoints,omitempty"`
	AllowUnboundedBody    *bool `json:"allow_unbounded_body,omitempty"`
}

type nativeListener struct {
	Name         string     `json:"name"`
	Address      string     `json:"address"`
	TLS          *nativeTLS `json:"tls,omitempty"`
	FilterChains []string   `json:"filter_chains,omitempty"`
}

type nativeTLS struct {
	CertPath string `json:"cert_path"`
	KeyPath  string `json:"key_path"`
}

type nativeChain struct {
	Name       string           `json:"name"`
	Conditions []map[string]any `json:"conditions,omitempty"`
	Filters    []map[string]any `json:"filters"`
}

// renderedConfig is the result of rendering a Proxy spec into the
// Praxis-native configuration.
type renderedConfig struct {
	// Config is the rendered Praxis configuration YAML.
	Config string

	// PolicyDocuments holds the document of every Policy the rendered
	// configuration references, keyed by the file path the rendered
	// policy filter reads it from. The controller collects the documents
	// while rendering; publishing them next to the proxy and mounting
	// them into the Praxis container are owned by separate tasks
	// (#19 and #20), which build on this collection.
	PolicyDocuments map[string]string
}

func renderProxyConfig(ctx context.Context, reader client.Reader, spec *proxyv1alpha1.ProxySpec, namespace string) (*renderedConfig, error) {
	cfg := nativeConfig{}
	rendered := &renderedConfig{PolicyDocuments: map[string]string{}}

	if spec.Admin != nil {
		cfg.Admin = &nativeAdmin{Address: spec.Admin.Address}
	}

	if spec.InsecureOptions != nil {
		cfg.InsecureOptions = &nativeInsecure{
			AllowPrivateEndpoints: spec.InsecureOptions.AllowPrivateEndpoints,
			AllowUnboundedBody:    spec.InsecureOptions.AllowUnboundedBody,
		}
	}

	cfg.Listeners = make([]nativeListener, 0, len(spec.Listeners))
	for _, listener := range spec.Listeners {
		bindAddr := defaultBindAddress
		if listener.BindAddress != nil {
			bindAddr = *listener.BindAddress
		}
		native := nativeListener{
			Name:         listener.Name,
			Address:      net.JoinHostPort(bindAddr, strconv.FormatInt(int64(listener.Port), 10)),
			FilterChains: listener.FilterChains,
		}
		if listener.TLS != nil {
			native.TLS = &nativeTLS{
				CertPath: fmt.Sprintf("%s/%s/tls.crt", tlsMountPrefix, listener.Name),
				KeyPath:  fmt.Sprintf("%s/%s/tls.key", tlsMountPrefix, listener.Name),
			}
		}
		cfg.Listeners = append(cfg.Listeners, native)
	}

	if len(spec.FilterChains) > 0 {
		cfg.FilterChains = make([]nativeChain, 0, len(spec.FilterChains))
		for _, chain := range spec.FilterChains {
			nc := nativeChain{
				Name:       chain.Name,
				Conditions: renderRequestConditions(chain.Conditions),
			}
			nc.Filters = make([]map[string]any, 0, len(chain.Filters))
			for _, entry := range chain.Filters {
				f, err := renderFilterEntry(ctx, reader, &entry, namespace, rendered.PolicyDocuments)
				if err != nil {
					return nil, fmt.Errorf("chain %q filter %q: %w", chain.Name, entry.Name, err)
				}
				nc.Filters = append(nc.Filters, f)
			}
			cfg.FilterChains = append(cfg.FilterChains, nc)
		}
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	rendered.Config = string(out)
	return rendered, nil
}

func renderFilterEntry(ctx context.Context, reader client.Reader, entry *proxyv1alpha1.PraxisFilterEntry, namespace string, policyDocuments map[string]string) (map[string]any, error) {
	filter := map[string]any{"name": entry.Name}

	switch {
	case entry.InlineFilter != nil:
		filter["filter"] = entry.InlineFilter.Type
		if entry.InlineFilter.Config != nil && entry.InlineFilter.Config.Raw != nil {
			var configMap map[string]any
			if err := json.Unmarshal(entry.InlineFilter.Config.Raw, &configMap); err != nil {
				return nil, fmt.Errorf("unmarshal inline config: %w", err)
			}
			maps.Copy(filter, configMap)
			filter["name"] = entry.Name
			filter["filter"] = entry.InlineFilter.Type
		}
	case isPolicyRef(entry.Ref):
		policy, err := resolvePolicy(ctx, reader, entry.Ref, namespace)
		if err != nil {
			return nil, err
		}
		path := policyConfigPath(refNamespace(entry.Ref, namespace), entry.Ref.Name)
		filter["filter"] = policyFilterType
		filter["config_path"] = path
		applyPolicyFilterConfig(filter, policy.Spec.Filter)
		policyDocuments[path] = policy.Spec.Document
	default:
		return nil, fmt.Errorf("unsupported filter: neither inline nor policy reference")
	}

	if entry.FailureMode != nil {
		filter["failure_mode"] = string(*entry.FailureMode)
	}
	if conds := renderRequestConditions(entry.Conditions); len(conds) > 0 {
		filter["conditions"] = conds
	}
	if conds := renderResponseConditions(entry.ResponseConditions); len(conds) > 0 {
		filter["response_conditions"] = conds
	}
	return filter, nil
}

// resolvePolicy fetches the Policy a filter entry references. The
// reference's namespace defaults to the proxy's own namespace.
func resolvePolicy(ctx context.Context, reader client.Reader, ref *proxyv1alpha1.PraxisFilterReference, namespace string) (*proxyv1alpha1.Policy, error) {
	policyNS := refNamespace(ref, namespace)
	policy := &proxyv1alpha1.Policy{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: ref.Name}, policy); err != nil {
		return nil, fmt.Errorf("resolve policy %s/%s: %w", policyNS, ref.Name, err)
	}
	return policy, nil
}

// policyConfigPath returns the file path the rendered policy filter reads
// its document from, and the key the collected document is stored under.
func policyConfigPath(namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s.yaml", policyMountPath, namespace, name)
}

// applyPolicyFilterConfig copies the Policy's typed filter options into
// the rendered filter entry. Praxis filter configs are flat: the filter's
// typed fields sit directly under the filter entry alongside the
// structural keys (name, config_path), so each set field is copied under
// its native name and unset fields are omitted.
func applyPolicyFilterConfig(filter map[string]any, config *proxyv1alpha1.PolicyFilterConfig) {
	if config == nil {
		return
	}
	if config.BodyAccess != nil {
		filter["body_access"] = string(*config.BodyAccess)
	}
	if config.InitTimeoutSeconds != nil {
		filter["init_timeout_secs"] = *config.InitTimeoutSeconds
	}
	if config.MaxBufferBytes != nil {
		filter["max_buffer_bytes"] = *config.MaxBufferBytes
	}
	if config.AllowPrivateIdp != nil {
		filter["allow_private_idp"] = *config.AllowPrivateIdp
	}
	if len(config.TrustedPrivateEndpoints) > 0 {
		filter["trusted_private_endpoints"] = config.TrustedPrivateEndpoints
	}
	if config.RequireProtocolMetadata != nil {
		filter["require_protocol_metadata"] = *config.RequireProtocolMetadata
	}
	if config.LLM != nil {
		llm := map[string]any{}
		if config.LLM.MaxRequestBytes != nil {
			llm["max_request_bytes"] = *config.LLM.MaxRequestBytes
		}
		if config.LLM.PromoteParams != nil {
			llm["promote_params"] = config.LLM.PromoteParams
		}
		if config.LLM.Provider != nil {
			llm["provider"] = *config.LLM.Provider
		}
		if config.LLM.RequireModel != nil {
			llm["require_model"] = *config.LLM.RequireModel
		}
		if config.LLM.RequireRoute != nil {
			llm["require_route"] = *config.LLM.RequireRoute
		}
		filter["llm"] = llm
	}
	if len(config.ContextHeaders) > 0 {
		filter["context_headers"] = config.ContextHeaders
	}
	if config.ContextMeta != nil {
		meta := map[string]any{}
		if len(config.ContextMeta.Tags) > 0 {
			meta["tags"] = config.ContextMeta.Tags
		}
		if config.ContextMeta.Scope != nil {
			meta["scope"] = *config.ContextMeta.Scope
		}
		if len(config.ContextMeta.Properties) > 0 {
			meta["properties"] = config.ContextMeta.Properties
		}
		filter["context_meta"] = meta
	}
}

func renderRequestConditions(conds []proxyv1alpha1.PraxisRequestCondition) []map[string]any {
	if len(conds) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(conds))
	for _, cond := range conds {
		entry := map[string]any{}
		if cond.When != nil {
			entry["when"] = renderRequestMatch(cond.When)
		}
		if cond.Unless != nil {
			entry["unless"] = renderRequestMatch(cond.Unless)
		}
		out = append(out, entry)
	}
	return out
}

func renderRequestMatch(match *proxyv1alpha1.PraxisRequestMatch) map[string]any {
	out := map[string]any{}
	if match.GRPC != nil {
		out["grpc"] = *match.GRPC
	}
	if match.Path != nil {
		out["path"] = *match.Path
	}
	if match.PathPrefix != nil {
		out["path_prefix"] = *match.PathPrefix
	}
	if len(match.Methods) > 0 {
		out["methods"] = match.Methods
	}
	if len(match.Headers) > 0 {
		out["headers"] = match.Headers
	}
	if len(match.HeadersPresent) > 0 {
		out["headers_present"] = match.HeadersPresent
	}
	if match.BoundUpstream != nil {
		out["bound_upstream"] = renderUpstreamMatch(match.BoundUpstream)
	}
	if match.SelectedUpstream != nil {
		out["selected_upstream"] = renderUpstreamMatch(match.SelectedUpstream)
	}
	return out
}

func renderUpstreamMatch(match *proxyv1alpha1.PraxisSelectedUpstreamMatch) map[string]any {
	out := map[string]any{}
	if match.ApplicationProtocol != nil {
		out["application_protocol"] = *match.ApplicationProtocol
	}
	if match.ApplicationProvider != nil {
		out["application_provider"] = *match.ApplicationProvider
	}
	return out
}

func renderResponseConditions(conds []proxyv1alpha1.PraxisResponseCondition) []map[string]any {
	if len(conds) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(conds))
	for _, cond := range conds {
		entry := map[string]any{}
		if cond.When != nil {
			entry["when"] = renderResponseMatch(cond.When)
		}
		if cond.Unless != nil {
			entry["unless"] = renderResponseMatch(cond.Unless)
		}
		out = append(out, entry)
	}
	return out
}

func renderResponseMatch(match *proxyv1alpha1.PraxisResponseMatch) map[string]any {
	out := map[string]any{}
	if len(match.Status) > 0 {
		out["status"] = match.Status
	}
	if len(match.Headers) > 0 {
		out["headers"] = match.Headers
	}
	if len(match.HeadersPresent) > 0 {
		out["headers_present"] = match.HeadersPresent
	}
	return out
}
