package model

import (
	"context"
	"net/http"
	"testing"
)

type recoveringTokenSource struct {
	token       string
	recoveries  int
	status      int
	rejected    string
	replacement string
}

func (s *recoveringTokenSource) Token(context.Context) (string, error) {
	return s.token, nil
}

func (s *recoveringTokenSource) Recover(_ context.Context, status int, rejectedToken string) (string, bool, error) {
	s.recoveries++
	s.status = status
	s.rejected = rejectedToken
	return s.replacement, status == http.StatusUnauthorized, nil
}

func TestDoTokenRequestRetriesOnceWithReplacement(t *testing.T) {
	source := &recoveringTokenSource{token: "rejected", replacement: "replacement"}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		want := "Bearer rejected"
		status := http.StatusUnauthorized
		if requests == 2 {
			want = "Bearer replacement"
			status = http.StatusOK
		}
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("authorization = %q, want %q", got, want)
		}
		return jsonResponse(status, `{}`), nil
	})}

	resp, err := doTokenRequest(context.Background(), client, source, func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, "https://provider.test/resource", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || requests != 2 {
		t.Fatalf("status=%d requests=%d", resp.StatusCode, requests)
	}
	if source.recoveries != 1 || source.status != http.StatusUnauthorized || source.rejected != "rejected" {
		t.Fatalf("recovery = %+v", source)
	}
}
