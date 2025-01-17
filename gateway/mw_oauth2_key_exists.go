package gateway

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ins-tykgw/tyk/apidef"
	"github.com/ins-tykgw/tyk/headers"
)

const (
	checkOAuthClientDeletedInetrval = 1 * time.Second
)

// Oauth2KeyExists will check if the key being used to access the API is in the request data,
// and then if the key is in the storage engine
type Oauth2KeyExists struct {
	BaseMiddleware
}

func (k *Oauth2KeyExists) Name() string {
	return "Oauth2KeyExists"
}

func (k *Oauth2KeyExists) EnabledForSpec() bool {
	return k.Spec.UseOauth2
}

// ProcessRequest will run any checks on the request on the way through the system, return an error to have the chain fail
func (k *Oauth2KeyExists) ProcessRequest(w http.ResponseWriter, r *http.Request, _ interface{}) (error, int) {
	logger := k.Logger()
	// We're using OAuth, start checking for access keys
	token := r.Header.Get(headers.Authorization)
	parts := strings.Split(token, " ")

	if len(parts) < 2 {
		logger.Info("Attempted access with malformed header, no auth header found.")

		return errors.New("Authorization field missing"), http.StatusBadRequest
	}

	if strings.ToLower(parts[0]) != "bearer" {
		logger.Info("Bearer token malformed")

		return errors.New("Bearer token malformed"), http.StatusBadRequest
	}

	accessToken := parts[1]
	logger = logger.WithField("key", obfuscateKey(accessToken))

	// get session for the given oauth token
	session, keyExists := k.CheckSessionAndIdentityForValidKey(accessToken, r)
	if !keyExists {
		logger.Warning("Attempted access with non-existent key.")

		// Fire Authfailed Event
		AuthFailed(k, r, accessToken)
		// Report in health check
		reportHealthValue(k.Spec, KeyFailure, "-1")

		return errors.New("Key not authorised"), http.StatusForbidden
	}

	// Make sure OAuth-client is still present
	oauthClientDeletedKey := "oauth-del-" + k.Spec.APIID + session.OauthClientID
	oauthClientDeleted := false
	// check if that oauth client was deleted with using  memory cache first
	if val, found := UtilCache.Get(oauthClientDeletedKey); found {
		oauthClientDeleted = val.(bool)
	} else {
		// if not cached in memory then hit Redis to get oauth-client from there
		if _, err := k.Spec.OAuthManager.OsinServer.Storage.GetClient(session.OauthClientID); err != nil {
			// set this oauth client as deleted in memory cache forever
			UtilCache.Set(oauthClientDeletedKey, true, -1)
			oauthClientDeleted = true
		} else {
			// set this oauth client as NOT deleted in memory cache for next N sec
			UtilCache.Set(oauthClientDeletedKey, false, checkOAuthClientDeletedInetrval)
		}
	}
	if oauthClientDeleted {
		logger.WithField("oauthClientID", session.OauthClientID).Warning("Attempted access for deleted OAuth client.")
		return errors.New("Key not authorised. OAuth client access was revoked"), http.StatusForbidden
	}

	// Set session state on context, we will need it later
	switch k.Spec.BaseIdentityProvidedBy {
	case apidef.OAuthKey, apidef.UnsetAuth:
		ctxSetSession(r, &session, accessToken, false)
	}

	// Request is valid, carry on
	return nil, http.StatusOK
}
