/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package genericapiserver

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/kubernetes/pkg/admission"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/latest"
	"k8s.io/kubernetes/pkg/api/rest"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apiserver"
	"k8s.io/kubernetes/pkg/auth/authenticator"
	"k8s.io/kubernetes/pkg/auth/authorizer"
	"k8s.io/kubernetes/pkg/auth/handlers"
	"k8s.io/kubernetes/pkg/registry/generic"
	genericetcd "k8s.io/kubernetes/pkg/registry/generic/etcd"
	ipallocator "k8s.io/kubernetes/pkg/registry/service/ipallocator"
	"k8s.io/kubernetes/pkg/storage"
	"k8s.io/kubernetes/pkg/ui"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/util/sets"

	systemd "github.com/coreos/go-systemd/daemon"
	"github.com/emicklei/go-restful"
	"github.com/emicklei/go-restful/swagger"
	"github.com/golang/glog"
	"golang.org/x/net/context"
)

const (
	DefaultEtcdPathPrefix = "/registry"
)

// StorageDestinations is a mapping from API group & resource to
// the underlying storage interfaces.
type StorageDestinations struct {
	APIGroups map[string]*StorageDestinationsForAPIGroup
}

type StorageDestinationsForAPIGroup struct {
	Default   storage.Interface
	Overrides map[string]storage.Interface
}

func NewStorageDestinations() StorageDestinations {
	return StorageDestinations{
		APIGroups: map[string]*StorageDestinationsForAPIGroup{},
	}
}

func (s *StorageDestinations) AddAPIGroup(group string, defaultStorage storage.Interface) {
	s.APIGroups[group] = &StorageDestinationsForAPIGroup{
		Default:   defaultStorage,
		Overrides: map[string]storage.Interface{},
	}
}

func (s *StorageDestinations) AddStorageOverride(group, resource string, override storage.Interface) {
	if _, ok := s.APIGroups[group]; !ok {
		s.AddAPIGroup(group, nil)
	}
	if s.APIGroups[group].Overrides == nil {
		s.APIGroups[group].Overrides = map[string]storage.Interface{}
	}
	s.APIGroups[group].Overrides[resource] = override
}

func (s *StorageDestinations) Get(group, resource string) storage.Interface {
	apigroup, ok := s.APIGroups[group]
	if !ok {
		glog.Errorf("No storage defined for API group: '%s'", apigroup)
		return nil
	}
	if apigroup.Overrides != nil {
		if client, exists := apigroup.Overrides[resource]; exists {
			return client
		}
	}
	return apigroup.Default
}

// Get all backends for all registered storage destinations.
// Used for getting all instances for health validations.
func (s *StorageDestinations) Backends() []string {
	backends := sets.String{}
	for _, group := range s.APIGroups {
		if group.Default != nil {
			for _, backend := range group.Default.Backends(context.TODO()) {
				backends.Insert(backend)
			}
		}
		if group.Overrides != nil {
			for _, storage := range group.Overrides {
				for _, backend := range storage.Backends(context.TODO()) {
					backends.Insert(backend)
				}
			}
		}
	}
	return backends.List()
}

// Specifies the overrides for various API group versions.
// This can be used to enable/disable entire group versions or specific resources.
type APIGroupVersionOverride struct {
	// Whether to enable or disable this group version.
	Disable bool
	// List of overrides for individual resources in this group version.
	ResourceOverrides map[string]bool
}

// Info about an API group.
type APIGroupInfo struct {
	GroupMeta latest.GroupMeta
	// Info about the resources in this group. Its a map from version to resource to the storage.
	VersionedResourcesStorageMap map[string]map[string]rest.Storage
	// True, if this is the legacy group ("/v1").
	IsLegacyGroup bool
	// OptionsExternalVersion controls the APIVersion used for common objects in the
	// schema like api.Status, api.DeleteOptions, and api.ListOptions. Other implementors may
	// define a version "v1beta1" but want to use the Kubernetes "v1" internal objects.
	// If nil, defaults to groupMeta.GroupVersion.
	// TODO: Remove this when https://github.com/kubernetes/kubernetes/issues/19018 is fixed.
	OptionsExternalVersion *unversioned.GroupVersion
}

