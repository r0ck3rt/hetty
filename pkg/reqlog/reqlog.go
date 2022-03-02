package reqlog

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"time"

	"github.com/oklog/ulid"

	"github.com/dstotijn/hetty/pkg/log"
	"github.com/dstotijn/hetty/pkg/proxy"
	"github.com/dstotijn/hetty/pkg/scope"
	"github.com/dstotijn/hetty/pkg/search"
)

type contextKey int

const LogBypassedKey contextKey = 0

var (
	ErrRequestNotFound    = errors.New("reqlog: request not found")
	ErrProjectIDMustBeSet = errors.New("reqlog: project ID must be set")
)

//nolint:gosec
var ulidEntropy = rand.New(rand.NewSource(time.Now().UnixNano()))

type RequestLog struct {
	ID        ulid.ULID
	ProjectID ulid.ULID

	URL    *url.URL
	Method string
	Proto  string
	Header http.Header
	Body   []byte

	Response *ResponseLog
}

type ResponseLog struct {
	Proto      string
	StatusCode int
	Status     string
	Header     http.Header
	Body       []byte
}

type Service interface {
	FindRequests(ctx context.Context) ([]RequestLog, error)
	FindRequestLogByID(ctx context.Context, id ulid.ULID) (RequestLog, error)
	ClearRequests(ctx context.Context, projectID ulid.ULID) error
	RequestModifier(next proxy.RequestModifyFunc) proxy.RequestModifyFunc
	ResponseModifier(next proxy.ResponseModifyFunc) proxy.ResponseModifyFunc
	SetActiveProjectID(id ulid.ULID)
	ActiveProjectID() ulid.ULID
	SetBypassOutOfScopeRequests(bool)
	BypassOutOfScopeRequests() bool
	SetFindReqsFilter(filter FindRequestsFilter)
	FindReqsFilter() FindRequestsFilter
}

type service struct {
	bypassOutOfScopeRequests bool
	findReqsFilter           FindRequestsFilter
	activeProjectID          ulid.ULID
	scope                    *scope.Scope
	repo                     Repository
	logger                   log.Logger
}

type FindRequestsFilter struct {
	ProjectID   ulid.ULID
	OnlyInScope bool
	SearchExpr  search.Expression
}

type Config struct {
	Scope      *scope.Scope
	Repository Repository
	Logger     log.Logger
}

func NewService(cfg Config) Service {
	s := &service{
		repo:   cfg.Repository,
		scope:  cfg.Scope,
		logger: cfg.Logger,
	}

	if s.logger == nil {
		s.logger = log.NewNopLogger()
	}

	return s
}

func (svc *service) FindRequests(ctx context.Context) ([]RequestLog, error) {
	return svc.repo.FindRequestLogs(ctx, svc.findReqsFilter, svc.scope)
}

func (svc *service) FindRequestLogByID(ctx context.Context, id ulid.ULID) (RequestLog, error) {
	return svc.repo.FindRequestLogByID(ctx, id)
}

func (svc *service) ClearRequests(ctx context.Context, projectID ulid.ULID) error {
	return svc.repo.ClearRequestLogs(ctx, projectID)
}

func (svc *service) storeResponse(ctx context.Context, reqLogID ulid.ULID, res *http.Response) error {
	resLog, err := ParseHTTPResponse(res)
	if err != nil {
		return err
	}

	return svc.repo.StoreResponseLog(ctx, reqLogID, resLog)
}

