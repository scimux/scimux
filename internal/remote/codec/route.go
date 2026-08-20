package codec

import (
	"net/http"
	"strings"
)

func requestBodyLimit(method, path string) int64 {
	// POST /api/nodes/{id}/attachments — router.go handleUploadAttachments
	if method == http.MethodPost && strings.HasSuffix(path, "/attachments") {
		return AttachUploadMax
	}
	// PUT /api/ui — router.go handleUIPut
	if method == http.MethodPut && path == "/api/ui" {
		return UIStateMax
	}
	// decodeJSON / every other request body
	return JSONBodyMax
}

func responseBodyLimit(_, path string) int64 {
	// GET /api/nodes/{id}/assets/{assetID} — handleAsset, agentAssetMaxBytes.
	// Contains("/assets/") also matches static GET /assets/; those files
	// are small and 50 MiB is only a ceiling.
	if strings.Contains(path, "/assets/") {
		return AgentAssetMax
	}
	// GET /api/nodes/{id}/attachments/{name} — handleAttachment, attachUploadMax
	if strings.Contains(path, "/attachments/") {
		return AttachUploadMax
	}
	// Everything else, including GET /api/state: 50 MiB anti-DoS backstop.
	// JSONBodyMax is a request cap; there is no 1 MiB response cap in scimux.
	return AgentAssetMax
}