// Config is a structure used to configure a GenericAPIServer.
type Config struct {
	StorageDestinations StorageDestinations
	// StorageVersions is a map between groups and their storage versions
	StorageVersions map[string]string
	// allow downstream consumers to disable the core controller loops
	EnableLogsSupport bool
	EnableUISupport   bool
	// allow downstream consumers to disable swagger
	EnableSwaggerSupport bool
	// Allows api group versions or specific resources to be conditionally enabled/disabled.
	APIGroupVersionOverrides map[string]APIGroupVersionOverride
	// allow downstream consumers to disable the index route
	EnableIndex           bool
	EnableProfiling       bool
	EnableWatchCache      bool
	APIPrefix             string
	APIGroupPrefix        string
	CorsAllowedOriginList []string
	Authenticator         authenticator.Request
	// TODO(roberthbailey): Remove once the server no longer supports http basic auth.
	SupportsBasicAuth      bool
	Authorizer             authorizer.Authorizer
	AdmissionControl       admission.Interface
	MasterServiceNamespace string

	// Map requests to contexts. Exported so downstream consumers can provider their own mappers
	RequestContextMapper api.RequestContextMapper

	// If specified, all web services will be registered into this container
	RestfulContainer *restful.Container

	// If specified, requests will be allocated a random timeout between this value, and twice this value.
	// Note that it is up to the request handlers to ignore or honor this timeout. In seconds.
	MinRequestTimeout int

	// Number of masters running; all masters must be started with the
	// same value for this field. (Numbers > 1 currently untested.)
	MasterCount int

	// The port on PublicAddress where a read-write server will be installed.
	// Defaults to 6443 if not set.
	ReadWritePort int

	// ExternalHost is the host name to use for external (public internet) facing URLs (e.g. Swagger)
	ExternalHost string

	// PublicAddress is the IP address where members of the cluster (kubelet,
	// kube-proxy, services, etc.) can reach the GenericAPIServer.
	// If nil or 0.0.0.0, the host's default interface will be used.
	PublicAddress net.IP

	// Control the interval that pod, node IP, and node heath status caches
	// expire.
	CacheTimeout time.Duration

	// The range of IPs to be assigned to services with type=ClusterIP or greater
	ServiceClusterIPRange *net.IPNet

	// The IP address for the GenericAPIServer service (must be inside ServiceClusterIPRange
	ServiceReadWriteIP net.IP

	// The range of ports to be assigned to services with type=NodePort or greater
	ServiceNodePortRange util.PortRange

	// Used to customize default proxy dial/tls options
	ProxyDialer          apiserver.ProxyDialerFunc
	ProxyTLSClientConfig *tls.Config

	// Additional ports to be exposed on the GenericAPIServer service
	// extraServicePorts is injectable in the event that more ports
	// (other than the default 443/tcp) are exposed on the GenericAPIServer
	// and those ports need to be load balanced by the GenericAPIServer
	// service because this pkg is linked by out-of-tree projects
	// like openshift which want to use the GenericAPIServer but also do
	// more stuff.
	ExtraServicePorts []api.ServicePort
	// Additional ports to be exposed on the GenericAPIServer endpoints
	// Port names should align with ports defined in ExtraServicePorts
	ExtraEndpointPorts []api.EndpointPort

	KubernetesServiceNodePort int
}

// GenericAPIServer contains state for a Kubernetes cluster api server.
type GenericAPIServer struct {
	// "Inputs", Copied from Config
	ServiceClusterIPRange *net.IPNet
	ServiceNodePortRange  util.PortRange
	cacheTimeout          time.Duration
	MinRequestTimeout     time.Duration

	mux                      apiserver.Mux
	MuxHelper                *apiserver.MuxHelper
	HandlerContainer         *restful.Container
	RootWebService           *restful.WebService
	enableLogsSupport        bool
	enableUISupport          bool
	enableSwaggerSupport     bool
	enableProfiling          bool
	enableWatchCache         bool
	APIPrefix                string
	APIGroupPrefix           string
	corsAllowedOriginList    []string
	authenticator            authenticator.Request
	authorizer               authorizer.Authorizer
	AdmissionControl         admission.Interface
	MasterCount              int
	ApiGroupVersionOverrides map[string]APIGroupVersionOverride
	RequestContextMapper     api.RequestContextMapper

	// External host is the name that should be used in external (public internet) URLs for this GenericAPIServer
	externalHost string
	// ClusterIP is the IP address of the GenericAPIServer within the cluster.
	ClusterIP            net.IP
	PublicReadWritePort  int
	ServiceReadWriteIP   net.IP
	ServiceReadWritePort int
	masterServices       *util.Runner
	ExtraServicePorts    []api.ServicePort
	ExtraEndpointPorts   []api.EndpointPort

	// storage contains the RESTful endpoints exposed by this GenericAPIServer
	storage map[string]rest.Storage

	// "Outputs"
	Handler         http.Handler
	InsecureHandler http.Handler

	// Used for custom proxy dialing, and proxy TLS options
	ProxyTransport http.RoundTripper

	KubernetesServiceNodePort int
}

