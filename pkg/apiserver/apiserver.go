package apiserver

import (
	"bytes"
	"context"
	"fmt"
	"github.com/emicklei/go-restful"
	"k8s.io/apimachinery/pkg/runtime/schema"
	urlruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	unionauth "k8s.io/apiserver/pkg/authentication/request/union"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/klog"
	"kubesphere.io/kubesphere/pkg/apiserver/authentication/authenticators/basic"
	"kubesphere.io/kubesphere/pkg/apiserver/authentication/authenticators/jwttoken"
	"kubesphere.io/kubesphere/pkg/apiserver/authentication/request/anonymous"
	"kubesphere.io/kubesphere/pkg/apiserver/authentication/request/basictoken"
	"kubesphere.io/kubesphere/pkg/apiserver/authentication/request/bearertoken"
	"kubesphere.io/kubesphere/pkg/apiserver/authentication/token"
	"kubesphere.io/kubesphere/pkg/apiserver/authorization/authorizer"
	"kubesphere.io/kubesphere/pkg/apiserver/authorization/authorizerfactory"
	authorizationoptions "kubesphere.io/kubesphere/pkg/apiserver/authorization/options"
	"kubesphere.io/kubesphere/pkg/apiserver/authorization/path"
	unionauthorizer "kubesphere.io/kubesphere/pkg/apiserver/authorization/union"
	apiserverconfig "kubesphere.io/kubesphere/pkg/apiserver/config"
	"kubesphere.io/kubesphere/pkg/apiserver/dispatch"
	"kubesphere.io/kubesphere/pkg/apiserver/filters"
	"kubesphere.io/kubesphere/pkg/apiserver/request"
	"kubesphere.io/kubesphere/pkg/informers"
	alertingv1 "kubesphere.io/kubesphere/pkg/kapis/alerting/v1"
	clusterkapisv1alpha1 "kubesphere.io/kubesphere/pkg/kapis/cluster/v1alpha1"
	configv1alpha2 "kubesphere.io/kubesphere/pkg/kapis/config/v1alpha2"
	devopsv1alpha2 "kubesphere.io/kubesphere/pkg/kapis/devops/v1alpha2"
	devopsv1alpha3 "kubesphere.io/kubesphere/pkg/kapis/devops/v1alpha3"
	iamapi "kubesphere.io/kubesphere/pkg/kapis/iam/v1alpha2"
	loggingv1alpha2 "kubesphere.io/kubesphere/pkg/kapis/logging/v1alpha2"
	monitoringv1alpha3 "kubesphere.io/kubesphere/pkg/kapis/monitoring/v1alpha3"
	networkv1alpha2 "kubesphere.io/kubesphere/pkg/kapis/network/v1alpha2"
	notificationv1 "kubesphere.io/kubesphere/pkg/kapis/notification/v1"
	"kubesphere.io/kubesphere/pkg/kapis/oauth"
	openpitrixv1 "kubesphere.io/kubesphere/pkg/kapis/openpitrix/v1"
	operationsv1alpha2 "kubesphere.io/kubesphere/pkg/kapis/operations/v1alpha2"
	resourcesv1alpha2 "kubesphere.io/kubesphere/pkg/kapis/resources/v1alpha2"
	resourcev1alpha3 "kubesphere.io/kubesphere/pkg/kapis/resources/v1alpha3"
	servicemeshv1alpha2 "kubesphere.io/kubesphere/pkg/kapis/servicemesh/metrics/v1alpha2"
	tenantv1alpha2 "kubesphere.io/kubesphere/pkg/kapis/tenant/v1alpha2"
	terminalv1alpha2 "kubesphere.io/kubesphere/pkg/kapis/terminal/v1alpha2"
	"kubesphere.io/kubesphere/pkg/models/iam/am"
	"kubesphere.io/kubesphere/pkg/models/iam/im"
	"kubesphere.io/kubesphere/pkg/simple/client/cache"
	"kubesphere.io/kubesphere/pkg/simple/client/devops"
	"kubesphere.io/kubesphere/pkg/simple/client/events"
	"kubesphere.io/kubesphere/pkg/simple/client/k8s"
	"kubesphere.io/kubesphere/pkg/simple/client/ldap"
	"kubesphere.io/kubesphere/pkg/simple/client/logging"
	"kubesphere.io/kubesphere/pkg/simple/client/monitoring"
	"kubesphere.io/kubesphere/pkg/simple/client/openpitrix"
	"kubesphere.io/kubesphere/pkg/simple/client/s3"
	"kubesphere.io/kubesphere/pkg/simple/client/sonarqube"
	"net"
	"net/http"
	rt "runtime"
	"strings"
	"time"
)

