package endpointpicker

import (
	"context"
	"fmt"
	"time"

	envoyclusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoycorev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoyendpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	envoyroutev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	envoytlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	upstreamsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	skubeclient "istio.io/istio/pkg/config/schema/kubeclient"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	infextv1a2 "sigs.k8s.io/gateway-api-inference-extension/api/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/client-go/clientset/versioned"

	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/extensions2/common"
	extplug "github.com/kgateway-dev/kgateway/v2/internal/kgateway/extensions2/plugin"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/plugins"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/utils"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/wellknown"
	"github.com/kgateway-dev/kgateway/v2/pkg/logging"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/ir"
	"github.com/kgateway-dev/kgateway/v2/pkg/reports"
)

// Derived from upstream Gateway API Inference Extension defaults (testdata/envoy.yaml).
const DefaultExtProcMaxRequests = 40000

var (
	logger = logging.New("plugin/inference-epp")

	inferencePoolGVK = wellknown.InferencePoolGVK
	inferencePoolGVR = inferencePoolGVK.GroupVersion().WithResource("inferencepools")
)

func registerTypes(cli versioned.Interface) {
	skubeclient.Register[*infextv1a2.InferencePool](
		inferencePoolGVR,
		inferencePoolGVK,
		func(c skubeclient.ClientGetter, namespace string, o metav1.ListOptions) (runtime.Object, error) {
			return cli.InferenceV1alpha2().InferencePools(namespace).List(context.Background(), o)
		},
		func(c skubeclient.ClientGetter, namespace string, o metav1.ListOptions) (watch.Interface, error) {
			return cli.InferenceV1alpha2().InferencePools(namespace).Watch(context.Background(), o)
		},
	)
}

func NewPlugin(ctx context.Context, commonCol *common.CommonCollections) *extplug.Plugin {
	// Create the inference extension clientset.
	cli, err := versioned.NewForConfig(commonCol.Client.RESTConfig())
	if err != nil {
		logger.Error("failed to create inference extension client", "error", err)
		return nil
	}

	// Register the InferencePool type to enable dynamic object translation.
	registerTypes(cli)

	// Create an InferencePool krt collection.
	poolCol := krt.WrapClient(kclient.NewFiltered[*infextv1a2.InferencePool](
		commonCol.Client,
		kclient.Filter{ObjectFilter: commonCol.Client.ObjectFilter()},
	), commonCol.KrtOpts.ToOptions("InferencePool")...)

	return NewPluginFromCollections(ctx, commonCol, poolCol)
}

func NewPluginFromCollections(
	ctx context.Context,
	commonCol *common.CommonCollections,
	poolCol krt.Collection[*infextv1a2.InferencePool],
) *extplug.Plugin {
	// Get the Services KRT collection for computing InferencePool status
	svcCol := commonCol.Services

	// The InferencePool group kind used by the BackendObjectIR and the ContributesBackendObjectIRs plugin.
	gk := schema.GroupKind{
		Group: inferencePoolGVK.Group,
		Kind:  inferencePoolGVK.Kind,
	}

	backendCol := krt.NewCollection(poolCol, func(kctx krt.HandlerContext, pool *infextv1a2.InferencePool) *ir.BackendObjectIR {
		// Validate the InferencePool and create the associated IR.
		irPool := newInferencePool(pool)
		errs := validatePool(pool, svcCol)
		if errs != nil {
			// If there are validation errors, add them to the IR.
			irPool.errors = errs
		}

		// Create a BackendObjectIR IR representation from the given InferencePool.
		objSrc := ir.ObjectSource{
			Kind:      gk.Kind,
			Group:     gk.Group,
			Namespace: pool.Namespace,
			Name:      pool.Name,
		}
		backend := ir.NewBackendObjectIR(objSrc, 0, "")
		backend.Obj = pool
		backend.GvPrefix = "endpoint-picker"
		backend.CanonicalHostname = ""
		backend.ObjIr = irPool
		return &backend
	}, commonCol.KrtOpts.ToOptions("InferencePoolIR")...)

	policyCol := krt.NewCollection(poolCol, func(kctx krt.HandlerContext, pool *infextv1a2.InferencePool) *ir.PolicyWrapper {
		// Validate the InferencePool and create the associated IR.
		irPool := newInferencePool(pool)
		errs := validatePool(pool, svcCol)
		if errs != nil {
			// If there are validation errors, add them to the IR.
			irPool.errors = errs
		}

		// Create a PolicyWrapper IR representation from the given InferencePool.
		return &ir.PolicyWrapper{
			ObjectSource: ir.ObjectSource{
				Group:     gk.Group,
				Kind:      gk.Kind,
				Namespace: pool.Namespace,
				Name:      pool.Name,
			},
			Policy:   pool,
			PolicyIR: irPool,
		}
	})

	// Return a plugin that contributes a policy and backend.
	return &extplug.Plugin{
		ContributesBackends: map[schema.GroupKind]extplug.BackendPlugin{
			gk: {
				Backends: backendCol,
				BackendInit: ir.BackendInit{
					InitBackend: processBackendObjectIR,
				},
			},
		},
		ContributesPolicies: map[schema.GroupKind]extplug.PolicyPlugin{
			gk: {
				Name:                      "endpoint-picker",
				Policies:                  policyCol,
				NewGatewayTranslationPass: newEndpointPickerPass,
			},
		},
		ContributesRegistration: map[schema.GroupKind]func(){
			gk: buildRegisterCallback(ctx, commonCol, backendCol),
		},
	}
}

