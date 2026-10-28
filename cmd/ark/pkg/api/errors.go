package api

// ErrorResponse represents an API error response
type ErrorResponse struct {
	Err ErrorDetail `json:"error"`
}

// ErrorDetail contains detailed error information
type ErrorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (er *ErrorResponse) Error() string {
	return er.Err.Message
}
