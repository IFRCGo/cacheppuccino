package main

import (
	"encoding/json"
	"net/http"
)

type APIError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

type APIResponse[T any] struct {
	Ok    bool      `json:"ok"`
	Data  *T        `json:"data,omitempty"`
	Error *APIError `json:"error,omitempty"`
}

func writeOK[T any](w http.ResponseWriter, status int, data T) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIResponse[T]{Ok: true, Data: &data})
}

func writeErr(w http.ResponseWriter, status int, code, message string, details map[string]string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIResponse[struct{}]{
		Ok: false,
		Error: &APIError{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
}