func (s *GenericAPIServer) StorageDecorator() generic.StorageDecorator {
	if s.enableWatchCache {
		return genericetcd.StorageWithCacher
	}
	return generic.UndecoratedStorage
}

// setDefaults fills in any fields not set that are required to have valid data.
func setDefaults(c *Config) {
	if c.ServiceClusterIPRange == nil {
		defaultNet := "10.0.0.0/24"
		glog.Warningf("Network range for service cluster IPs is unspecified. Defaulting to %v.", defaultNet)
		_, serviceClusterIPRange, err := net.ParseCIDR(defaultNet)
		if err != nil {
			glog.Fatalf("Unable to parse CIDR: %v", err)
		}
		if size := ipallocator.RangeSize(serviceClusterIPRange); size < 8 {
			glog.Fatalf("The service cluster IP range must be at least %d IP addresses", 8)
		}
		c.ServiceClusterIPRange = serviceClusterIPRange
	}
	if c.ServiceReadWriteIP == nil {
		// Select the first valid IP from ServiceClusterIPRange to use as the GenericAPIServer service IP.
		serviceReadWriteIP, err := ipallocator.GetIndexedIP(c.ServiceClusterIPRange, 1)
		if err != nil {
			glog.Fatalf("Failed to generate service read-write IP for GenericAPIServer service: %v", err)
		}
		glog.V(4).Infof("Setting GenericAPIServer service IP to %q (read-write).", serviceReadWriteIP)
		c.ServiceReadWriteIP = serviceReadWriteIP
	}
	if c.ServiceNodePortRange.Size == 0 {
		// TODO: Currently no way to specify an empty range (do we need to allow this?)
		// We should probably allow this for clouds that don't require NodePort to do load-balancing (GCE)
		// but then that breaks the strict nestedness of ServiceType.
		// Review post-v1
		defaultServiceNodePortRange := util.PortRange{Base: 30000, Size: 2768}
		c.ServiceNodePortRange = defaultServiceNodePortRange
		glog.Infof("Node port range unspecified. Defaulting to %v.", c.ServiceNodePortRange)
	}
	if c.MasterCount == 0 {
		// Clearly, there will be at least one GenericAPIServer.
		c.MasterCount = 1
	}
	if c.ReadWritePort == 0 {
		c.ReadWritePort = 6443
	}
	if c.CacheTimeout == 0 {
		c.CacheTimeout = 5 * time.Second
	}
	if c.RequestContextMapper == nil {
		c.RequestContextMapper = api.NewRequestContextMapper()
	}
}

