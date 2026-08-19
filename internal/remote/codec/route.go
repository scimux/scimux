package codec

import (
	"net/http"
	"strings"
)

func requestBodyLimit(method, path string) int64 {
	if method == http.MethodPost && strings.HasSuffix(path, "/attachments") {
		return AttachUploadMax
	}
	if method == http.MethodPut && path == "/api/ui" {
		return UIStateMax
	}
	return JSONBodyMax
}

func responseBodyLimit(method, path string) int64 {
	if strings.Contains(path, "/assets/") {
		return AgentAssetMax
	}
	if strings.Contains(path, "/attachments/") {
		return AttachUploadMax
	}
	return JSONBodyMax
}
