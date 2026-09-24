package gateway

import (
	"net/http"
	"strings"
)

func newAuthorizedRequest(token string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "/admin/ban", strings.NewReader("{}"))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}