// New returns a new instance of GenericAPIServer from the given config.
// Certain config fields will be set to a default value if unset,
// including:
//   ServiceClusterIPRange
//   ServiceNodePortRange
//   MasterCount
//   ReadWritePort
//   PublicAddress
// Public fields:
//   Handler -- The returned GenericAPIServer has a field TopHandler which is an
//   http.Handler which handles all the endpoints provided by the GenericAPIServer,
//   including the API, the UI, and miscellaneous debugging endpoints.  All
//   these are subject to authorization and authentication.
//   InsecureHandler -- an http.Handler which handles all the same
//   endpoints as Handler, but no authorization and authentication is done.
// Public methods:
//   HandleWithAuth -- Allows caller to add an http.Handler for an endpoint
//   that uses the same authentication and authorization (if any is configured)
//   as the GenericAPIServer's built-in endpoints.
//   If the caller wants to add additional endpoints not using the GenericAPIServer's
//   auth, then the caller should create a handler for those endpoints, which delegates the
//   any unhandled paths to "Handler".
func New(c *Config) *GenericAPIServer {
	setDefaults(c)

	s := &GenericAPIServer{
		ServiceClusterIPRange:    c.ServiceClusterIPRange,
		ServiceNodePortRange:     c.ServiceNodePortRange,
		RootWebService:           new(restful.WebService),
		enableLogsSupport:        c.EnableLogsSupport,
		enableUISupport:          c.EnableUISupport,
		enableSwaggerSupport:     c.EnableSwaggerSupport,
		enableProfiling:          c.EnableProfiling,
		enableWatchCache:         c.EnableWatchCache,
		APIPrefix:                c.APIPrefix,
		APIGroupPrefix:           c.APIGroupPrefix,
		corsAllowedOriginList:    c.CorsAllowedOriginList,
		authenticator:            c.Authenticator,
		authorizer:               c.Authorizer,
		AdmissionControl:         c.AdmissionControl,
		ApiGroupVersionOverrides: c.APIGroupVersionOverrides,
		RequestContextMapper:     c.RequestContextMapper,

		cacheTimeout:      c.CacheTimeout,
		MinRequestTimeout: time.Duration(c.MinRequestTimeout) * time.Second,

		MasterCount:         c.MasterCount,
		externalHost:        c.ExternalHost,
		ClusterIP:           c.PublicAddress,
		PublicReadWritePort: c.ReadWritePort,
		ServiceReadWriteIP:  c.ServiceReadWriteIP,
		// TODO: ServiceReadWritePort should be passed in as an argument, it may not always be 443
		ServiceReadWritePort: 443,
		ExtraServicePorts:    c.ExtraServicePorts,
		ExtraEndpointPorts:   c.ExtraEndpointPorts,

		KubernetesServiceNodePort: c.KubernetesServiceNodePort,
	}

	var handlerContainer *restful.Container
	if c.RestfulContainer != nil {
		s.mux = c.RestfulContainer.ServeMux
		handlerContainer = c.RestfulContainer
	} else {
		mux := http.NewServeMux()
		s.mux = mux
		handlerContainer = NewHandlerContainer(mux)
	}
	s.HandlerContainer = handlerContainer
	// Use CurlyRouter to be able to use regular expressions in paths. Regular expressions are required in paths for example for proxy (where the path is proxy/{kind}/{name}/{*})
	s.HandlerContainer.Router(restful.CurlyRouter{})
	s.MuxHelper = &apiserver.MuxHelper{Mux: s.mux, RegisteredPaths: []string{}}

	s.init(c)

	return s
}

func (s *GenericAPIServer) NewRequestInfoResolver() *apiserver.RequestInfoResolver {
	return &apiserver.RequestInfoResolver{
		sets.NewString(strings.Trim(s.APIPrefix, "/"), strings.Trim(s.APIGroupPrefix, "/")), // all possible API prefixes
		sets.NewString(strings.Trim(s.APIPrefix, "/")),                                      // APIPrefixes that won't have groups (legacy)
	}
}

// HandleWithAuth adds an http.Handler for pattern to an http.ServeMux
// Applies the same authentication and authorization (if any is configured)
// to the request is used for the GenericAPIServer's built-in endpoints.
func (s *GenericAPIServer) HandleWithAuth(pattern string, handler http.Handler) {
	// TODO: Add a way for plugged-in endpoints to translate their
	// URLs into attributes that an Authorizer can understand, and have
	// sensible policy defaults for plugged-in endpoints.  This will be different
	// for generic endpoints versus REST object endpoints.
	// TODO: convert to go-restful
	s.MuxHelper.Handle(pattern, handler)
}

// HandleFuncWithAuth adds an http.Handler for pattern to an http.ServeMux
// Applies the same authentication and authorization (if any is configured)
// to the request is used for the GenericAPIServer's built-in endpoints.
func (s *GenericAPIServer) HandleFuncWithAuth(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	// TODO: convert to go-restful
	s.MuxHelper.HandleFunc(pattern, handler)
}

func NewHandlerContainer(mux *http.ServeMux) *restful.Container {
	container := restful.NewContainer()
	container.ServeMux = mux
	apiserver.InstallRecoverHandler(container)
	return container
}

