// Package api serves the control plane's REST API under /api/v1 (CP-10,
// ADR-0036) on the server that oapi-codegen generates from api/openapi.yaml:
// JSON errors, strict request bodies, ETags and If-Match (CP-14), and cursor
// pages.
//
// Every operation needs a bearer token holding one of the roles the spec
// lists for it, checked in one place before the request is read (CP-11,
// CP-12, SEC-08, ADR-0040), and every change runs in one store transaction
// with its audit record (CP-13).
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"reflect"
	"strconv"
	"strings"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// BasePath is where the API is served.
const BasePath = "/api/v1"

// MaxBody is the largest request body (ADR-0036).
const MaxBody = 1 << 20

// Resolver resolves upstream host names for the SEC-06 check.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Server implements every operation of api/openapi.yaml.
type Server struct {
	store    *store.Store
	allowed  Allowlist
	resolver Resolver
	auth     Authenticator
	log      *slog.Logger
}

// Allowlist is the upstream allow-list of SEC-06, the same networks as the
// gateways' (ADR-0036).
type Allowlist interface {
	Allows(ip netip.Addr) bool
}

var _ apiv1.StrictServerInterface = (*Server)(nil)

// New returns a server over st; upstream endpoints must be in allowed
// (SEC-06), and callers are authenticated by authn (CP-11).
func New(st *store.Store, allowed Allowlist, resolver Resolver, authn Authenticator, log *slog.Logger) *Server {
	return &Server{store: st, allowed: allowed, resolver: resolver, auth: authn, log: log}
}

// Handler returns the API's HTTP handler; it serves BasePath and answers 404
// elsewhere. Each operation's path and method go through guard first
// (ADR-0040); other requests reach only the 404, never the body reader or
// the generated server.
func (s *Server) Handler() http.Handler {
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, &apiError{http.StatusNotFound, apiv1.ErrorCodeNotFound, "no such path", nil})
	})
	mux := http.NewServeMux()
	mux.Handle("/", notFound)
	strict := apiv1.NewStrictHandlerWithOptions(s, []apiv1.StrictMiddlewareFunc{strictBody}, apiv1.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			s.writeError(w, r, invalid("body: "+jsonMessage(err)))
		},
		ResponseErrorHandlerFunc: s.writeError,
	})
	apiv1.HandlerWithOptions(strict, apiv1.StdHTTPServerOptions{BaseURL: BasePath, BaseRouter: mux, ErrorHandlerFunc: s.paramError})
	api := s.readBody(mux)
	guarded := http.NewServeMux()
	guarded.Handle("/", notFound)
	for pattern, op := range operations() {
		guarded.Handle(pattern, s.guard(op, api))
	}
	return requestID(guarded)
}

type requestIDKey struct{}

// requestID propagates or generates X-Request-Id, as the gateway does
// (ADR-0013), and answers with it.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := telemetry.RequestID(r.Header.Values(telemetry.RequestIDHeader))
		w.Header().Set(telemetry.RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func requestIDOf(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type bodyKey struct{}

// readBody reads a request's body, at most MaxBody bytes of JSON, so
// strictBody can decode it again strictly; an import's is at most
// MaxImportBody bytes of YAML.
func (s *Server) readBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength == 0 || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		mediaType, limit := "application/json", int64(MaxBody)
		if r.Method+" "+r.URL.Path == importOperation {
			mediaType, limit = "application/yaml", MaxImportBody
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != mediaType {
			s.writeError(w, r, &apiError{http.StatusUnsupportedMediaType, apiv1.ErrorCodeUnsupportedMediaType, "the body must be " + mediaType, nil})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			s.writeError(w, r, &apiError{http.StatusRequestEntityTooLarge, apiv1.ErrorCodePayloadTooLarge, fmt.Sprintf("the body is over %d bytes", limit), nil})
			return
		}
		if err != nil {
			s.writeError(w, r, invalid("the body could not be read"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyKey{}, body)))
	})
}

// strictBody decodes a request's JSON body again, rejecting unknown fields
// and trailing data, which the generated decoder accepts: a typo is never
// silently dropped (ADR-0036).
func strictBody(next apiv1.StrictHandlerFunc, _ string) apiv1.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		body := reflect.ValueOf(request).FieldByName("Body")
		if body.IsValid() && body.Kind() == reflect.Pointer && !body.IsNil() {
			raw, _ := ctx.Value(bodyKey{}).([]byte)
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(reflect.New(body.Type().Elem()).Interface()); err != nil {
				return nil, invalid("body: " + jsonMessage(err))
			}
			if dec.More() {
				return nil, invalid("body: data after the JSON value")
			}
		}
		return next(ctx, w, r, request)
	}
}

// jsonMessage is a JSON decoding error without Go type names.
func jsonMessage(err error) string {
	if errors.Is(err, io.EOF) {
		return "required"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected EOF"
	}
	if se, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Sprintf("%s at offset %d", se, se.Offset)
	}
	msg := err.Error()
	if f, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
		return "unknown field " + f
	}
	if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return fmt.Sprintf("%s: wrong type, want %s", te.Field, jsonType(te.Type))
	}
	return strings.TrimPrefix(msg, "json: ")
}

