/*
Copyright 2020 Gravitational, Inc.

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

package app

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gravitational/trace"
	"github.com/julienschmidt/httprouter"

	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/httplib"
	"github.com/gravitational/teleport/lib/utils"
)

type fragmentRequest struct {
	StateValue         string `json:"state_value"`
	CookieValue        string `json:"cookie_value"`
	SubjectCookieValue string `json:"subject_cookie_value"`
}

// handleFragment handles fragment authentication. Returns a Javascript
// application that reads in the fragment which submits an POST request to
// the same handler which can validate and set the cookie.
func (h *Handler) handleFragment(w http.ResponseWriter, r *http.Request, p httprouter.Params) error {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		// If the state query parameter is not set, generate a new state token,
		// store it in a cookie and redirect back to the app launcher.
		if q.Get("state") == "" {
			stateToken, err := utils.CryptoRandomHex(auth.TokenLenBytes)
			if err != nil {
				h.log.WithError(err).Debugf("Failed to generate and encode random numbers.")
				return trace.AccessDenied("access denied")
			}
			h.setAuthStateCookie(w, stateToken)
			urlParams := launcherURLParams{
				clusterName: q.Get("cluster"),
				publicAddr:  q.Get("addr"),
				awsRole:     q.Get("awsrole"),
				path:        q.Get("path"),
				stateToken:  stateToken,
			}
			return h.redirectToLauncher(w, r, urlParams)
		}

		nonce, err := utils.CryptoRandomHex(auth.TokenLenBytes)
		if err != nil {
			h.log.WithError(err).Debugf("Failed to generate and encode random numbers.")
			return trace.AccessDenied("access denied")
		}
		SetRedirectPageHeaders(w.Header(), nonce)
		fmt.Fprintf(w, js, nonce)
		return nil

	case http.MethodPost:
		httplib.SetNoCacheHeaders(w.Header())
		var req fragmentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return trace.Wrap(err)
		}

		// Validate that the caller-provided state token matches the stored state token.
		stateCookie, err := r.Cookie(AuthStateCookieName)
		if err != nil || stateCookie.Value == "" {
			h.log.Warn("Request failed: state cookie is not set.")
			return trace.AccessDenied("access denied")
		}
		if subtle.ConstantTimeCompare([]byte(req.StateValue), []byte(stateCookie.Value)) != 1 {
			h.log.Warn("Request failed: state token does not match.")
			return trace.AccessDenied("access denied")
		}

		// Prevent reuse of the same state token.
		h.setAuthStateCookie(w, "")

		// Validate that the caller is asking for a session that exists and that they have the secret
		// session token for.
		ws, err := h.c.AccessPoint.GetAppSession(r.Context(), types.GetAppSessionRequest{
			SessionID: req.CookieValue,
		})
		if err != nil {
			h.log.Warn("Request failed: session does not exist.")
			return trace.AccessDenied("access denied")
		}
		if err := checkSubjectToken(&req, ws); err != nil {
			h.log.Warnf("Request failed: %v.", err)
			h.c.AuthClient.EmitAuditEvent(h.closeContext, &apievents.AuthAttempt{
				Metadata: apievents.Metadata{
					Type: events.AuthAttemptEvent,
					Code: events.AuthAttemptFailureCode,
				},
				UserMetadata: apievents.UserMetadata{
					Login: ws.GetUser(),
					User:  "unknown",
				},
				ConnectionMetadata: apievents.ConnectionMetadata{
					LocalAddr:  r.Host,
					RemoteAddr: r.RemoteAddr,
				},
				Status: apievents.Status{
					Success: false,
					Error:   err.Error(),
				},
			})
			return trace.AccessDenied("access denied")
		}

		// Set the "Set-Cookie" header on the response.
		// Set Same-Site policy for the session cookies to None in order to
		// support redirects that identity providers do during SSO auth.
		// Otherwise the session cookie won't be sent and the user will
		// get redirected to the application launcher.
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Set-Cookie/SameSite
		http.SetCookie(w, &http.Cookie{
			Name:     CookieName,
			Value:    req.CookieValue,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteNoneMode,
		})
		http.SetCookie(w, &http.Cookie{
			Name:     SubjectCookieName,
			Value:    ws.GetBearerToken(),
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteNoneMode,
		})
		return nil
	default:
		return trace.BadParameter("unsupported method %q", r.Method)
	}
}

// checkSubjectToken checks that the subject cookie value in the fragment request is not empty and matches the session bearer token.
func checkSubjectToken(req *fragmentRequest, ws types.WebSession) error {
	if req.SubjectCookieValue == "" {
		return trace.AccessDenied("subject session token is not set")
	}
	if subtle.ConstantTimeCompare([]byte(req.SubjectCookieValue), []byte(ws.GetBearerToken())) != 1 {
		return trace.AccessDenied("subject session token does not match")
	}
	return nil
}

func (h *Handler) setAuthStateCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     AuthStateCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
		Expires:  h.c.Clock.Now().UTC().Add(1 * time.Minute),
	})
}
