package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deis/router/utils"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
)

// RouterConfig is the primary type used to encapsulate all router configuration.
type RouterConfig struct {
	WorkerProcesses          string      `json:"workerProcesses"`
	MaxWorkerConnections     int         `json:"maxWorkerConnections"`
	DefaultTimeout           int         `json:"defaultTimeout"`
	ServerNameHashMaxSize    int         `json:"serverNameHashMaxSize"`
	ServerNameHashBucketSize int         `json:"serverNameHashBucketSize"`
	GzipConfig               *GzipConfig `json:"gzipConfig"`
	BodySize                 int         `json:"bodySize"`
	ProxyRealIPCIDR          string      `json:"proxyRealIpCidr"`
	ErrorLogLevel            string      `json:"errorLogLevel"`
	Domain                   string      `json:"domain"`
	UseProxyProtocol         bool        `json:"useProxyProtocol"`
	EnforceWhitelists        bool        `json:"enforceWhitelists"`
	AppConfigs               []*AppConfig
	BuilderConfig            *BuilderConfig
}

func newRouterConfig() *RouterConfig {
	return &RouterConfig{
		WorkerProcesses:          "auto",
		MaxWorkerConnections:     768,
		DefaultTimeout:           1300,
		ServerNameHashMaxSize:    512,
		ServerNameHashBucketSize: 64,
		GzipConfig:               newGzipConfig(),
		BodySize:                 1,
		ProxyRealIPCIDR:          "10.0.0.0/8",
		ErrorLogLevel:            "error",
		UseProxyProtocol:         false,
		EnforceWhitelists:        false,
	}
}

// GzipConfig encapsulates gzip configuration.
type GzipConfig struct {
	CompLevel   int    `json:"compLevel"`
	Disable     string `json:"disable"`
	HTTPVersion string `json:"httpVersion"`
	MinLength   int    `json:"minLength"`
	Proxied     string `json:"proxied"`
	Types       string `json:"types"`
	Vary        string `json:"vary"`
}

func newGzipConfig() *GzipConfig {
	return &GzipConfig{
		CompLevel:   5,
		Disable:     "msie6",
		HTTPVersion: "1.1",
		MinLength:   256,
		Proxied:     "any",
		Types:       "application/atom+xml application/javascript application/json application/rss+xml application/vnd.ms-fontobject application/x-font-ttf application/x-web-app-manifest+json application/xhtml+xml application/xml font/opentype image/svg+xml image/x-icon text/css text/plain text/x-component",
		Vary:        "on",
	}
}

// AppConfig encapsulates the configuration for all routes to a single back end.
type AppConfig struct {
	Domains        []string `json:"domains"`
	Whitelist      []string `json:"whitelist"`
	ConnectTimeout int      `json:"connectTimeout"`
	TCPTimeout     int      `json:"tcpTimeout"`
	ServiceIP      string
}

func newAppConfig(routerConfig *RouterConfig) *AppConfig {
	return &AppConfig{
		ConnectTimeout: 30,
		TCPTimeout:     routerConfig.DefaultTimeout,
	}
}

// BuilderConfig encapsulates the configuration of the deis-builder-- if it's in use.
type BuilderConfig struct {
	ConnectTimeout int `json:"connectTimeout"`
	TCPTimeout     int `json:"tcpTimeout"`
	ServiceIP      string
}

func newBuilderConfig() *BuilderConfig {
	return &BuilderConfig{
		ConnectTimeout: 10,
		TCPTimeout:     1200,
	}
}

var namespace string

func init() {
	namespace = utils.GetOpt("POD_NAMESPACE", "default")
}

// Build creates a RouterConfig configuration object by querying the k8s API for
// relevant metadata concerning itself and all routable services.
func Build(kubeClient *client.Client) (*RouterConfig, error) {
	// Get all relevant information from k8s:
	//   deis-router rc
	//   All services with label "routable=true"
	//   deis-builder service, if it exists
	// These are used to construct a model...
	routerRC, err := getRC(kubeClient)
	if err != nil {
		return nil, err
	}
	appServices, err := getAppServices(kubeClient)
	if err != nil {
		return nil, err
	}
	// builderService might be nil if it's not found and that's ok.
	builderService, err := getBuilderService(kubeClient)
	if err != nil {
		return nil, err
	}
	// Build the model...
	routerConfig, err := build(kubeClient, routerRC, appServices, builderService)
	if err != nil {
		return nil, err
	}
	return routerConfig, nil
}

