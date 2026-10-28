// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	strictjson "sigs.k8s.io/json"
)

// Mount registers supported operations and JSON 405 fallbacks; wrap the completed mux with WithLimits.
// Omitting verbs enables CRUDL. The server also enforces its registered operations.
func Mount[E any](mux *http.ServeMux, path string, server ResourceServer[E], verbs ...Operation) {
	verbs = operations(verbs)
	var collection, item []string
	for _, route := range []struct {
		op      Operation
		pattern string
	}{
		{OpCreate, "POST " + path},
		{OpList, "GET " + path},
		{OpGet, "GET " + path + "/{name}"},
		{OpUpdate, "PUT " + path + "/{name}"},
		{OpDelete, "DELETE " + path + "/{name}"},
	} {
		if slices.Contains(verbs, route.op) {
			mux.HandleFunc(route.pattern, func(w http.ResponseWriter, r *http.Request) {
				opts := &RequestOptions{}
				value, status, err := serveHTTP(server, route.op, r.WithContext(WithOptions(r.Context(), opts)))
				switch {
				case err != nil:
					WriteError(w, err)
				case value == nil:
					w.WriteHeader(status)
				default:
					if opts.ETag != "" {
						w.Header().Set("ETag", strconv.Quote(opts.ETag))
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(value)
				}
			})
			method, _, _ := strings.Cut(route.pattern, " ")
			if route.op == OpCreate || route.op == OpList {
				collection = append(collection, method)
			} else {
				item = append(item, method)
			}
		}
	}
	for _, fallback := range []struct {
		path  string
		verbs []string
	}{{path, collection}, {path + "/{name}", item}} {
		mux.HandleFunc(fallback.path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", strings.Join(fallback.verbs, ", "))
			WriteError(w, problem(405, "method_not_allowed", "The operation is not supported."))
		})
	}
}

// WithLimits applies request deadlines, a body size limit, and no-store responses to a complete surface.
// Zero options use a 1 MiB body limit and a 30-second deadline; longer deadlines are capped at 30 seconds.
// Cancellation is cooperative and does not emit a timeout response; supported writers also bound body reads.
// The hosting http.Server still needs transport timeouts for work outside this handler, such as reading headers.
func WithLimits(next http.Handler, opts ServerOptions) http.Handler {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	if opts.RequestTimeout <= 0 || opts.RequestTimeout > requestTimeout {
		opts.RequestTimeout = requestTimeout
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), opts.RequestTimeout)
		defer cancel()
		deadline, _ := ctx.Deadline()
		// Standard HTTP transports bound body reads too. Wrappers must expose
		// Unwrap; unsupported test/custom writers retain context-only deadlines.
		err := http.NewResponseController(w).SetReadDeadline(deadline)
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			WriteError(w, problem(500, "deadline_failed", "The request deadline could not be established."))
			return
		}
		// net/http resets this for the next request. Do not unbound an unread
		// timed-out body while the server is finishing the current request.
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, opts.MaxBodyBytes)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WriteError writes public Error diagnostics and replaces unrecognized errors with a generic 500 response.
func WriteError(w http.ResponseWriter, err error) {
	var public *Error
	if !errors.As(err, &public) {
		public = problem(500, "internal_error", "The request could not be completed.")
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(public.Status)
	_ = json.NewEncoder(w).Encode(public)
}

// decode checks the media type and maps strict JSON, size, and read failures to public errors.
func decode[E any](r *http.Request) (E, error) {
	var value E
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return value, problem(415, "unsupported_media_type", "Use application/json.")
	}
	body, err := decodeStrict(r.Body, &value)
	if err != nil {
		if timeout, ok := errors.AsType[net.Error](err); ok && timeout.Timeout() {
			r.Close = true
			return value, problem(408, "request_timeout", "The request body deadline expired.")
		}
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return value, problem(413, "body_too_large", "The request body exceeds the limit.")
		}
		return value, Invalid(FieldError{
			Code: "invalid_json", Message: "Use one JSON object with documented fields and unique keys.",
		})
	}
	requestOptions(r.Context()).body = body
	return value, nil
}

