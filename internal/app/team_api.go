package app

import (
	"net/http"
	"time"

	"github.com/scimux/scimux/internal/team"
)

// newTeamHandler is the /api/team surface. The stub answers with the
// ordinary not-found page so the route tests fail on assertions.
func newTeamHandler(dataDir string, now func() time.Time, client *team.Client) http.Handler {
	return http.NotFoundHandler()
}

// newTeamClient builds the client Run installs. Tests replace it.
var newTeamClient = func() *team.Client { return team.NewClient() }
