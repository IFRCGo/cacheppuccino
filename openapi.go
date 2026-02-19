package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
)

// Concrete envelope types (so OpenAPI can describe the real JSON)
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
		fmt.Println(err.Error())
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to build openapi spec", nil)
		return
	}

	// IMPORTANT: return raw OpenAPI JSON (no { ok, data } envelope)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(spec)
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

	// Create JSON content with an inlined schema (no component refs)
	jsonContentFor := func(example any) (openapi3.Content, error) {
		ref, err := gen.NewSchemaRefForValue(example, nil)
		if err != nil {
			return nil, err
		}

		// Force inline schema so we don't depend on $ref behavior
		if ref == nil || ref.Value == nil {
			ref = &openapi3.SchemaRef{Value: openapi3.NewObjectSchema()}
		} else {
			ref = &openapi3.SchemaRef{Value: ref.Value}
		}

		return openapi3.Content{
			"application/json": &openapi3.MediaType{
				Schema: ref,
			},
		}, nil
	}

	newResponses := func(items map[string]*openapi3.ResponseRef) *openapi3.Responses {
		rs := openapi3.NewResponses()
		for code, ref := range items {
			rs.Set(code, ref)
		}
		return rs
	}

	spec := &openapi3.T{
		OpenAPI: "3.0.3",
		Info: &openapi3.Info{
			Title:   "cacheppuccino",
			Version: "0.1.0",
		},
		Servers: openapi3.Servers{
			{URL: baseURL},
		},
		Paths: &openapi3.Paths{},
	}

	// ---- /healthz
	health200, err := jsonContentFor(APIResponseHealth{})
	if err != nil {
		return nil, err
	}
	spec.Paths.Set("/healthz", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "Liveness endpoint",
			OperationID: "getHealthz",
			Responses: newResponses(map[string]*openapi3.ResponseRef{
				"200": {Value: &openapi3.Response{
					Description: ptrString("OK"),
					Content:     health200,
				}},
			}),
		},
	})

	// ---- /readyz
	ready200, err := jsonContentFor(APIResponseReady{})
	if err != nil {
		return nil, err
	}
	errResp, err := jsonContentFor(APIResponseError{})
	if err != nil {
		return nil, err
	}
	spec.Paths.Set("/readyz", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "Readiness endpoint",
			OperationID: "getReadyz",
			Responses: newResponses(map[string]*openapi3.ResponseRef{
				"200": {Value: &openapi3.Response{
					Description: ptrString("Ready"),
					Content:     ready200,
				}},
				"503": {Value: &openapi3.Response{
					Description: ptrString("Not ready"),
					Content:     errResp,
				}},
			}),
		},
	})

	// ---- /status
	status200, err := jsonContentFor(APIResponseStatus{})
	if err != nil {
		return nil, err
	}
	spec.Paths.Set("/status", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "Service status",
			OperationID: "getStatus",
			Responses: newResponses(map[string]*openapi3.ResponseRef{
				"200": {Value: &openapi3.Response{
					Description: ptrString("OK"),
					Content:     status200,
				}},
			}),
		},
	})

	// ---- /strings parameters
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

	// ---- /strings
	strings200, err := jsonContentFor(APIResponseStrings{})
	if err != nil {
		return nil, err
	}
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
				"200": {Value: &openapi3.Response{
					Description: ptrString("OK"),
					Content:     strings200,
				}},
				"400": {Value: &openapi3.Response{
					Description: ptrString("Bad request"),
					Content:     errResp,
				}},
				"500": {Value: &openapi3.Response{
					Description: ptrString("Internal error"),
					Content:     errResp,
				}},
			}),
		},
	})

	// ---- /openapi.json
	// We return the OpenAPI document itself (raw), so we just describe it as "object".
	spec.Paths.Set("/openapi.json", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "OpenAPI schema",
			OperationID: "getOpenAPI",
			Responses: newResponses(map[string]*openapi3.ResponseRef{
				"200": {Value: &openapi3.Response{
					Description: ptrString("OK"),
					Content: openapi3.Content{
						"application/json": &openapi3.MediaType{
							Schema: &openapi3.SchemaRef{Value: openapi3.NewObjectSchema()},
						},
					},
				}},
			}),
		},
	})

	if err := spec.Validate(context.Background()); err != nil {
		return nil, err
	}
	return spec, nil
}

func ptrString(v string) *string { return &v }
func ptrBool(v bool) *bool       { return &v }