// endpointPickerPass implements ir.ProxyTranslationPass. It collects any references to IR inferencePools.
type endpointPickerPass struct {
	// usedPools defines a map of IR inferencePools keyed by NamespacedName.
	usedPools map[types.NamespacedName]*inferencePool
	ir.UnimplementedProxyTranslationPass

	reporter reports.Reporter
}

var _ ir.ProxyTranslationPass = &endpointPickerPass{}

func newEndpointPickerPass(ctx context.Context, tctx ir.GwTranslationCtx, reporter reports.Reporter) ir.ProxyTranslationPass {
	return &endpointPickerPass{
		usedPools: make(map[types.NamespacedName]*inferencePool),
		reporter:  reporter,
	}
}

func (p *endpointPickerPass) Name() string {
	return "endpoint-picker"
}

// ApplyForBackend updates the Envoy route for each InferencePool-backed HTTPRoute.
func (p *endpointPickerPass) ApplyForBackend(
	ctx context.Context,
	pCtx *ir.RouteBackendContext,
	in ir.HttpBackend,
	out *envoyroutev3.Route,
) error {
	if p == nil || pCtx == nil || pCtx.Backend == nil {
		return nil
	}

	// Ensure the backend object is an InferencePool.
	irPool, ok := pCtx.Backend.ObjIr.(*inferencePool)
	if !ok || irPool == nil {
		return nil
	}

	// Store this pool in our map, keyed by NamespacedName.
	nn := types.NamespacedName{
		Namespace: irPool.objMeta.GetNamespace(),
		Name:      irPool.objMeta.GetName(),
	}
	p.usedPools[nn] = irPool

	// Ensure RouteAction is initialized.
	if out.GetRoute() == nil {
		out.Action = &envoyroutev3.Route_Route{
			Route: &envoyroutev3.RouteAction{},
		}
	}

	// Point the route to the ORIGINAL_DST cluster for this pool.
	out.GetRoute().ClusterSpecifier = &envoyroutev3.RouteAction_Cluster{
		Cluster: clusterNameOriginalDst(irPool.objMeta.GetName(), irPool.objMeta.GetNamespace()),
	}

	// Build the route-level ext_proc override that points to this pool's ext_proc cluster.
	override := &extprocv3.ExtProcPerRoute{
		Override: &extprocv3.ExtProcPerRoute_Overrides{
			Overrides: &extprocv3.ExtProcOverrides{
				GrpcService: &envoycorev3.GrpcService{
					Timeout: durationpb.New(10 * time.Second),
					TargetSpecifier: &envoycorev3.GrpcService_EnvoyGrpc_{
						EnvoyGrpc: &envoycorev3.GrpcService_EnvoyGrpc{
							ClusterName: clusterNameExtProc(
								irPool.objMeta.GetName(),
								irPool.objMeta.GetNamespace(),
							),
							Authority: authorityForPool(irPool),
						},
					},
				},
			},
		},
	}

	// Attach per-route override to typed_per_filter_config.
	pCtx.TypedFilterConfig.AddTypedConfig(wellknown.InfPoolTransformationFilterName, override)

	return nil
}