const (
	// ApiRootPath defines the root path of all KubeSphere apis.
	ApiRootPath = "/kapis"

	// MimeMergePatchJson is the mime header used in merge request
	MimeMergePatchJson = "application/merge-patch+json"

	//
	MimeJsonPatchJson = "application/json-patch+json"
)

type APIServer struct {

	// number of kubesphere apiserver
	ServerCount int

	//
	Server *http.Server

	Config *apiserverconfig.Config

	// webservice container, where all webservice defines
	container *restful.Container

	// kubeClient is a collection of all kubernetes(include CRDs) objects clientset
	KubernetesClient k8s.Client

	// informerFactory is a collection of all kubernetes(include CRDs) objects informers,
	// mainly for fast query
	InformerFactory informers.InformerFactory

	// cache is used for short lived objects, like session
	CacheClient cache.Interface

	// monitoring client set
	MonitoringClient monitoring.Interface

	//
	OpenpitrixClient openpitrix.Client

	//
	LoggingClient logging.Interface

	//
	DevopsClient devops.Interface

	//
	S3Client s3.Interface

	//
	LdapClient ldap.Interface

	SonarClient sonarqube.SonarInterface

	EventsClient events.Client
}

func (s *APIServer) PrepareRun() error {

	s.container = restful.NewContainer()
	s.container.Filter(logRequestAndResponse)
	s.container.Router(restful.CurlyRouter{})
	s.container.RecoverHandler(func(panicReason interface{}, httpWriter http.ResponseWriter) {
		logStackOnRecover(panicReason, httpWriter)
	})

	s.installKubeSphereAPIs()

	for _, ws := range s.container.RegisteredWebServices() {
		klog.V(2).Infof("%s", ws.RootPath())
	}

	s.Server.Handler = s.container

	s.buildHandlerChain()

	return nil
}

// Install all kubesphere api groups
// Installation happens before all informers start to cache objects, so
//   any attempt to list objects using listers will get empty results.
func (s *APIServer) installKubeSphereAPIs() {
	urlruntime.Must(configv1alpha2.AddToContainer(s.container, s.Config))
	urlruntime.Must(resourcev1alpha3.AddToContainer(s.container, s.InformerFactory))
	urlruntime.Must(loggingv1alpha2.AddToContainer(s.container, s.KubernetesClient, s.LoggingClient))
	urlruntime.Must(monitoringv1alpha3.AddToContainer(s.container, s.KubernetesClient.Kubernetes(), s.MonitoringClient))
	urlruntime.Must(openpitrixv1.AddToContainer(s.container, s.InformerFactory, s.OpenpitrixClient))
	urlruntime.Must(networkv1alpha2.AddToContainer(s.container, s.Config.NetworkOptions.WeaveScopeHost))
	urlruntime.Must(operationsv1alpha2.AddToContainer(s.container, s.KubernetesClient.Kubernetes()))
	urlruntime.Must(resourcesv1alpha2.AddToContainer(s.container, s.KubernetesClient.Kubernetes(), s.InformerFactory,
		s.KubernetesClient.Master()))
	urlruntime.Must(tenantv1alpha2.AddToContainer(s.container, s.InformerFactory, s.KubernetesClient.Kubernetes(),
		s.KubernetesClient.KubeSphere(), s.EventsClient))
	urlruntime.Must(terminalv1alpha2.AddToContainer(s.container, s.KubernetesClient.Kubernetes(), s.KubernetesClient.Config()))
	urlruntime.Must(clusterkapisv1alpha1.AddToContainer(s.container,
		s.InformerFactory.KubernetesSharedInformerFactory(),
		s.InformerFactory.KubeSphereSharedInformerFactory(),
		s.Config.MultiClusterOptions.ProxyPublishService,
		s.Config.MultiClusterOptions.ProxyPublishAddress,
		s.Config.MultiClusterOptions.AgentImage))
	urlruntime.Must(iamapi.AddToContainer(s.container,
		im.NewOperator(s.KubernetesClient.KubeSphere(), s.InformerFactory),
		am.NewOperator(s.InformerFactory, s.KubernetesClient.KubeSphere(), s.KubernetesClient.Kubernetes()),
		s.Config.AuthenticationOptions))
	urlruntime.Must(oauth.AddToContainer(s.container,
		im.NewOperator(s.KubernetesClient.KubeSphere(), s.InformerFactory),
		token.NewJwtTokenIssuer(token.DefaultIssuerName, s.Config.AuthenticationOptions, s.CacheClient),
		s.Config.AuthenticationOptions))
	urlruntime.Must(servicemeshv1alpha2.AddToContainer(s.container))
	urlruntime.Must(devopsv1alpha2.AddToContainer(s.container,
		s.InformerFactory.KubeSphereSharedInformerFactory(),
		s.DevopsClient,
		s.SonarClient,
		s.KubernetesClient.KubeSphere(),
		s.S3Client))
	urlruntime.Must(devopsv1alpha3.AddToContainer(s.container,
		s.DevopsClient,
		s.KubernetesClient.Kubernetes(),
		s.KubernetesClient.KubeSphere(),
		s.InformerFactory.KubeSphereSharedInformerFactory(),
		s.InformerFactory.KubernetesSharedInformerFactory()))
	urlruntime.Must(notificationv1.AddToContainer(s.container, s.Config.NotificationOptions.Endpoint))
	urlruntime.Must(alertingv1.AddToContainer(s.container, s.Config.AlertingOptions.Endpoint))
}

