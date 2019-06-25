// +build jq

package main

import (
	"testing"

	"github.com/ins-apigw/tyk/apidef"
	"github.com/ins-apigw/tyk/test"
)

func testPrepareJQMiddleware() {
	buildAndLoadAPI(func(spec *APISpec) {
		spec.Proxy.ListenPath = "/"
		spec.EnableContextVars = true
		updateAPIVersion(spec, "v1", func(v *apidef.VersionInfo) {
			v.UseExtendedPaths = true
			v.ExtendedPaths.TransformJQ = []apidef.TransformJQMeta{{
				Path:   "/jq",
				Method: "POST",
				Filter: `{"body": (.body + {"TRANSFORMED-REQUEST-BY-JQ": true, path: ._tyk_context.path, user_agent: ._tyk_context.headers_User_Agent}), "rewrite_headers": {"X-added-rewrite-headers": .body.foo}, "tyk_context": { "foo-val": .body.foo}}`,
			}}
		})
	})
}

func TestJQMiddleware(t *testing.T) {
	ts := newTykTestServer()
	defer ts.Close()

	testPrepareJQMiddleware()

	bodyContextVar := `\"path\":\"/jq\"`
	headersBodyVar := `"X-Added-Rewrite-Headers":"bar"`

	ts.Run(t, []test.TestCase{
		{Path: "/jq", Method: "POST", Data: `{"foo": "bar"}`, Code: 200, BodyMatch: bodyContextVar},
		{Path: "/jq", Method: "POST", Data: `{"foo": "bar"}`, Code: 200, BodyMatch: headersBodyVar},
		{Path: "/jq", Method: "POST", Data: `wrong json`, Code: 415},
	}...)
}

func BenchmarkJQMiddleware(b *testing.B) {
	b.ReportAllocs()

	ts := newTykTestServer()
	defer ts.Close()

	testPrepareJQMiddleware()

	bodyContextVar := `\"path\":\"/jq\"`
	headersBodyVar := `"X-Added-Rewrite-Headers":"bar"`

	for i := 0; i < b.N; i++ {
		ts.Run(b, []test.TestCase{
			{Path: "/jq", Method: "POST", Data: `{"foo": "bar"}`, Code: 200, BodyMatch: bodyContextVar},
			{Path: "/jq", Method: "POST", Data: `{"foo": "bar"}`, Code: 200, BodyMatch: headersBodyVar},
			{Path: "/jq", Method: "POST", Data: `wrong json`, Code: 415},
		}...)
	}
}
