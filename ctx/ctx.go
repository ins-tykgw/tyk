package ctx

import (
	"context"
	"net/http"

	"github.com/ins-tykgw/tyk/storage"
	"github.com/ins-tykgw/tyk/user"
)

const (
	SessionData = iota
	UpdateSession
	AuthToken
	HashedAuthToken
	VersionData
	VersionDefault
	OrgSessionContext
	ContextData
	RetainHost
	TrackThisEndpoint
	DoNotTrackThisEndpoint
	UrlRewritePath
	RequestMethod
	OrigRequestURL
	LoopLevel
	LoopLevelLimit
	ThrottleLevel
	ThrottleLevelLimit
	Trace
	CheckLoopLimits
)

func setContext(r *http.Request, ctx context.Context) {
	r2 := r.WithContext(ctx)
	*r = *r2
}

func ctxSetSession(r *http.Request, s *user.SessionState, token string, scheduleUpdate bool) {
	if s == nil {
		panic("setting a nil context SessionData")
	}

	if token == "" {
		token = GetAuthToken(r)
	}

	if s.KeyHashEmpty() {
		s.SetKeyHash(storage.HashKey(token))
	}

	ctx := r.Context()
	ctx = context.WithValue(ctx, SessionData, s)
	ctx = context.WithValue(ctx, AuthToken, token)

	if scheduleUpdate {
		ctx = context.WithValue(ctx, UpdateSession, true)
	}

	setContext(r, ctx)
}

func GetAuthToken(r *http.Request) string {
	if v := r.Context().Value(AuthToken); v != nil {
		return v.(string)
	}
	return ""
}

func GetSession(r *http.Request) *user.SessionState {
	if v := r.Context().Value(SessionData); v != nil {
		return v.(*user.SessionState)
	}
	return nil
}

func SetSession(r *http.Request, s *user.SessionState, token string, scheduleUpdate bool) {
	ctxSetSession(r, s, token, scheduleUpdate)
}
