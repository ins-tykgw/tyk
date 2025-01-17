// +build !coprocess

package gateway

import (
	"net/http"

	"github.com/Sirupsen/logrus"

	"github.com/ins-tykgw/tyk/apidef"
	"github.com/ins-tykgw/tyk/config"
	"github.com/ins-tykgw/tyk/coprocess"
)

const (
	EH_CoProcessHandler apidef.TykEventHandlerName = "cp_dynamic_handler"
)

type Dispatcher interface {
	DispatchEvent([]byte)
	LoadModules()
	HandleMiddlewareCache(*apidef.BundleManifest, string)
	Reload()
}

var (
	GlobalDispatcher Dispatcher
	EnableCoProcess  = false
)

type CoProcessMiddleware struct {
	BaseMiddleware
	HookType         coprocess.HookType
	HookName         string
	MiddlewareDriver apidef.MiddlewareDriver
	RawBodyOnly      bool

	successHandler *SuccessHandler
}

func (m *CoProcessMiddleware) Name() string {
	return "CoProcessMiddlewareDummy"
}

func (m *CoProcessMiddleware) EnabledForSpec() bool { return false }
func (m *CoProcessMiddleware) ProcessRequest(w http.ResponseWriter, r *http.Request, _ interface{}) (error, int) {
	return nil, 200
}

type CoProcessEventHandler struct {
	Spec *APISpec
}

func (l *CoProcessEventHandler) Init(handlerConf interface{}) error {
	return nil
}
func (l *CoProcessEventHandler) HandleEvent(em config.EventMessage) {}

func CoProcessInit() error {
	log.WithFields(logrus.Fields{
		"prefix": "coprocess",
	}).Info("Disabled feature")
	return nil
}

func DoCoprocessReload() {}