// decodeStrict accepts one object with known fields and unique keys, preserving original field presence.
// The caller must bound the reader before this function buffers its contents.
func decodeStrict(reader io.Reader, dest any) (map[string]any, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading JSON: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return nil, fmt.Errorf("JSON object required")
	}
	var body map[string]any
	for _, target := range []any{&body, dest} {
		strictErrors, decodeErr := strictjson.UnmarshalStrict(data, target)
		if decodeErr != nil || len(strictErrors) != 0 {
			return nil, fmt.Errorf("invalid, unknown or duplicate JSON fields")
		}
	}
	return body, nil
}

// query permits only the named parameters, each appearing once.
func query(r *http.Request, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, Invalid()
	}
	for key, entries := range values {
		if !slices.Contains(allowed, key) || len(entries) != 1 {
			return nil, Invalid(FieldError{
				Code: "invalid_query", Message: "The query contains an unsupported or repeated parameter.",
			})
		}
	}
	return values, nil
}

// versionHeader extracts one quoted If-Match resourceVersion; the client enforces its presence and validity.
func versionHeader(r *http.Request) (string, error) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || len(values[0]) < 3 || values[0][0] != '"' || values[0][len(values[0])-1] != '"' {
		return "", Invalid(FieldError{Code: invalidPreconditionCode, Message: "Use one quoted resource version."})
	}
	value := values[0][1 : len(values[0])-1]
	if strings.ContainsAny(value, "\" ,*") {
		return "", Invalid(FieldError{Code: invalidPreconditionCode, Message: "Use one quoted resource version."})
	}
	return value, nil
}

// noBody rejects any body content without reading beyond its first byte.
func noBody(r *http.Request) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(data) != 0 {
		return Invalid(FieldError{Code: "unexpected_body", Message: "This operation does not accept a request body."})
	}
	return nil
}

// serveHTTP parses the transport contract once and dispatches to the typed CRUDL interface.
func serveHTTP[E any](server ResourceServer[E], op Operation, r *http.Request) (any, int, error) {
	var allowed []string
	switch op {
	case OpList:
		allowed = []string{"page_size", pageKeyParameter}
	case OpDelete:
		allowed = []string{"propagation_policy"}
	}
	values, err := query(r, allowed...)
	if err != nil {
		return nil, 0, err
	}
	if op != OpCreate && op != OpUpdate {
		if err = noBody(r); err != nil {
			return nil, 0, err
		}
	}
	if op == OpUpdate || op == OpDelete {
		requestOptions(r.Context()).IfMatch, err = versionHeader(r)
		if err != nil {
			return nil, 0, err
		}
	}
	switch op {
	case OpCreate, OpUpdate:
		in, err := decode[E](r)
		if err != nil {
			return nil, 0, err
		}
		if op == OpCreate {
			value, err := server.Create(r.Context(), in)
			return value, http.StatusCreated, err
		}
		value, err := server.Update(r.Context(), r.PathValue("name"), in)
		return value, http.StatusOK, err
	case OpGet:
		value, err := server.Get(r.Context(), r.PathValue("name"))
		return value, http.StatusOK, err
	case OpList:
		opts := ListOptions{PageKey: values.Get(pageKeyParameter)}
		if values.Has("page_size") {
			opts.PageSize, err = strconv.Atoi(values.Get("page_size"))
			if err != nil || opts.PageSize == 0 {
				return nil, 0, Invalid()
			}
		}
		value, err := server.List(r.Context(), opts)
		return value, http.StatusOK, err
	default:
		err = server.Delete(r.Context(), r.PathValue("name"), DeleteOptions{
			PropagationPolicy: values.Get("propagation_policy"),
		})
		return nil, http.StatusAccepted, err
	}
}