func (s *APIServer) Run(stopCh <-chan struct{}) (err error) {

	err = s.waitForResourceSync(stopCh)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-stopCh
		_ = s.Server.Shutdown(ctx)
	}()

	klog.V(0).Infof("Start listening on %s", s.Server.Addr)
	if s.Server.TLSConfig != nil {
		err = s.Server.ListenAndServeTLS("", "")
	} else {
		err = s.Server.ListenAndServe()
	}

	return err
}

func (s *APIServer) buildHandlerChain() {
	requestInfoResolver := &request.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis", "kapis", "kapi"),
		GrouplessAPIPrefixes: sets.NewString("api", "kapi"),
	}

	handler := s.Server.Handler
	handler = filters.WithKubeAPIServer(handler, s.KubernetesClient.Config(), &errorResponder{})

	if s.Config.MultiClusterOptions.Enable {
		clusterDispatcher := dispatch.NewClusterDispatch(s.InformerFactory.KubeSphereSharedInformerFactory().Cluster().V1alpha1().Clusters().Lister())
		handler = filters.WithMultipleClusterDispatcher(handler, clusterDispatcher)
	}

	var authorizers authorizer.Authorizer

	switch s.Config.AuthorizationOptions.Mode {
	case authorizationoptions.AlwaysAllow:
		authorizers = authorizerfactory.NewAlwaysAllowAuthorizer()
	case authorizationoptions.AlwaysDeny:
		authorizers = authorizerfactory.NewAlwaysDenyAuthorizer()
	default:
		fallthrough
	case authorizationoptions.RBAC:
		excludedPaths := []string{"/oauth/*", "/kapis/config.kubesphere.io/*"}
		pathAuthorizer, _ := path.NewAuthorizer(excludedPaths)
		amOperator := am.NewReadOnlyOperator(s.InformerFactory)
		authorizers = unionauthorizer.New(pathAuthorizer, authorizerfactory.NewRBACAuthorizer(amOperator))
	}

	handler = filters.WithAuthorization(handler, authorizers)

	// authenticators are unordered
	authn := unionauth.New(anonymous.NewAuthenticator(),
		basictoken.New(basic.NewBasicAuthenticator(im.NewOperator(s.KubernetesClient.KubeSphere(), s.InformerFactory))),
		bearertoken.New(jwttoken.NewTokenAuthenticator(token.NewJwtTokenIssuer(token.DefaultIssuerName, s.Config.AuthenticationOptions, s.CacheClient))))
	handler = filters.WithAuthentication(handler, authn)
	handler = filters.WithRequestInfo(handler, requestInfoResolver)
	s.Server.Handler = handler
}

