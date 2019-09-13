package transport

import (
	"encoding/json"

	"github.com/umbracle/go-web3/jsonrpc/codec"
	"github.com/valyala/fasthttp"
)

// HTTP is an http transport
type HTTP struct {
	addr   string
	client *fasthttp.Client
}

func newHTTP(addr string) *HTTP {
	return &HTTP{
		addr:   addr,
		client: &fasthttp.Client{},
	}
}

// Close implements the transport interface
func (h *HTTP) Close() error {
	return nil
}

// Call implements the transport interface
func (h *HTTP) Call(method string, out interface{}, params ...interface{}) error {
	// Encode json-rpc request
	request := codec.Request{
		Method: method,
	}
	if len(params) > 0 {
		data, err := json.Marshal(params)
		if err != nil {
			return err
		}
		request.Params = data
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI(h.addr)
	req.Header.SetMethod("POST")
	req.Header.SetContentType("application/json")
	req.SetBody(raw)

	if err := h.client.Do(req, res); err != nil {
		return err
	}

	// Decode json-rpc response
	var response codec.Response
	if err := json.Unmarshal(res.Body(), &response); err != nil {
		return err
	}
	if response.Error != nil {
		return response.Error
	}
	if err := json.Unmarshal(response.Result, out); err != nil {
		return err
	}
	return nil
}
