//+build !jq

package main

import (
	"net/http"

	"github.com/ins-apigw/tyk/user"
)

type ResponseTransformJQMiddleware struct {
	Spec *APISpec
}

func (h *ResponseTransformJQMiddleware) Init(c interface{}, spec *APISpec) error {
	h.Spec = spec

	return nil
}

func (h *ResponseTransformJQMiddleware) HandleResponse(rw http.ResponseWriter, res *http.Response, req *http.Request, ses *user.SessionState) error {
	log.Warning("JQ transforms not supported")
	return nil
}
