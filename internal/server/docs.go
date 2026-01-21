package server

import (
	"net/http"
)

var openapiSpec []byte

// SetOpenAPISpec sets the OpenAPI specification content for serving
func SetOpenAPISpec(spec []byte) {
	openapiSpec = spec
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(swaggerUIHTML))
}

func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(openapiSpec)
}

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Mithlond Agent API</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
    <script>
        window.onload = function() {
            SwaggerUIBundle({
                url: "/openapi.yaml",
                dom_id: '#swagger-ui',
                presets: [
                    SwaggerUIBundle.presets.apis
                ]
            });
        };
    </script>
</body>
</html>`