// HttpFilters returns one ext_proc filter, using the well-known filter name.
func (p *endpointPickerPass) HttpFilters(ctx context.Context, fc ir.FilterChainCommon) ([]plugins.StagedHttpFilter, error) {
	if p == nil || len(p.usedPools) == 0 {
		return nil, nil
	}

	// Create a pool as placeholder for the static config
	tmpPool := &inferencePool{
		objMeta: metav1.ObjectMeta{
			Name:      "placeholder-pool",
			Namespace: "placeholder-namespace",
		},
		configRef: &service{
			ObjectSource: ir.ObjectSource{Name: "placeholder-service"},
			ports:        []servicePort{{name: "grpc", portNum: 9002}},
		},
	}

	// Static ExternalProcessor that will be overridden by ExtProcPerRoute
	extProcSettings := &extprocv3.ExternalProcessor{
		GrpcService: &envoycorev3.GrpcService{
			TargetSpecifier: &envoycorev3.GrpcService_EnvoyGrpc_{
				EnvoyGrpc: &envoycorev3.GrpcService_EnvoyGrpc{
					ClusterName: clusterNameExtProc(
						tmpPool.objMeta.GetName(),
						tmpPool.objMeta.GetNamespace(),
					),
					Authority: authorityForPool(tmpPool),
				},
			},
		},
		ProcessingMode: &extprocv3.ProcessingMode{
			RequestHeaderMode:   extprocv3.ProcessingMode_SEND,
			RequestBodyMode:     extprocv3.ProcessingMode_FULL_DUPLEX_STREAMED,
			RequestTrailerMode:  extprocv3.ProcessingMode_SEND,
			ResponseBodyMode:    extprocv3.ProcessingMode_FULL_DUPLEX_STREAMED,
			ResponseHeaderMode:  extprocv3.ProcessingMode_SEND,
			ResponseTrailerMode: extprocv3.ProcessingMode_SEND,
		},
		MessageTimeout:   durationpb.New(5 * time.Second),
		FailureModeAllow: false,
	}

	stagedFilter, err := plugins.NewStagedFilter(
		wellknown.InfPoolTransformationFilterName,
		extProcSettings,
		plugins.BeforeStage(plugins.RouteStage),
	)
	if err != nil {
		return nil, err
	}

	return []plugins.StagedHttpFilter{stagedFilter}, nil
}

// ResourcesToAdd returns the ext_proc clusters for all used InferencePools.
func (p *endpointPickerPass) ResourcesToAdd(ctx context.Context) ir.Resources {
	if p == nil || len(p.usedPools) == 0 {
		return ir.Resources{}
	}

	var clusters []*envoyclusterv3.Cluster
	for _, pool := range p.usedPools {
		c := buildExtProcCluster(pool)
		if c != nil {
			clusters = append(clusters, c)
		}
	}

	return ir.Resources{Clusters: clusters}
}

// processBackendObjectIR builds the ORIGINAL_DST cluster for each InferencePool.
func processBackendObjectIR(ctx context.Context, in ir.BackendObjectIR, out *envoyclusterv3.Cluster) *ir.EndpointsForBackend {
	out.ConnectTimeout = durationpb.New(1000 * time.Second)

	out.ClusterDiscoveryType = &envoyclusterv3.Cluster_Type{
		Type: envoyclusterv3.Cluster_ORIGINAL_DST,
	}

	out.LbPolicy = envoyclusterv3.Cluster_CLUSTER_PROVIDED
	out.LbConfig = &envoyclusterv3.Cluster_OriginalDstLbConfig_{
		OriginalDstLbConfig: &envoyclusterv3.Cluster_OriginalDstLbConfig{
			UseHttpHeader:  true,
			HttpHeaderName: "x-gateway-destination-endpoint",
		},
	}

	out.CircuitBreakers = &envoyclusterv3.CircuitBreakers{
		Thresholds: []*envoyclusterv3.CircuitBreakers_Thresholds{
			{
				MaxConnections:     wrapperspb.UInt32(DefaultExtProcMaxRequests),
				MaxPendingRequests: wrapperspb.UInt32(DefaultExtProcMaxRequests),
				MaxRequests:        wrapperspb.UInt32(DefaultExtProcMaxRequests),
			},
		},
	}

	out.Name = clusterNameOriginalDst(in.Name, in.Namespace)

	return nil
}

