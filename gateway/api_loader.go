package gateway

import (
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"github.com/justinas/alice"

	"github.com/ins-tykgw/tyk/apidef"
	"github.com/ins-tykgw/tyk/config"
	"github.com/ins-tykgw/tyk/coprocess"
	"github.com/ins-tykgw/tyk/storage"
	"github.com/ins-tykgw/tyk/trace"
)

type ChainObject struct {
	Domain         string
	ListenOn       string
	ThisHandler    http.Handler
	RateLimitChain http.Handler
	RateLimitPath  string
	Open           bool
	Index          int
	Skip           bool
	Subrouter      *mux.Router
}

func prepareStorage() (storage.RedisCluster, storage.RedisCluster, storage.RedisCluster, RPCStorageHandler, RPCStorageHandler) {
	redisStore := storage.RedisCluster{KeyPrefix: "apikey-", HashKeys: config.Global().HashKeys}
	redisOrgStore := storage.RedisCluster{KeyPrefix: "orgkey."}
	healthStore := storage.RedisCluster{KeyPrefix: "apihealth."}
	rpcAuthStore := RPCStorageHandler{KeyPrefix: "apikey-", HashKeys: config.Global().HashKeys}
	rpcOrgStore := RPCStorageHandler{KeyPrefix: "orgkey."}

	FallbackKeySesionManager.Init(&redisStore)

	return redisStore, redisOrgStore, healthStore, rpcAuthStore, rpcOrgStore
}

func skipSpecBecauseInvalid(spec *APISpec, logger *logrus.Entry) bool {

	if spec.Proxy.ListenPath == "" {
		logger.Error("Listen path is empty")
		return true
	}

	if strings.Contains(spec.Proxy.ListenPath, " ") {
		logger.Error("Listen path contains spaces, is invalid")
		return true
	}

	_, err := url.Parse(spec.Proxy.TargetURL)
	if err != nil {
		logger.Error("couldn't parse target URL: ", err)
		return true
	}

	return false
}

func generateDomainPath(hostname, listenPath string) string {
	return hostname + listenPath
}

func countApisByListenHash(specs []*APISpec) map[string]int {
	count := make(map[string]int, len(specs))
	// We must track the hostname no matter what
	for _, spec := range specs {
		domainHash := generateDomainPath(spec.Domain, spec.Proxy.ListenPath)
		if count[domainHash] == 0 {
			dN := spec.Domain
			if dN == "" {
				dN = "(no host)"
			}
			mainLog.WithFields(logrus.Fields{
				"api_name": spec.Name,
				"domain":   dN,
			}).Info("Tracking hostname")
		}
		count[domainHash]++
	}
	return count
}

