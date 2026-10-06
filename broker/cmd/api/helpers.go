package main

import (
	"logwolf-toolbox/json"
	"net/http"
)

type jsonResponse struct {
	Error   bool   `json:"error"`
	Message string `json:"message"`
	// Code tells apart refusals that share a status, for clients to act on;
	// see the code constants. Most errors have none.
	Code string      `json:"code,omitempty"`
	Data interface{} `json:"data,omitempty"`
}

// Codes of the refusals a client can act on without reading the message.
const (
	// codeRateLimited is a 429 for too many requests in a short time: a key
	// past its ingestion rate, or an address past its failed
	// authentications. Retry-After is seconds away.
	codeRateLimited = "rate_limited"
	// codeQuotaExceeded is a 429 for an organization that has used its
	// monthly event quota. Retry-After is when the month is over.
	codeQuotaExceeded = "quota_exceeded"
)

func (app *Config) readJSON(w http.ResponseWriter, r *http.Request, data interface{}) error {
	return json.ReadJSON(w, r, data)
}

func (app *Config) writeJSON(w http.ResponseWriter, status int, data interface{}, headers ...http.Header) error {
	return json.WriteJSON(w, status, data, headers...)
}

func (app *Config) errorJSON(w http.ResponseWriter, err error, status ...int) error {
	return json.ErrorJSON(w, err, status...)
}

// errorCodeJSON is errorJSON with a code in the body.
func (app *Config) errorCodeJSON(w http.ResponseWriter, err error, status int, code string) error {
	return app.writeJSON(w, status, jsonResponse{Error: true, Message: err.Error(), Code: code})
}