func jsonType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int32, reflect.Int64, reflect.Int:
		return "an integer"
	case reflect.Float64, reflect.Float32:
		return "a number"
	case reflect.Slice:
		return "an array"
	}
	return "an object"
}

// apiError is an error answer (ADR-0036).
type apiError struct {
	status  int
	code    apiv1.ErrorCode
	msg     string
	details []apiv1.ErrorDetail
}

func (e *apiError) Error() string { return e.msg }

func invalid(msg string) *apiError {
	return &apiError{http.StatusBadRequest, apiv1.ErrorCodeInvalidRequest, msg, nil}
}

func validation(entity, path, msg string) *apiError {
	d := apiv1.ErrorDetail{Entity: &entity, Message: &msg}
	if path != "" {
		d.Path = &path
	}
	return &apiError{http.StatusUnprocessableEntity, apiv1.ErrorCodeValidationFailed, "validation failed: " + msg, []apiv1.ErrorDetail{d}}
}

// paramError answers a path, query or header parameter the generated code
// could not bind: a missing If-Match is 428, anything else 400.
func (s *Server) paramError(w http.ResponseWriter, r *http.Request, err error) {
	if h, ok := errors.AsType[*apiv1.RequiredHeaderError](err); ok && h.ParamName == "If-Match" {
		s.writeError(w, r, &apiError{http.StatusPreconditionRequired, apiv1.ErrorCodePreconditionRequired, "If-Match is required: send the ETag of the entity's GET", nil})
		return
	}
	s.writeError(w, r, invalid(err.Error()))
}

// writeError answers err as the API's error body. Errors that are not the
// API's or the store's are logged with the request ID and answered 500,
// never echoed.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var e *apiError
	var pre *store.PreconditionError
	var conflict *store.ConflictError
	var inv *store.InvalidError
	switch {
	case errors.As(err, &e):
	case errors.Is(err, store.ErrNotFound):
		e = &apiError{http.StatusNotFound, apiv1.ErrorCodeNotFound, "no such entity", nil}
	case errors.As(err, &pre):
		e = &apiError{http.StatusPreconditionFailed, apiv1.ErrorCodePreconditionFailed,
			fmt.Sprintf("If-Match does not match: the current version is %d", pre.Current), []apiv1.ErrorDetail{{Version: &pre.Current}}}
	case errors.As(err, &conflict):
		e = &apiError{http.StatusConflict, apiv1.ErrorCodeConflict, conflict.Msg, nil}
	case errors.As(err, &inv):
		e = validation("", inv.Path, inv.Msg)
	case store.Unavailable(err):
		s.log.Warn("Postgres unavailable", "request_id", requestIDOf(r.Context()), "err", err)
		e = &apiError{http.StatusServiceUnavailable, apiv1.ErrorCodeUnavailable, "the database is unavailable", nil}
	default:
		s.log.Error("request failed", "request_id", requestIDOf(r.Context()), "method", r.Method, "path", r.URL.Path, "err", err)
		e = &apiError{http.StatusInternalServerError, apiv1.ErrorCodeInternal, "internal error", nil}
	}
	body := apiv1.Error{Error: e.msg, Code: e.code, RequestId: requestIDOf(r.Context())}
	if len(e.details) > 0 {
		body.Details = &e.details
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(body)
}

// Pages (UI-06, ADR-0036).

const (
	defaultLimit = 50
	maxLimit     = 500
)

// page reads a list's limit and cursor; the cursor is base64url of the last
// row's ID.
func page(limit *int32, cursor *string) (store.Page, error) {
	p := store.Page{Limit: defaultLimit}
	if limit != nil {
		if *limit < 1 || *limit > maxLimit {
			return p, invalid(fmt.Sprintf("limit: want 1 to %d", maxLimit))
		}
		p.Limit = *limit
	}
	if cursor != nil {
		b, err := base64.RawURLEncoding.DecodeString(*cursor)
		if err == nil {
			p.After, err = strconv.ParseInt(string(b), 10, 64)
		}
		if err != nil || p.After < 0 {
			return p, invalid("cursor: not a cursor this API gave")
		}
	}
	return p, nil
}

// pageOf cuts rows, which the store fetched one past the page, to the page,
// and returns the next page's cursor, nil on the last page.
func pageOf[T any](rows []T, p store.Page, id func(T) int64) ([]T, *string) {
	if len(rows) <= int(p.Limit) {
		return rows, nil
	}
	rows = rows[:p.Limit]
	c := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id(rows[len(rows)-1]), 10)))
	return rows, &c
}

// ETags (CP-14).

func etag(version int64) *string {
	t := `"` + strconv.FormatInt(version, 10) + `"`
	return &t
}

// ifMatch returns the version an If-Match names, or -1, which no entity has,
// when it is not a strong ETag of this API.
func ifMatch(h string) int64 {
	v, ok := strings.CutPrefix(h, `"`)
	if v, ok2 := strings.CutSuffix(v, `"`); ok && ok2 {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return -1
}

func location(collection string, id int64) *string {
	l := BasePath + "/" + collection + "/" + strconv.FormatInt(id, 10)
	return &l
}
