/*
Copyright (c) 2019 the Octant contributors. All Rights Reserved.
SPDX-License-Identifier: Apache-2.0
*/

package dash

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"contrib.go.opencensus.io/exporter/jaeger"
	"github.com/skratchdot/open-golang/open"
	"github.com/soheilhy/cmux"
	"github.com/spf13/afero"
	"github.com/spf13/viper"
	"go.opencensus.io/trace"

	"github.com/vmware-tanzu/octant/internal/api"
	"github.com/vmware-tanzu/octant/internal/cluster"
	"github.com/vmware-tanzu/octant/internal/config"
	ocontext "github.com/vmware-tanzu/octant/internal/context"
	"github.com/vmware-tanzu/octant/internal/describer"
	oerrors "github.com/vmware-tanzu/octant/internal/errors"
	internalLog "github.com/vmware-tanzu/octant/internal/log"
	"github.com/vmware-tanzu/octant/internal/module"
	"github.com/vmware-tanzu/octant/internal/modules/applications"
	"github.com/vmware-tanzu/octant/internal/modules/clusteroverview"
	"github.com/vmware-tanzu/octant/internal/modules/configuration"
	"github.com/vmware-tanzu/octant/internal/modules/localcontent"
	"github.com/vmware-tanzu/octant/internal/modules/overview"
	"github.com/vmware-tanzu/octant/internal/modules/workloads"
	"github.com/vmware-tanzu/octant/internal/objectstore"
	"github.com/vmware-tanzu/octant/internal/portforward"
	"github.com/vmware-tanzu/octant/pkg/action"
	"github.com/vmware-tanzu/octant/pkg/log"
	"github.com/vmware-tanzu/octant/pkg/octant"
	"github.com/vmware-tanzu/octant/pkg/plugin"
	pluginAPI "github.com/vmware-tanzu/octant/pkg/plugin/api"
	"github.com/vmware-tanzu/octant/pkg/store"
	"github.com/vmware-tanzu/octant/web"
)

type Options struct {
	EnableOpenCensus       bool
	DisableClusterOverview bool
	KubeConfig             string
	Namespace              string
	Namespaces             []string
	FrontendURL            string
	BrowserPath            string
	Context                string
	ClientQPS              float32
	ClientBurst            int
	UserAgent              string
	BuildInfo              config.BuildInfo
}

type Runner struct {
	dash                   *dash
	pluginManager          *plugin.Manager
	moduleManager          *module.Manager
	actionManager          *action.Manager
	websocketClientManager *api.WebsocketClientManager
	apiCreated             bool
	fs                     afero.Fs
}

func NewRunner(ctx context.Context, logger log.Logger, options Options) (*Runner, error) {
	ctx = internalLog.WithLoggerContext(ctx, logger)

	r := Runner{}

	if options.Context != "" {
		logger.With("initial-context", options.Context).Infof("Setting initial context from user flags")
	}

	actionManger := action.NewManager(logger)
	r.actionManager = actionManger

	websocketClientManager := api.NewWebsocketClientManager(ctx, r.actionManager)
	r.websocketClientManager = websocketClientManager
	go websocketClientManager.Run(ctx)

	listener, err := buildListener()
	if err != nil {
		err = fmt.Errorf("failed to create net listener: %w", err)
		return nil, fmt.Errorf("use OCTANT_LISTENER_ADDR to set host:port: %w", err)
	}

	var pluginService *pluginAPI.GRPCService
	var apiService api.Service
	var apiErr error

	r.fs = afero.NewOsFs()

	if options.KubeConfig, err = ValidateKubeConfig(logger, options.KubeConfig, r.fs); err == nil {
		apiService, pluginService, apiErr = r.initAPI(ctx, logger, options)
		if apiErr != nil {
			return nil, fmt.Errorf("failed to start service api: %w", apiErr)
		}
	} else {
		logger.Infof("no valid kube config found, initializing loading API")
		// Initialize the API
		apiService = api.NewLoadingAPI(ctx, api.PathPrefix, r.actionManager, r.websocketClientManager, logger)
	}

	d, err := newDash(listener, options.Namespace, options.FrontendURL, options.BrowserPath, apiService, pluginService, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create dash instance: %w", err)
	}

	if viper.GetBool("disable-open-browser") {
		d.willOpenBrowser = false
	}

	r.dash = d

	return &r, nil
}

