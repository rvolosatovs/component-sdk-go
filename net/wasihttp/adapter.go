package wasihttp

import (
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/bytecodealliance/wasm-tools-go/cm"
	"go.wasmcloud.dev/component/gen/wasi/http/types"
	"go.wasmcloud.dev/component/gen/wasi/io/streams"
)

var _ http.ResponseWriter = (*responseOutparamWriter)(nil)

type IncomingRequest = types.IncomingRequest

type responseOutparamWriter struct {
	outparam    types.ResponseOutparam
	response    types.OutgoingResponse
	wasiHeaders types.Fields
	httpHeaders http.Header
	body        *types.OutgoingBody
	stream      *streams.OutputStream

	headerOnce sync.Once
	headerErr  error

	statuscode int
}

func (row *responseOutparamWriter) Header() http.Header {
	return row.httpHeaders
}

func (row *responseOutparamWriter) Write(buf []byte) (int, error) {
	// NOTE(lxf): If this is the first write, make sure we set the headers/statuscode
	row.headerOnce.Do(row.reconcile)
	if row.headerErr != nil {
		return 0, row.headerErr
	}

	contents := cm.ToList(buf)
	writeResult := row.stream.Write(contents)
	if writeResult.IsErr() {
		if writeResult.Err().Closed() {
			return 0, io.EOF
		}

		return 0, fmt.Errorf("failed to write to response body's stream: %s", writeResult.Err().LastOperationFailed().ToDebugString())
	}

	row.stream.BlockingFlush()

	return int(contents.Len()), nil
}

func (row *responseOutparamWriter) WriteHeader(statusCode int) {
	row.headerOnce.Do(func() {
		row.statuscode = statusCode
		row.reconcile()
	})
}

// reconcile headers from go to wasi
func (row *responseOutparamWriter) reconcileHeaders() error {
	for key, vals := range row.httpHeaders {
		fieldVals := []types.FieldValue{}
		for _, val := range vals {
			fieldVals = append(fieldVals, types.FieldValue(cm.ToList([]uint8(val))))
		}

		if result := row.wasiHeaders.Set(types.FieldKey(key), cm.ToList(fieldVals)); result.IsErr() {
			return fmt.Errorf("failed to set header %s: %s", key, result.Err())
		}
	}

	// NOTE(lxf): once headers are written we clear them out so they can emit http trailers
	row.httpHeaders = http.Header{}

	return nil
}

func (row *responseOutparamWriter) reconcile() {
	if row.headerErr = row.reconcileHeaders(); row.headerErr != nil {
		return
	}

	row.response = types.NewOutgoingResponse(row.wasiHeaders)
	row.response.SetStatusCode(types.StatusCode(row.statuscode))

	bodyResult := row.response.Body()
	if bodyResult.IsErr() {
		row.headerErr = fmt.Errorf("failed to acquire resource handle to response body: %s", bodyResult.Err())
		return
	}
	row.body = bodyResult.OK()

	writeResult := row.body.Write()
	if writeResult.IsErr() {
		row.headerErr = fmt.Errorf("failed to acquire resource handle for response body's stream: %s", writeResult.Err())
		return
	}
	row.stream = writeResult.OK()

	result := cm.OK[cm.Result[types.ErrorCodeShape, types.OutgoingResponse, types.ErrorCode]](row.response)
	types.ResponseOutparamSet(row.outparam, result)
}

func (row *responseOutparamWriter) Close() error {
	row.stream.BlockingFlush()
	row.stream.ResourceDrop()

	var maybeTrailers cm.Option[types.Fields]
	wasiTrailers := types.NewFields()
	for key, vals := range row.httpHeaders {
		fieldVals := []types.FieldValue{}
		for _, val := range vals {
			fieldVals = append(fieldVals, types.FieldValue(cm.ToList([]uint8(val))))
		}

		if result := wasiTrailers.Set(types.FieldKey(key), cm.ToList(fieldVals)); result.IsErr() {
			return fmt.Errorf("failed to set trailer %s: %s", key, result.Err())
		}
	}
	if len(row.httpHeaders) > 0 {
		maybeTrailers = cm.Some(wasiTrailers)
	} else {
		maybeTrailers = cm.None[types.Fields]()
	}

	res := types.OutgoingBodyFinish(*row.body, maybeTrailers)
	if res.IsErr() {
		return fmt.Errorf("failed to set trailer: %v", res.Err())
	}
	return nil
}

