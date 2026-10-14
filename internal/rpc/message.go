package rpc

import (
	"bytes"
	"encoding/json"
	"strconv"
)

const version = "2.0"

// Request is a JSON-RPC 2.0 request or, when ID is nil, a notification.
type Request struct {
	Method string
	// Params is the raw params value (an object or array) or nil when absent.
	Params json.RawMessage
	// ID is the raw request id (a JSON string, number or null), echoed
	// verbatim in the response. A nil ID marks a notification.
	ID json.RawMessage
}

// IsNotification reports whether r carries no id and so expects no response.
func (r Request) IsNotification() bool { return r.ID == nil }

// MarshalJSON encodes r with the mandatory "jsonrpc":"2.0" member.
func (r Request) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
		ID      json.RawMessage `json:"id,omitempty"`
	}{version, r.Method, r.Params, r.ID})
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result and Error is
// meaningful; a nil Error means success.
type Response struct {
	// ID is the raw id of the request being answered, or nil/null when the
	// request's id could not be determined.
	ID     json.RawMessage
	Result json.RawMessage
	Error  *Error
}

// MarshalJSON encodes r with the mandatory "jsonrpc":"2.0" member. The id is
// always emitted (as null when unknown) and a successful response always
// carries a result (null when empty).
func (r Response) MarshalJSON() ([]byte, error) {
	id := r.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	if r.Error != nil {
		return json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   *Error          `json:"error"`
		}{version, id, r.Error})
	}
	res := r.Result
	if len(res) == 0 {
		res = json.RawMessage("null")
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{version, id, res})
}

// incoming is one classified element of an inbound payload. Exactly one of
// its fields is set.
type incoming struct {
	req     *Request  // valid request or notification
	resp    *Response // response to one of our outgoing calls
	invalid *Response // error response to send back for a malformed element
}

// parsePayload classifies an inbound payload. The returned bool reports
// whether the payload was a (non-empty) batch. Payload-level failures (bad
// JSON, empty batch) yield a single invalid item and batch == false.
func parsePayload(data []byte) ([]incoming, bool) {
	data = bytes.TrimSpace(data)
	if !json.Valid(data) {
		return []incoming{invalidItem(nil, CodeParseError, "parse error")}, false
	}
	if len(data) > 0 && data[0] == '[' {
		var elems []json.RawMessage
		if err := json.Unmarshal(data, &elems); err != nil {
			return []incoming{invalidItem(nil, CodeParseError, "parse error")}, false
		}
		if len(elems) == 0 {
			return []incoming{invalidItem(nil, CodeInvalidRequest, "invalid request: empty batch")}, false
		}
		items := make([]incoming, len(elems))
		for i, e := range elems {
			items[i] = parseElement(e)
		}
		return items, true
	}
	return []incoming{parseElement(data)}, false
}

func invalidItem(id json.RawMessage, code int, msg string) incoming {
	return incoming{invalid: &Response{ID: id, Error: NewError(code, msg)}}
}

// validID reports whether raw is a legal id value (string, number or null).
func validID(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '"', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return false
}

func parseElement(raw json.RawMessage) incoming {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return invalidItem(nil, CodeInvalidRequest, "invalid request")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return invalidItem(nil, CodeInvalidRequest, "invalid request")
	}

	id, hasID := m["id"]
	var respID json.RawMessage // id used when answering with an error
	if hasID && validID(id) {
		respID = id
	}

	if methodRaw, ok := m["method"]; ok {
		var jv string
		if json.Unmarshal(m["jsonrpc"], &jv) != nil || jv != version {
			return invalidItem(respID, CodeInvalidRequest, `invalid request: "jsonrpc" must be "2.0"`)
		}
		var method string
		if json.Unmarshal(methodRaw, &method) != nil || method == "" {
			return invalidItem(respID, CodeInvalidRequest, "invalid request: method must be a non-empty string")
		}
		if hasID && !validID(id) {
			return invalidItem(nil, CodeInvalidRequest, "invalid request: id must be a string, number or null")
		}
		req := &Request{Method: method}
		if p, ok := m["params"]; ok {
			p = bytes.TrimSpace(p)
			if len(p) == 0 || (p[0] != '{' && p[0] != '[') {
				return invalidItem(respID, CodeInvalidRequest, "invalid request: params must be an object or array")
			}
			req.Params = p
		}
		if hasID {
			req.ID = bytes.TrimSpace(id)
		}
		return incoming{req: req}
	}

	result, hasResult := m["result"]
	errRaw, hasErr := m["error"]
	if hasResult != hasErr && hasID {
		var jv string
		if json.Unmarshal(m["jsonrpc"], &jv) == nil && jv == version {
			resp := &Response{ID: bytes.TrimSpace(id)}
			if hasResult {
				resp.Result = result
				return incoming{resp: resp}
			}
			var e Error
			if json.Unmarshal(errRaw, &e) == nil {
				resp.Error = &e
				return incoming{resp: resp}
			}
		}
	}
	return invalidItem(respID, CodeInvalidRequest, "invalid request")
}

// callID returns the numeric id of a response to one of our own calls.
func callID(raw json.RawMessage) (uint64, bool) {
	n, err := strconv.ParseUint(string(bytes.TrimSpace(raw)), 10, 64)
	return n, err == nil
}
