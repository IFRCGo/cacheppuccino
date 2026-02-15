package main

import (
	"context"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
)

// Materialized envelope types for OpenAPI generation.
// (Generics don't map nicely to OpenAPI, so we document concrete shapes.)
type APIResponseHealth struct {
	Ok   bool            `json:"ok"`
	Data *HealthResponse `json:"data,omitempty"`
}

type APIResponseReady struct {
	Ok   bool           `json:"ok"`
	Data *ReadyResponse `json:"data,omitempty"`
}

type APIResponseStatus struct {
	Ok   bool            `json:"ok"`
	Data *StatusResponse `json:"data,omitempty"`
}

type APIResponseStrings struct {
	Ok   bool             `json:"ok"`
	Data *StringsResponse `json:"data,omitempty"`
}

type APIResponseError struct {
	Ok    bool      `json:"ok"`
	Error *APIError `json:"error,omitempty"`
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	baseURL := inferBaseURL(r)
	spec, err := buildOpenAPISpec(baseURL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to build openapi spec", nil)
		return
	}

	// Keep your consistent envelope:
	// { "ok": true, "data": <openapi spec object> }
	writeOK(w, http.StatusOK, *spec)
}

func inferBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	host := r.Host
	if host == "" {
		host = "localhost:8080"
	}

	return scheme + "://" + host
}