func processSpec(spec *APISpec, apisByListen map[string]int,
	redisStore, redisOrgStore, healthStore, rpcAuthStore, rpcOrgStore storage.Handler,
	subrouter *mux.Router, logger *logrus.Entry) *ChainObject {

	var chainDef ChainObject
	chainDef.Subrouter = subrouter

	logger = logger.WithFields(logrus.Fields{
		"org_id":   spec.OrgID,
		"api_id":   spec.APIID,
		"api_name": spec.Name,
	})

	var coprocessLog = logger.WithFields(logrus.Fields{
		"prefix": "coprocess",
	})

	logger.Info("Loading API")

	if len(spec.TagHeaders) > 0 {
		// Ensure all headers marked for tagging are lowercase
		lowerCaseHeaders := make([]string, len(spec.TagHeaders))
		for i, k := range spec.TagHeaders {
			lowerCaseHeaders[i] = strings.ToLower(k)

		}
		spec.TagHeaders = lowerCaseHeaders
	}

	if skipSpecBecauseInvalid(spec, logger) {
		logger.Warning("Spec not valid, skipped!")
		chainDef.Skip = true
		return &chainDef
	}

	// Expose API only to looping
	if spec.Internal {
		chainDef.Skip = true
	}

	pathModified := false
	for {
		hash := generateDomainPath(spec.Domain, spec.Proxy.ListenPath)
		if apisByListen[hash] < 2 {
			// not a duplicate
			break
		}
		if !pathModified {
			prev := getApiSpec(spec.APIID)
			if prev != nil && prev.Proxy.ListenPath == spec.Proxy.ListenPath {
				// if this APIID was already loaded and
				// had this listen path, let it keep it.
				break
			}
			spec.Proxy.ListenPath += "-" + spec.APIID
			pathModified = true
		} else {
			// keep adding '_' chars
			spec.Proxy.ListenPath += "_"
		}
	}
	if pathModified {
		logger.Error("Listen path collision, changed to ", spec.Proxy.ListenPath)
	}

	// Set up LB targets:
	if spec.Proxy.EnableLoadBalancing {
		sl := apidef.NewHostListFromList(spec.Proxy.Targets)
		spec.Proxy.StructuredTargetList = sl
	}

	// Initialise the auth and session managers (use Redis for now)
	authStore := redisStore
	orgStore := redisOrgStore
	switch spec.AuthProvider.StorageEngine {
	case LDAPStorageEngine:
		storageEngine := LDAPStorageHandler{}
		storageEngine.LoadConfFromMeta(spec.AuthProvider.Meta)
		authStore = &storageEngine
	case RPCStorageEngine:
		authStore = rpcAuthStore
		orgStore = rpcOrgStore
		spec.GlobalConfig.EnforceOrgDataAge = true
		globalConf := config.Global()
		globalConf.EnforceOrgDataAge = true
		config.SetGlobal(globalConf)
	}

	sessionStore := redisStore
	switch spec.SessionProvider.StorageEngine {
	case RPCStorageEngine:
		sessionStore = rpcAuthStore
	}

	// Health checkers are initialised per spec so that each API handler has it's own connection and redis storage pool
	spec.Init(authStore, sessionStore, healthStore, orgStore)

	// Set up all the JSVM middleware
	var mwAuthCheckFunc apidef.MiddlewareDefinition
	mwPreFuncs := []apidef.MiddlewareDefinition{}
	mwPostFuncs := []apidef.MiddlewareDefinition{}
	mwPostAuthCheckFuncs := []apidef.MiddlewareDefinition{}

	var mwDriver apidef.MiddlewareDriver

	var prefix string
	if spec.CustomMiddlewareBundle != "" {
		if err := loadBundle(spec); err != nil {
			logger.Error("Couldn't load bundle")
		}
		tykBundlePath := filepath.Join(config.Global().MiddlewarePath, "bundles")
		bundleNameHash := md5.New()
		io.WriteString(bundleNameHash, spec.CustomMiddlewareBundle)
		bundlePath := fmt.Sprintf("%s_%x", spec.APIID, bundleNameHash.Sum(nil))
		prefix = filepath.Join(tykBundlePath, bundlePath)
	}

	logger.Debug("Initializing API")
	var mwPaths []string

	mwPaths, mwAuthCheckFunc, mwPreFuncs, mwPostFuncs, mwPostAuthCheckFuncs, mwDriver = loadCustomMiddleware(spec)

	if config.Global().EnableJSVM && mwDriver == apidef.OttoDriver {
		spec.JSVM.LoadJSPaths(mwPaths, prefix)
	}

	if spec.EnableBatchRequestSupport {
		addBatchEndpoint(spec, subrouter)
	}

	if spec.UseOauth2 {
		logger.Debug("Loading OAuth Manager")
		oauthManager := addOAuthHandlers(spec, subrouter)
		logger.Debug("-- Added OAuth Handlers")

		spec.OAuthManager = oauthManager
		logger.Debug("Done loading OAuth Manager")
	}

	enableVersionOverrides := false
	for _, versionData := range spec.VersionData.Versions {
		if versionData.OverrideTarget != "" {
			enableVersionOverrides = true
			break
		}
	}

	// Already vetted
	spec.target, _ = url.Parse(spec.Proxy.TargetURL)

	var proxy ReturningHttpHandler
	if enableVersionOverrides {
		logger.Info("Multi target enabled")
		proxy = NewMultiTargetProxy(spec)
	} else {
		proxy = TykNewSingleHostReverseProxy(spec.target, spec)
	}

	// Create the response processors
	createResponseMiddlewareChain(spec)

	baseMid := BaseMiddleware{Spec: spec, Proxy: proxy, logger: logger}

	for _, v := range baseMid.Spec.VersionData.Versions {
		if len(v.ExtendedPaths.CircuitBreaker) > 0 {
			baseMid.Spec.CircuitBreakerEnabled = true
		}
		if len(v.ExtendedPaths.HardTimeouts) > 0 {
			baseMid.Spec.EnforcedTimeoutEnabled = true
		}
	}

	keyPrefix := "cache-" + spec.APIID
	cacheStore := storage.RedisCluster{KeyPrefix: keyPrefix, IsCache: true}
	cacheStore.Connect()

	var chain http.Handler
	var chainArray []alice.Constructor
	var authArray []alice.Constructor

	if spec.UseKeylessAccess {
		chainDef.Open = true
		logger.Info("Checking security policy: Open")
	}

	handleCORS(&chainArray, spec)

	for _, obj := range mwPreFuncs {
		if mwDriver == apidef.GoPluginDriver {
			mwAppendEnabled(
				&chainArray,
				&GoPluginMiddleware{
					BaseMiddleware: baseMid,
					Path:           obj.Path,
					SymbolName:     obj.Name,
				},
			)
		} else if mwDriver != apidef.OttoDriver {
			coprocessLog.Debug("Registering coprocess middleware, hook name: ", obj.Name, "hook type: Pre", ", driver: ", mwDriver)
			mwAppendEnabled(&chainArray, &CoProcessMiddleware{baseMid, coprocess.HookType_Pre, obj.Name, mwDriver, obj.RawBodyOnly, nil})
		} else {
			chainArray = append(chainArray, createDynamicMiddleware(obj.Name, true, obj.RequireSession, baseMid))
		}
	}

	mwAppendEnabled(&chainArray, &RateCheckMW{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &IPWhiteListMiddleware{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &IPBlackListMiddleware{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &CertificateCheckMW{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &OrganizationMonitor{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &VersionCheck{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &RequestSizeLimitMiddleware{baseMid})
	mwAppendEnabled(&chainArray, &MiddlewareContextVars{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &TrackEndpointMiddleware{baseMid})

	if !spec.UseKeylessAccess {
		// Select the keying method to use for setting session states
		if mwAppendEnabled(&authArray, &Oauth2KeyExists{baseMid}) {
			logger.Info("Checking security policy: OAuth")
		}

		if mwAppendEnabled(&authArray, &BasicAuthKeyIsValid{baseMid, nil, nil}) {
			logger.Info("Checking security policy: Basic")
		}

		if mwAppendEnabled(&authArray, &HMACMiddleware{BaseMiddleware: baseMid}) {
			logger.Info("Checking security policy: HMAC")
		}

		if mwAppendEnabled(&authArray, &JWTMiddleware{baseMid}) {
			logger.Info("Checking security policy: JWT")
		}

		if mwAppendEnabled(&authArray, &OpenIDMW{BaseMiddleware: baseMid}) {
			logger.Info("Checking security policy: OpenID")
		}

		coprocessAuth := EnableCoProcess && mwDriver != apidef.OttoDriver && spec.EnableCoProcessAuth
		ottoAuth := !coprocessAuth && mwDriver == apidef.OttoDriver && spec.EnableCoProcessAuth
		gopluginAuth := !coprocessAuth && !ottoAuth && mwDriver == apidef.GoPluginDriver && spec.UseGoPluginAuth

		if coprocessAuth {
			// TODO: check if mwAuthCheckFunc is available/valid
			coprocessLog.Debug("Registering coprocess middleware, hook name: ", mwAuthCheckFunc.Name, "hook type: CustomKeyCheck", ", driver: ", mwDriver)

			newExtractor(spec, baseMid)
			mwAppendEnabled(&authArray, &CoProcessMiddleware{baseMid, coprocess.HookType_CustomKeyCheck, mwAuthCheckFunc.Name, mwDriver, mwAuthCheckFunc.RawBodyOnly, nil})
		}

		if ottoAuth {
			logger.Info("----> Checking security policy: JS Plugin")

			authArray = append(authArray, createDynamicMiddleware(mwAuthCheckFunc.Name, true, false, baseMid))
		}

		if gopluginAuth {
			mwAppendEnabled(
				&authArray,
				&GoPluginMiddleware{
					BaseMiddleware: baseMid,
					Path:           mwAuthCheckFunc.Path,
					SymbolName:     mwAuthCheckFunc.Name,
				},
			)
		}

		if spec.UseStandardAuth || len(authArray) == 0 {
			logger.Info("Checking security policy: Token")
			authArray = append(authArray, createMiddleware(&AuthKey{baseMid}))
		}

		chainArray = append(chainArray, authArray...)

		for _, obj := range mwPostAuthCheckFuncs {
			if mwDriver == apidef.GoPluginDriver {
				mwAppendEnabled(
					&chainArray,
					&GoPluginMiddleware{
						BaseMiddleware: baseMid,
						Path:           obj.Path,
						SymbolName:     obj.Name,
					},
				)
			} else {
				coprocessLog.Debug("Registering coprocess middleware, hook name: ", obj.Name, "hook type: Pre", ", driver: ", mwDriver)
				mwAppendEnabled(&chainArray, &CoProcessMiddleware{baseMid, coprocess.HookType_PostKeyAuth, obj.Name, mwDriver, obj.RawBodyOnly, nil})
			}
		}

		mwAppendEnabled(&chainArray, &StripAuth{baseMid})
		mwAppendEnabled(&chainArray, &KeyExpired{baseMid})
		mwAppendEnabled(&chainArray, &AccessRightsCheck{baseMid})
		mwAppendEnabled(&chainArray, &GranularAccessMiddleware{baseMid})
		mwAppendEnabled(&chainArray, &RateLimitAndQuotaCheck{baseMid})
	}

	mwAppendEnabled(&chainArray, &RateLimitForAPI{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &ValidateJSON{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &TransformMiddleware{baseMid})
	mwAppendEnabled(&chainArray, &TransformJQMiddleware{baseMid})
	mwAppendEnabled(&chainArray, &TransformHeaders{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &URLRewriteMiddleware{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &TransformMethod{BaseMiddleware: baseMid})
	mwAppendEnabled(&chainArray, &RedisCacheMiddleware{BaseMiddleware: baseMid, CacheStore: &cacheStore})
	mwAppendEnabled(&chainArray, &VirtualEndpoint{BaseMiddleware: baseMid})

	for _, obj := range mwPostFuncs {
		if mwDriver == apidef.GoPluginDriver {
			mwAppendEnabled(
				&chainArray,
				&GoPluginMiddleware{
					BaseMiddleware: baseMid,
					Path:           obj.Path,
					SymbolName:     obj.Name,
				},
			)
		} else if mwDriver != apidef.OttoDriver {
			coprocessLog.Debug("Registering coprocess middleware, hook name: ", obj.Name, "hook type: Post", ", driver: ", mwDriver)
			mwAppendEnabled(&chainArray, &CoProcessMiddleware{baseMid, coprocess.HookType_Post, obj.Name, mwDriver, obj.RawBodyOnly, nil})
		} else {
			chainArray = append(chainArray, createDynamicMiddleware(obj.Name, false, obj.RequireSession, baseMid))
		}
	}

	chain = alice.New(chainArray...).Then(&DummyProxyHandler{SH: SuccessHandler{baseMid}})

	if !spec.UseKeylessAccess {
		var simpleArray []alice.Constructor
		mwAppendEnabled(&simpleArray, &IPWhiteListMiddleware{baseMid})
		mwAppendEnabled(&simpleArray, &IPBlackListMiddleware{BaseMiddleware: baseMid})
		mwAppendEnabled(&simpleArray, &OrganizationMonitor{BaseMiddleware: baseMid})
		mwAppendEnabled(&simpleArray, &VersionCheck{BaseMiddleware: baseMid})
		simpleArray = append(simpleArray, authArray...)
		mwAppendEnabled(&simpleArray, &KeyExpired{baseMid})
		mwAppendEnabled(&simpleArray, &AccessRightsCheck{baseMid})

		rateLimitPath := spec.Proxy.ListenPath + "tyk/rate-limits/"

		logger.Debug("Rate limit endpoint is: ", rateLimitPath)

		chainDef.RateLimitPath = rateLimitPath
		chainDef.RateLimitChain = alice.New(simpleArray...).
			Then(http.HandlerFunc(userRatesCheck))
	}

	logger.Debug("Setting Listen Path: ", spec.Proxy.ListenPath)

	if trace.IsEnabled() {
		chainDef.ThisHandler = trace.Handle(spec.Name, chain)
	} else {
		chainDef.ThisHandler = chain
	}
	chainDef.ListenOn = spec.Proxy.ListenPath + "{rest:.*}"
	chainDef.Domain = spec.Domain

	logger.WithFields(logrus.Fields{
		"prefix":      "gateway",
		"user_ip":     "--",
		"server_name": "--",
		"user_id":     "--",
	}).Info("API Loaded")

	return &chainDef
}

// Check for recursion
const defaultLoopLevelLimit = 5

func isLoop(r *http.Request) (bool, error) {
	if r.URL.Scheme != "tyk" {
		return false, nil
	}

	limit := ctxLoopLevelLimit(r)
	if limit == 0 {
		limit = defaultLoopLevelLimit
	}

	if ctxLoopLevel(r) > limit {
		return true, fmt.Errorf("Loop level too deep. Found more than %d loops in single request", limit)
	}

	return true, nil
}

type DummyProxyHandler struct {
	SH SuccessHandler
}

func (d *DummyProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if found, err := isLoop(r); found {
		if err != nil {
			handler := ErrorHandler{*d.SH.Base()}
			handler.HandleError(w, r, err.Error(), http.StatusInternalServerError, true)
			return
		}

		r.URL.Scheme = "http"
		if methodOverride := r.URL.Query().Get("method"); methodOverride != "" {
			r.Method = methodOverride
		}

		var handler http.Handler
		if r.URL.Hostname() == "self" {
			handler = d.SH.Spec.middlewareChain
		} else {
			ctxSetVersionInfo(r, nil)

			if targetAPI := fuzzyFindAPI(r.URL.Hostname()); targetAPI != nil {
				handler = targetAPI.middlewareChain
			} else {
				handler := ErrorHandler{*d.SH.Base()}
				handler.HandleError(w, r, "Can't detect loop target", http.StatusInternalServerError, true)
				return
			}
		}

		// No need to handle errors, in all error cases limit will be set to 0
		loopLevelLimit, _ := strconv.Atoi(r.URL.Query().Get("loop_limit"))
		ctxSetCheckLoopLimits(r, r.URL.Query().Get("check_limits") == "true")

		if origURL := ctxGetOrigRequestURL(r); origURL != nil {
			r.URL.Host = origURL.Host
			r.URL.RawQuery = origURL.RawQuery
			ctxSetOrigRequestURL(r, nil)
		}

		ctxIncLoopLevel(r, loopLevelLimit)

		handler.ServeHTTP(w, r)
	} else {
		d.SH.ServeHTTP(w, r)
	}
}

func loadGlobalApps(router *mux.Router) {
	// we need to make a full copy of the slice, as loadApps will
	// use in-place to sort the apis.
	apisMu.RLock()
	specs := make([]*APISpec, len(apiSpecs))
	copy(specs, apiSpecs)
	apisMu.RUnlock()
	loadApps(specs, router)

	if config.Global().NewRelic.AppName != "" {
		mainLog.Info("Adding NewRelic instrumentation")
		AddNewRelicInstrumentation(NewRelicApplication, router)
	}
}

func trimCategories(name string) string {
	if i := strings.Index(name, "#"); i != -1 {
		return name[:i-1]
	}

	return name
}

func fuzzyFindAPI(search string) *APISpec {
	if search == "" {
		return nil
	}

	apisMu.RLock()
	defer apisMu.RUnlock()

	for _, api := range apisByID {
		if api.APIID == search ||
			api.Id.Hex() == search ||
			replaceNonAlphaNumeric(trimCategories(api.Name)) == search {
			return api
		}
	}

	return nil
}

// Create the individual API (app) specs based on live configurations and assign middleware
func loadApps(specs []*APISpec, muxer *mux.Router) {
	hostname := config.Global().HostName
	if hostname != "" {
		muxer = muxer.Host(hostname).Subrouter()
		mainLog.Info("API hostname set: ", hostname)
	}

	mainLog.Info("Loading API configurations.")

	tmpSpecRegister := make(map[string]*APISpec)

	// Only create this once, add other types here as needed, seems wasteful but we can let the GC handle it
	redisStore, redisOrgStore, healthStore, rpcAuthStore, rpcOrgStore := prepareStorage()

	// sort by listen path from longer to shorter, so that /foo
	// doesn't break /foo-bar
	sort.Slice(specs, func(i, j int) bool {
		return len(specs[i].Proxy.ListenPath) > len(specs[j].Proxy.ListenPath)
	})

	// Create a new handler for each API spec
	loadList := make([]*ChainObject, len(specs))
	apisByListen := countApisByListenHash(specs)

	// Set up the host sub-routers first, since we need to set up
	// exactly one per host. If we set up one per API definition,
	// only one of the APIs will work properly, since the router
	// doesn't backtrack and will stop at the first host sub-router
	// match.
	hostRouters := map[string]*mux.Router{"": muxer}
	var hosts []string
	for _, spec := range specs {
		hosts = append(hosts, spec.Domain)
	}

	if trace.IsEnabled() {
		for _, spec := range specs {
			trace.AddTracer(spec.Name)
		}
	}
	// Decreasing sort by length and chars, so that the order of
	// creation of the host sub-routers is deterministic and
	// consistent with the order of the paths.
	sort.Slice(hosts, func(i, j int) bool {
		h1, h2 := hosts[i], hosts[j]
		if len(h1) != len(h2) {
			return len(h1) > len(h2)
		}
		return h1 > h2
	})
	for _, host := range hosts {
		if !config.Global().EnableCustomDomains {
			continue // disabled
		}
		if hostRouters[host] != nil {
			continue // already set up a subrouter
		}
		mainLog.WithField("domain", host).Info("Sub-router created for domain")
		hostRouters[host] = muxer.Host(host).Subrouter()
	}

	for i, spec := range specs {
		subrouter := hostRouters[spec.Domain]
		if subrouter == nil {
			mainLog.WithFields(logrus.Fields{
				"domain": spec.Domain,
				"api_id": spec.APIID,
			}).Warning("Trying to load API with Domain when custom domains are disabled.")
			subrouter = muxer
		}

		chainObj := processSpec(spec, apisByListen, &redisStore, &redisOrgStore, &healthStore, &rpcAuthStore, &rpcOrgStore, subrouter, logrus.NewEntry(log))
		apisMu.Lock()
		spec.middlewareChain = chainObj.ThisHandler
		apisMu.Unlock()

		// TODO: This will not deal with skipped APis well
		tmpSpecRegister[spec.APIID] = spec
		loadList[i] = chainObj
	}

	for _, chainObj := range loadList {
		if chainObj.Skip {
			continue
		}
		if !chainObj.Open {
			chainObj.Subrouter.Handle(chainObj.RateLimitPath, chainObj.RateLimitChain)
		}

		mainLog.Infof("Processed and listening on: %s%s", chainObj.Domain, chainObj.ListenOn)
		chainObj.Subrouter.Handle(chainObj.ListenOn, chainObj.ThisHandler)
	}

	// All APIs processed, now we can healthcheck
	// Add a root message to check all is OK
	muxer.HandleFunc("/"+config.Global().HealthCheckEndpointName, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Hello Tiki")
	})

	// Swap in the new register
	apisMu.Lock()

	// release current specs resources before overwriting map
	for _, curSpec := range apisByID {
		curSpec.Release()
	}

	apisByID = tmpSpecRegister
	apisMu.Unlock()

	mainLog.Debug("Checker host list")

	// Kick off our host checkers
	if !config.Global().UptimeTests.Disable {
		SetCheckerHostList()
	}

	mainLog.Debug("Checker host Done")

	mainLog.Info("Initialised API Definitions")
}
