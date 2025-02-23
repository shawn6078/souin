package souin

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/darkweak/souin/api"
	"github.com/darkweak/souin/cache/coalescing"
	"github.com/darkweak/souin/configurationtypes"
	"github.com/darkweak/souin/plugins"
	"github.com/darkweak/souin/rfc"
)

const (
	getterContextCtxKey key = "getter_context"
)

var (
	DefaultConfiguration = plugins.BaseConfiguration{
		DefaultCache: &configurationtypes.DefaultCache{
			TTL: configurationtypes.Duration{
				Duration: 10 * time.Second,
			},
		},
		LogLevel: "info",
	}
	DevDefaultConfiguration = plugins.BaseConfiguration{
		API: configurationtypes.API{
			BasePath: "/souin-api",
			Prometheus: configurationtypes.APIEndpoint{
				Enable: true,
			},
			Souin: configurationtypes.APIEndpoint{
				Enable: true,
			},
		},
		DefaultCache: &configurationtypes.DefaultCache{
			Regex: configurationtypes.Regex{
				Exclude: "/excluded",
			},
			TTL: configurationtypes.Duration{
				Duration: 5 * time.Second,
			},
		},
		LogLevel: "debug",
	}
)

// SouinGoZeroMiddleware declaration.
type (
	key                   string
	SouinGoZeroMiddleware struct {
		plugins.SouinBasePlugin
		Configuration *plugins.BaseConfiguration
		bufPool       *sync.Pool
	}
	getterContext struct {
		next http.HandlerFunc
		rw   http.ResponseWriter
		req  *http.Request
	}
)

func NewHTTPCache(c plugins.BaseConfiguration) *SouinGoZeroMiddleware {
	s := SouinGoZeroMiddleware{}
	s.Configuration = &c
	s.bufPool = &sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}

	s.Retriever = plugins.DefaultSouinPluginInitializerFromConfiguration(&c)
	s.RequestCoalescing = coalescing.Initialize()
	s.MapHandler = api.GenerateHandlerMap(s.Configuration, s.Retriever.GetTransport())

	return &s
}

func (s *SouinGoZeroMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		req := s.Retriever.GetContext().Method.SetContext(r)
		if b, handler := s.HandleInternally(req); b {
			handler(rw, req)

			return
		}

		if !plugins.CanHandle(req, s.Retriever) {
			rw.Header().Add("Cache-Status", "Souin; fwd=uri-miss")
			next(rw, r)

			return
		}

		customWriter := &plugins.CustomWriter{
			Response: &http.Response{},
			Buf:      s.bufPool.Get().(*bytes.Buffer),
			Rw:       rw,
		}
		req = s.Retriever.GetContext().SetContext(req)
		getterCtx := getterContext{next, customWriter, req}
		ctx := context.WithValue(req.Context(), getterContextCtxKey, getterCtx)
		req = req.WithContext(ctx)
		if plugins.HasMutation(req, rw) {
			next(rw, r)

			return
		}
		req.Header.Set("Date", time.Now().UTC().Format(time.RFC1123))
		combo := ctx.Value(getterContextCtxKey).(getterContext)

		_ = plugins.DefaultSouinPluginCallback(customWriter, req, s.Retriever, nil, func(_ http.ResponseWriter, _ *http.Request) error {
			var e error
			combo.next(customWriter, r)

			combo.req.Response = customWriter.Response
			if combo.req.Response, e = s.Retriever.GetTransport().(*rfc.VaryTransport).UpdateCacheEventually(combo.req); e != nil {
				return e
			}

			_, _ = customWriter.Send()
			return e
		})
	}
}