func getRC(kubeClient *client.Client) (*api.ReplicationController, error) {
	rcClient := kubeClient.ReplicationControllers(namespace)
	rc, err := rcClient.Get("deis-router")
	if err != nil {
		return nil, err
	}
	return rc, nil
}

func getAppServices(kubeClient *client.Client) (*api.ServiceList, error) {
	serviceClient := kubeClient.Services(api.NamespaceAll)
	servicesSelector, err := labels.Parse("routable==true")
	if err != nil {
		return nil, err
	}
	services, err := serviceClient.List(servicesSelector)
	if err != nil {
		return nil, err
	}
	return services, nil
}

// getBuilderService will return the service named "deis-builder" from the same namespace as
// the router, but will return nil (without error) if no such service exists.
func getBuilderService(kubeClient *client.Client) (*api.Service, error) {
	serviceClient := kubeClient.Services(namespace)
	service, err := serviceClient.Get("deis-builder")
	if err != nil {
		statusErr, ok := err.(*errors.StatusError)
		// If the issue is just that no deis-builder was found, that's ok.
		if ok && statusErr.Status().Code == 404 {
			// We'll just return nil instead of a found *api.Service.
			return nil, nil
		}
		return nil, err
	}
	return service, nil
}

func build(kubeClient *client.Client, routerRC *api.ReplicationController, appServices *api.ServiceList, builderService *api.Service) (*RouterConfig, error) {
	routerConfig, err := buildRouterConfig(routerRC)
	if err != nil {
		return nil, err
	}
	for _, appService := range appServices.Items {
		appConfig, err := buildAppConfig(kubeClient, appService, routerConfig)
		if err != nil {
			return nil, err
		}
		if appConfig != nil {
			routerConfig.AppConfigs = append(routerConfig.AppConfigs, appConfig)
		}
	}
	if builderService != nil {
		builderConfig, err := buildBuilderConfig(builderService)
		if err != nil {
			return nil, err
		}
		if builderConfig != nil {
			routerConfig.BuilderConfig = builderConfig
		}
	}
	return routerConfig, nil
}

func buildRouterConfig(rc *api.ReplicationController) (*RouterConfig, error) {
	routerConfig := newRouterConfig()
	annotations, ok := rc.Annotations["deis.io/routerConfig"]
	// If no annotations are found, we can still return some default router configuration.
	if !ok {
		return routerConfig, nil
	}
	err := json.Unmarshal([]byte(annotations), routerConfig)
	if err != nil {
		return nil, err
	}
	return routerConfig, nil
}

func buildAppConfig(kubeClient *client.Client, service api.Service, routerConfig *RouterConfig) (*AppConfig, error) {
	annotations, ok := service.Annotations["deis.io/routerConfig"]
	// If no annotations are found, we don't have the information we need to build routes
	// to this application.  Abort.
	if !ok {
		return nil, nil
	}
	appConfig := newAppConfig(routerConfig)
	err := json.Unmarshal([]byte(annotations), appConfig)
	if err != nil {
		return nil, err
	}
	if routerConfig.Domain != "" {
		for i, domain := range appConfig.Domains {
			if !strings.Contains(domain, ".") {
				appConfig.Domains[i] = fmt.Sprintf("%s.%s", domain, routerConfig.Domain)
			}
		}
	}
	appConfig.ServiceIP = service.Spec.ClusterIP
	return appConfig, nil
}

func buildBuilderConfig(service *api.Service) (*BuilderConfig, error) {
	builderConfig := newBuilderConfig()
	builderConfig.ServiceIP = service.Spec.ClusterIP
	annotations, ok := service.Annotations["deis.io/routerConfig"]
	// If no annotations are found, we can still return some default builder configuration.
	if !ok {
		return builderConfig, nil
	}
	err := json.Unmarshal([]byte(annotations), builderConfig)
	if err != nil {
		return nil, err
	}
	return builderConfig, nil
}