func buildOpenAPISpec(baseURL string) (*openapi3.T, error) {
	gen := openapi3gen.NewGenerator()

	schemas := openapi3.Schemas{}

	addSchema := func(name string, exampleValue any) error {
		ref, err := gen.NewSchemaRefForValue(exampleValue, nil)
		if err != nil {
			return err
		}
		schemas[name] = ref
		return nil
	}

	// Core schemas
	if err := addSchema("APIError", APIError{}); err != nil {
		return nil, err
	}
	if err := addSchema("HealthResponse", HealthResponse{}); err != nil {
		return nil, err
	}
	if err := addSchema("ReadyResponse", ReadyResponse{}); err != nil {
		return nil, err
	}
	if err := addSchema("StatusResponse", StatusResponse{}); err != nil {
		return nil, err
	}
	if err := addSchema("StringsResponse", StringsResponse{}); err != nil {
		return nil, err
	}

	// Envelope schemas
	if err := addSchema("APIResponse_HealthResponse", APIResponseHealth{}); err != nil {
		return nil, err
	}
	if err := addSchema("APIResponse_ReadyResponse", APIResponseReady{}); err != nil {
		return nil, err
	}
	if err := addSchema("APIResponse_StatusResponse", APIResponseStatus{}); err != nil {
		return nil, err
	}
	if err := addSchema("APIResponse_StringsResponse", APIResponseStrings{}); err != nil {
		return nil, err
	}
	if err := addSchema("APIResponse_Error", APIResponseError{}); err != nil {
		return nil, err
	}

	// OpenAPI endpoint response envelope schema:
	// We intentionally document `data` as a generic object to avoid massive/cyclic schemas.
	schemas["APIResponse_OpenAPI"] = &openapi3.SchemaRef{
		Value: openapi3.NewObjectSchema().
			WithProperty("ok", openapi3.NewBoolSchema()).
			WithProperty("data", openapi3.NewObjectSchema()).
			WithRequired([]string{"ok", "data"}),
	}

	spec := &openapi3.T{
		OpenAPI: "3.0.3",
		Info: &openapi3.Info{
			Title:   "cacheppuccino",
			Version: "0.1.0",
		},
		Servers: openapi3.Servers{
			&openapi3.Server{URL: baseURL},
		},
		Components: &openapi3.Components{
			Schemas: schemas,
		},
		Paths: &openapi3.Paths{},
	}

	jsonContent := func(schemaName string) openapi3.Content {
		return openapi3.Content{
			"application/json": &openapi3.MediaType{
				Schema: &openapi3.SchemaRef{Ref: "#/components/schemas/" + schemaName},
			},
		}
	}

	// Helper for pointer-based Responses field (newer kin-openapi versions).
	newResponses := func(items map[string]*openapi3.ResponseRef) *openapi3.Responses {
		rs := openapi3.NewResponses()
		for code, ref := range items {
			rs.Set(code, ref)
		}
		return rs
	}

	// /healthz
	spec.Paths.Set("/healthz", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "Liveness endpoint",
			OperationID: "getHealthz",
			Responses: newResponses(map[string]*openapi3.ResponseRef{
				"200": {
					Value: &openapi3.Response{
						Description: ptrString("OK"),
						Content:     jsonContent("APIResponse_HealthResponse"),
					},
				},
			}),
		},
	})

	// /readyz
	spec.Paths.Set("/readyz", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "Readiness endpoint",
			OperationID: "getReadyz",
			Responses: newResponses(map[string]*openapi3.ResponseRef{
				"200": {
					Value: &openapi3.Response{
						Description: ptrString("Ready"),
						Content:     jsonContent("APIResponse_ReadyResponse"),
					},
				},
				"503": {
					Value: &openapi3.Response{
						Description: ptrString("Not ready"),
						Content:     jsonContent("APIResponse_Error"),
					},
				},
			}),
		},
	})

	// /status
	spec.Paths.Set("/status", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "Service status",
			OperationID: "getStatus",
			Responses: newResponses(map[string]*openapi3.ResponseRef{
				"200": {
					Value: &openapi3.Response{
						Description: ptrString("OK"),
						Content:     jsonContent("APIResponse_StatusResponse"),
					},
				},
			}),
		},
	})

	// /strings parameters
	paramLang := &openapi3.ParameterRef{
		Value: &openapi3.Parameter{
			Name:        "lang",
			In:          "query",
			Required:    true,
			Description: "Language code (e.g. en, fr, es).",
			Schema:      &openapi3.SchemaRef{Value: openapi3.NewStringSchema()},
			Example:     "en",
		},
	}

	paramPage := &openapi3.ParameterRef{
		Value: &openapi3.Parameter{
			Name:        "page",
			In:          "query",
			Required:    false,
			Description: "Repeatable page param. Example: /strings?page=home&page=about&lang=en",
			Schema:      &openapi3.SchemaRef{Value: openapi3.NewStringSchema()},
			Style:       "form",
			Explode:     ptrBool(true),
			Example:     "home",
		},
	}

	paramPages := &openapi3.ParameterRef{
		Value: &openapi3.Parameter{
			Name:        "pages",
			In:          "query",
			Required:    false,
			Description: "Comma-separated pages. Example: /strings?pages=home,about&lang=en",
			Schema:      &openapi3.SchemaRef{Value: openapi3.NewStringSchema()},
			Example:     "home,about",
		},
	}

	// /strings
	spec.Paths.Set("/strings", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "Get strings for one or more pages in a given language",
			OperationID: "getStrings",
			Parameters: openapi3.Parameters{
				paramLang,
				paramPage,
				paramPages,
			},
			Responses: newResponses(map[string]*openapi3.ResponseRef{
				"200": {
					Value: &openapi3.Response{
						Description: ptrString("OK"),
						Content:     jsonContent("APIResponse_StringsResponse"),
					},
				},
				"400": {
					Value: &openapi3.Response{
						Description: ptrString("Bad request"),
						Content:     jsonContent("APIResponse_Error"),
					},
				},
				"500": {
					Value: &openapi3.Response{
						Description: ptrString("Internal error"),
						Content:     jsonContent("APIResponse_Error"),
					},
				},
			}),
		},
	})

	// /openapi.json
	spec.Paths.Set("/openapi.json", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "OpenAPI schema",
			OperationID: "getOpenAPI",
			Responses: newResponses(map[string]*openapi3.ResponseRef{
				"200": {
					Value: &openapi3.Response{
						Description: ptrString("OK"),
						Content:     jsonContent("APIResponse_OpenAPI"),
					},
				},
			}),
		},
	})

	if err := spec.Validate(context.Background()); err != nil {
		return nil, err
	}

	return spec, nil
}

func ptrString(v string) *string {
	return &v
}

func ptrBool(v bool) *bool {
	return &v
}