func (r *Runner) Start(ctx context.Context, logger log.Logger, options Options, startupCh, shutdownCh chan bool) error {
	go func() {
		if err := r.dash.Run(ctx, startupCh); err != nil {
			logger.Debugf("running dashboard service: %v", err)
		}
	}()

	if !r.apiCreated {
		logger.Infof("waiting for kube config ...")
		options.KubeConfig = <-ocontext.KubeConfigChFrom(ctx)

		if options.KubeConfig == "" {
			return errors.New("unexpected empty kube config")
		}
		logger.Debugf("Loading configuration: %v", options.KubeConfig)
		apiService, pluginService, err := r.initAPI(ctx, logger, options)
		if err != nil {
			logger.Errorf("cannot create api: %v", err)
		}
		r.dash.apiHandler = apiService
		r.dash.pluginService = pluginService
	}
	r.startAPIService(ctx, logger)

	<-ctx.Done()

	shutdownCtx := internalLog.WithLoggerContext(context.Background(), logger)

	r.moduleManager.Unload()
	r.pluginManager.Stop(shutdownCtx)

	shutdownCh <- true
	return nil
}

func (r *Runner) initAPI(ctx context.Context, logger log.Logger, options Options) (*api.API, *pluginAPI.GRPCService, error) {
	frontendProxy := pluginAPI.FrontendProxy{}

	restConfigOptions := cluster.RESTConfigOptions{
		QPS:       options.ClientQPS,
		Burst:     options.ClientBurst,
		UserAgent: options.UserAgent,
	}
	clusterClient, err := cluster.FromKubeConfig(ctx, options.KubeConfig, options.Context, options.Namespace, options.Namespaces, restConfigOptions)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to init cluster client, does your kube config have a current-context set?: %w", err)
	}

	if options.EnableOpenCensus {
		if err := enableOpenCensus(); err != nil {
			logger.Infof("Enabling OpenCensus")
			return nil, nil, fmt.Errorf("enabling open census: %w", err)
		}
	}

	nsClient, err := clusterClient.NamespaceClient()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create namespace client: %w", err)
	}

	// If not overridden, use initial namespace from current context in KUBECONFIG
	if options.Namespace == "" {
		options.Namespace = nsClient.InitialNamespace()
	}

	logger.Debugf("initial namespace for dashboard is %s", options.Namespace)

	appObjectStore, err := initObjectStore(ctx, clusterClient)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing store: %w", err)
	}

	errorStore, err := oerrors.NewErrorStore()
	if err != nil {
		return nil, nil, fmt.Errorf("initializing error store: %w", err)
	}

	crdWatcher, err := describer.NewDefaultCRDWatcher(ctx, clusterClient, appObjectStore, errorStore)
	if err != nil {
		var ae *oerrors.AccessError
		if errors.As(err, &ae) {
			if ae.Name() == oerrors.OctantAccessError {
				logger.Warnf("skipping CRD watcher due to access denied error starting watcher")
			}
		} else {
			return nil, nil, fmt.Errorf("initializing CRD watcher: %w", err)
		}
	}

	portForwarder, err := initPortForwarder(ctx, clusterClient, appObjectStore)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing port forwarder: %w", err)
	}

	mo := &moduleOptions{
		clusterClient: clusterClient,
		namespace:     options.Namespace,
		logger:        logger,
		actionManager: r.actionManager,
	}
	moduleManager, err := initModuleManager(mo)
	if err != nil {
		return nil, nil, fmt.Errorf("init module manager: %w", err)
	}

	r.moduleManager = moduleManager
	pluginDashboardService := &pluginAPI.GRPCService{
		ObjectStore:            appObjectStore,
		PortForwarder:          portForwarder,
		NamespaceInterface:     nsClient,
		FrontendProxy:          frontendProxy,
		WebsocketClientManager: r.websocketClientManager,
	}

	pluginManager, err := initPlugin(moduleManager, r.actionManager, r.websocketClientManager, pluginDashboardService)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing plugin manager: %w", err)
	}

	r.pluginManager = pluginManager

	buildInfo := config.BuildInfo{
		Version: options.BuildInfo.Version,
		Commit:  options.BuildInfo.Commit,
		Time:    options.BuildInfo.Time,
	}

	dashConfig := config.NewLiveConfig(
		clusterClient,
		crdWatcher,
		options.KubeConfig,
		logger,
		moduleManager,
		appObjectStore,
		errorStore,
		pluginManager,
		portForwarder,
		options.Context,
		restConfigOptions,
		buildInfo)

	pluginManager.SetOctantClient(dashConfig)

	if err := watchConfigs(ctx, dashConfig, options.KubeConfig); err != nil {
		return nil, nil, fmt.Errorf("set up config watcher: %w", err)
	}

	moduleList, err := initModules(ctx, dashConfig, options.Namespace, options)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing modules: %w", err)
	}

	for _, mod := range moduleList {
		if err := moduleManager.Register(mod); err != nil {
			return nil, nil, fmt.Errorf("loading module %s: %w", mod.Name(), err)
		}
	}

	if err := pluginManager.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("start plugin manager: %w", err)
	}

	// Watch for CRDs after modules initialized
	if err := crdWatcher.Watch(ctx); err != nil {
		return nil, nil, fmt.Errorf("unable to start CRD watcher: %w", err)
	}

	apiService := api.New(ctx, api.PathPrefix, r.actionManager, r.websocketClientManager, dashConfig)
	frontendProxy.FrontendUpdateController = apiService

	r.apiCreated = true
	return apiService, pluginDashboardService, nil
}

