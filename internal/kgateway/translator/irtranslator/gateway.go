package irtranslator

import (
	"sort"
	"strconv"

	envoyclusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoylistenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	envoyroutev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"golang.org/x/net/context"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"istio.io/istio/pkg/slices"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	extensionsplug "github.com/kgateway-dev/kgateway/v2/internal/kgateway/extensions2/plugin"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/ir"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/query"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/translator/metrics"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/wellknown"
	"github.com/kgateway-dev/kgateway/v2/pkg/logging"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/reporter"
	"github.com/kgateway-dev/kgateway/v2/pkg/reports"
	"github.com/kgateway-dev/kgateway/v2/pkg/settings"
)

var logger = logging.New("translator/ir")

type Translator struct {
	ContributedPolicies  map[schema.GroupKind]extensionsplug.PolicyPlugin
	RouteReplacementMode settings.RouteReplacementMode
	metrics              metrics.TranslatorMetricsRecorder
}

type TranslationPassPlugins map[schema.GroupKind]*TranslationPass

type TranslationResult struct {
	Routes        []*envoyroutev3.RouteConfiguration
	Listeners     []*envoylistenerv3.Listener
	ExtraClusters []*envoyclusterv3.Cluster
}

// Translate IR to gateway. IR is self contained, so no need for krt context
func (t *Translator) Translate(gw ir.GatewayIR, reporter reports.Reporter) TranslationResult {
	if t.metrics == nil {
		t.metrics = metrics.NewTranslatorMetricsRecorder("TranslateGatewayIR")
	}

	defer t.metrics.TranslationStart()(nil)

	pass := t.newPass(reporter)
	var res TranslationResult

	for _, l := range gw.Listeners {
		// TODO: propagate errors so we can allow the retain last config mode
		l, routes := t.ComputeListener(context.TODO(), pass, gw, l, reporter)
		res.Listeners = append(res.Listeners, l)
		res.Routes = append(res.Routes, routes...)
	}

	for _, c := range pass {
		if c != nil {
			r := c.ResourcesToAdd(context.TODO())
			res.ExtraClusters = append(res.ExtraClusters, r.Clusters...)
		}
	}

	return res
}

func getReporterForFilterChain(gw ir.GatewayIR, reporter reports.Reporter, filterChainName string) reporter.ListenerReporter {
	listener := slices.FindFunc(gw.SourceObject.Listeners, func(l ir.Listener) bool {
		return filterChainName == query.GenerateRouteKey(l.Parent, string(l.Name))
	})
	if listener == nil {
		// This should never happen, but keep this as a safeguard.
		return reporter.Gateway(gw.SourceObject.Obj).ListenerName(filterChainName)
	}
	return listener.GetParentReporter(reporter).ListenerName(string(listener.Name))
}