// init initializes GenericAPIServer.
func (s *GenericAPIServer) init(c *Config) {

	if c.ProxyDialer != nil || c.ProxyTLSClientConfig != nil {
		s.ProxyTransport = util.SetTransportDefaults(&http.Transport{
			Dial:            c.ProxyDialer,
			TLSClientConfig: c.ProxyTLSClientConfig,
		})
	}

	// Register root handler.
	// We do not register this using restful Webservice since we do not want to surface this in api docs.
	// Allow GenericAPIServer to be embedded in contexts which already have something registered at the root
	if c.EnableIndex {
		s.mux.HandleFunc("/", apiserver.IndexHandler(s.HandlerContainer, s.MuxHelper))
	}

	if c.EnableLogsSupport {
		apiserver.InstallLogsSupport(s.MuxHelper)
	}
	if c.EnableUISupport {
		ui.InstallSupport(s.MuxHelper, s.enableSwaggerSupport)
	}

	if c.EnableProfiling {
		s.mux.HandleFunc("/debug/pprof/", pprof.Index)
		s.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		s.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	}

	handler := http.Handler(s.mux.(*http.ServeMux))

	// TODO: handle CORS and auth using go-restful
	// See github.com/emicklei/go-restful/blob/GenericAPIServer/examples/restful-CORS-filter.go, and
	// github.com/emicklei/go-restful/blob/GenericAPIServer/examples/restful-basic-authentication.go

	if len(c.CorsAllowedOriginList) > 0 {
		allowedOriginRegexps, err := util.CompileRegexps(c.CorsAllowedOriginList)
		if err != nil {
			glog.Fatalf("Invalid CORS allowed origin, --cors-allowed-origins flag was set to %v - %v", strings.Join(c.CorsAllowedOriginList, ","), err)
		}
		handler = apiserver.CORS(handler, allowedOriginRegexps, nil, nil, "true")
	}

	s.InsecureHandler = handler

	attributeGetter := apiserver.NewRequestAttributeGetter(s.RequestContextMapper, s.NewRequestInfoResolver())
	handler = apiserver.WithAuthorizationCheck(handler, attributeGetter, s.authorizer)

	// Install Authenticator
	if c.Authenticator != nil {
		authenticatedHandler, err := handlers.NewRequestAuthenticator(s.RequestContextMapper, c.Authenticator, handlers.Unauthorized(c.SupportsBasicAuth), handler)
		if err != nil {
			glog.Fatalf("Could not initialize authenticator: %v", err)
		}
		handler = authenticatedHandler
	}

	// TODO: Make this optional?  Consumers of GenericAPIServer depend on this currently.
	s.Handler = handler

	// After all wrapping is done, put a context filter around both handlers
	if handler, err := api.NewRequestContextFilter(s.RequestContextMapper, s.Handler); err != nil {
		glog.Fatalf("Could not initialize request context filter: %v", err)
	} else {
		s.Handler = handler
	}

	if handler, err := api.NewRequestContextFilter(s.RequestContextMapper, s.InsecureHandler); err != nil {
		glog.Fatalf("Could not initialize request context filter: %v", err)
	} else {
		s.InsecureHandler = handler
	}
}

// Exposes the given group versions in API.
func (s *GenericAPIServer) InstallAPIGroups(groupsInfo []APIGroupInfo) error {
	for _, apiGroupInfo := range groupsInfo {
		if err := s.installAPIGroup(&apiGroupInfo); err != nil {
			return err
		}
	}
	return nil
}