// buildExtProcCluster builds and returns a "STRICT_DNS" cluster from the given pool.
func buildExtProcCluster(pool *inferencePool) *envoyclusterv3.Cluster {
	if pool == nil || pool.configRef == nil || len(pool.configRef.ports) != 1 {
		return nil
	}

	name := clusterNameExtProc(pool.objMeta.GetName(), pool.objMeta.GetNamespace())
	c := &envoyclusterv3.Cluster{
		Name:           name,
		ConnectTimeout: durationpb.New(10 * time.Second),
		ClusterDiscoveryType: &envoyclusterv3.Cluster_Type{
			Type: envoyclusterv3.Cluster_STRICT_DNS,
		},
		LbPolicy: envoyclusterv3.Cluster_LEAST_REQUEST,
		LoadAssignment: &envoyendpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*envoyendpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*envoyendpointv3.LbEndpoint{{
					HealthStatus: envoycorev3.HealthStatus_HEALTHY,
					HostIdentifier: &envoyendpointv3.LbEndpoint_Endpoint{
						Endpoint: &envoyendpointv3.Endpoint{
							Address: &envoycorev3.Address{
								Address: &envoycorev3.Address_SocketAddress{
									SocketAddress: &envoycorev3.SocketAddress{
										Address:  fmt.Sprintf("%s.%s.svc", pool.configRef.Name, pool.objMeta.Namespace),
										Protocol: envoycorev3.SocketAddress_TCP,
										PortSpecifier: &envoycorev3.SocketAddress_PortValue{
											PortValue: uint32(pool.configRef.ports[0].portNum),
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
		// Ensure Envoy accepts untrusted certificates.
		TransportSocket: &envoycorev3.TransportSocket{
			Name: "envoy.transport_sockets.tls",
			ConfigType: &envoycorev3.TransportSocket_TypedConfig{
				TypedConfig: func() *anypb.Any {
					tlsCtx := &envoytlsv3.UpstreamTlsContext{
						CommonTlsContext: &envoytlsv3.CommonTlsContext{
							ValidationContextType: &envoytlsv3.CommonTlsContext_ValidationContext{},
						},
					}
					anyTLS, _ := utils.MessageToAny(tlsCtx)
					return anyTLS
				}(),
			},
		},
	}

	http2Opts := &upstreamsv3.HttpProtocolOptions{
		UpstreamProtocolOptions: &upstreamsv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &upstreamsv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &upstreamsv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
					Http2ProtocolOptions: &envoycorev3.Http2ProtocolOptions{},
				},
			},
		},
	}

	anyHttp2, _ := utils.MessageToAny(http2Opts)
	c.TypedExtensionProtocolOptions = map[string]*anypb.Any{
		"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": anyHttp2,
	}

	return c
}

func clusterNameExtProc(name, ns string) string {
	return fmt.Sprintf("endpointpicker_%s_%s_ext_proc", name, ns)
}

func clusterNameOriginalDst(name, ns string) string {
	return fmt.Sprintf("endpointpicker_%s_%s_original_dst", name, ns)
}

// authorityForPool formats the gRPC authority based on the given InferencePool IR.
func authorityForPool(pool *inferencePool) string {
	ns := pool.objMeta.GetNamespace()
	svc := pool.configRef.Name
	port := pool.configRef.ports[0].portNum
	return fmt.Sprintf("%s.%s.svc:%d", svc, ns, port)
}