func (s *APIServer) waitForResourceSync(stopCh <-chan struct{}) error {
	klog.V(0).Info("Start cache objects")

	discoveryClient := s.KubernetesClient.Kubernetes().Discovery()
	_, apiResourcesList, err := discoveryClient.ServerGroupsAndResources()
	if err != nil {
		return err
	}

	isResourceExists := func(resource schema.GroupVersionResource) bool {
		for _, apiResource := range apiResourcesList {
			if apiResource.GroupVersion == resource.GroupVersion().String() {
				for _, rsc := range apiResource.APIResources {
					if rsc.Name == resource.Resource {
						return true
					}
				}
			}
		}
		return false
	}

	// resources we have to create informer first
	k8sGVRs := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "namespaces"},
		{Group: "", Version: "v1", Resource: "nodes"},
		{Group: "", Version: "v1", Resource: "resourcequotas"},
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "", Version: "v1", Resource: "services"},
		{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "", Version: "v1", Resource: "configmaps"},

		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},

		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "apps", Version: "v1", Resource: "daemonsets"},
		{Group: "apps", Version: "v1", Resource: "replicasets"},
		{Group: "apps", Version: "v1", Resource: "statefulsets"},
		{Group: "apps", Version: "v1", Resource: "controllerrevisions"},

		{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"},

		{Group: "batch", Version: "v1", Resource: "jobs"},
		{Group: "batch", Version: "v1beta1", Resource: "cronjobs"},

		{Group: "extensions", Version: "v1beta1", Resource: "ingresses"},

		{Group: "autoscaling", Version: "v2beta2", Resource: "horizontalpodautoscalers"},

		{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
	}

	for _, gvr := range k8sGVRs {
		if !isResourceExists(gvr) {
			klog.Warningf("resource %s not exists in the cluster", gvr)
		} else {
			_, err := s.InformerFactory.KubernetesSharedInformerFactory().ForResource(gvr)
			if err != nil {
				klog.Errorf("cannot create informer for %s", gvr)
				return err
			}
		}
	}

	s.InformerFactory.KubernetesSharedInformerFactory().Start(stopCh)
	s.InformerFactory.KubernetesSharedInformerFactory().WaitForCacheSync(stopCh)

	ksInformerFactory := s.InformerFactory.KubeSphereSharedInformerFactory()

	ksGVRs := []schema.GroupVersionResource{
		{Group: "tenant.kubesphere.io", Version: "v1alpha1", Resource: "workspaces"},
		{Group: "tenant.kubesphere.io", Version: "v1alpha2", Resource: "workspacetemplates"},
		{Group: "iam.kubesphere.io", Version: "v1alpha2", Resource: "users"},
		{Group: "iam.kubesphere.io", Version: "v1alpha2", Resource: "globalroles"},
		{Group: "iam.kubesphere.io", Version: "v1alpha2", Resource: "globalrolebindings"},
		{Group: "iam.kubesphere.io", Version: "v1alpha2", Resource: "workspaceroles"},
		{Group: "iam.kubesphere.io", Version: "v1alpha2", Resource: "workspacerolebindings"},
		{Group: "cluster.kubesphere.io", Version: "v1alpha1", Resource: "clusters"},
		{Group: "devops.kubesphere.io", Version: "v1alpha3", Resource: "devopsprojects"},
	}

	devopsGVRs := []schema.GroupVersionResource{
		{Group: "devops.kubesphere.io", Version: "v1alpha1", Resource: "s2ibinaries"},
		{Group: "devops.kubesphere.io", Version: "v1alpha1", Resource: "s2ibuildertemplates"},
		{Group: "devops.kubesphere.io", Version: "v1alpha1", Resource: "s2iruns"},
		{Group: "devops.kubesphere.io", Version: "v1alpha1", Resource: "s2ibuilders"},
	}

	servicemeshGVRs := []schema.GroupVersionResource{
		{Group: "servicemesh.kubesphere.io", Version: "v1alpha2", Resource: "strategies"},
		{Group: "servicemesh.kubesphere.io", Version: "v1alpha2", Resource: "servicepolicies"},
	}

	// skip caching devops resources if devops not enabled
	if s.DevopsClient != nil {
		ksGVRs = append(ksGVRs, devopsGVRs...)
	}

	// skip caching servicemesh resources if servicemesh not enabled
	if s.KubernetesClient.Istio() != nil {
		ksGVRs = append(ksGVRs, servicemeshGVRs...)
	}

	for _, gvr := range ksGVRs {
		if !isResourceExists(gvr) {
			klog.Warningf("resource %s not exists in the cluster", gvr)
		} else {
			_, err := ksInformerFactory.ForResource(gvr)
			if err != nil {
				return err
			}
		}
	}

	ksInformerFactory.Start(stopCh)
	ksInformerFactory.WaitForCacheSync(stopCh)

	appInformerFactory := s.InformerFactory.ApplicationSharedInformerFactory()

	appGVRs := []schema.GroupVersionResource{
		{Group: "app.k8s.io", Version: "v1beta1", Resource: "applications"},
	}

	for _, gvr := range appGVRs {
		if !isResourceExists(gvr) {
			klog.Warningf("resource %s not exists in the cluster", gvr)
		} else {
			_, err = appInformerFactory.ForResource(gvr)
			if err != nil {
				return err
			}
		}
	}

	appInformerFactory.Start(stopCh)
	appInformerFactory.WaitForCacheSync(stopCh)

	snapshotInformerFactory := s.InformerFactory.SnapshotSharedInformerFactory()
	snapshotGVRs := []schema.GroupVersionResource{
		{Group: "snapshot.storage.k8s.io", Version: "v1beta1", Resource: "volumesnapshotclasses"},
		{Group: "snapshot.storage.k8s.io", Version: "v1beta1", Resource: "volumesnapshots"},
		{Group: "snapshot.storage.k8s.io", Version: "v1beta1", Resource: "volumesnapshotcontents"},
	}
	for _, gvr := range snapshotGVRs {
		if !isResourceExists(gvr) {
			klog.Warningf("resource %s not exists in the cluster", gvr)
		} else {
			_, err = snapshotInformerFactory.ForResource(gvr)
			if err != nil {
				return err
			}
		}
	}
	snapshotInformerFactory.Start(stopCh)
	snapshotInformerFactory.WaitForCacheSync(stopCh)

	apiextensionsInformerFactory := s.InformerFactory.ApiExtensionSharedInformerFactory()
	apiextensionsGVRs := []schema.GroupVersionResource{
		{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
	}

	for _, gvr := range apiextensionsGVRs {
		if !isResourceExists(gvr) {
			klog.Warningf("resource %s not exists in the cluster", gvr)
		} else {
			_, err = apiextensionsInformerFactory.ForResource(gvr)
			if err != nil {
				return err
			}
		}
	}
	apiextensionsInformerFactory.Start(stopCh)
	apiextensionsInformerFactory.WaitForCacheSync(stopCh)

	klog.V(0).Info("Finished caching objects")

	return nil

}

func logStackOnRecover(panicReason interface{}, w http.ResponseWriter) {
	var buffer bytes.Buffer
	buffer.WriteString(fmt.Sprintf("recover from panic situation: - %v\r\n", panicReason))
	for i := 2; ; i += 1 {
		_, file, line, ok := rt.Caller(i)
		if !ok {
			break
		}
		buffer.WriteString(fmt.Sprintf("    %s:%d\r\n", file, line))
	}
	klog.Errorln(buffer.String())

	headers := http.Header{}
	if ct := w.Header().Get("Content-Type"); len(ct) > 0 {
		headers.Set("Accept", ct)
	}

	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte("Internal server error"))
}

func logRequestAndResponse(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	start := time.Now()
	chain.ProcessFilter(req, resp)
	klog.V(4).Infof("%s - \"%s %s %s\" %d %d %dms",
		getRequestIP(req),
		req.Request.Method,
		req.Request.RequestURI,
		req.Request.Proto,
		resp.StatusCode(),
		resp.ContentLength(),
		time.Since(start)/time.Millisecond,
	)
}

func getRequestIP(req *restful.Request) string {
	address := strings.Trim(req.Request.Header.Get("X-Real-Ip"), " ")
	if address != "" {
		return address
	}

	address = strings.Trim(req.Request.Header.Get("X-Forwarded-For"), " ")
	if address != "" {
		return address
	}

	address, _, err := net.SplitHostPort(req.Request.RemoteAddr)
	if err != nil {
		return req.Request.RemoteAddr
	}

	return address
}

type errorResponder struct{}

func (e *errorResponder) Error(w http.ResponseWriter, req *http.Request, err error) {
	klog.Error(err)
	responsewriters.InternalError(w, req, err)
}
