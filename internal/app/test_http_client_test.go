package app

import (
	"net/http"
	"time"
)

// Local acceptance endpoints must answer promptly. A per-request bound keeps
// a broken handler from outliving the outer polling deadline and hiding the
// useful assertion behind the package timeout.
var (
	testHTTPClient     = &http.Client{Timeout: 10 * time.Second}
	testPollHTTPClient = &http.Client{Timeout: 2 * time.Second}
)