func (svc *service) RequestModifier(next proxy.RequestModifyFunc) proxy.RequestModifyFunc {
	return func(req *http.Request) {
		next(req)

		clone := req.Clone(req.Context())

		var body []byte

		if req.Body != nil {
			// TODO: Use io.LimitReader.
			var err error

			body, err = ioutil.ReadAll(req.Body)
			if err != nil {
				svc.logger.Errorw("Failed to read request body for logging.",
					"error", err)
				return
			}

			req.Body = ioutil.NopCloser(bytes.NewBuffer(body))
			clone.Body = ioutil.NopCloser(bytes.NewBuffer(body))
		}

		// Bypass logging if no project is active.
		if svc.activeProjectID.Compare(ulid.ULID{}) == 0 {
			ctx := context.WithValue(req.Context(), LogBypassedKey, true)
			*req = *req.WithContext(ctx)

			svc.logger.Debugw("Bypassed logging: no active project.",
				"url", req.URL.String())

			return
		}

		// Bypass logging if this setting is enabled and the incoming request
		// doesn't match any scope rules.
		if svc.bypassOutOfScopeRequests && !svc.scope.Match(clone, body) {
			ctx := context.WithValue(req.Context(), LogBypassedKey, true)
			*req = *req.WithContext(ctx)

			svc.logger.Debugw("Bypassed logging: request doesn't match any scope rules.",
				"url", req.URL.String())

			return
		}

		reqLog := RequestLog{
			ID:        ulid.MustNew(ulid.Timestamp(time.Now()), ulidEntropy),
			ProjectID: svc.activeProjectID,
			Method:    clone.Method,
			URL:       clone.URL,
			Proto:     clone.Proto,
			Header:    clone.Header,
			Body:      body,
		}

		err := svc.repo.StoreRequestLog(req.Context(), reqLog)
		if err != nil {
			svc.logger.Errorw("Failed to store request log.",
				"error", err)
			return
		}

		svc.logger.Debugw("Stored request log.",
			"reqLogID", reqLog.ID.String(),
			"url", reqLog.URL.String())

		ctx := context.WithValue(req.Context(), proxy.ReqLogIDKey, reqLog.ID)
		*req = *req.WithContext(ctx)
	}
}

func (svc *service) ResponseModifier(next proxy.ResponseModifyFunc) proxy.ResponseModifyFunc {
	return func(res *http.Response) error {
		if err := next(res); err != nil {
			return err
		}

		if bypassed, _ := res.Request.Context().Value(LogBypassedKey).(bool); bypassed {
			return nil
		}

		reqLogID, ok := res.Request.Context().Value(proxy.ReqLogIDKey).(ulid.ULID)
		if !ok {
			return errors.New("reqlog: request is missing ID")
		}

		clone := *res

		// TODO: Use io.LimitReader.
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return fmt.Errorf("reqlog: could not read response body: %w", err)
		}

		res.Body = ioutil.NopCloser(bytes.NewBuffer(body))
		clone.Body = ioutil.NopCloser(bytes.NewBuffer(body))

		go func() {
			if err := svc.storeResponse(context.Background(), reqLogID, &clone); err != nil {
				svc.logger.Errorw("Failed to store response log.",
					"error", err)
			} else {
				svc.logger.Debugw("Stored response log.",
					"reqLogID", reqLogID.String())
			}
		}()

		return nil
	}
}

func (svc *service) SetActiveProjectID(id ulid.ULID) {
	svc.activeProjectID = id
}

func (svc *service) ActiveProjectID() ulid.ULID {
	return svc.activeProjectID
}

func (svc *service) SetFindReqsFilter(filter FindRequestsFilter) {
	svc.findReqsFilter = filter
}

func (svc *service) FindReqsFilter() FindRequestsFilter {
	return svc.findReqsFilter
}

func (svc *service) SetBypassOutOfScopeRequests(bypass bool) {
	svc.bypassOutOfScopeRequests = bypass
}

func (svc *service) BypassOutOfScopeRequests() bool {
	return svc.bypassOutOfScopeRequests
}

func ParseHTTPResponse(res *http.Response) (ResponseLog, error) {
	if res.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(res.Body)
		if err != nil {
			return ResponseLog{}, fmt.Errorf("reqlog: could not create gzip reader: %w", err)
		}
		defer gzipReader.Close()

		buf := &bytes.Buffer{}

		//nolint:gosec
		if _, err := io.Copy(buf, gzipReader); err != nil {
			return ResponseLog{}, fmt.Errorf("reqlog: could not read gzipped response body: %w", err)
		}

		res.Body = io.NopCloser(buf)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return ResponseLog{}, fmt.Errorf("reqlog: could not read body: %w", err)
	}

	return ResponseLog{
		Proto:      res.Proto,
		StatusCode: res.StatusCode,
		Status:     res.Status,
		Header:     res.Header,
		Body:       body,
	}, nil
}