// initObjectStore initializes the cluster object store interface
func initObjectStore(ctx context.Context, client cluster.ClientInterface) (store.Store, error) {
	if client == nil {
		return nil, fmt.Errorf("nil cluster client")
	}

	resourceAccess := objectstore.NewResourceAccess(client)
	appObjectStore, err := objectstore.NewDynamicCache(ctx, client, objectstore.Access(resourceAccess))

	if err != nil {
		return nil, fmt.Errorf("creating object store for app: %w", err)
	}

	return appObjectStore, nil
}

func initPortForwarder(ctx context.Context, client cluster.ClientInterface, appObjectStore store.Store) (portforward.PortForwarder, error) {
	return portforward.Default(ctx, client, appObjectStore)
}

type moduleOptions struct {
	clusterClient  *cluster.Cluster
	crdWatcher     config.CRDWatcher
	namespace      string
	logger         log.Logger
	pluginManager  *plugin.Manager
	portForwarder  portforward.PortForwarder
	kubeConfigPath string
	actionManager  *action.Manager
}

func initModules(ctx context.Context, dashConfig config.Dash, namespace string, options Options) ([]module.Module, error) {
	var list []module.Module

	podViewOptions := workloads.Options{
		DashConfig: dashConfig,
	}
	workloadModule, err := workloads.New(ctx, podViewOptions)
	if err != nil {
		return nil, fmt.Errorf("initialize workload module: %w", err)
	}

	list = append(list, workloadModule)

	if viper.GetBool("enable-feature-applications") {
		applicationsOptions := applications.Options{
			DashConfig: dashConfig,
		}
		applicationsModule := applications.New(ctx, applicationsOptions)
		list = append(list, applicationsModule)
	}

	overviewOptions := overview.Options{
		Namespace:  namespace,
		DashConfig: dashConfig,
	}
	overviewModule, err := overview.New(ctx, overviewOptions)
	if err != nil {
		return nil, fmt.Errorf("create overview module: %w", err)
	}

	list = append(list, overviewModule)

	if !options.DisableClusterOverview {
		clusterOverviewOptions := clusteroverview.Options{
			DashConfig: dashConfig,
		}
		clusterOverviewModule, err := clusteroverview.New(ctx, clusterOverviewOptions)
		if err != nil {
			return nil, fmt.Errorf("create cluster overview module: %w", err)
		}

		list = append(list, clusterOverviewModule)
	}

	configurationOptions := configuration.Options{
		DashConfig:     dashConfig,
		KubeConfigPath: dashConfig.KubeConfigPath(),
	}
	configurationModule := configuration.New(ctx, configurationOptions)

	list = append(list, configurationModule)

	localContentPath := viper.GetString("local-content")
	if localContentPath != "" {
		localContentModule := localcontent.New(localContentPath)
		list = append(list, localContentModule)
	}

	return list, nil
}

// initModuleManager initializes the moduleManager (and currently the modules themselves)
func initModuleManager(options *moduleOptions) (*module.Manager, error) {
	moduleManager, err := module.NewManager(options.clusterClient, options.namespace, options.actionManager, options.logger)
	if err != nil {
		return nil, fmt.Errorf("create module manager: %w", err)
	}

	return moduleManager, nil
}

func buildListener() (net.Listener, error) {
	listenerAddr := api.ListenerAddr()
	conn, err := net.DialTimeout("tcp", listenerAddr, time.Millisecond*500)
	if err != nil {
		return net.Listen("tcp", listenerAddr)
	}
	_ = conn.Close()
	return nil, fmt.Errorf("tcp %s: dial: already in use", listenerAddr)
}

