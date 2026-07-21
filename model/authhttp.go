package model

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

type credentialError struct{ err error }

func (e *credentialError) Error() string { return e.err.Error() }
func (e *credentialError) Unwrap() error { return e.err }

// doTokenRequest sends an authenticated request and gives a recoverable token
// source one chance to replace a rejected credential. Request bodies must be
// rebuilt by build for each attempt.
func doTokenRequest(
	ctx context.Context,
	client *http.Client,
	source TokenSource,
	build func(token string) (*http.Request, error),
) (*http.Response, error) {
	token, err := source.Token(ctx)
	if err != nil {
		return nil, &credentialError{err: err}
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := build(token)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if attempt == 0 {
			if recoverable, ok := source.(RecoverableTokenSource); ok {
				replacement, retry, recoverErr := recoverable.Recover(ctx, resp.StatusCode, token)
				if recoverErr != nil {
					resp.Body.Close()
					return nil, &credentialError{err: fmt.Errorf("recover rejected credential: %w", recoverErr)}
				}
				if retry {
					_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
					resp.Body.Close()
					token = replacement
					continue
				}
			}
		}
		return resp, nil
	}
	return nil, fmt.Errorf("credential recovery exhausted")
}
