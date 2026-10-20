package proxy

import (
	"encoding/json"
	"errors"
	"io"
)

type minimaxPeekReader struct {
	io.Reader
	err error
}

func (r *minimaxPeekReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

var errMinimaxInBandError = errors.New("minimax structured error")

// Codes checked against https://platform.minimaxi.com/docs/api-reference/errorcode
// and the ChatCompletionResp schema at
// https://platform.minimaxi.com/docs/api-reference/text-chat-openai
// on 2026-10-13 (documentation check, not live API acceptance).
// 2056 means M Plan resource limits in five-hour and weekly windows;
// resources recover when the windows expire, with no reset time in the response.
func minimaxReasonForCode(code int64) string {
	switch code {
	case 1004, 2049:
		return "invalid_token"
	case 1008:
		return "billing_error"
	case 2056:
		return "quota_exceeded"
	case 1002, 1041, 2045:
		return "rate_limited"
	}
	return ""
}

func decodeMinimaxBaseResp(body []byte) (int64, bool) {
	var envelope struct {
		BaseResp struct {
			Code *int64 `json:"status_code"`
		} `json:"base_resp"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.BaseResp.Code == nil {
		return 0, false
	}
	return *envelope.BaseResp.Code, true
}

func minimaxErrorStatus(reason string) int {
	switch reason {
	case "invalid_token":
		return 401
	case "billing_error":
		return 402
	}
	return 429
}