func (s *GenericAPIServer) Run(options *ServerRunOptions) {
	// We serve on 2 ports.  See docs/accessing_the_api.md
	secureLocation := ""
	if options.SecurePort != 0 {
		secureLocation = net.JoinHostPort(options.BindAddress.String(), strconv.Itoa(options.SecurePort))
	}
	insecureLocation := net.JoinHostPort(options.InsecureBindAddress.String(), strconv.Itoa(options.InsecurePort))

	var sem chan bool
	if options.MaxRequestsInFlight > 0 {
		sem = make(chan bool, options.MaxRequestsInFlight)
	}

	longRunningRE := regexp.MustCompile(options.LongRunningRequestRE)
	longRunningTimeout := func(req *http.Request) (<-chan time.Time, string) {
		// TODO unify this with apiserver.MaxInFlightLimit
		if longRunningRE.MatchString(req.URL.Path) || req.URL.Query().Get("watch") == "true" {
			return nil, ""
		}
		return time.After(time.Minute), ""
	}

	if secureLocation != "" {
		handler := apiserver.TimeoutHandler(s.Handler, longRunningTimeout)
		secureServer := &http.Server{
			Addr:           secureLocation,
			Handler:        apiserver.MaxInFlightLimit(sem, longRunningRE, apiserver.RecoverPanics(handler)),
			MaxHeaderBytes: 1 << 20,
			TLSConfig: &tls.Config{
				// Change default from SSLv3 to TLSv1.0 (because of POODLE vulnerability)
				MinVersion: tls.VersionTLS10,
			},
		}

		if len(options.ClientCAFile) > 0 {
			clientCAs, err := util.CertPoolFromFile(options.ClientCAFile)
			if err != nil {
				glog.Fatalf("Unable to load client CA file: %v", err)
			}
			// Populate PeerCertificates in requests, but don't reject connections without certificates
			// This allows certificates to be validated by authenticators, while still allowing other auth types
			secureServer.TLSConfig.ClientAuth = tls.RequestClientCert
			// Specify allowed CAs for client certificates
			secureServer.TLSConfig.ClientCAs = clientCAs
		}

		glog.Infof("Serving securely on %s", secureLocation)
		if options.TLSCertFile == "" && options.TLSPrivateKeyFile == "" {
			options.TLSCertFile = path.Join(options.CertDirectory, "apiserver.crt")
			options.TLSPrivateKeyFile = path.Join(options.CertDirectory, "apiserver.key")
			// TODO (cjcullen): Is ClusterIP the right address to sign a cert with?
			alternateIPs := []net.IP{s.ServiceReadWriteIP}
			alternateDNS := []string{"kubernetes.default.svc", "kubernetes.default", "kubernetes"}
			// It would be nice to set a fqdn subject alt name, but only the kubelets know, the apiserver is clueless
			// alternateDNS = append(alternateDNS, "kubernetes.default.svc.CLUSTER.DNS.NAME")
			if err := util.GenerateSelfSignedCert(s.ClusterIP.String(), options.TLSCertFile, options.TLSPrivateKeyFile, alternateIPs, alternateDNS); err != nil {
				glog.Errorf("Unable to generate self signed cert: %v", err)
			} else {
				glog.Infof("Using self-signed cert (%options, %options)", options.TLSCertFile, options.TLSPrivateKeyFile)
			}
		}

		go func() {
			defer util.HandleCrash()
			for {
				// err == systemd.SdNotifyNoSocket when not running on a systemd system
				if err := systemd.SdNotify("READY=1\n"); err != nil && err != systemd.SdNotifyNoSocket {
					glog.Errorf("Unable to send systemd daemon successful start message: %v\n", err)
				}
				if err := secureServer.ListenAndServeTLS(options.TLSCertFile, options.TLSPrivateKeyFile); err != nil {
					glog.Errorf("Unable to listen for secure (%v); will try again.", err)
				}
				time.Sleep(15 * time.Second)
			}
		}()
	} else {
		// err == systemd.SdNotifyNoSocket when not running on a systemd system
		if err := systemd.SdNotify("READY=1\n"); err != nil && err != systemd.SdNotifyNoSocket {
			glog.Errorf("Unable to send systemd daemon successful start message: %v\n", err)
		}
	}

	handler := apiserver.TimeoutHandler(s.InsecureHandler, longRunningTimeout)
	http := &http.Server{
		Addr:           insecureLocation,
		Handler:        apiserver.RecoverPanics(handler),
		MaxHeaderBytes: 1 << 20,
	}
	glog.Infof("Serving insecurely on %s", insecureLocation)
	glog.Fatal(http.ListenAndServe())
}

