// SPDX-License-Identifier: Apache-2.0

// Package resource serves stateless CRUDL APIs backed by Kubernetes custom resources.
// ResourceHandler validates and converts resource values; ResourceServer persists them.
// Registrations mount HTTP routes for the resource pairs selected by the host.
// The host supplies datastore access, trusted namespace resolution, authentication,
// authorization, and HTTP limits.
package resource

import (
	"context"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// Operation identifies a public operation, independently of datastore verbs.
type Operation string

// Supported public operations.
const (
	OpCreate Operation = "create"
	OpGet    Operation = "get"
	OpList   Operation = "list"
	OpUpdate Operation = "update"
	OpDelete Operation = "delete"
)

// Scope specifies the datastore scope of a resource.
type Scope string

// Supported resource scopes.
const (
	Namespaced    Scope = "Namespaced"
	ClusterScoped Scope = "Cluster"
)

// ResourceHandler validates and converts either side of an external/internal resource pair.
// Paired type parameters use E for external resources and I for internal resources.
// T is the destination type, so the same contract supports conversion in either direction.
type ResourceHandler[T any] interface {
	// Validate contains only public-specific rules; datastore admission owns schema validation.
	Validate() error
	// Convert selects the destination shape and preserves identity and writable spec field names, structure and values.
	// Renaming or transforming writable fields is unsupported.
	Convert() (T, error)
}

// ResourceServer provides HTTP-independent CRUDL over external resources E.
// Update and Delete require the resourceVersion returned as ETag, supplied through WithOptions.
// Direct writes use E's JSON field presence; use optional pointers and omit zero read-only fields.
type ResourceServer[E any] interface {
	Create(context.Context, E) (E, error)
	Update(context.Context, string, E) (E, error)
	Get(context.Context, string) (E, error)
	List(context.Context, ListOptions) (List[E], error)
	Delete(context.Context, string, DeleteOptions) error
}

// ResourceInfo registers datastore identity and enabled operations separately from resource values.
// Empty Verbs enables CRUDL; an empty FieldManager uses the shared "nvke-resource" manager.
type ResourceInfo struct {
	GVK          schema.GroupVersionKind
	Resource     string
	Scope        Scope
	Verbs        []Operation
	HasStatus    bool
	FieldManager string
}

// operations snapshots enabled verbs and supplies the default CRUDL set.
func operations(verbs []Operation) []Operation {
	if len(verbs) == 0 {
		return []Operation{OpCreate, OpGet, OpList, OpUpdate, OpDelete}
	}
	return slices.Clone(verbs)
}

// ClientConfig supplies datastore access and trusted request scope.
// The engine does not implement caller authentication or authorization.
type ClientConfig struct {
	Datastore *rest.Config
	Surface   string
	// Namespace is required only for namespaced resources and resolves scope once per operation.
	// The trusted host supplies it; namespace must not come from untrusted resource input.
	Namespace func(context.Context) (string, error)
}

// RequestOptions carries concurrency information for one operation.
// IfMatch is the required resourceVersion; Create, Get and Update populate ETag on success.
// Allocate a fresh value for each call and do not share it across goroutines.
type RequestOptions struct {
	IfMatch string
	ETag    string
	body    map[string]any
}

type optionsKey struct{}

// WithOptions attaches per-call concurrency options to a context without changing CRUDL signatures.
func WithOptions(ctx context.Context, opts *RequestOptions) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

// requestOptions returns this call's options, or empty options when none were supplied.
func requestOptions(ctx context.Context) *RequestOptions {
	if opts, ok := ctx.Value(optionsKey{}).(*RequestOptions); ok && opts != nil {
		return opts
	}
	return &RequestOptions{}
}

// List contains a bounded datastore snapshot page.
type List[E any] struct {
	Items   []E    `json:"items"`
	PageKey string `json:"page_key"`
	HasMore bool   `json:"has_more"`
}

// ListOptions are the only public list controls.
type ListOptions struct {
	PageSize int
	PageKey  string
}

// DeleteOptions are the only public delete controls.
type DeleteOptions struct{ PropagationPolicy string }

// ServerOptions configures WithLimits; zero values use its default body size and deadline.
type ServerOptions struct {
	MaxBodyBytes   int64
	RequestTimeout time.Duration
}