type dash struct {
	mux             cmux.CMux
	listener        net.Listener
	uiURL           string
	browserPath     string
	namespace       string
	defaultHandler  func() (http.Handler, error)
	apiHandler      api.Service
	willOpenBrowser bool
	logger          log.Logger
	handlerFactory  *octant.HandlerFactory
	server          http.Server
	pluginService   pluginAPI.Service
}

func newDash(listener net.Listener, namespace, uiURL string, browserPath string, apiHandler api.Service, pluginHandler pluginAPI.Service, logger log.Logger) (*dash, error) {
	hf := octant.NewHandlerFactory(
		octant.BackendHandler(apiHandler.Handler),
		octant.FrontendURL(viper.GetString("proxy-frontend")))

	return &dash{
		mux:             cmux.New(listener),
		handlerFactory:  hf,
		listener:        listener,
		namespace:       namespace,
		uiURL:           uiURL,
		browserPath:     browserPath,
		defaultHandler:  web.Handler,
		willOpenBrowser: true,
		apiHandler:      apiHandler,
		pluginService:   pluginHandler,
		logger:          logger,
	}, nil
}

func (d *dash) SetFrontendHandler(fn octant.HandlerFactoryFunc) {
	d.handlerFactory.SetFrontend(fn)
}

func (d *dash) Run(ctx context.Context, startupCh chan bool) error {
	handler, err := d.handlerFactory.Handler(ctx)
	if err != nil {
		return err
	}

	d.server = http.Server{Handler: handler}

	// Enable serving the plugin API on the same endpoint as the Octant streaming API.
	// This enables remote gRPC plugins.
	// if d.pluginService != nil {
	//     grpcl := d.mux.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
	//     go serveGRPC(grpcl, d.pluginService)
	// }

	http1 := d.mux.Match(cmux.Any())
	go func() {
		if err = d.server.Serve(http1); err != nil && err != http.ErrServerClosed {
			d.logger.Errorf("http server: %v", err)
			os.Exit(1) // TODO graceful shutdown for other goroutines (GH#494)
		}
	}()

	go func() {
		if err := d.mux.Serve(); err != nil {
			errMessage := err.Error()
			if !strings.Contains(errMessage, "use of closed network connection") {
				panic(err)
			}
		}
	}()

	dashboardURL := fmt.Sprintf("http://%s", d.listener.Addr())

	d.logger.Infof("Dashboard is available at %s\n", dashboardURL)

	if startupCh != nil {
		startupCh <- true
	}

	if d.willOpenBrowser {
		runURL := dashboardURL
		if d.browserPath != "" {
			if !strings.HasPrefix(d.browserPath, "/") {
				d.browserPath = "/" + d.browserPath
			}
			runURL += d.browserPath
		}
		if err = open.Run(runURL); err != nil {
			d.logger.Warnf("unable to open browser: %v", err)
		}
	}

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	return d.server.Shutdown(shutdownCtx)
}

func enableOpenCensus() error {
	agentEndpointURI := "localhost:6831"

	je, err := jaeger.NewExporter(jaeger.Options{
		AgentEndpoint: agentEndpointURI,
		Process: jaeger.Process{
			ServiceName: "octant",
		},
	})

	if err != nil {
		return fmt.Errorf("failed to create Jaeger exporter: %w", err)
	}

	trace.RegisterExporter(je)
	trace.ApplyConfig(trace.Config{DefaultSampler: trace.AlwaysSample()})

	return nil
}

// ValidateKubeConfig returns a valid file list of kube config(s)
func ValidateKubeConfig(logger log.Logger, kubeConfig string, fs afero.Fs) (string, error) {
	fileList := []string{}
	paths := filepath.SplitList(kubeConfig)

	for _, path := range paths {
		exists, err := afero.Exists(fs, path)
		if err != nil {
			logger.Errorf("check path exists: %v", err)
		}

		if exists {
			fileList = append(fileList, path)
			continue
		}
		logger.Infof("cannot find kube config: %v", path)
	}

	if len(fileList) > 0 {
		return strings.Join(fileList, string(filepath.ListSeparator)), nil
	}
	return "", fmt.Errorf("no kubeconfig found")
}

func (r *Runner) startAPIService(ctx context.Context, logger log.Logger) {
	hf := octant.NewHandlerFactory(
		octant.BackendHandler(r.dash.apiHandler.Handler),
		octant.FrontendURL(viper.GetString("proxy-frontend")))

	var err error
	r.dash.server.Handler, err = hf.Handler(ctx)
	if err != nil {
		logger.Errorf("cannot create handler: %v", err)
	}

	logger.Infof("using api service")
}
