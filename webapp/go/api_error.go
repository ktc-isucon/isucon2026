package isureal

import (
	"errors"
	"io"
	"net/http"

	"github.com/labstack/echo/v5"
)

type APIError struct {
	Status int
	Code   string
}

type APIErrorResponse struct {
	Error string `json:"error"`
}

func (e *APIError) Error() string { return e.Code }

func NewAPIError(status int, code string) error {
	return &APIError{Status: status, Code: code}
}

func APIErrorHandler(c *echo.Context, err error) {
	if response, _ := echo.UnwrapResponse(c.Response()); response != nil && response.Committed {
		return
	}
	status, message := http.StatusInternalServerError, err.Error()
	var apiErr *APIError
	var echoErr echo.HTTPStatusCoder
	switch {
	case errors.As(err, &apiErr):
		status, message = apiErr.Status, apiErr.Code
	case errors.As(err, &echoErr):
		status = echoErr.StatusCode()
	}
	if status >= http.StatusInternalServerError {
		c.Logger().Error("request failed", "error", err)
	}
	// 本文を読まずに返して接続を閉じると、アップロード中のnginxが502にしてしまう(IP BANの403など)。
	// 上限つきで読み捨ててからエラーを返す
	if req := c.Request(); req.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, 32<<20))
	}
	body := APIErrorResponse{
		Error: message,
	}
	if writeErr := c.JSON(status, body); writeErr != nil {
		c.Logger().Error("failed to write error response", "error", writeErr)
	}
}
