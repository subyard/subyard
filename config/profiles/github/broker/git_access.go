package githubbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitAccess deliberately leaves /token and its installation-wide scope unchanged.
type GitAccess struct {
	Version int    `json:"version"`
	State   string `json:"state"`
	Token   *Token `json:"token,omitempty"`
}

type gitAccessError struct{ state string }

func (e *gitAccessError) Error() string { return "GitHub Git access: " + e.state }

func permissionState(grants map[string]string, operation string) string {
	if grants == nil {
		return "unknown"
	}
	contents := grants["contents"]
	if operation == "read" && (contents == "read" || contents == "write") {
		return "available"
	}
	if operation == "push" && contents == "write" && grants["workflows"] == "write" {
		return "available"
	}
	return "insufficient-permissions"
}

func (handler Handler) gitAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", 405)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, requestLimit+1))
	var selection struct {
		Repository string `json:"repository"`
		Operation  string `json:"operation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err != nil || len(raw) > requestLimit || decoder.Decode(&selection) != nil || decoder.Decode(new(any)) != io.EOF || !validRepository(selection.Repository) || (selection.Operation != "read" && selection.Operation != "push") {
		http.Error(w, "invalid GitHub Git access request", http.StatusBadRequest)
		return
	}
	access := GitAccess{Version: 1, State: "unavailable"}
	if handler.Issuer != nil {
		token, issueErr := handler.Issuer.issueRepository(r.Context(), selection.Repository, selection.Operation)
		switch {
		case issueErr == nil:
			access.State = "available"
			access.Token = &token
		case errors.Is(issueErr, errRepositoryUnavailable):
			access.State = "no-repo-access"
		default:
			var denied *gitAccessError
			if errors.As(issueErr, &denied) {
				access.State = denied.state
			}
		}
	}
	writeJSON(w, http.StatusOK, access)
}

func (client Client) gitAccess(ctx context.Context, repository, operation string) GitAccess {
	raw, _ := json.Marshal(map[string]string{"repository": repository, "operation": operation})
	body, _, err := client.request(ctx, http.MethodPost, "/git-access/v1", strings.NewReader(string(raw)))
	var access GitAccess
	if err != nil || json.Unmarshal(body, &access) != nil || access.Version != 1 {
		return GitAccess{Version: 1, State: "unavailable"}
	}
	if access.State == "available" && (access.Token == nil || len(access.Token.Value) > 4096 || !tokenPattern.MatchString(access.Token.Value) || !access.Token.ExpiresAt.After(time.Now())) {
		return GitAccess{Version: 1, State: "unknown"}
	}
	return access
}