// convert the ResponseOutparam to http.ResponseWriter
func NewHttpResponseWriter(out types.ResponseOutparam) *responseOutparamWriter {
	row := &responseOutparamWriter{
		outparam:    out,
		httpHeaders: http.Header{},
		wasiHeaders: types.NewFields(),
		statuscode:  http.StatusOK,
	}

	return row
}

// convert the IncomingRequest to http.Request
func NewHttpRequest(ir IncomingRequest) (req *http.Request, err error) {
	method, err := methodToString(ir.Method())
	if err != nil {
		return nil, err
	}

	authority := "localhost"
	if auth := ir.Authority(); !auth.None() {
		authority = *auth.Some()
	}

	pathWithQuery := "/"
	if p := ir.PathWithQuery(); !p.None() {
		pathWithQuery = *p.Some()
	}

	body, trailers, err := NewIncomingBodyTrailer(ir)
	if err != nil {
		return nil, fmt.Errorf("failed to consume incoming request %s", err)
	}

	url := fmt.Sprintf("http://%s%s", authority, pathWithQuery)
	req, err = http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Trailer = trailers

	toHttpHeader(ir.Headers(), &req.Header)

	req.Host = authority
	req.URL.Host = authority
	req.RequestURI = pathWithQuery

	return req, nil
}

func methodToString(m types.Method) (string, error) {
	if m.Connect() {
		return "CONNECT", nil
	} else if m.Delete() {
		return "DELETE", nil
	} else if m.Get() {
		return "GET", nil
	} else if m.Head() {
		return "HEAD", nil
	} else if m.Options() {
		return "OPTIONS", nil
	} else if m.Patch() {
		return "PATCH", nil
	} else if m.Post() {
		return "POST", nil
	} else if m.Put() {
		return "PUT", nil
	} else if m.Trace() {
		return "TRACE", nil
	} else if other := m.Other(); other != nil {
		return *other, fmt.Errorf("unknown http method '%s'", *other)
	}
	return "", fmt.Errorf("failed to convert http method")
}

func toHttpHeader(src types.Fields, dest *http.Header) {
	for _, f := range src.Entries().Slice() {
		key := string(f.F0)
		value := string(cm.List[uint8](f.F1).Slice())
		dest.Add(key, value)
	}
}

// convert the IncomingRequest to http.Request
func NewOutgoingHttpRequest(req *http.Request) (types.OutgoingRequest, error) {
	headers := types.NewFields()
	if err := toWasiHeader(req.Header, headers); err != nil {
		return types.NewOutgoingRequest(headers), err
	}

	or := types.NewOutgoingRequest(headers)

	or.SetAuthority(cm.Some(req.Host))
	or.SetMethod(toWasiMethod(req.Method))
	or.SetPathWithQuery(cm.Some(req.URL.Path + "?" + req.URL.Query().Encode()))

	switch req.URL.Scheme {
	case "http":
		or.SetScheme(cm.Some(types.SchemeHTTP()))
	case "https":
		or.SetScheme(cm.Some(types.SchemeHTTPS()))
	default:
		or.SetScheme(cm.Some(types.SchemeOther(req.URL.Scheme)))
	}

	return or, nil
}

func toWasiHeader(src http.Header, dest types.Fields) error {
	for k, v := range src {
		key := types.FieldKey(k)
		fieldVals := []types.FieldValue{}

		for _, val := range v {
			fieldVals = append(fieldVals, types.FieldValue(cm.ToList([]uint8(val))))
		}

		// TODO(rjindal): check error
		res := dest.Set(key, cm.ToList(fieldVals))
		if res.IsErr() {
			return fmt.Errorf("failed to set header %s: %s", k, res.Err())
		}
	}

	return nil
}

func toWasiMethod(s string) types.Method {
	switch s {
	case http.MethodConnect:
		return types.MethodConnect()
	case http.MethodDelete:
		return types.MethodDelete()
	case http.MethodGet:
		return types.MethodGet()
	case http.MethodHead:
		return types.MethodHead()
	case http.MethodOptions:
		return types.MethodOptions()
	case http.MethodPatch:
		return types.MethodPatch()
	case http.MethodPost:
		return types.MethodPost()
	case http.MethodPut:
		return types.MethodPut()
	case http.MethodTrace:
		return types.MethodTrace()
	default:
		return types.MethodOther(s)
	}
}