func (s *GenericAPIServer) installAPIGroup(apiGroupInfo *APIGroupInfo) error {
	apiPrefix := s.APIGroupPrefix
	if apiGroupInfo.IsLegacyGroup {
		apiPrefix = s.APIPrefix
	}

	// Install REST handlers for all the versions in this group.
	apiVersions := []string{}
	for _, groupVersion := range apiGroupInfo.GroupMeta.GroupVersions {
		apiVersions = append(apiVersions, groupVersion.Version)

		apiGroupVersion, err := s.getAPIGroupVersion(apiGroupInfo, groupVersion, apiPrefix)
		if err != nil {
			return err
		}
		if apiGroupInfo.OptionsExternalVersion != nil {
			apiGroupVersion.OptionsExternalVersion = apiGroupInfo.OptionsExternalVersion
		}

		if err := apiGroupVersion.InstallREST(s.HandlerContainer); err != nil {
			return fmt.Errorf("Unable to setup API %v: %v", apiGroupInfo, err)
		}
	}
	// Install the version handler.
	if apiGroupInfo.IsLegacyGroup {
		// Add a handler at /api to enumerate the supported api versions.
		apiserver.AddApiWebService(s.HandlerContainer, apiPrefix, apiVersions)
	} else {
		// Add a handler at /apis/<groupName> to enumerate all versions supported by this group.
		apiVersionsForDiscovery := []unversioned.GroupVersionForDiscovery{}
		for _, groupVersion := range apiGroupInfo.GroupMeta.GroupVersions {
			apiVersionsForDiscovery = append(apiVersionsForDiscovery, unversioned.GroupVersionForDiscovery{
				GroupVersion: groupVersion.String(),
				Version:      groupVersion.Version,
			})
		}
		preferedVersionForDiscovery := unversioned.GroupVersionForDiscovery{
			GroupVersion: apiGroupInfo.GroupMeta.GroupVersion.String(),
			Version:      apiGroupInfo.GroupMeta.GroupVersion.Version,
		}
		apiGroup := unversioned.APIGroup{
			Name:             apiGroupInfo.GroupMeta.GroupVersion.Group,
			Versions:         apiVersionsForDiscovery,
			PreferredVersion: preferedVersionForDiscovery,
		}
		apiserver.AddGroupWebService(s.HandlerContainer, apiPrefix+"/"+apiGroup.Name, apiGroup)
	}
	apiserver.InstallServiceErrorHandler(s.HandlerContainer, s.NewRequestInfoResolver(), apiVersions)
	return nil
}

func (s *GenericAPIServer) getAPIGroupVersion(apiGroupInfo *APIGroupInfo, groupVersion unversioned.GroupVersion, apiPrefix string) (*apiserver.APIGroupVersion, error) {
	storage := make(map[string]rest.Storage)
	for k, v := range apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version] {
		storage[strings.ToLower(k)] = v
	}
	version, err := s.newAPIGroupVersion(apiGroupInfo.GroupMeta, groupVersion)
	version.Root = apiPrefix
	version.Storage = storage
	return version, err
}

func (s *GenericAPIServer) newAPIGroupVersion(groupMeta latest.GroupMeta, groupVersion unversioned.GroupVersion) (*apiserver.APIGroupVersion, error) {
	versionInterface, err := groupMeta.InterfacesFor(groupVersion)
	if err != nil {
		return nil, err
	}
	return &apiserver.APIGroupVersion{
		RequestInfoResolver: s.NewRequestInfoResolver(),

		Creater:   api.Scheme,
		Convertor: api.Scheme,
		Typer:     api.Scheme,

		GroupVersion: groupVersion,
		Linker:       groupMeta.SelfLinker,
		Mapper:       groupMeta.RESTMapper,
		Codec:        versionInterface.Codec,

		Admit:   s.AdmissionControl,
		Context: s.RequestContextMapper,

		MinRequestTimeout: s.MinRequestTimeout,
	}, nil
}

// InstallSwaggerAPI installs the /swaggerapi/ endpoint to allow schema discovery
// and traversal.  It is optional to allow consumers of the Kubernetes GenericAPIServer to
// register their own web services into the Kubernetes mux prior to initialization
// of swagger, so that other resource types show up in the documentation.
func (s *GenericAPIServer) InstallSwaggerAPI() {
	hostAndPort := s.externalHost
	protocol := "https://"

	// TODO: this is kind of messed up, we should just pipe in the full URL from the outside, rather
	// than guessing at it.
	if len(s.externalHost) == 0 && s.ClusterIP != nil {
		host := s.ClusterIP.String()
		if s.PublicReadWritePort != 0 {
			hostAndPort = net.JoinHostPort(host, strconv.Itoa(s.PublicReadWritePort))
		}
	}
	webServicesUrl := protocol + hostAndPort

	// Enable swagger UI and discovery API
	swaggerConfig := swagger.Config{
		WebServicesUrl:  webServicesUrl,
		WebServices:     s.HandlerContainer.RegisteredWebServices(),
		ApiPath:         "/swaggerapi/",
		SwaggerPath:     "/swaggerui/",
		SwaggerFilePath: "/swagger-ui/",
	}
	swagger.RegisterSwaggerService(swaggerConfig, s.HandlerContainer)
}
