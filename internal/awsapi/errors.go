package awsapi

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Error is an error response from an AWS API.
type Error struct {
	Service    string // "ec2", "secretsmanager" or "sts"
	Action     string
	StatusCode int
	Code       string
	Message    string
	RequestID  string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("aws %s %s: %s", e.Service, e.Action, e.Code)
	if e.Message != "" {
		s += ": " + e.Message
	}
	s += fmt.Sprintf(" (status %d", e.StatusCode)
	if e.RequestID != "" {
		s += ", request id " + e.RequestID
	}
	return s + ")"
}

// IsCode reports whether err is an API error with one of the codes.
func IsCode(err error, codes ...string) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	for _, c := range codes {
		if e.Code == c {
			return true
		}
	}
	return false
}

// IsAccessDenied reports whether err says the caller's credentials lack the permission. For a
// DryRun call that is the failure case: the real call would be refused.
func IsAccessDenied(err error) bool {
	return IsCode(err, "UnauthorizedOperation", "AccessDenied", "AccessDeniedException")
}

// IsNotFound reports whether err says that something the call named does not exist: an instance,
// an address, an allocation or a secret.
func IsNotFound(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == "ResourceNotFoundException" || strings.HasSuffix(e.Code, ".NotFound")
}

// transportError is a request that got no HTTP answer.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

var throttleCodes = map[string]bool{
	"RequestLimitExceeded":     true,
	"Throttling":               true,
	"ThrottlingException":      true,
	"ThrottledException":       true,
	"TooManyRequestsException": true,
	"RequestThrottled":         true,
	"InternalError":            true,
	"InternalFailure":          true,
	"ServiceUnavailable":       true,
	"Unavailable":              true,
}

// retryable reports whether the same request may succeed when sent again: no answer, a server
// error or a throttle. DryRunOperation (412) and every 4xx that is a refusal are final.
func retryable(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return throttleCodes[e.Code]
}

// parseXMLError reads the error body of the EC2 and STS Query APIs:
// <Response><Errors><Error>...</Error></Errors><RequestID/></Response> for EC2 and
// <ErrorResponse><Error>...</Error><RequestId/></ErrorResponse> for STS.
func parseXMLError(status int, body []byte) *Error {
	var doc struct {
		Errors []struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Errors>Error"`
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
		RequestID  string `xml:"RequestID"`
		RequestId2 string `xml:"RequestId"`
	}
	e := &Error{StatusCode: status}
	if xml.Unmarshal(body, &doc) == nil {
		e.Code, e.Message = doc.Error.Code, doc.Error.Message
		if len(doc.Errors) > 0 {
			e.Code, e.Message = doc.Errors[0].Code, doc.Errors[0].Message
		}
		e.RequestID = doc.RequestID + doc.RequestId2
	}
	if e.Code == "" {
		e.Code = http.StatusText(status)
		e.Message = snippet(body)
	}
	return e
}

// parseJSONError reads the error of an awsJson1.1 service. The code is in the "__type" field or
// the X-Amzn-ErrorType header, possibly as "namespace#Code" or "Code:url".
func parseJSONError(status int, h http.Header, body []byte) *Error {
	var doc struct {
		Type     string `json:"__type"`
		Message  string `json:"Message"`
		MessageL string `json:"message"`
	}
	e := &Error{StatusCode: status, RequestID: h.Get("X-Amzn-RequestId")}
	code := h.Get("X-Amzn-ErrorType")
	if json.Unmarshal(body, &doc) == nil {
		e.Message = doc.Message + doc.MessageL
		if code == "" {
			code = doc.Type
		}
	}
	if i := strings.LastIndexByte(code, '#'); i >= 0 {
		code = code[i+1:]
	}
	code, _, _ = strings.Cut(code, ":")
	e.Code = code
	if e.Code == "" {
		e.Code = http.StatusText(status)
		e.Message = snippet(body)
	}
	return e
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