func (t *Translator) ComputeListener(
	ctx context.Context,
	pass TranslationPassPlugins,
	gw ir.GatewayIR,
	lis ir.ListenerIR,
	reporter reports.Reporter,
) (*envoylistenerv3.Listener, []*envoyroutev3.RouteConfiguration) {
	hasTls := false
	gwreporter := reporter.Gateway(gw.SourceObject.Obj)
	var routes []*envoyroutev3.RouteConfiguration
	ret := &envoylistenerv3.Listener{
		Name:    lis.Name,
		Address: computeListenerAddress(lis.BindAddress, lis.BindPort, gwreporter),
	}
	if gw.PerConnectionBufferLimitBytes != nil {
		ret.PerConnectionBufferLimitBytes = &wrapperspb.UInt32Value{Value: *gw.PerConnectionBufferLimitBytes}
	}
	t.runListenerPlugins(ctx, pass, gw, lis, ret)

	for _, hfc := range lis.HttpFilterChain {
		fct := filterChainTranslator{
			listener:        lis,
			gateway:         gw,
			routeConfigName: hfc.FilterChainName,
			PluginPass:      pass,
		}

		// compute routes
		hr := httpRouteConfigurationTranslator{
			gw:                       gw,
			listener:                 lis,
			routeConfigName:          hfc.FilterChainName,
			fc:                       hfc.FilterChainCommon,
			attachedPolicies:         hfc.AttachedPolicies,
			reporter:                 reporter,
			requireTlsOnVirtualHosts: hfc.FilterChainCommon.TLS != nil,
			PluginPass:               pass,
			logger:                   logger.With("route_config_name", hfc.FilterChainName),
			routeReplacementMode:     t.RouteReplacementMode,
		}
		rc := hr.ComputeRouteConfiguration(ctx, hfc.Vhosts)
		if rc != nil {
			routes = append(routes, rc)

			// Record metrics for the number of domains per listener.
			// Only one domain per virtual host is supported currently, but that may change in the future,
			// so loop through the virtual hosts and count the domains.
			domainsOnListener := 0
			for _, vhost := range rc.GetVirtualHosts() {
				domainsOnListener += len(vhost.GetDomains())
			}

			setDomainsPerListener(domainsPerListenerMetricLabels{
				Namespace:   hr.gw.SourceObject.GetNamespace(),
				GatewayName: hr.gw.SourceObject.GetName(),
				Port:        strconv.Itoa(int(lis.BindPort)),
			}, domainsOnListener)
		}

		// compute chains

		// TODO: make sure that all matchers are unique
		rl := getReporterForFilterChain(gw, reporter, hfc.FilterChainName)
		fc := fct.initFilterChain(ctx, hfc.FilterChainCommon, rl)
		fc.Filters = fct.computeHttpFilters(ctx, hfc, rl)
		ret.FilterChains = append(ret.GetFilterChains(), fc)
		if len(hfc.Matcher.SniDomains) > 0 {
			hasTls = true
		}
	}

	fct := filterChainTranslator{
		listener:   lis,
		gateway:    gw,
		PluginPass: pass,
	}

	for _, tfc := range lis.TcpFilterChain {
		rl := getReporterForFilterChain(gw, reporter, tfc.FilterChainName)
		fc := fct.initFilterChain(ctx, tfc.FilterChainCommon, rl)
		fc.Filters = fct.computeTcpFilters(ctx, tfc, rl)
		ret.FilterChains = append(ret.GetFilterChains(), fc)
		if len(tfc.Matcher.SniDomains) > 0 {
			hasTls = true
		}
	}
	// sort filter chains for idempotency
	sort.Slice(ret.GetFilterChains(), func(i, j int) bool {
		return ret.GetFilterChains()[i].GetName() < ret.GetFilterChains()[j].GetName()
	})
	if hasTls {
		ret.ListenerFilters = append(ret.GetListenerFilters(), tlsInspectorFilter())
	}

	return ret, routes
}

func (t *Translator) runListenerPlugins(ctx context.Context, pass TranslationPassPlugins, gw ir.GatewayIR, l ir.ListenerIR, out *envoylistenerv3.Listener) {
	var attachedPolicies ir.AttachedPolicies
	attachedPolicies.Append(l.AttachedPolicies, gw.AttachedHttpPolicies)
	for _, gk := range attachedPolicies.ApplyOrderedGroupKinds() {
		pols := attachedPolicies.Policies[gk]
		pass := pass[gk]
		if pass == nil {
			// TODO: report user error - they attached a non http policy
			continue
		}
		for _, pol := range pols {
			pctx := &ir.ListenerContext{
				Policy: pol.PolicyIr,
				PolicyAncestorRef: gwv1.ParentReference{
					Group:     ptr.To(gwv1.Group(wellknown.GatewayGVK.Group)),
					Kind:      ptr.To(gwv1.Kind(wellknown.GatewayGVK.Kind)),
					Namespace: ptr.To(gwv1.Namespace(gw.SourceObject.GetNamespace())),
					Name:      gwv1.ObjectName(gw.SourceObject.GetName()),
				},
			}
			pass.ApplyListenerPlugin(ctx, pctx, out)
			// TODO: check return value, if error returned, log error and report condition
		}
	}
}

func (t *Translator) newPass(reporter reports.Reporter) TranslationPassPlugins {
	ret := TranslationPassPlugins{}
	for k, v := range t.ContributedPolicies {
		if v.NewGatewayTranslationPass == nil {
			continue
		}
		tp := v.NewGatewayTranslationPass(context.TODO(), ir.GwTranslationCtx{}, reporter)
		if tp != nil {
			ret[k] = &TranslationPass{
				ProxyTranslationPass: tp,
				Name:                 v.Name,
				MergePolicies:        v.MergePolicies,
			}
		}
	}
	return ret
}

type TranslationPass struct {
	ir.ProxyTranslationPass
	Name string
	// If the plugin supports policy merging, it must implement MergePolicies
	// such that policies ordered from high to low priority, both hierarchically
	// and within the same hierarchy, are Merged into a single Policy
	MergePolicies func(policies []ir.PolicyAtt) ir.PolicyAtt
}
